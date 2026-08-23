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

// StateMachine is a quest's shape: an initial state, the states, and the
// directed edges between them. It is JSON in the database so a DM can
// describe a quest that branches; this type is the validated form of that
// JSON.
type StateMachine struct {
	Initial string      `json:"initial"`
	States  []string    `json:"states"`
	Edges   []StateEdge `json:"edges"`
}

// StateEdge is one legal move from one state to another.
type StateEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ParseStateMachine decodes and validates a quest's machine JSON: at least one
// state, every state name non-empty and unique, the initial state present,
// and every edge endpoint a declared state.
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

// Validate checks the machine's shape. States are compared exactly.
func (m StateMachine) Validate() error {
	if len(m.States) == 0 {
		return fmt.Errorf("%w: a state machine needs at least one state", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, st := range m.States {
		st = strings.TrimSpace(st)
		if st == "" {
			return fmt.Errorf("%w: state names must not be empty", ErrInvalid)
		}
		if seen[st] {
			return fmt.Errorf("%w: state %q declared twice", ErrInvalid, st)
		}
		seen[st] = true
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

// HasEdge reports whether from -> to is a legal move.
func (m StateMachine) HasEdge(from, to string) bool {
	for _, e := range m.Edges {
		if e.From == from && e.To == to {
			return true
		}
	}
	return false
}

// Marshal encodes the machine for storage.
func (m StateMachine) Marshal() (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode state machine: %w", err)
	}
	return string(b), nil
}

// Quest is a state machine in progress: which state it sits in and the moves
// that got it there.
type Quest struct {
	ID           string
	CampaignID   string
	Name         string
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

// CreateQuest adds a quest in its initial state.
func (s *Store) CreateQuest(ctx context.Context, campaignID, name string, machine StateMachine) (*Quest, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: quest name is required", ErrInvalid)
	}
	if err := machine.Validate(); err != nil {
		return nil, err
	}
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}
	machineJSON, err := machine.Marshal()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	q := &Quest{
		ID: uuid.NewString(), CampaignID: campaignID, Name: name,
		Machine: machine, CurrentState: machine.Initial,
		CreatedAt: now, UpdatedAt: now,
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO quests (id, campaign_id, name, state_machine, current_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		q.ID, q.CampaignID, q.Name, machineJSON, q.CurrentState, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("insert quest: %w", err)
	}
	return q, nil
}

const questCols = `id, campaign_id, name, state_machine, current_state, created_at, updated_at`

func scanQuest(row interface{ Scan(...any) error }) (*Quest, error) {
	var (
		q            Quest
		machineJSON  string
		createdMilli int64
		updatedMilli int64
	)
	if err := row.Scan(&q.ID, &q.CampaignID, &q.Name, &machineJSON, &q.CurrentState,
		&createdMilli, &updatedMilli); err != nil {
		return nil, err
	}
	m, err := ParseStateMachine(machineJSON)
	if err != nil {
		return nil, fmt.Errorf("quest %s carries an invalid state machine: %w", q.ID, err)
	}
	q.Machine = m
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

// TransitionQuest moves a quest from its current state to toState. The move
// must be an edge the machine actually has — a quest is a state machine, not
// a checkbox — and the move is recorded with the event that caused it when
// one is given. The event must belong to the same campaign.
func (s *Store) TransitionQuest(ctx context.Context, campaignID, questID, toState, eventID string) (*Quest, error) {
	q, err := s.GetQuest(ctx, ScopeDM, campaignID, questID)
	if err != nil {
		return nil, err
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
