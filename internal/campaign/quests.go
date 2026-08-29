package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// States builds a state list from bare keys — the original plain-string
// shape, as code. Labels and terminal markers are set by the caller after,
// or written as full struct literals.
func States(keys ...string) []State {
	out := make([]State, len(keys))
	for i, k := range keys {
		out[i] = State{Key: k}
	}
	return out
}

// StateMachine is a quest's shape: an initial state, the states, and the
// directed edges between them. It is JSON in the database so a DM can
// describe a quest that branches; this type is the validated form of that
// JSON.
//
// Two shapes of `states` parse: the original array of plain strings — every
// quest authored before MAD-369 — and the keyed form carrying labels, detail
// and terminal markers. Both are the same machine once parsed; Marshal always
// writes the keyed form.
type StateMachine struct {
	Initial string      `json:"initial"`
	States  []State     `json:"states"`
	Edges   []StateEdge `json:"edges"`
}

// State is one node of the machine: its key (the identifier edges and
// transitions name), a human label, prose detail, and whether it is an
// ending. Terminal is "" for a passing state and success | failure |
// abandoned for an ending.
type State struct {
	Key      string `json:"key"`
	Label    string `json:"label,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Terminal string `json:"terminal,omitempty"`
}

// Terminal markers. A terminal state is an ending: the machine has no reason
// to leave it, and quest_dead_end does not fire on one.
const (
	TerminalNone      = ""
	TerminalSuccess   = "success"
	TerminalFailure   = "failure"
	TerminalAbandoned = "abandoned"
)

var terminalMarkers = map[string]bool{
	TerminalNone: true, TerminalSuccess: true,
	TerminalFailure: true, TerminalAbandoned: true,
}

// StateEdge is one legal move from one state to another, named: the label is
// what distinguishes two branches out of one state ("trust the survivor" vs
// "accuse the survivor"), and requires names the fact ids the move presumes
// discovered — the join quest_transition_ungrounded checks against awareness.
type StateEdge struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Label    string   `json:"label,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Requires []string `json:"requires,omitempty"`
}

// UnmarshalJSON decodes both `states` shapes: an array of plain strings (the
// original form) and an array of keyed state objects. Plain strings become
// keyed states with empty labels, so nothing downstream sees a difference.
func (m *StateMachine) UnmarshalJSON(b []byte) error {
	var raw struct {
		Initial string          `json:"initial"`
		States  json.RawMessage `json:"states"`
		Edges   []StateEdge     `json:"edges"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := StateMachine{Initial: raw.Initial, Edges: raw.Edges}
	trimmed := strings.TrimSpace(string(raw.States))
	if trimmed != "" && trimmed != "null" {
		// A JSON string (not array) is invalid either way; the array forms
		// are told apart by their first element: "a" is the original
		// plain-string shape, {...} the keyed one.
		first := strings.TrimSpace(trimmed[1:])
		if first != "" && first[0] == '"' {
			var keys []string
			if err := json.Unmarshal(raw.States, &keys); err != nil {
				return err
			}
			for _, k := range keys {
				out.States = append(out.States, State{Key: k})
			}
		} else if err := json.Unmarshal(raw.States, &out.States); err != nil {
			return err
		}
	}
	*m = out
	return nil
}

// ParseStateMachine decodes and validates a quest's machine JSON: at least one
// state, every state key non-empty and unique with a legal terminal marker,
// the initial state present, and every edge endpoint a declared state.
func ParseStateMachine(raw string) (StateMachine, error) {
	var m StateMachine
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return StateMachine{}, fmt.Errorf("%w: state machine is not valid JSON: %v", ErrInvalid, err)
	}
	if err := m.Validate(); err != nil {
		return StateMachine{}, err
	}
	return m, nil
}

// Validate checks the machine's shape. Keys are compared exactly.
func (m StateMachine) Validate() error {
	if len(m.States) == 0 {
		return fmt.Errorf("%w: a state machine needs at least one state", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, st := range m.States {
		key := strings.TrimSpace(st.Key)
		if key == "" {
			return fmt.Errorf("%w: state names must not be empty", ErrInvalid)
		}
		if seen[key] {
			return fmt.Errorf("%w: state %q declared twice", ErrInvalid, key)
		}
		if !terminalMarkers[st.Terminal] {
			return fmt.Errorf("%w: state %q terminal %q is not one of success, failure or abandoned", ErrInvalid, key, st.Terminal)
		}
		seen[key] = true
	}
	if !seen[m.Initial] {
		return fmt.Errorf("%w: initial state %q is not a declared state", ErrInvalid, m.Initial)
	}
	for _, e := range m.Edges {
		if !seen[e.From] {
			return fmt.Errorf("%w: edge from undeclared state %q", ErrInvalid, e.From)
		}
		if !seen[e.To] {
			return fmt.Errorf("%w: edge to undeclared state %q", ErrInvalid, e.To)
		}
	}
	return nil
}

// State returns the declared state with this key.
func (m StateMachine) State(key string) (State, bool) {
	for _, st := range m.States {
		if st.Key == key {
			return st, true
		}
	}
	return State{}, false
}

// IsTerminal reports whether the state is an ending (success, failure or
// abandoned — anything but a passing state).
func (m StateMachine) IsTerminal(key string) bool {
	st, ok := m.State(key)
	return ok && st.Terminal != TerminalNone
}

// Terminal returns the state's terminal marker, "" for a passing state and
// for an undeclared one (the checks fire on undeclared states separately).
func (m StateMachine) Terminal(key string) string {
	st, ok := m.State(key)
	if !ok {
		return TerminalNone
	}
	return st.Terminal
}

// HasEdge reports whether from -> to is a legal move.
func (m StateMachine) HasEdge(from, to string) bool {
	for _, e := range m.Edges {
		if e.From == from && e.To == to {
			return true
		}
	}
	return false
}

// Edge returns the declared edge from -> to. Two branches out of one state
// are distinguished by their labels, so parallel edges (same from/to, same
// label) collapse to the first.
func (m StateMachine) Edge(from, to string) (StateEdge, bool) {
	for _, e := range m.Edges {
		if e.From == from && e.To == to {
			return e, true
		}
	}
	return StateEdge{}, false
}

// Reachable returns the set of state keys reachable from start along declared
// edges, start included.
func (m StateMachine) Reachable(start string) map[string]bool {
	adj := map[string][]string{}
	for _, e := range m.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	out := map[string]bool{start: true}
	frontier := []string{start}
	for len(frontier) > 0 {
		var next []string
		for _, from := range frontier {
			for _, to := range adj[from] {
				if !out[to] {
					out[to] = true
					next = append(next, to)
				}
			}
		}
		frontier = next
	}
	return out
}

// Marshal encodes the machine for storage. The keyed state form is always
// written, whatever shape parsed in.
func (m StateMachine) Marshal() (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode state machine: %w", err)
	}
	return string(b), nil
}

// Quest statuses. 'abandoned' is also the soft delete: DELETE on the REST
// surface sets it, and nothing else removes a quest.
const (
	QuestActive    = "active"
	QuestComplete  = "complete"
	QuestFailed    = "failed"
	QuestAbandoned = "abandoned"
)

var questStatuses = map[string]bool{
	QuestActive: true, QuestComplete: true, QuestFailed: true, QuestAbandoned: true,
}

// Quest visibility. A quest defaults to secret: one the party has not been
// offered is DM planning material, and the player journal reads public ones
// only.
const (
	QuestVisibilityPublic = "public"
	QuestVisibilitySecret = "secret"
)

var questVisibilities = map[string]bool{
	QuestVisibilityPublic: true, QuestVisibilitySecret: true,
}

// Quest entity roles: what an entity is to a quest, one join table rather
// than five nullable columns, because a quest touches many entities.
const (
	QuestRoleGiver    = "giver"
	QuestRoleSubject  = "subject"
	QuestRoleObstacle = "obstacle"
	QuestRoleReward   = "reward"
	QuestRoleSite     = "site"
)

var questEntityRoles = map[string]bool{
	QuestRoleGiver: true, QuestRoleSubject: true, QuestRoleObstacle: true,
	QuestRoleReward: true, QuestRoleSite: true,
}

// Quest state fact dispositions: a state that requires a fact presumes the
// party discovered it; a state that reveals one is a clue path.
const (
	QuestFactRequires = "requires"
	QuestFactReveals  = "reveals"
)

var questFactDispositions = map[string]bool{
	QuestFactRequires: true, QuestFactReveals: true,
}

// Quest is a state machine in progress: which state it sits in and the moves
// that got it there.
type Quest struct {
	ID           string
	CampaignID   string
	Name         string
	Summary      string
	Status       string
	Visibility   string
	ActID        string
	Machine      StateMachine
	CurrentState string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// QuestTransition is one recorded move, optionally tied to the event that
// caused it.
type QuestTransition struct {
	ID        string
	QuestID   string
	FromState string
	ToState   string
	EventID   string
	CreatedAt time.Time
}

// QuestEntity is one link from a quest into the entity graph.
type QuestEntity struct {
	ID        string
	QuestID   string
	EntityID  string
	Role      string
	CreatedAt time.Time
}

// QuestStateFact ties one state of a quest to one fact: what the state
// presumes discovered (requires) or puts on a clue path (reveals).
type QuestStateFact struct {
	ID          string
	QuestID     string
	StateKey    string
	FactID      string
	Disposition string
	CreatedAt   time.Time
}

// QuestInput is everything a quest is created with. Visibility and status
// default: a new quest is active and secret until the DM says otherwise.
type QuestInput struct {
	Name       string
	Summary    string
	Machine    StateMachine
	Visibility string // "" -> secret
	ActID      string // "" -> none; no foreign key by design (MAD-369)
}

// CreateQuest adds a quest in its initial state.
func (s *Store) CreateQuest(ctx context.Context, campaignID string, in QuestInput) (*Quest, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: quest name is required", ErrInvalid)
	}
	if err := in.Machine.Validate(); err != nil {
		return nil, err
	}
	visibility := in.Visibility
	if visibility == "" {
		visibility = QuestVisibilitySecret
	}
	if !questVisibilities[visibility] {
		return nil, fmt.Errorf("%w: quest visibility %q", ErrInvalid, visibility)
	}
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}
	if in.ActID != "" {
		if err := s.actInCampaign(ctx, in.ActID, campaignID); err != nil {
			return nil, err
		}
	}
	machineJSON, err := in.Machine.Marshal()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	q := &Quest{
		ID: uuid.NewString(), CampaignID: campaignID, Name: name, Summary: in.Summary,
		Status: QuestActive, Visibility: visibility, ActID: in.ActID,
		Machine: in.Machine, CurrentState: in.Machine.Initial,
		CreatedAt: now, UpdatedAt: now,
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO quests (id, campaign_id, name, summary, status, visibility, act_id, state_machine, current_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		q.ID, q.CampaignID, q.Name, q.Summary, q.Status, q.Visibility, q.ActID,
		machineJSON, q.CurrentState, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("insert quest: %w", err)
	}
	return q, nil
}

// actInCampaign checks that an act id names an act of this campaign. The
// quests table deliberately carries no foreign key to acts (MAD-369): acts
// and quests may be authored in either order, and the dangling_reference
// check sweeps whatever bypasses this validation.
func (s *Store) actInCampaign(ctx context.Context, id, campaignID string) error {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM acts WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: act %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("check act: %w", err)
	}
	return nil
}

const questCols = `id, campaign_id, name, summary, status, visibility, act_id, state_machine, current_state, created_at, updated_at`

func scanQuest(row interface{ Scan(...any) error }) (*Quest, error) {
	var (
		q            Quest
		machineJSON  string
		actID        sql.NullString
		createdMilli int64
		updatedMilli int64
	)
	if err := row.Scan(&q.ID, &q.CampaignID, &q.Name, &q.Summary, &q.Status, &q.Visibility,
		&actID, &machineJSON, &q.CurrentState, &createdMilli, &updatedMilli); err != nil {
		return nil, err
	}
	m, err := ParseStateMachine(machineJSON)
	if err != nil {
		return nil, fmt.Errorf("quest %s carries an invalid state machine: %w", q.ID, err)
	}
	q.Machine = m
	q.ActID = actID.String
	q.CreatedAt = time.UnixMilli(createdMilli).UTC()
	q.UpdatedAt = time.UnixMilli(updatedMilli).UTC()
	return &q, nil
}

// GetQuest returns one quest of a campaign. DM-scope reads only: a quest's
// shape and state are DM planning material.
func (s *Store) GetQuest(ctx context.Context, scope Scope, campaignID, id string) (*Quest, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+questCols+` FROM quests WHERE id = ? AND campaign_id = ?`, id, campaignID)
	q, err := scanQuest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: quest %s", ErrNotFound, id)
	}
	return q, err
}

// ListQuests returns a campaign's quests by name. DM-scope reads only.
func (s *Store) ListQuests(ctx context.Context, scope Scope, campaignID string) ([]Quest, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+questCols+` FROM quests WHERE campaign_id = ? ORDER BY name COLLATE NOCASE`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list quests: %w", err)
	}
	defer rows.Close()
	var out []Quest
	for rows.Next() {
		q, err := scanQuest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *q)
	}
	return out, rows.Err()
}

// QuestUpdate patches a quest. A nil field leaves that column alone; an
// act_id of "" clears it. Replacing the machine may not orphan history: a
// state or edge a recorded transition already used must survive the edit,
// and the refusal names the transition that blocks it.
type QuestUpdate struct {
	Name       *string
	Summary    *string
	Status     *string
	Visibility *string
	ActID      *string
	Machine    *StateMachine
}

// UpdateQuest applies a patch to one quest of a campaign.
func (s *Store) UpdateQuest(ctx context.Context, campaignID, questID string, up QuestUpdate) (*Quest, error) {
	q, err := s.GetQuest(ctx, ScopeDM, campaignID, questID)
	if err != nil {
		return nil, err
	}
	sets := []string{"updated_at = ?"}
	args := []any{time.Now().UTC().UnixMilli()}
	if up.Name != nil {
		name := strings.TrimSpace(*up.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: quest name is required", ErrInvalid)
		}
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if up.Summary != nil {
		sets = append(sets, "summary = ?")
		args = append(args, *up.Summary)
	}
	if up.Status != nil {
		if !questStatuses[*up.Status] {
			return nil, fmt.Errorf("%w: quest status %q", ErrInvalid, *up.Status)
		}
		sets = append(sets, "status = ?")
		args = append(args, *up.Status)
	}
	if up.Visibility != nil {
		if !questVisibilities[*up.Visibility] {
			return nil, fmt.Errorf("%w: quest visibility %q", ErrInvalid, *up.Visibility)
		}
		sets = append(sets, "visibility = ?")
		args = append(args, *up.Visibility)
	}
	if up.ActID != nil {
		if *up.ActID != "" {
			if err := s.actInCampaign(ctx, *up.ActID, campaignID); err != nil {
				return nil, err
			}
		}
		sets = append(sets, "act_id = ?")
		// The column is NOT NULL: clearing is the empty string, never NULL.
		args = append(args, *up.ActID)
	}
	if up.Machine != nil {
		if err := up.Machine.Validate(); err != nil {
			return nil, err
		}
		// An edit may not orphan history: the state the quest sits in, every
		// state and every edge a recorded transition used. The refusal names
		// the transition that blocks it.
		if _, ok := up.Machine.State(q.CurrentState); !ok {
			return nil, fmt.Errorf("%w: quest %s sits in state %q; an edit may not remove the state it occupies",
				ErrInvalid, questID, q.CurrentState)
		}
		history, err := s.QuestTransitions(ctx, ScopeDM, campaignID, questID)
		if err != nil {
			return nil, err
		}
		for _, t := range history {
			if err := machineSurvivesTransition(*up.Machine, t); err != nil {
				return nil, fmt.Errorf("%w: transition %s (%s -> %s): %w",
					ErrInvalid, t.ID, t.FromState, t.ToState, err)
			}
		}
		machineJSON, err := up.Machine.Marshal()
		if err != nil {
			return nil, err
		}
		sets = append(sets, "state_machine = ?")
		args = append(args, machineJSON)
	}
	args = append(args, questID, campaignID)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE quests SET `+strings.Join(sets, ", ")+` WHERE id = ? AND campaign_id = ?`, args...); err != nil {
		return nil, fmt.Errorf("update quest: %w", err)
	}
	return s.GetQuest(ctx, ScopeDM, campaignID, questID)
}

// machineSurvivesTransition refuses a machine that drops a state or the edge
// one recorded move still used: a quest whose own history is permanently
// invalid under quest_transition_invalid is worse than a refused edit.
func machineSurvivesTransition(m StateMachine, t QuestTransition) error {
	if _, ok := m.State(t.FromState); !ok {
		return fmt.Errorf("the machine no longer declares state %q", t.FromState)
	}
	if _, ok := m.State(t.ToState); !ok {
		return fmt.Errorf("the machine no longer declares state %q", t.ToState)
	}
	if !m.HasEdge(t.FromState, t.ToState) {
		return fmt.Errorf("the machine no longer has the edge %s -> %s", t.FromState, t.ToState)
	}
	return nil
}

// DeleteQuest is the soft delete: the quest's status becomes 'abandoned' and
// everything — machine, transitions, links — survives. A campaign's own
// history is not removable.
func (s *Store) DeleteQuest(ctx context.Context, campaignID, questID string) (*Quest, error) {
	abandoned := QuestAbandoned
	return s.UpdateQuest(ctx, campaignID, questID, QuestUpdate{Status: &abandoned})
}

// TransitionQuest moves a quest from its current state to toState. The move
// must be an edge the machine actually has — a quest is a state machine, not
// a checkbox — and the move is recorded with the event that caused it when
// one is given. The event must belong to the same campaign. Only an active
// quest moves: complete, failed and abandoned are endings.
func (s *Store) TransitionQuest(ctx context.Context, campaignID, questID, toState, eventID string) (*Quest, error) {
	q, err := s.GetQuest(ctx, ScopeDM, campaignID, questID)
	if err != nil {
		return nil, err
	}
	if q.Status != QuestActive {
		return nil, fmt.Errorf("%w: quest %q is %s; only an active quest moves", ErrInvalid, q.Name, q.Status)
	}
	if !q.Machine.HasEdge(q.CurrentState, toState) {
		return nil, fmt.Errorf("%w: quest %q cannot move %s -> %s; the machine has no such edge",
			ErrInvalid, q.Name, q.CurrentState, toState)
	}
	if eventID != "" {
		if err := s.eventInCampaign(ctx, eventID, campaignID); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("quest tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quest_transitions (id, quest_id, from_state, to_state, event_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), questID, q.CurrentState, toState, nullString(eventID), now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert transition: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE quests SET current_state = ?, updated_at = ? WHERE id = ?`, toState, now.UnixMilli(), questID); err != nil {
		return nil, fmt.Errorf("update quest state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("quest commit: %w", err)
	}
	q.CurrentState = toState
	q.UpdatedAt = now
	return q, nil
}

// QuestTransitions lists a quest's moves in the order they happened.
// DM-scope reads only.
func (s *Store) QuestTransitions(ctx context.Context, scope Scope, campaignID, questID string) ([]QuestTransition, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	if _, err := s.GetQuest(ctx, ScopeDM, campaignID, questID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, quest_id, from_state, to_state, event_id, created_at
		  FROM quest_transitions WHERE quest_id = ? ORDER BY rowid`, questID)
	if err != nil {
		return nil, fmt.Errorf("list transitions: %w", err)
	}
	defer rows.Close()
	var out []QuestTransition
	for rows.Next() {
		var (
			t       QuestTransition
			event   sql.NullString
			created int64
		)
		if err := rows.Scan(&t.ID, &t.QuestID, &t.FromState, &t.ToState, &event, &created); err != nil {
			return nil, err
		}
		t.EventID = event.String
		t.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

/* ---------- the links into the graph and the knowledge layer ---------- */

// AddQuestEntity links an entity into a quest. The entity must be a live
// member of the same campaign; the role is the vocabulary quest_entities
// checks.
func (s *Store) AddQuestEntity(ctx context.Context, campaignID, questID, entityID, role string) (*QuestEntity, error) {
	if !questEntityRoles[role] {
		return nil, fmt.Errorf("%w: quest entity role %q", ErrInvalid, role)
	}
	if _, err := s.GetQuest(ctx, ScopeDM, campaignID, questID); err != nil {
		return nil, err
	}
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM entities WHERE id = ? AND campaign_id = ?`, entityID, campaignID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: entity %s", ErrNotFound, entityID)
	}
	if err != nil {
		return nil, fmt.Errorf("check quest entity: %w", err)
	}
	if status == StatusDeleted {
		return nil, fmt.Errorf("%w: entity %s is deleted", ErrInvalid, entityID)
	}
	link := &QuestEntity{
		ID: uuid.NewString(), QuestID: questID, EntityID: entityID,
		Role: role, CreatedAt: time.Now().UTC(),
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO quest_entities (id, quest_id, entity_id, role, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		link.ID, link.QuestID, link.EntityID, link.Role, link.CreatedAt.UnixMilli())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("%w: entity %s already linked as %q", ErrAlreadyExists, entityID, role)
		}
		return nil, fmt.Errorf("insert quest entity: %w", err)
	}
	return link, nil
}

// RemoveQuestEntity unlinks an entity from a quest. An empty role removes
// every role the entity holds in the quest; a named role removes just that
// one.
func (s *Store) RemoveQuestEntity(ctx context.Context, campaignID, questID, entityID, role string) error {
	if role != "" && !questEntityRoles[role] {
		return fmt.Errorf("%w: quest entity role %q", ErrInvalid, role)
	}
	if _, err := s.GetQuest(ctx, ScopeDM, campaignID, questID); err != nil {
		return err
	}
	q := `DELETE FROM quest_entities WHERE quest_id = ? AND entity_id = ?`
	args := []any{questID, entityID}
	if role != "" {
		q += ` AND role = ?`
		args = append(args, role)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("delete quest entity: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: entity %s is not linked to quest %s", ErrNotFound, entityID, questID)
	}
	return nil
}

// QuestEntities lists a quest's links into the entity graph, by role then
// entity. DM-scope reads only.
func (s *Store) QuestEntities(ctx context.Context, scope Scope, campaignID, questID string) ([]QuestEntity, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	if _, err := s.GetQuest(ctx, ScopeDM, campaignID, questID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, quest_id, entity_id, role, created_at
		  FROM quest_entities WHERE quest_id = ? ORDER BY role, entity_id`, questID)
	if err != nil {
		return nil, fmt.Errorf("list quest entities: %w", err)
	}
	defer rows.Close()
	var out []QuestEntity
	for rows.Next() {
		var (
			l       QuestEntity
			created int64
		)
		if err := rows.Scan(&l.ID, &l.QuestID, &l.EntityID, &l.Role, &created); err != nil {
			return nil, err
		}
		l.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetQuestStateFact ties one state of a quest to one fact. The state must be
// one the quest's machine declares and the fact one of the campaign's; the
// disposition is requires or reveals. No REST surface carries this yet — the
// generators (MAD-371) and hand-authored campaigns are the writers.
func (s *Store) SetQuestStateFact(ctx context.Context, campaignID, questID, stateKey, factID, disposition string) (*QuestStateFact, error) {
	if !questFactDispositions[disposition] {
		return nil, fmt.Errorf("%w: disposition %q", ErrInvalid, disposition)
	}
	q, err := s.GetQuest(ctx, ScopeDM, campaignID, questID)
	if err != nil {
		return nil, err
	}
	if _, ok := q.Machine.State(stateKey); !ok {
		return nil, fmt.Errorf("%w: state %q is not a declared state of quest %q", ErrInvalid, stateKey, q.Name)
	}
	var one int
	err = s.db.QueryRowContext(ctx,
		`SELECT 1 FROM facts WHERE id = ? AND campaign_id = ?`, factID, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: fact %s", ErrNotFound, factID)
	}
	if err != nil {
		return nil, fmt.Errorf("check fact: %w", err)
	}
	row := &QuestStateFact{
		ID: uuid.NewString(), QuestID: questID, StateKey: stateKey,
		FactID: factID, Disposition: disposition, CreatedAt: time.Now().UTC(),
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO quest_state_facts (id, quest_id, state_key, fact_id, disposition, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		row.ID, row.QuestID, row.StateKey, row.FactID, row.Disposition, row.CreatedAt.UnixMilli())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("%w: fact %s is already tied to state %q", ErrAlreadyExists, factID, stateKey)
		}
		return nil, fmt.Errorf("insert quest state fact: %w", err)
	}
	return row, nil
}

// QuestStateFacts lists a quest's state-to-fact ties. DM-scope reads only.
func (s *Store) QuestStateFacts(ctx context.Context, scope Scope, campaignID, questID string) ([]QuestStateFact, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	if _, err := s.GetQuest(ctx, ScopeDM, campaignID, questID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, quest_id, state_key, fact_id, disposition, created_at
		  FROM quest_state_facts WHERE quest_id = ? ORDER BY state_key, fact_id`, questID)
	if err != nil {
		return nil, fmt.Errorf("list quest state facts: %w", err)
	}
	defer rows.Close()
	var out []QuestStateFact
	for rows.Next() {
		var (
			r       QuestStateFact
			created int64
		)
		if err := rows.Scan(&r.ID, &r.QuestID, &r.StateKey, &r.FactID, &r.Disposition, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
