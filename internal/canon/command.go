package canon

// The natural-language command interface (MAD-363): the AI as the command
// line for the campaign database. "Create a level 5 necromancer who
// secretly works for the Duke" stages a structured NPC; "put him in
// Blackwater" stages a relationship; "add him to next session" writes a
// scene cast row. Every graph mutation goes through StageBatch behind the
// one review gate; the spine rows a DM writes by hand (cast, aliases) are
// written the way the scene designer writes them — directly, at DM scope,
// which is the only scope this surface serves.
//
// Compute the structure first: the model never sees the campaign as
// free-form text to answer, and the server never sees SQL or trusted JSON.
// The model fills ONE slot frame drawn from a closed vocabulary built from
// the campaign's own schema — entity kinds, the controlled relationship
// types, predicates already in use, the cast roles, the next session's
// scenes — plus the entity roster it may copy names from. Then everything
// deterministic happens here:
//
//   - References resolve against the campaign, never the model's memory:
//     every entity slot goes through campaign.ResolveName (aliases
//     included). Exactly one match resolves; zero or many is a clarifying
//     question with the candidates attached, and nothing is staged.
//   - Pronouns bind through an explicit referent stack persisted in
//     command_log: "him" is the entity the previous command proposed, and
//     only while that proposal is still live — accepted means the entity
//     exists (the review's result_ref), open means ask (the DM has not
//     decided), dismissed means the referent expired.
//   - A create whose name already resolves to one entity would mint a
//     near-duplicate — the entity_merge_candidate problem — so it proposes
//     a merge (an alias on the existing entity) instead of a second body.
//   - "undo" drops the newest open command batch: nothing was written, so
//     undoing costs nothing. That is the payoff for staging.
//
// A verb the vocabulary does not cover says so plainly; a model fill the
// cross-field parser refuses becomes a question, not a guess.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

/* ---------- the closed vocabulary ---------- */

// Command verbs. This list is the whole interface: a command the model
// cannot map onto one of them is answered with "not a command", never with
// a plausible wrong mutation.
const (
	CommandVerbCreateEntity = "create_entity"
	CommandVerbCreateRel    = "create_relationship"
	CommandVerbCreateFact   = "create_fact"
	CommandVerbAddToScene   = "add_to_scene"
	CommandVerbMergeNames   = "merge_names"
	CommandVerbNone         = "none"
)

var commandVerbs = []string{
	CommandVerbCreateEntity, CommandVerbCreateRel, CommandVerbCreateFact,
	CommandVerbAddToScene, CommandVerbMergeNames, CommandVerbNone,
}

// pronounTokens are the references that bind through the referent stack
// instead of ResolveName. Deliberately tiny and objective: anything looser
// ("the old man") is a name the campaign may or may not know.
var pronounTokens = map[string]bool{
	"him": true, "her": true, "it": true, "them": true, "they": true,
	"that": true, "this": true, "he": true, "she": true,
}

// isPronoun reports whether a filled slot is a bare pronoun.
func isPronoun(ref string) bool {
	return pronounTokens[strings.ToLower(strings.TrimSpace(ref))]
}

// commandUndoTexts are the deterministic undo recognitions: no model call,
// because undo must work exactly when nothing else does.
var commandUndoTexts = map[string]bool{
	"undo": true, "undo that": true, "undo it": true, "undo this": true,
	"drop that": true, "drop it": true,
}

// Bounds: commands are short by construction, and every list the prompt
// carries is capped so the vocabulary stays bounded on a 300-session
// campaign.
const (
	commandMaxLen        = 600
	commandRosterCap     = 80
	commandPredicateCap  = 40
	commandReferentRows  = 10
	commandMissCandidate = 6
)

/* ---------- results ---------- */

// CommandCandidate is one candidate a clarifying question attaches: an
// entity the DM may have meant, a scene to choose, a proposal to accept.
type CommandCandidate struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Kind    string `json:"kind,omitempty"`
	Summary string `json:"summary,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Clarification is the question an ambiguous command comes back as. It
// stages nothing and writes nothing.
type Clarification struct {
	Question   string             `json:"question"`
	Candidates []CommandCandidate `json:"candidates,omitempty"`
}

// Referent is one entity proposal a command left on the referent stack:
// the review that carries it, the name a later pronoun resolves to, and
// the kind for rendering. Its lifetime is the review's.
type Referent struct {
	ReviewID string `json:"review_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// Command result kinds — the response shapes a surface renders, and the
// command_log rows' CHECK list.
const (
	CommandResultBatch       = "batch"
	CommandResultQuestion    = "question"
	CommandResultUnsupported = "unsupported"
	CommandResultUndo        = "undo"
	CommandResultWritten     = "written"
	CommandResultNoop        = "noop"
)

// CommandResult is one command exchange's outcome. Exactly one of
// Batch / Question / Written carries the payload when the kind says so.
type CommandResult struct {
	Kind      string         `json:"kind"`
	Verb      string         `json:"verb,omitempty"`
	Message   string         `json:"message"`
	Batch     *Batch         `json:"batch,omitempty"`
	Question  *Clarification `json:"question,omitempty"`
	Written   map[string]any `json:"written,omitempty"`
	Referents []Referent     `json:"referents,omitempty"`
	CreatedAt time.Time      `json:"created_at,omitempty"`
}

// CommandInput is one DM command.
type CommandInput struct {
	CampaignID string
	Text       string
	CreatedBy  string
}

/* ---------- the slot frame ---------- */

// SlotFrame is the parsed, cross-field-validated fill of one command: the
// verb plus the DM's own words in that verb's slots. References stay as
// text — pronouns or names — because resolution is the server's job.
type SlotFrame struct {
	Verb          string
	Name          string // create_entity: the new entity's name
	Kind          string // create_entity
	Summary       string // create_entity
	RelType       string // create_entity (with RelTarget) / create_relationship
	RelTarget     string // create_entity: who the new entity relates to
	FromEntity    string // create_relationship
	ToEntity      string // create_relationship
	Subject       string // create_fact: an entity reference
	Statement     string // create_fact
	Predicate     string // create_fact
	ObjectKind    string // create_fact: literal | entity
	ObjectLiteral string
	ObjectEntity  string
	Visibility    string // create_fact
	Scene         string // add_to_scene: scene id or exact name
	Role          string // add_to_scene
	Entity        string // add_to_scene / merge_names: the subject
	MergeInto     string // merge_names: the surviving entity
}

// ParseSlotFrame turns the harness's validated values into a SlotFrame,
// enforcing the cross-field rules the per-field schema cannot: each verb
// needs its own slots, references must not be self-relating, and a merge
// needs a real name. Every problem is a sentence the response can quote.
// Pure: no database, no model — the unit tests live here.
func ParseSlotFrame(values map[string]any) (*SlotFrame, []string) {
	f := &SlotFrame{
		Verb:          slotString(values, "verb"),
		Name:          slotString(values, "name"),
		Kind:          slotString(values, "kind"),
		Summary:       slotString(values, "summary"),
		RelType:       slotString(values, "rel_type"),
		RelTarget:     slotString(values, "rel_target"),
		FromEntity:    slotString(values, "from_entity"),
		ToEntity:      slotString(values, "to_entity"),
		Subject:       slotString(values, "subject"),
		Statement:     slotString(values, "statement"),
		Predicate:     slotString(values, "predicate"),
		ObjectKind:    slotString(values, "object_kind"),
		ObjectLiteral: slotString(values, "object_literal"),
		ObjectEntity:  slotString(values, "object_entity"),
		Visibility:    slotString(values, "visibility"),
		Scene:         slotString(values, "scene"),
		Role:          slotString(values, "role"),
		Entity:        slotString(values, "entity"),
		MergeInto:     slotString(values, "merge_into"),
	}
	if f.Verb == "" {
		f.Verb = CommandVerbNone
	}
	known := map[string]bool{}
	for _, v := range commandVerbs {
		known[v] = true
	}
	var problems []string
	if !known[f.Verb] {
		return nil, []string{fmt.Sprintf("verb %q is not one of the command verbs", f.Verb)}
	}
	switch f.Verb {
	case CommandVerbCreateEntity:
		if f.Name == "" {
			problems = append(problems, "creating an entity needs a name — try “create an npc called Vess”")
		}
		if f.Kind == "" {
			problems = append(problems, "creating an entity needs a kind (npc, faction, location, …)")
		}
		if (f.RelTarget == "") != (f.RelType == "") {
			problems = append(problems, "a relationship needs both a type and a target")
		}
	case CommandVerbCreateRel:
		if f.RelType == "" {
			problems = append(problems, "a relationship needs a type from the controlled vocabulary")
		}
		if f.FromEntity == "" || f.ToEntity == "" {
			problems = append(problems, "a relationship needs both ends — name (or point at) the two entities")
		}
		if strings.EqualFold(f.FromEntity, f.ToEntity) {
			problems = append(problems, "an entity cannot relate to itself")
		}
	case CommandVerbCreateFact:
		if f.Subject == "" {
			problems = append(problems, "a fact needs a subject — whose truth is it")
		}
		if f.Statement == "" {
			problems = append(problems, "a fact needs its statement, in one plain sentence")
		}
		if f.Predicate == "" {
			problems = append(problems, "a fact needs a predicate from those already in use")
		}
		switch f.ObjectKind {
		case "", "literal":
			if f.ObjectLiteral == "" {
				problems = append(problems, "a literal fact needs its object")
			}
		case "entity":
			if f.ObjectEntity == "" {
				problems = append(problems, "an entity fact needs its object entity")
			}
		default:
			problems = append(problems, fmt.Sprintf("object kind %q is not literal or entity", f.ObjectKind))
		}
	case CommandVerbAddToScene:
		if f.Entity == "" {
			problems = append(problems, "adding to a scene needs who — name (or point at) the entity")
		}
		if f.Role == "" {
			f.Role = story.RolePresent
		}
	case CommandVerbMergeNames:
		if f.Entity == "" {
			problems = append(problems, "a merge needs the name to absorb")
		}
		if f.MergeInto == "" {
			problems = append(problems, "a merge needs the entity the name belongs to")
		}
		if isPronoun(f.Entity) {
			problems = append(problems, "a merge needs the duplicate's name, not a pronoun")
		}
	}
	return f, problems
}

// slotString reads one trimmed, quote-stripped string slot.
func slotString(values map[string]any, key string) string {
	sv, _ := values[key].(string)
	s := strings.TrimSpace(sv)
	s = strings.Trim(s, `"'`)
	return strings.TrimSpace(s)
}

/* ---------- the referent stack ---------- */

// referentState is what the reviews table says about one referent's
// proposal when a pronoun tries to bind to it.
type referentState struct {
	Status    string
	ResultRef string
}

// The bind outcomes. Everything but bound asks.
const (
	referentBound     = "bound"
	referentNone      = "empty"
	referentPending   = "pending"
	referentExpired   = "expired"
	referentAmbiguous = "ambiguous"
)

// bindPronoun resolves a pronoun against the referent stack: the entity
// proposals of the most recent command that made any. One live proposal
// binds (accepted means its entity exists — result_ref is the id); an open
// proposal asks because the DM has not decided; a dismissed one expired;
// several ask which. Pure given the states — the unit tests live here.
func bindPronoun(stack []Referent, states map[string]referentState) (string, string, []CommandCandidate) {
	if len(stack) == 0 {
		return "", referentNone, nil
	}
	candidates := make([]CommandCandidate, 0, len(stack))
	for _, r := range stack {
		c := CommandCandidate{Name: r.Name, Kind: r.Kind}
		if id := states[r.ReviewID].ResultRef; id != "" {
			c.ID = id
		}
		switch states[r.ReviewID].Status {
		case ReviewOpen:
			c.Note = "still a proposal — accept it in the review queue"
		case ReviewDismissed:
			c.Note = "dismissed — never became canon"
		}
		candidates = append(candidates, c)
	}
	if len(stack) > 1 {
		return "", referentAmbiguous, candidates
	}
	r := stack[0]
	switch states[r.ReviewID].Status {
	case ReviewAccepted, ReviewModified:
		if id := states[r.ReviewID].ResultRef; id != "" {
			return id, referentBound, nil
		}
		return "", referentExpired, candidates // accepted but unapplied: dead end
	case ReviewOpen:
		return "", referentPending, candidates
	default: // dismissed, or a foreign id that resolves nowhere
		return "", referentExpired, candidates
	}
}

// loadReferentStack reads the command transcript's newest rows and returns
// the entity proposals of the most recent command that made any — the
// pronoun bind targets — plus every referent on the way (for candidates).
func (s *Store) loadReferentStack(ctx context.Context, campaignID string) ([]Referent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT referents FROM command_log WHERE campaign_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`,
		campaignID, commandReferentRows)
	if err != nil {
		return nil, fmt.Errorf("command referents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var stack []Referent
		if err := json.Unmarshal([]byte(raw), &stack); err != nil || len(stack) == 0 {
			continue
		}
		return stack, nil
	}
	return nil, rows.Err()
}

// referentStates loads the reviews behind a stack.
func (s *Store) referentStates(ctx context.Context, stack []Referent) (map[string]referentState, error) {
	out := map[string]referentState{}
	for _, r := range stack {
		if _, done := out[r.ReviewID]; done {
			continue
		}
		var st referentState
		err := s.db.QueryRowContext(ctx,
			`SELECT status, result_ref FROM canon_reviews WHERE id = ?`, r.ReviewID).
			Scan(&st.Status, &st.ResultRef)
		if errors.Is(err, sql.ErrNoRows) {
			out[r.ReviewID] = referentState{Status: ReviewDismissed} // vanished: treat as expired
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("referent review: %w", err)
		}
		out[r.ReviewID] = st
	}
	return out, nil
}

/* ---------- the vocabulary the model sees ---------- */

type rosterEntry struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Summary string `json:"summary,omitempty"`
}

type sceneEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type commandSession struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Ordinal int64        `json:"ordinal"`
	Scenes  []sceneEntry `json:"scenes,omitempty"`
}

// commandVocab is the closed vocabulary one command runs against, built
// from the campaign's own schema.
type commandVocab struct {
	Kinds      []string
	RelTypes   []string
	Predicates []string
	Roster     []rosterEntry
	Next       *commandSession
}

// loadCommandVocabulary reads the pools: entity kinds (the CHECK list),
// the controlled relationship types, predicates already in use, the entity
// roster, and the next planned session with its scenes.
func (s *Store) loadCommandVocabulary(ctx context.Context, campaignID string) (*commandVocab, error) {
	v := &commandVocab{Kinds: EntityKinds()}
	relTypes, err := s.RelationshipTypes(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	v.RelTypes = relTypes

	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT predicate FROM facts WHERE campaign_id = ? ORDER BY predicate LIMIT ?`,
		campaignID, commandPredicateCap)
	if err != nil {
		return nil, fmt.Errorf("command predicates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		v.Predicates = append(v.Predicates, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx,
		`SELECT name, kind, COALESCE(summary,'') FROM entities
		  WHERE campaign_id = ? AND COALESCE(status,'active') <> 'deleted'
		  ORDER BY name COLLATE NOCASE LIMIT ?`, campaignID, commandRosterCap)
	if err != nil {
		return nil, fmt.Errorf("command roster: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e rosterEntry
		if err := rows.Scan(&e.Name, &e.Kind, &e.Summary); err != nil {
			return nil, err
		}
		if len(e.Summary) > 140 {
			e.Summary = e.Summary[:137] + "…"
		}
		v.Roster = append(v.Roster, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var next commandSession
	err = s.db.QueryRowContext(ctx,
		`SELECT id, COALESCE(name,''), ordinal FROM game_sessions
		  WHERE campaign_id = ? AND status = 'planned' ORDER BY ordinal LIMIT 1`, campaignID).
		Scan(&next.ID, &next.Name, &next.Ordinal)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return v, nil
	case err != nil:
		return nil, fmt.Errorf("command next session: %w", err)
	}
	if next.Name == "" {
		next.Name = fmt.Sprintf("session %d", next.Ordinal)
	}
	rows, err = s.db.QueryContext(ctx,
		`SELECT id, COALESCE(name,''), kind FROM scenes WHERE campaign_id = ? AND session_id = ? ORDER BY ordinal`,
		campaignID, next.ID)
	if err != nil {
		return nil, fmt.Errorf("command session scenes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sc sceneEntry
		if err := rows.Scan(&sc.ID, &sc.Name, &sc.Kind); err != nil {
			return nil, err
		}
		if sc.Name == "" {
			sc.Name = sc.ID
		}
		next.Scenes = append(next.Scenes, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	v.Next = &next
	return v, nil
}

/* ---------- the prompt ---------- */

type recentProposalView struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

type commandStructure struct {
	Command           string               `json:"command"`
	Verbs             []string             `json:"verbs"`
	EntityKinds       []string             `json:"entity_kinds"`
	RelationshipTypes []string             `json:"relationship_types"`
	Predicates        []string             `json:"predicates_in_use"`
	CastRoles         []string             `json:"cast_roles"`
	Roster            []rosterEntry        `json:"entity_roster"`
	NextSession       *commandSession      `json:"next_session,omitempty"`
	RecentProposals   []recentProposalView `json:"recent_proposals_the_pronouns_point_at,omitempty"`
}

// buildCommandStructure assembles the prompt's structure block.
func buildCommandStructure(text string, v *commandVocab, stack []Referent, states map[string]referentState) commandStructure {
	st := commandStructure{
		Command:           text,
		Verbs:             commandVerbs,
		EntityKinds:       v.Kinds,
		RelationshipTypes: v.RelTypes,
		Predicates:        v.Predicates,
		CastRoles:         story.CastRoles,
		Roster:            v.Roster,
		NextSession:       v.Next,
	}
	for _, r := range stack {
		status := states[r.ReviewID].Status
		if status == "" {
			status = "gone"
		}
		st.RecentProposals = append(st.RecentProposals, recentProposalView{Name: r.Name, Kind: r.Kind, Status: status})
	}
	return st
}

// commandFields declares the slot frame's schema. Every slot is optional
// at the harness level — a verb's own requirements are cross-field, and a
// blanket "required" would push the model into inventing slots for verbs
// that do not need them.
func commandFields(v *commandVocab) []FieldSpec {
	return []FieldSpec{
		{Key: "verb", Required: true, Enum: commandVerbs,
			Desc: "The command's verb: creating an entity, creating a relationship, recording a fact, adding someone to a scene of the next session, merging a duplicate name — or none."},
		{Key: "name", MaxLen: 80,
			Desc: "create_entity: the new entity's name, in the DM's words."},
		{Key: "kind", Enum: v.Kinds,
			Desc: "create_entity: the new entity's kind."},
		{Key: "summary", MaxLen: 280,
			Desc: "create_entity: one or two sentences describing them, carrying the DM's specifics (class, level, role)."},
		{Key: "rel_type", Pool: v.RelTypes,
			Desc: "create_entity / create_relationship: the relationship's type. Legal values only."},
		{Key: "rel_target", MaxLen: 120,
			Desc: "create_entity: who the new entity relates to — a roster name or a pronoun, verbatim."},
		{Key: "from_entity", MaxLen: 120,
			Desc: "create_relationship: one end — a roster name or a pronoun, verbatim."},
		{Key: "to_entity", MaxLen: 120,
			Desc: "create_relationship: the other end — a roster name or a pronoun, verbatim."},
		{Key: "subject", MaxLen: 120,
			Desc: "create_fact: whose truth it is — a roster name or a pronoun, verbatim."},
		{Key: "statement", MaxLen: 400,
			Desc: "create_fact: the fact as one plain sentence, in the DM's words."},
		{Key: "predicate", Pool: v.Predicates,
			Desc: "create_fact: the predicate. Legal values (those already in use) only."},
		{Key: "object_kind", Enum: []string{"literal", "entity"},
			Desc: "create_fact: whether the fact's object is a literal or an entity."},
		{Key: "object_literal", MaxLen: 200,
			Desc: "create_fact: the literal object, when object_kind is literal."},
		{Key: "object_entity", MaxLen: 120,
			Desc: "create_fact: the object entity — a roster name or a pronoun, verbatim."},
		{Key: "visibility", Enum: VisibilityValues(),
			Desc: "create_fact: public, or secret when the DM says so. Default public."},
		{Key: "scene", MaxLen: 120,
			Desc: "add_to_scene: the scene's id or exact name from next_session's scenes. Omit when the text only names the session."},
		{Key: "role", Enum: story.CastRoles,
			Desc: "add_to_scene: focus, present, offstage or mentioned. Default present."},
		{Key: "entity", MaxLen: 120,
			Desc: "add_to_scene / merge_names: the entity being added or the duplicate name — a roster name or a pronoun (add_to_scene only), verbatim."},
		{Key: "merge_into", MaxLen: 120,
			Desc: "merge_names: the surviving entity the name should resolve to — a roster name, verbatim."},
	}
}

const commandSystemPrompt = `You are Grimoire's command parser. The DM's text is a command for the campaign database in plain English. Your only job is to fill one slot frame: choose the verb and copy the DM's own words into that verb's slots. You never answer the text, never invent ids, never emit SQL, and never add anything the DM did not state.

STRICT RULES
1. verb comes from the legal values. When the text is a question, a conversation, or none of the command shapes, verb is "none" — say so by leaving every other slot empty.
2. Fill only the slots the chosen verb needs; omit the rest entirely.
3. Entity references are the DM's own naming: a name from the roster, or a pronoun (him, her, it, them) when the text uses one. Copy them verbatim — the server resolves every reference against the campaign, not you. Never output an id. When the text names an entity the roster does not list, copy the name anyway.
4. kind, rel_type, predicate, role, visibility and scene come only from their legal values or the structure's lists.
5. statement and summary are the DM's own meaning in plain sentences — no embellishment.`

/* ---------- the engine ---------- */

// RunCommand runs one natural-language command: text in, a result out.
// Every outcome — batch, question, unsupported, undo, written, noop — is
// logged to the command transcript, because the referent stack and "undo
// that" are only real if the transcript is.
func (s *Store) RunCommand(ctx context.Context, in CommandInput) (*CommandResult, error) {
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" {
		return nil, fmt.Errorf("%w: a command cannot be empty", ErrInvalid)
	}
	if len([]rune(in.Text)) > commandMaxLen {
		return nil, fmt.Errorf("%w: a command is at most %d characters", ErrInvalid, commandMaxLen)
	}
	if _, err := s.loadCampaign(ctx, in.CampaignID); err != nil {
		return nil, err
	}
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}

	// Undo is deterministic and never needs the model.
	if IsUndo(in.Text) {
		res, err := s.runCommandUndo(ctx, in)
		if err != nil {
			return nil, err
		}
		return s.logCommand(ctx, in, res, nil)
	}

	if s.model == nil {
		return nil, errOffline
	}

	vocab, err := s.loadCommandVocabulary(ctx, in.CampaignID)
	if err != nil {
		return nil, err
	}
	stack, err := s.loadReferentStack(ctx, in.CampaignID)
	if err != nil {
		return nil, err
	}
	states, err := s.referentStates(ctx, stack)
	if err != nil {
		return nil, err
	}

	gen, err := s.Generate(ctx, GenerateInput{
		System:    commandSystemPrompt,
		Structure: buildCommandStructure(in.Text, vocab, stack, states),
		Fields:    commandFields(vocab),
		Note:      "Fill the slot frame for the command in the structure block. References are copied verbatim — the server resolves them.",
	})
	if err != nil {
		return nil, err
	}
	frame, problems := ParseSlotFrame(gen.Values)
	if len(problems) > 0 {
		return s.logCommand(ctx, in, &CommandResult{
			Kind:    CommandResultQuestion,
			Message: "The command was unclear: " + strings.Join(problems, "; ") + ".",
			Question: &Clarification{
				Question: "The command was unclear: " + strings.Join(problems, "; ") + ".",
			},
		}, nil)
	}

	var res *CommandResult
	var referents []Referent
	switch frame.Verb {
	case CommandVerbNone:
		res = &CommandResult{Kind: CommandResultUnsupported, Message: commandUnsupportedMessage}
	case CommandVerbCreateEntity:
		res, referents, err = s.commandCreateEntity(ctx, in, frame, stack, states)
	case CommandVerbCreateRel:
		res, err = s.commandCreateRelationship(ctx, in, frame, stack, states)
	case CommandVerbCreateFact:
		res, err = s.commandCreateFact(ctx, in, frame, stack, states)
	case CommandVerbAddToScene:
		res, err = s.commandAddToScene(ctx, in, frame, vocab, stack, states)
	case CommandVerbMergeNames:
		res, err = s.commandMergeNames(ctx, in, frame, stack, states)
	}
	if err != nil {
		return nil, err
	}
	return s.logCommand(ctx, in, res, referents)
}

const commandUnsupportedMessage = "That is not one of the commands. The command line understands: create an entity (“create a level 5 necromancer called Vess”), create a relationship (“put Vess in Blackwater”), record a fact (“record that the Duke keeps a ledger”), add someone to the next session's scenes (“add Vess to next session”), merge a duplicate name (“merge Vess into Vess the Quiet”), and undo. Questions belong to the campaign Grimoire."

// normalizeUndo lowercases, collapses whitespace and strips terminal
// punctuation so "Undo that." recognises.
func normalizeUndo(text string) string {
	t := strings.ToLower(strings.Join(strings.Fields(text), " "))
	return strings.TrimRight(t, ".!?")
}

// IsUndo reports whether a text is one of the deterministic undo forms —
// the one command that never needs the model, and so the one that must
// keep working when no key is configured.
func IsUndo(text string) bool {
	return commandUndoTexts[normalizeUndo(text)]
}

/* ---------- reference resolution ---------- */

// resolveCommandRef resolves one entity-reference slot. A pronoun binds
// through the referent stack; a name goes through campaign.ResolveName
// (aliases included). Exactly one match resolves; zero or many is a
// clarifying question with the candidates attached — never a guess.
func (s *Store) resolveCommandRef(ctx context.Context, campaignID, ref string, stack []Referent, states map[string]referentState) (string, string, *Clarification) {
	if isPronoun(ref) {
		id, why, candidates := bindPronoun(stack, states)
		if why == referentBound {
			return id, "", nil
		}
		return "", "", pronounQuestion(ref, why, candidates)
	}
	hits, err := s.campaigns.ResolveName(ctx, campaign.ScopeDM, campaignID, ref)
	if err != nil {
		return "", err.Error(), nil
	}
	switch len(hits) {
	case 1:
		return hits[0].ID, "", nil
	case 0:
		return "", "", &Clarification{
			Question:   fmt.Sprintf("Nothing in this campaign answers to %q. Create it first (for example “create a location called %s”), or name one of these:", ref, ref),
			Candidates: s.missCandidates(ctx, campaignID, ref),
		}
	default:
		q := &Clarification{Question: fmt.Sprintf("%q matches several entities — which one?", ref)}
		for _, h := range hits {
			q.Candidates = append(q.Candidates, CommandCandidate{ID: h.ID, Name: h.Name, Kind: h.Kind, Summary: h.Summary})
		}
		return "", "", q
	}
}

// pronounQuestion renders the bind failure as a question.
func pronounQuestion(ref, why string, candidates []CommandCandidate) *Clarification {
	switch why {
	case referentPending:
		name := ""
		if len(candidates) > 0 {
			name = candidates[0].Name
		}
		return &Clarification{
			Question:   fmt.Sprintf("%q would be %s — still a proposal waiting on your review. Accept it in the review queue first, then try again.", ref, name),
			Candidates: candidates,
		}
	case referentExpired:
		name := ""
		if len(candidates) > 0 {
			name = candidates[0].Name
		}
		return &Clarification{
			Question:   fmt.Sprintf("%q would be %s, but that proposal was dismissed — the entity was never created. Name an entity instead.", ref, name),
			Candidates: candidates,
		}
	case referentAmbiguous:
		return &Clarification{
			Question:   fmt.Sprintf("%q could mean several recent proposals — which one?", ref),
			Candidates: candidates,
		}
	default:
		return &Clarification{
			Question: fmt.Sprintf("%q has nothing to bind to — no recent command proposed an entity. Name the entity instead.", ref),
		}
	}
}

// missCandidates offers the near names for a reference that resolved to
// nothing, so the question still carries candidates worth reading.
func (s *Store) missCandidates(ctx context.Context, campaignID, ref string) []CommandCandidate {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, kind, COALESCE(summary,'') FROM entities
		  WHERE campaign_id = ? AND COALESCE(status,'active') <> 'deleted'
		    AND (name LIKE ? OR EXISTS (SELECT 1 FROM entity_aliases a WHERE a.entity_id = entities.id AND a.name LIKE ?))
		  ORDER BY name COLLATE NOCASE LIMIT ?`,
		campaignID, "%"+ref+"%", "%"+ref+"%", commandMissCandidate)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CommandCandidate
	for rows.Next() {
		var c CommandCandidate
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Summary); err != nil {
			return nil
		}
		if len(c.Summary) > 120 {
			c.Summary = c.Summary[:117] + "…"
		}
		out = append(out, c)
	}
	return out
}

// entityName resolves an id back to a name for messages.
func (s *Store) entityName(ctx context.Context, campaignID, id string) string {
	var name string
	if err := s.db.QueryRowContext(ctx,
		`SELECT name FROM entities WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&name); err != nil {
		return id
	}
	return name
}

/* ---------- the verbs ---------- */

// commandCreateEntity stages a new entity (plus its optional first
// relationship) as one proposal batch, and leaves the entity on the
// referent stack. A name that already resolves to one entity would mint
// the near-duplicate entity_merge_candidate exists to flag, so it proposes
// a merge instead — as a question with the existing entity attached.
func (s *Store) commandCreateEntity(ctx context.Context, in CommandInput, f *SlotFrame, stack []Referent, states map[string]referentState) (*CommandResult, []Referent, error) {
	hits, err := s.campaigns.ResolveName(ctx, campaign.ScopeDM, in.CampaignID, f.Name)
	if err != nil {
		return nil, nil, err
	}
	if len(hits) == 1 {
		h := hits[0]
		return &CommandResult{
			Kind: CommandResultQuestion,
			Verb: f.Verb,
			Message: fmt.Sprintf("%q already names %s (%s) — creating a second would be the duplicate the integrity check flags. Reuse them by name, give yours a different name, or record more about them with a fact.",
				f.Name, h.Name, h.Kind),
			Question: &Clarification{
				Question: fmt.Sprintf("%q already names %s (%s) — did you mean them?", f.Name, h.Name, h.Kind),
				Candidates: []CommandCandidate{{
					ID: h.ID, Name: h.Name, Kind: h.Kind, Summary: h.Summary,
					Note: "reuse this entity instead of creating a duplicate",
				}},
			},
		}, nil, nil
	}
	if len(hits) > 1 {
		q := &Clarification{Question: fmt.Sprintf("%q already matches several entities — a new one would only add to the confusion. Which did you mean?", f.Name)}
		for _, h := range hits {
			q.Candidates = append(q.Candidates, CommandCandidate{ID: h.ID, Name: h.Name, Kind: h.Kind, Summary: h.Summary})
		}
		return &CommandResult{Kind: CommandResultQuestion, Verb: f.Verb,
			Message: q.Question, Question: q}, nil, nil
	}

	// The optional first relationship's target resolves now, so nothing
	// stages against a reference the campaign cannot answer.
	var relToID, relToName string
	if f.RelType != "" && f.RelTarget != "" {
		id, errMsg, q := s.resolveCommandRef(ctx, in.CampaignID, f.RelTarget, stack, states)
		if q != nil {
			return &CommandResult{Kind: CommandResultQuestion, Verb: f.Verb, Message: q.Question, Question: q}, nil, nil
		}
		if errMsg != "" {
			return nil, nil, fmt.Errorf("%s", errMsg)
		}
		relToID = id
		relToName = s.entityName(ctx, in.CampaignID, id)
	}

	items := []BatchItemInput{{
		ID: "entity", Kind: "entity",
		Subject: fmt.Sprintf("%s (%s)", f.Name, f.Kind),
		Summary: fmt.Sprintf("Create %s", f.Name),
		Payload: map[string]any{
			"local_id": "entity", "kind": f.Kind, "name": f.Name, "summary": f.Summary,
		},
	}}
	message := fmt.Sprintf("%q does not exist yet — this proposal creates them (%s).", f.Name, f.Kind)
	if relToID != "" {
		items = append(items, BatchItemInput{
			ID: "first-rel", Kind: "relationship", DependsOn: []string{"entity"},
			Subject: fmt.Sprintf("%s %s %s", f.Name, f.RelType, relToName),
			Summary: fmt.Sprintf("%s %s %s", f.Name, f.RelType, relToName),
			Payload: map[string]any{
				"from_entity": "entity", "rel_type": f.RelType, "to_entity": relToID,
			},
		})
		message += fmt.Sprintf(" It also links them to %s with %s.", relToName, f.RelType)
	}
	message += " Nothing is canon until you accept the batch."

	batch, err := s.StageBatch(ctx, BatchInput{
		CampaignID: in.CampaignID, Source: BatchSourceNLCommand,
		Prompt: in.Text, CreatedBy: in.CreatedBy, Items: items,
	})
	if err != nil {
		return nil, nil, err
	}
	var referents []Referent
	for _, it := range batch.Items {
		if it.Kind == ReviewProposedEntity {
			referents = append(referents, Referent{ReviewID: it.ID, Name: f.Name, Kind: f.Kind})
		}
	}
	return &CommandResult{
		Kind: CommandResultBatch, Verb: f.Verb, Message: message,
		Batch: batch, Referents: referents,
	}, referents, nil
}

// commandCreateRelationship stages one typed edge between two resolved
// entities. An edge that already exists is said so, plainly — staging a
// duplicate behind the gate would only waste the DM's review.
func (s *Store) commandCreateRelationship(ctx context.Context, in CommandInput, f *SlotFrame, stack []Referent, states map[string]referentState) (*CommandResult, error) {
	fromID, errMsg, q := s.resolveCommandRef(ctx, in.CampaignID, f.FromEntity, stack, states)
	if q != nil {
		return &CommandResult{Kind: CommandResultQuestion, Verb: f.Verb, Message: q.Question, Question: q}, nil
	}
	if errMsg != "" {
		return nil, fmt.Errorf("%s", errMsg)
	}
	toID, errMsg, q := s.resolveCommandRef(ctx, in.CampaignID, f.ToEntity, stack, states)
	if q != nil {
		return &CommandResult{Kind: CommandResultQuestion, Verb: f.Verb, Message: q.Question, Question: q}, nil
	}
	if errMsg != "" {
		return nil, fmt.Errorf("%s", errMsg)
	}
	fromName := s.entityName(ctx, in.CampaignID, fromID)
	toName := s.entityName(ctx, in.CampaignID, toID)

	var one int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM relationships WHERE from_entity = ? AND rel_type = ? AND to_entity = ?`,
		fromID, f.RelType, toID).Scan(&one); err == nil {
		return &CommandResult{Kind: CommandResultNoop, Verb: f.Verb,
			Message: fmt.Sprintf("That relationship already exists — %s %s %s.", fromName, f.RelType, toName)}, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("command rel check: %w", err)
	}

	batch, err := s.StageBatch(ctx, BatchInput{
		CampaignID: in.CampaignID, Source: BatchSourceNLCommand,
		Prompt: in.Text, CreatedBy: in.CreatedBy,
		Items: []BatchItemInput{{
			ID: "rel", Kind: "relationship",
			Subject: fmt.Sprintf("%s %s %s", fromName, f.RelType, toName),
			Summary: fmt.Sprintf("%s %s %s", fromName, f.RelType, toName),
			Payload: map[string]any{"from_entity": fromID, "rel_type": f.RelType, "to_entity": toID},
		}},
	})
	if err != nil {
		return nil, err
	}
	return &CommandResult{
		Kind: CommandResultBatch, Verb: f.Verb,
		Message: fmt.Sprintf("Staged for review: %s %s %s. Nothing is canon until you accept the batch.", fromName, f.RelType, toName),
		Batch:   batch,
	}, nil
}

// commandCreateFact stages one fact with ai_proposed provenance behind the
// gate — the same path every machine-proposed fact takes.
func (s *Store) commandCreateFact(ctx context.Context, in CommandInput, f *SlotFrame, stack []Referent, states map[string]referentState) (*CommandResult, error) {
	subjectID, errMsg, q := s.resolveCommandRef(ctx, in.CampaignID, f.Subject, stack, states)
	if q != nil {
		return &CommandResult{Kind: CommandResultQuestion, Verb: f.Verb, Message: q.Question, Question: q}, nil
	}
	if errMsg != "" {
		return nil, fmt.Errorf("%s", errMsg)
	}
	payload := map[string]any{
		"subject":    subjectID,
		"predicate":  f.Predicate,
		"statement":  f.Statement,
		"visibility": visibilityOrDefault(f.Visibility),
	}
	var objectName string
	if f.ObjectKind == "entity" {
		id, errMsg, q := s.resolveCommandRef(ctx, in.CampaignID, f.ObjectEntity, stack, states)
		if q != nil {
			return &CommandResult{Kind: CommandResultQuestion, Verb: f.Verb, Message: q.Question, Question: q}, nil
		}
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		payload["object_entity"] = id
		objectName = s.entityName(ctx, in.CampaignID, id)
	} else {
		payload["object_literal"] = f.ObjectLiteral
	}
	subjectName := s.entityName(ctx, in.CampaignID, subjectID)
	visLabel := ""
	if payload["visibility"] == campaign.VisibilitySecret {
		visLabel = "secret "
	}
	batch, err := s.StageBatch(ctx, BatchInput{
		CampaignID: in.CampaignID, Source: BatchSourceNLCommand,
		Prompt: in.Text, CreatedBy: in.CreatedBy,
		Items: []BatchItemInput{{
			ID: "fact", Kind: "fact",
			Subject: f.Statement,
			Summary: factSummary(subjectName, objectName, f),
			Payload: payload,
		}},
	})
	if err != nil {
		return nil, err
	}
	return &CommandResult{
		Kind: CommandResultBatch, Verb: f.Verb,
		Message: fmt.Sprintf("Staged for review: the %sfact “%s”. Nothing is canon until you accept the batch.", visLabel, f.Statement),
		Batch:   batch,
	}, nil
}

func visibilityOrDefault(v string) string {
	if v == campaign.VisibilitySecret {
		return campaign.VisibilitySecret
	}
	return campaign.VisibilityPublic
}

// factSummary renders the queue line for a staged fact.
func factSummary(subject, object string, f *SlotFrame) string {
	if object != "" {
		return fmt.Sprintf("%s %s %s", subject, f.Predicate, object)
	}
	return fmt.Sprintf("%s %s %s", subject, f.Predicate, f.ObjectLiteral)
}

// commandAddToScene writes a scene cast row — the spine row a DM adds by
// hand, written the way the scene designer writes cast: directly, at DM
// scope. The entity must exist (a pronoun binds only to an accepted
// proposal); the scene comes from the next session's, or is asked for when
// the session has several.
func (s *Store) commandAddToScene(ctx context.Context, in CommandInput, f *SlotFrame, vocab *commandVocab, stack []Referent, states map[string]referentState) (*CommandResult, error) {
	entityID, errMsg, q := s.resolveCommandRef(ctx, in.CampaignID, f.Entity, stack, states)
	if q != nil {
		return &CommandResult{Kind: CommandResultQuestion, Verb: f.Verb, Message: q.Question, Question: q}, nil
	}
	if errMsg != "" {
		return nil, fmt.Errorf("%s", errMsg)
	}
	if vocab.Next == nil {
		return &CommandResult{Kind: CommandResultNoop, Verb: f.Verb,
			Message: "There is no planned next session — create one (or plan one) before casting anyone in it."}, nil
	}

	var scene sceneEntry
	if f.Scene != "" {
		found := false
		for _, sc := range vocab.Next.Scenes {
			if strings.EqualFold(sc.ID, f.Scene) || strings.EqualFold(sc.Name, f.Scene) {
				scene, found = sc, true
				break
			}
		}
		if !found {
			q := &Clarification{Question: fmt.Sprintf("%q is not a scene of %s — which scene?", f.Scene, vocab.Next.Name)}
			for _, sc := range vocab.Next.Scenes {
				q.Candidates = append(q.Candidates, CommandCandidate{ID: sc.ID, Name: sc.Name, Kind: sc.Kind})
			}
			if len(q.Candidates) == 0 {
				return &CommandResult{Kind: CommandResultNoop, Verb: f.Verb,
					Message: fmt.Sprintf("%s has no scenes planned yet — plan it before casting anyone in it.", vocab.Next.Name)}, nil
			}
			return &CommandResult{Kind: CommandResultQuestion, Verb: f.Verb, Message: q.Question, Question: q}, nil
		}
	} else {
		switch len(vocab.Next.Scenes) {
		case 0:
			return &CommandResult{Kind: CommandResultNoop, Verb: f.Verb,
				Message: fmt.Sprintf("%s has no scenes planned yet — plan it before casting anyone in it.", vocab.Next.Name)}, nil
		case 1:
			scene = vocab.Next.Scenes[0]
		default:
			q := &Clarification{Question: fmt.Sprintf("%s has several scenes — which one?", vocab.Next.Name)}
			for _, sc := range vocab.Next.Scenes {
				q.Candidates = append(q.Candidates, CommandCandidate{ID: sc.ID, Name: sc.Name, Kind: sc.Kind})
			}
			return &CommandResult{Kind: CommandResultQuestion, Verb: f.Verb, Message: q.Question, Question: q}, nil
		}
	}

	stories, err := story.New(s.db)
	if err != nil {
		return nil, err
	}
	sc, err := stories.GetScene(ctx, campaign.ScopeDM, in.CampaignID, scene.ID)
	if err != nil {
		return nil, err
	}
	entityName := s.entityName(ctx, in.CampaignID, entityID)
	for _, c := range sc.Cast {
		if c.EntityID == entityID {
			return &CommandResult{Kind: CommandResultNoop, Verb: f.Verb,
				Message: fmt.Sprintf("%s is already in the cast of %q.", entityName, sceneName(sc, scene))}, nil
		}
	}
	if _, err := stories.AddCast(ctx, in.CampaignID, scene.ID, entityID, f.Role); err != nil {
		return nil, err
	}
	return &CommandResult{
		Kind: CommandResultWritten, Verb: f.Verb,
		Message: fmt.Sprintf("Added %s to %q (%s) as %s.", entityName, sceneName(sc, scene), vocab.Next.Name, f.Role),
		Written: map[string]any{
			"scene_id": scene.ID, "scene_name": sceneName(sc, scene),
			"session_id": vocab.Next.ID, "entity_id": entityID,
			"entity_name": entityName, "role": f.Role,
		},
	}, nil
}

func sceneName(sc *story.Scene, fallback sceneEntry) string {
	if sc != nil && sc.Name != "" {
		return sc.Name
	}
	return fallback.Name
}

// commandMergeNames performs the one merge the graph supports: the
// duplicate name becomes an alias of the surviving entity, exactly the
// write POST /entities/{eid}/names makes. A name that already belongs to
// a different entity is a two-entity merge — beyond the vocabulary, said
// so plainly rather than half-done.
func (s *Store) commandMergeNames(ctx context.Context, in CommandInput, f *SlotFrame, stack []Referent, states map[string]referentState) (*CommandResult, error) {
	intoID, errMsg, q := s.resolveCommandRef(ctx, in.CampaignID, f.MergeInto, stack, states)
	if q != nil {
		return &CommandResult{Kind: CommandResultQuestion, Verb: f.Verb, Message: q.Question, Question: q}, nil
	}
	if errMsg != "" {
		return nil, fmt.Errorf("%s", errMsg)
	}
	hits, err := s.campaigns.ResolveName(ctx, campaign.ScopeDM, in.CampaignID, f.Entity)
	if err != nil {
		return nil, err
	}
	for _, h := range hits {
		if h.ID == intoID {
			return &CommandResult{Kind: CommandResultNoop, Verb: f.Verb,
				Message: fmt.Sprintf("%q already resolves to %s — nothing to merge.", f.Entity, h.Name)}, nil
		}
	}
	if len(hits) > 0 {
		other := hits[0]
		return &CommandResult{Kind: CommandResultNoop, Verb: f.Verb,
			Message: fmt.Sprintf("%q already belongs to %s (%s). Merging two entities into one is beyond the command vocabulary — resolve it by hand in the campaign editor.", f.Entity, other.Name, other.Kind)}, nil
	}
	intoName := s.entityName(ctx, in.CampaignID, intoID)
	if _, err := s.campaigns.AddEntityName(ctx, in.CampaignID, intoID, f.Entity, campaign.NameAlias); err != nil {
		return nil, err
	}
	return &CommandResult{
		Kind: CommandResultWritten, Verb: f.Verb,
		Message: fmt.Sprintf("%q now also resolves to %s — one entity, two names. The merge the integrity check would have asked for, done.", f.Entity, intoName),
		Written: map[string]any{"entity_id": intoID, "entity_name": intoName, "alias": f.Entity},
	}, nil
}

// runCommandUndo drops the newest open command batch. Undo before
// acceptance costs nothing because nothing was written — dismissing the
// batch is the whole operation.
func (s *Store) runCommandUndo(ctx context.Context, in CommandInput) (*CommandResult, error) {
	var batchID string
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, created_at FROM proposal_batches
		  WHERE campaign_id = ? AND source = ? AND status = ?
		  ORDER BY created_at DESC, rowid DESC LIMIT 1`,
		in.CampaignID, BatchSourceNLCommand, BatchOpen).Scan(&batchID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return &CommandResult{Kind: CommandResultNoop, Verb: "undo",
			Message: "Nothing to undo — no pending command proposals."}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("command undo lookup: %w", err)
	}
	decided, err := s.DecideBatch(ctx, in.CampaignID, batchID, DecisionDismiss, nil, in.CreatedBy)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, it := range decided.Items {
		if it.Status == ReviewDismissed {
			n++
		}
	}
	text := ""
	if decided.Batch != nil {
		text = decided.Batch.Prompt
	}
	return &CommandResult{
		Kind: CommandResultUndo, Verb: "undo",
		Message: fmt.Sprintf("Dropped the staged command “%s” (%d item%s) — nothing had been written.", text, n, plural(n)),
	}, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

/* ---------- the transcript ---------- */

// logCommand appends one exchange to the command transcript and stamps the
// result with its row identity.
func (s *Store) logCommand(ctx context.Context, in CommandInput, res *CommandResult, referents []Referent) (*CommandResult, error) {
	if res == nil {
		return nil, fmt.Errorf("%w: nothing to log", ErrInvalid)
	}
	response, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("command response: %w", err)
	}
	refs := "[]"
	if len(referents) > 0 {
		if b, err := json.Marshal(referents); err == nil {
			refs = string(b)
		}
	}
	res.CreatedAt = s.now()
	var batchID any // NULL when the exchange staged no batch
	if res.Batch != nil {
		batchID = res.Batch.ID
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO command_log (id, campaign_id, user_id, text, kind, verb, message, response, batch_id, referents, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), in.CampaignID, in.CreatedBy, in.Text, res.Kind, res.Verb,
		res.Message, string(response), batchID, refs, res.CreatedAt.UnixMilli()); err != nil {
		return nil, fmt.Errorf("command log: %w", err)
	}
	return res, nil
}

// CommandLogRow is one transcript row as the API renders it.
type CommandLogRow struct {
	ID        string          `json:"id"`
	Text      string          `json:"text"`
	Kind      string          `json:"kind"`
	Verb      string          `json:"verb,omitempty"`
	Message   string          `json:"message"`
	Response  json.RawMessage `json:"response,omitempty"`
	BatchID   string          `json:"batch_id,omitempty"`
	Referents []Referent      `json:"referents,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// CommandLog reads the transcript, newest first.
func (s *Store) CommandLog(ctx context.Context, campaignID string, limit int) ([]CommandLogRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, text, kind, verb, message, response, COALESCE(batch_id,''), referents, created_at
		  FROM command_log WHERE campaign_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`,
		campaignID, limit)
	if err != nil {
		return nil, fmt.Errorf("command log: %w", err)
	}
	defer rows.Close()
	var out []CommandLogRow
	for rows.Next() {
		var r CommandLogRow
		var created int64
		var refs, response string
		if err := rows.Scan(&r.ID, &r.Text, &r.Kind, &r.Verb, &r.Message, &response, &r.BatchID, &refs, &created); err != nil {
			return nil, err
		}
		r.Response = json.RawMessage(response)
		r.CreatedAt = time.UnixMilli(created).UTC()
		if refs != "" && refs != "[]" {
			_ = json.Unmarshal([]byte(refs), &r.Referents)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
