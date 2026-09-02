package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Severity of an integrity finding. Errors mean the graph contradicts its own
// rules; reviews mean a human should look; warnings are soft signals (none
// exist yet — awareness-dependent checks land with the knowledge layer);
// infos are nudges, not problems.
type Severity string

const (
	SeverityError  Severity = "error"
	SeverityReview Severity = "review"
	SeverityWarn   Severity = "warning"
	SeverityInfo   Severity = "info"
)

// Finding is one integrity result: a check code, how bad it is, which record
// tripped it, and a message a human can act on. Codes are stable strings —
// later stages key ledgers on them.
type Finding struct {
	Check      string
	Severity   Severity
	RecordKind string // fact, entity, event, quest, relationship, ...
	RecordID   string
	Message    string
}

// Check codes.
const (
	CheckFactWithoutProvenance     = "fact_without_provenance"
	CheckDanglingReference         = "dangling_reference"
	CheckCauseAfterEffect          = "cause_after_effect"
	CheckQuestTransitionInvalid    = "quest_transition_invalid"
	CheckContradictoryFacts        = "contradictory_facts"
	CheckEntityMergeCandidate      = "entity_merge_candidate"
	CheckDuplicateFact             = "duplicate_fact"
	CheckEventAfterClock           = "event_after_clock"
	CheckMissedSchedule            = "missed_schedule"
	CheckClockNeverAdvanced        = "clock_never_advanced"
	CheckPlanIllegalState          = "plan_illegal_state"
	CheckPlanWithoutFaction        = "plan_without_faction"
	CheckPlanStalled               = "plan_stalled"
	CheckFactionNoAntagonist       = "faction_no_antagonist"
	CheckQuestStateUnreachable     = "quest_state_unreachable"
	CheckQuestDeadEnd              = "quest_dead_end"
	CheckQuestNoEnding             = "quest_no_ending"
	CheckQuestTransitionUngrounded = "quest_transition_ungrounded"
)

// AwarenessView is one awareness row as the campaign checks read it: the
// knower, the fact, the stance. The full row and every scoped read of it
// live in internal/knowledge; this package only joins against it, and only
// ever at the DM scope the snapshot already requires.
type AwarenessView struct {
	Knower string
	FactID string
	Stance string
}

// Snapshot is a campaign's whole graph in memory. Check is pure over this
// struct — no DB, no clock, no network — which is what makes every rule
// unit-testable and the whole set safe to run anywhere.
type Snapshot struct {
	CampaignID string
	Entities   []Entity
	Names      []EntityName
	Facts      []Fact
	// ProvenanceCount is per fact id; the rows themselves live in the store.
	ProvenanceCount map[string]int
	Contradictions  []Contradiction
	// CoveredFacts is the set of fact ids appearing in fact_versions of open
	// contradictions — pairs the register already holds.
	CoveredFacts     map[string]bool
	Events           []Event
	Relationships    []Relationship
	Quests           []Quest
	QuestTransitions []QuestTransition
	// QuestEntities and QuestStateFacts are the links into the graph and the
	// knowledge layer (MAD-369); the dangling check reads them.
	QuestEntities   []QuestEntity
	QuestStateFacts []QuestStateFact
	// Awareness is the campaign's awareness rows reduced to what the quest
	// checks read: who holds what, at which stance. The full rows and their
	// scoped retrieval live in internal/knowledge.
	Awareness []AwarenessView
	// ActIDs is the campaign's act ids; quests.act_id carries no foreign
	// key by design, and this is what sweeps it.
	ActIDs map[string]bool
	// Clock is the campaign's current in-world day; the schedule and
	// event-dating checks read it (MAD-365).
	Clock int64
	// SessionCount is how many game_sessions the campaign has played.
	SessionCount int
	// AdvanceCount is how many clock_advances rows exist — "has the clock
	// ever moved on purpose".
	AdvanceCount int
	// Schedule is the campaign's scheduled_events.
	Schedule []ScheduledEvent
	// FactionPlans is the campaign's faction_plans, the lean projection the
	// plan checks read (MAD-366). The full progression model is
	// internal/faction; this view carries only what the rules need.
	FactionPlans []FactionPlanView
	// Rumors and RumorHolders are the campaign's rumour mill (MAD-374):
	// the statements in circulation and who repeats them, truth values
	// included — this is the DM snapshot. The rumor checks and the
	// location dossier read them; the scoped reads live in
	// internal/knowledge.
	Rumors       []Rumor
	RumorHolders []RumorHolder
}

// FactionPlanView is one faction plan as the integrity checks see it: ids,
// the machine, the rate, the day columns, and whether any step declares an
// opposing (enemy_plan) precondition. Loaded directly from the migration's
// tables so campaign never imports the progression package.
type FactionPlanView struct {
	ID            string
	FactionEntity string
	Name          string
	CurrentState  string
	Status        string
	Machine       StateMachine
	RatePerDay    float64
	StartedDay    *int64
	LastAdvanced  *int64
	// HasEnemyRequirement reports whether any step carries an enemy_plan
	// precondition — the "opposing precondition" of faction_no_antagonist.
	HasEnemyRequirement bool
}

// StalledAfterDays is how long an active, positive-rate plan may sit without
// advancing before plan_stalled fires. A planning constant, not a rule of
// the world: a month of in-world silence is the signal a DM asked for.
const StalledAfterDays = 30

// Integrity loads a campaign snapshot and runs every check over it. It is
// deliberately read-only: findings are information, and the DM decides what
// to do about them. The CLI `campaign check` subcommand calls this.
//
// The scope must be the DM's: the checker walks every fact, secret and
// proposed included, so this is an unscoped read the DM scope alone holds.
func Integrity(ctx context.Context, scope Scope, db *sql.DB, campaignID string) ([]Finding, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	snap, err := LoadSnapshot(ctx, ScopeDM, db, campaignID)
	if err != nil {
		return nil, err
	}
	return Check(snap), nil
}

// LoadSnapshot reads one campaign's graph into memory for the checks.
// DM-scope only, like Integrity: the snapshot carries every fact, secrets and
// proposals included, and nothing below the DM should ever hold one.
func LoadSnapshot(ctx context.Context, scope Scope, db *sql.DB, campaignID string) (*Snapshot, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	s := &Snapshot{CampaignID: campaignID, ProvenanceCount: map[string]int{}, CoveredFacts: map[string]bool{}}

	rows, err := db.QueryContext(ctx, `SELECT `+entityCols+` FROM entities WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity entities: %w", err)
	}
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		s.Entities = append(s.Entities, *e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `
		SELECT n.id, n.entity_id, n.name, n.kind, n.created_at FROM entity_aliases n
		 JOIN entities e ON e.id = n.entity_id WHERE e.campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity names: %w", err)
	}
	for rows.Next() {
		var (
			n       EntityName
			created int64
		)
		if err := rows.Scan(&n.ID, &n.EntityID, &n.Name, &n.Kind, &created); err != nil {
			rows.Close()
			return nil, err
		}
		s.Names = append(s.Names, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT `+factCols+` FROM facts WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity facts: %w", err)
	}
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		s.Facts = append(s.Facts, *f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT fact_id, COUNT(*) FROM fact_provenance GROUP BY fact_id`)
	if err != nil {
		return nil, fmt.Errorf("integrity provenance: %w", err)
	}
	for rows.Next() {
		var (
			factID string
			n      int
		)
		if err := rows.Scan(&factID, &n); err != nil {
			rows.Close()
			return nil, err
		}
		s.ProvenanceCount[factID] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `
		SELECT id, campaign_id, subject_entity, predicate, status, resolution_note, created_at
		  FROM contradictions WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity contradictions: %w", err)
	}
	var openIDs []string
	for rows.Next() {
		var (
			c       Contradiction
			created int64
		)
		if err := rows.Scan(&c.ID, &c.CampaignID, &c.SubjectEntity, &c.Predicate, &c.Status,
			&c.ResolutionNote, &created); err != nil {
			rows.Close()
			return nil, err
		}
		c.CreatedAt = unixMilli(created)
		s.Contradictions = append(s.Contradictions, c)
		if c.Status == ContradictionOpen {
			openIDs = append(openIDs, c.ID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, id := range openIDs {
		rows, err = db.QueryContext(ctx,
			`SELECT fact_id FROM fact_versions WHERE contradiction_id = ?`, id)
		if err != nil {
			return nil, fmt.Errorf("integrity fact versions: %w", err)
		}
		for rows.Next() {
			var factID string
			if err := rows.Scan(&factID); err != nil {
				rows.Close()
				return nil, err
			}
			s.CoveredFacts[factID] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	rows, err = db.QueryContext(ctx, `SELECT `+eventCols+` FROM events WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity events: %w", err)
	}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		s.Events = append(s.Events, *e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Event links for the whole campaign.
	rows, err = db.QueryContext(ctx, `
		SELECT l.id, l.from_event, l.to_event, l.link FROM event_links l
		 JOIN events e ON e.id = l.from_event WHERE e.campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity links: %w", err)
	}
	eventLinks := map[string][]EventLinkRef{}
	for rows.Next() {
		var l EventLinkRef
		if err := rows.Scan(&l.ID, &l.FromEvent, &l.ToEvent, &l.Link); err != nil {
			rows.Close()
			return nil, err
		}
		eventLinks[l.FromEvent] = append(eventLinks[l.FromEvent], l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT `+relationshipCols+` FROM relationships r
		 WHERE r.from_entity IN (SELECT id FROM entities WHERE campaign_id = ?)`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity relationships: %w", err)
	}
	for rows.Next() {
		r, err := scanRelationship(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		s.Relationships = append(s.Relationships, *r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT `+questCols+` FROM quests WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity quests: %w", err)
	}
	for rows.Next() {
		q, err := scanQuest(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		s.Quests = append(s.Quests, *q)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `
		SELECT t.id, t.quest_id, t.from_state, t.to_state, t.event_id, t.created_at
		  FROM quest_transitions t JOIN quests q ON q.id = t.quest_id WHERE q.campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity quest transitions: %w", err)
	}
	for rows.Next() {
		var (
			t       QuestTransition
			event   sql.NullString
			created int64
		)
		if err := rows.Scan(&t.ID, &t.QuestID, &t.FromState, &t.ToState, &event, &created); err != nil {
			rows.Close()
			return nil, err
		}
		t.EventID = event.String
		t.CreatedAt = unixMilli(created)
		s.QuestTransitions = append(s.QuestTransitions, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// The quest links into the graph and the knowledge layer (MAD-369).
	rows, err = db.QueryContext(ctx, `
		SELECT e.id, e.quest_id, e.entity_id, e.role, e.created_at
		  FROM quest_entities e JOIN quests q ON q.id = e.quest_id WHERE q.campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity quest entities: %w", err)
	}
	for rows.Next() {
		var (
			l       QuestEntity
			created int64
		)
		if err := rows.Scan(&l.ID, &l.QuestID, &l.EntityID, &l.Role, &created); err != nil {
			rows.Close()
			return nil, err
		}
		l.CreatedAt = unixMilli(created)
		s.QuestEntities = append(s.QuestEntities, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `
		SELECT f.id, f.quest_id, f.state_key, f.fact_id, f.disposition, f.created_at
		  FROM quest_state_facts f JOIN quests q ON q.id = f.quest_id WHERE q.campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity quest state facts: %w", err)
	}
	for rows.Next() {
		var (
			r       QuestStateFact
			created int64
		)
		if err := rows.Scan(&r.ID, &r.QuestID, &r.StateKey, &r.FactID, &r.Disposition, &created); err != nil {
			rows.Close()
			return nil, err
		}
		r.CreatedAt = unixMilli(created)
		s.QuestStateFacts = append(s.QuestStateFacts, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// The awareness join the ungrounded check needs: who holds what, at
	// which stance. DM-scope snapshot, like everything else here.
	rows, err = db.QueryContext(ctx,
		`SELECT knower, fact_id, stance FROM awareness WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity awareness: %w", err)
	}
	for rows.Next() {
		var a AwarenessView
		if err := rows.Scan(&a.Knower, &a.FactID, &a.Stance); err != nil {
			rows.Close()
			return nil, err
		}
		s.Awareness = append(s.Awareness, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// The acts a quest may point its act_id at — no foreign key by design,
	// so the snapshot carries the ids the dangling check sweeps against.
	s.ActIDs = map[string]bool{}
	rows, err = db.QueryContext(ctx,
		`SELECT id FROM acts WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity acts: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		s.ActIDs[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Attach links to events for the cause_after_effect check.
	for i := range s.Events {
		s.Events[i].Links = eventLinks[s.Events[i].ID]
	}

	// The clock face: current day, whether time has ever been moved on
	// purpose, and the schedule waiting on it (MAD-365).
	if err := db.QueryRowContext(ctx,
		`SELECT clock FROM campaigns WHERE id = ?`, campaignID).Scan(&s.Clock); err != nil {
		return nil, fmt.Errorf("integrity clock: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM game_sessions WHERE campaign_id = ?`, campaignID).Scan(&s.SessionCount); err != nil {
		return nil, fmt.Errorf("integrity sessions: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ?`, campaignID).Scan(&s.AdvanceCount); err != nil {
		return nil, fmt.Errorf("integrity advances: %w", err)
	}

	rows, err = db.QueryContext(ctx,
		`SELECT `+scheduleCols+` FROM scheduled_events WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity schedule: %w", err)
	}
	for rows.Next() {
		e, err := scanScheduledEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		s.Schedule = append(s.Schedule, *e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Faction plans and whether any step declares an enemy_plan precondition.
	rows, err = db.QueryContext(ctx, `
		SELECT p.id, p.faction_entity, p.name, p.state_machine, p.current_state, p.status,
		       p.rate_per_day, p.started_day, p.last_advanced_day, s.requires_json
		  FROM faction_plans p
		  LEFT JOIN faction_plan_steps s ON s.plan_id = p.id
		 WHERE p.campaign_id = ?
		 ORDER BY p.id, s.rowid`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity faction plans: %w", err)
	}
	byPlan := map[string]*FactionPlanView{}
	var order []string
	for rows.Next() {
		var (
			id, faction, name, machineJSON, current, status string
			rate                                            float64
			started, lastAdvanced                           sql.NullInt64
			requires                                        sql.NullString
		)
		if err := rows.Scan(&id, &faction, &name, &machineJSON, &current, &status,
			&rate, &started, &lastAdvanced, &requires); err != nil {
			rows.Close()
			return nil, err
		}
		view, ok := byPlan[id]
		if !ok {
			view = &FactionPlanView{
				ID: id, FactionEntity: faction, Name: name,
				CurrentState: current, Status: status, RatePerDay: rate,
			}
			// A machine that no longer parses reads as an empty machine:
			// plan_illegal_state fires, which is the honest finding.
			if m, err := ParseStateMachine(machineJSON); err == nil {
				view.Machine = m
			}
			if started.Valid {
				v := started.Int64
				view.StartedDay = &v
			}
			if lastAdvanced.Valid {
				v := lastAdvanced.Int64
				view.LastAdvanced = &v
			}
			byPlan[id] = view
			order = append(order, id)
		}
		if requires.Valid && requires.String != "" && requiresJSONHasEnemy(requires.String) {
			view.HasEnemyRequirement = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, id := range order {
		s.FactionPlans = append(s.FactionPlans, *byPlan[id])
	}

	// The rumour mill (MAD-374): statements in circulation and who repeats
	// them, truth values included — this is the DM snapshot the checks and
	// the dossier read. The player-safe reads live in internal/knowledge.
	rows, err = db.QueryContext(ctx, `
		SELECT id, campaign_id, statement, truth, COALESCE(about_entity, ''), COALESCE(fact_id, ''),
		       origin, spread, status, dm_only, created_by, created_at, updated_at
		  FROM rumors WHERE campaign_id = ?
		 ORDER BY created_at, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity rumors: %w", err)
	}
	for rows.Next() {
		var (
			r             Rumor
			about, factID sql.NullString
			created, upd  int64
		)
		if err := rows.Scan(&r.ID, &r.CampaignID, &r.Statement, &r.Truth, &about, &factID,
			&r.Origin, &r.Spread, &r.Status, &r.DMOnly, &r.CreatedBy, &created, &upd); err != nil {
			rows.Close()
			return nil, err
		}
		r.AboutEntity = about.String
		r.FactID = factID.String
		r.CreatedAt = unixMilli(created)
		r.UpdatedAt = unixMilli(upd)
		s.Rumors = append(s.Rumors, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `
		SELECT h.rumor_id, h.entity_id, h.variant, COALESCE(h.since_event, ''), h.created_at
		  FROM rumor_holders h JOIN rumors r ON r.id = h.rumor_id
		 WHERE r.campaign_id = ?
		 ORDER BY h.rumor_id, h.entity_id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("integrity rumor holders: %w", err)
	}
	for rows.Next() {
		var (
			h       RumorHolder
			created int64
		)
		if err := rows.Scan(&h.RumorID, &h.EntityID, &h.Variant, &h.SinceEvent, &created); err != nil {
			rows.Close()
			return nil, err
		}
		h.CreatedAt = unixMilli(created)
		s.RumorHolders = append(s.RumorHolders, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	return s, nil
}

// requiresJSONHasEnemy reports whether a step's requires_json declares an
// enemy_plan clause. Parsed, not substring-matched: hand-edited JSON is the
// normal case this survives.
func requiresJSONHasEnemy(raw string) bool {
	var reqs []map[string]any
	if err := json.Unmarshal([]byte(raw), &reqs); err != nil {
		return false
	}
	for _, req := range reqs {
		if clause, ok := req["enemy_plan"]; ok && clause != nil {
			return true
		}
	}
	return false
}

// Check runs every integrity rule over a snapshot, pure and deterministic.
// Findings are sorted by check, then record, so output is stable.
func Check(snap *Snapshot) []Finding {
	var out []Finding
	out = append(out, checkFactWithoutProvenance(snap)...)
	out = append(out, checkDanglingReference(snap)...)
	out = append(out, checkCauseAfterEffect(snap)...)
	out = append(out, checkQuestTransitionInvalid(snap)...)
	out = append(out, checkContradictoryFacts(snap)...)
	out = append(out, checkEntityMergeCandidate(snap)...)
	out = append(out, checkDuplicateFact(snap)...)
	out = append(out, checkEventAfterClock(snap)...)
	out = append(out, checkMissedSchedule(snap)...)
	out = append(out, checkClockNeverAdvanced(snap)...)
	out = append(out, checkPlanIllegalState(snap)...)
	out = append(out, checkPlanWithoutFaction(snap)...)
	out = append(out, checkPlanStalled(snap)...)
	out = append(out, checkFactionNoAntagonist(snap)...)
	out = append(out, checkQuestStateUnreachable(snap)...)
	out = append(out, checkQuestDeadEnd(snap)...)
	out = append(out, checkQuestNoEnding(snap)...)
	out = append(out, checkQuestTransitionUngrounded(snap)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Check != out[j].Check {
			return out[i].Check < out[j].Check
		}
		return out[i].RecordID < out[j].RecordID
	})
	return out
}

// checkFactWithoutProvenance: every fact carries at least one provenance row.
// The store enforces it on the write path; this catches whatever bypassed the
// store. A fact with no provenance is a bug by definition.
func checkFactWithoutProvenance(snap *Snapshot) []Finding {
	var out []Finding
	for _, f := range snap.Facts {
		if snap.ProvenanceCount[f.ID] == 0 {
			out = append(out, Finding{
				Check: CheckFactWithoutProvenance, Severity: SeverityError,
				RecordKind: "fact", RecordID: f.ID,
				Message: fmt.Sprintf("fact %q has no provenance row; every fact must say where it came from", f.Statement),
			})
		}
	}
	return out
}

// checkDanglingReference: a fact, event, edge or transition pointing at a
// row that is gone or soft-deleted. Entities are soft-deleted ('deleted'
// status) precisely so this check can see what still points at them.
func checkDanglingReference(snap *Snapshot) []Finding {
	var out []Finding
	entities := map[string]Entity{}
	for _, e := range snap.Entities {
		entities[e.ID] = e
	}
	live := func(id string) bool {
		e, ok := entities[id]
		return ok && e.Status != StatusDeleted
	}
	facts := map[string]bool{}
	for _, f := range snap.Facts {
		facts[f.ID] = true
	}
	events := map[string]bool{}
	for _, e := range snap.Events {
		events[e.ID] = true
	}
	for _, f := range snap.Facts {
		if !live(f.SubjectEntity) {
			out = append(out, dangling("fact", f.ID, f.Statement, "subject entity", f.SubjectEntity))
		}
		if f.ObjectEntity != "" && !live(f.ObjectEntity) {
			out = append(out, dangling("fact", f.ID, f.Statement, "object entity", f.ObjectEntity))
		}
		if f.SupersededBy != "" && !facts[f.SupersededBy] {
			out = append(out, dangling("fact", f.ID, f.Statement, "superseding fact", f.SupersededBy))
		}
	}
	for _, e := range snap.Events {
		if e.LocationEntity != "" && !live(e.LocationEntity) {
			out = append(out, dangling("event", e.ID, e.Summary, "location entity", e.LocationEntity))
		}
	}
	for _, r := range snap.Relationships {
		if !live(r.FromEntity) {
			out = append(out, dangling("relationship", r.ID, r.RelType, "from entity", r.FromEntity))
		}
		if !live(r.ToEntity) {
			out = append(out, dangling("relationship", r.ID, r.RelType, "to entity", r.ToEntity))
		}
		if r.JustifiedByFact != "" && !facts[r.JustifiedByFact] {
			out = append(out, dangling("relationship", r.ID, r.RelType, "justifying fact", r.JustifiedByFact))
		}
		if r.SinceEvent != "" && !events[r.SinceEvent] {
			out = append(out, dangling("relationship", r.ID, r.RelType, "since event", r.SinceEvent))
		}
	}
	for _, t := range snap.QuestTransitions {
		if t.EventID != "" && !events[t.EventID] {
			out = append(out, dangling("quest_transition", t.ID,
				t.FromState+" -> "+t.ToState, "event", t.EventID))
		}
	}
	// The quest links (MAD-369): a link row pointing at an entity that is
	// gone or soft-deleted, a state-fact tie pointing at a fact that is not
	// there or a state the machine never declared, and a quest whose act_id
	// names an act the campaign no longer holds.
	for _, l := range snap.QuestEntities {
		if !live(l.EntityID) {
			out = append(out, dangling("quest_entity", l.ID, l.Role, "entity", l.EntityID))
		}
	}
	machines := map[string]StateMachine{}
	for _, q := range snap.Quests {
		machines[q.ID] = q.Machine
	}
	for _, r := range snap.QuestStateFacts {
		if !facts[r.FactID] {
			out = append(out, dangling("quest_state_fact", r.ID, r.StateKey, "fact", r.FactID))
			continue
		}
		if m, ok := machines[r.QuestID]; ok {
			if _, declared := m.State(r.StateKey); !declared {
				out = append(out, dangling("quest_state_fact", r.ID, r.FactID, "state", r.StateKey))
			}
		}
	}
	for _, q := range snap.Quests {
		if q.ActID != "" && !snap.ActIDs[q.ActID] {
			out = append(out, dangling("quest", q.ID, q.Name, "act", q.ActID))
		}
	}
	return out
}

func dangling(kind, id, what, refKind, refID string) Finding {
	return Finding{
		Check: CheckDanglingReference, Severity: SeverityError,
		RecordKind: kind, RecordID: id,
		Message: fmt.Sprintf("%s %q points at %s %s that no longer exists", kind, what, refKind, refID),
	}
}

// checkCauseAfterEffect: a 'caused' link whose cause is dated after its
// effect on the campaign clock. Unknown clocks (nil) are skipped — an
// undated pair is not a violation, it is an undated pair.
func checkCauseAfterEffect(snap *Snapshot) []Finding {
	var out []Finding
	byID := map[string]Event{}
	for _, e := range snap.Events {
		byID[e.ID] = e
	}
	for _, from := range snap.Events {
		for _, l := range from.Links {
			if l.Link != LinkCaused || l.FromEvent != from.ID {
				continue
			}
			to, ok := byID[l.ToEvent]
			if !ok {
				continue // dangling_reference owns missing events
			}
			if from.ClockAt != nil && to.ClockAt != nil && *from.ClockAt > *to.ClockAt {
				out = append(out, Finding{
					Check: CheckCauseAfterEffect, Severity: SeverityError,
					RecordKind: "event", RecordID: from.ID,
					Message: fmt.Sprintf("%q (clock day %d) is recorded as causing %q, which happened earlier (clock day %d)",
						from.Summary, *from.ClockAt, to.Summary, *to.ClockAt),
				})
			}
		}
	}
	return out
}

// checkQuestTransitionInvalid: a recorded move along an edge the quest's
// machine does not have, or naming a state the machine never declared.
func checkQuestTransitionInvalid(snap *Snapshot) []Finding {
	var out []Finding
	machines := map[string]StateMachine{}
	for _, q := range snap.Quests {
		machines[q.ID] = q.Machine
	}
	for _, t := range snap.QuestTransitions {
		m, ok := machines[t.QuestID]
		if !ok {
			continue
		}
		if !m.HasEdge(t.FromState, t.ToState) {
			out = append(out, Finding{
				Check: CheckQuestTransitionInvalid, Severity: SeverityError,
				RecordKind: "quest_transition", RecordID: t.ID,
				Message: fmt.Sprintf("quest moved %s -> %s along an edge its state machine does not have", t.FromState, t.ToState),
			})
		}
	}
	return out
}

// checkContradictoryFacts: two credibly-sourced facts (canon or derived)
// asserting different objects for the same subject and predicate, with no
// open register entry covering them. The register is the fix: registering the
// pair downgrades both to 'contested' and this check stops firing.
func checkContradictoryFacts(snap *Snapshot) []Finding {
	type key struct{ subject, predicate string }
	byKey := map[key][]Fact{}
	for _, f := range snap.Facts {
		if f.Confidence != ConfidenceCanon && f.Confidence != ConfidenceDerived {
			continue
		}
		if f.SupersededBy != "" {
			continue // retconned history is allowed to disagree with the present
		}
		k := key{f.SubjectEntity, f.Predicate}
		byKey[k] = append(byKey[k], f)
	}
	var out []Finding
	for k, group := range byKey {
		if len(group) < 2 {
			continue
		}
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				a, b := group[i], group[j]
				if a.ObjectEntity == b.ObjectEntity && a.ObjectLiteral == b.ObjectLiteral {
					continue // same object is duplicate_fact's problem
				}
				if snap.CoveredFacts[a.ID] && snap.CoveredFacts[b.ID] {
					continue // the register already holds this pair
				}
				out = append(out, Finding{
					Check: CheckContradictoryFacts, Severity: SeverityReview,
					RecordKind: "fact", RecordID: a.ID,
					Message: fmt.Sprintf("%q and fact %s both credibly assert %q on the same subject with different objects; register the contradiction or supersede one side",
						a.Statement, b.ID, k.predicate),
				})
			}
		}
	}
	return out
}

// checkEntityMergeCandidate: one name on two non-deleted entities — the "Tom
// the innkeeper is also Thomas Vane" problem in reverse, where two rows
// really are the same person. Name matching folds case and surrounding
// whitespace only; anything smarter belongs to a model, not a join.
func checkEntityMergeCandidate(snap *Snapshot) []Finding {
	entities := map[string]Entity{}
	for _, e := range snap.Entities {
		entities[e.ID] = e
	}
	byName := map[string][]string{}
	for _, n := range snap.Names {
		e, ok := entities[n.EntityID]
		if !ok || e.Status == StatusDeleted {
			continue
		}
		norm := strings.ToLower(strings.TrimSpace(n.Name))
		byName[norm] = append(byName[norm], n.EntityID)
	}
	var out []Finding
	for _, ids := range byName {
		distinct := map[string]bool{}
		for _, id := range ids {
			distinct[id] = true
		}
		if len(distinct) < 2 {
			continue
		}
		sorted := make([]string, 0, len(distinct))
		for id := range distinct {
			sorted = append(sorted, id)
		}
		sort.Strings(sorted)
		out = append(out, Finding{
			Check: CheckEntityMergeCandidate, Severity: SeverityReview,
			RecordKind: "entity", RecordID: sorted[0],
			Message: fmt.Sprintf("entities %s share a name; they may be one entity recorded twice", strings.Join(sorted, ", ")),
		})
	}
	return out
}

// checkDuplicateFact: the same subject/predicate/object asserted by two fact
// rows, neither superseded. Duplicates are noise the deterministic engine and
// the DM both trip over.
func checkDuplicateFact(snap *Snapshot) []Finding {
	type key struct{ subject, predicate, objectEntity, objectLiteral string }
	seen := map[key]string{}
	reported := map[key]bool{}
	var out []Finding
	for _, f := range snap.Facts {
		if f.SupersededBy != "" {
			continue
		}
		k := key{f.SubjectEntity, f.Predicate, f.ObjectEntity, f.ObjectLiteral}
		if first, ok := seen[k]; ok {
			if !reported[k] {
				reported[k] = true
				out = append(out, Finding{
					Check: CheckDuplicateFact, Severity: SeverityReview,
					RecordKind: "fact", RecordID: first,
					Message: fmt.Sprintf("facts %s and %s assert the same subject/predicate/object", first, f.ID),
				})
			}
			continue
		}
		seen[k] = f.ID
	}
	return out
}

// checkEventAfterClock: an event dated past the campaign's current day. A
// warning, not an error — the DM may be recording something the table has
// not lived through yet (a prophecy, a flash-forward), but it deserves a
// look because events are meant to have happened.
func checkEventAfterClock(snap *Snapshot) []Finding {
	var out []Finding
	for _, e := range snap.Events {
		if e.ClockAt == nil || *e.ClockAt <= snap.Clock {
			continue
		}
		out = append(out, Finding{
			Check: CheckEventAfterClock, Severity: SeverityWarn,
			RecordKind: "event", RecordID: e.ID,
			Message: fmt.Sprintf("%q is dated to clock day %d, past the campaign's current day %d",
				e.Summary, *e.ClockAt, snap.Clock),
		})
	}
	return out
}

// checkMissedSchedule: a pending schedule entry whose day is behind the
// clock. The world moved past a plan nobody resolved: fire it, cancel it, or
// mark it missed — but decide.
func checkMissedSchedule(snap *Snapshot) []Finding {
	var out []Finding
	for _, e := range snap.Schedule {
		if e.Status != SchedulePending || e.Day >= snap.Clock {
			continue
		}
		out = append(out, Finding{
			Check: CheckMissedSchedule, Severity: SeverityWarn,
			RecordKind: "scheduled_event", RecordID: e.ID,
			Message: fmt.Sprintf("%q (day %d) is still pending behind the clock at day %d; fire, cancel or miss it",
				e.Name, e.Day, snap.Clock),
		})
	}
	return out
}

// checkClockNeverAdvanced: sessions played while the clock sits at the start.
// An info, not an accusation — some tables genuinely never track time — but
// every other clock feature silently degrades when this is true.
func checkClockNeverAdvanced(snap *Snapshot) []Finding {
	if snap.SessionCount == 0 || snap.Clock != 0 || snap.AdvanceCount > 0 {
		return nil
	}
	return []Finding{{
		Check: CheckClockNeverAdvanced, Severity: SeverityInfo,
		RecordKind: "campaign", RecordID: snap.CampaignID,
		Message: fmt.Sprintf("%d session(s) played and the clock has never left day 0; advancing it unlocks dates, travel and the schedule",
			snap.SessionCount),
	}}
}

// checkPlanIllegalState: a faction plan sitting in a state its own machine
// does not declare — the plan analogue of the quest check. A machine that no
// longer parses reads as empty and fires here too, which is the honest
// finding for hand-edited JSON.
func checkPlanIllegalState(snap *Snapshot) []Finding {
	var out []Finding
	for _, p := range snap.FactionPlans {
		declared := map[string]bool{}
		for _, st := range p.Machine.States {
			declared[st.Key] = true
		}
		if declared[p.CurrentState] {
			continue
		}
		out = append(out, Finding{
			Check: CheckPlanIllegalState, Severity: SeverityError,
			RecordKind: "faction_plan", RecordID: p.ID,
			Message: fmt.Sprintf("plan %q sits in state %q, which its state machine does not declare", p.Name, p.CurrentState),
		})
	}
	return out
}

// checkPlanWithoutFaction: a plan whose owner is not a live faction —
// missing, a different kind, or destroyed. The write path refuses all three;
// this catches whatever bypassed it.
func checkPlanWithoutFaction(snap *Snapshot) []Finding {
	var out []Finding
	entities := map[string]Entity{}
	for _, e := range snap.Entities {
		entities[e.ID] = e
	}
	for _, p := range snap.FactionPlans {
		owner, ok := entities[p.FactionEntity]
		if !ok {
			out = append(out, Finding{
				Check: CheckPlanWithoutFaction, Severity: SeverityError,
				RecordKind: "faction_plan", RecordID: p.ID,
				Message: fmt.Sprintf("plan %q is owned by entity %s, which does not exist", p.Name, p.FactionEntity),
			})
			continue
		}
		if owner.Kind != KindFaction {
			out = append(out, Finding{
				Check: CheckPlanWithoutFaction, Severity: SeverityError,
				RecordKind: "faction_plan", RecordID: p.ID,
				Message: fmt.Sprintf("plan %q is owned by %q, which is a %s, not a faction", p.Name, owner.Name, owner.Kind),
			})
			continue
		}
		if owner.Status == StatusDestroyed {
			out = append(out, Finding{
				Check: CheckPlanWithoutFaction, Severity: SeverityError,
				RecordKind: "faction_plan", RecordID: p.ID,
				Message: fmt.Sprintf("plan %q belongs to %q, which is destroyed", p.Name, owner.Name),
			})
		}
	}
	return out
}

// checkPlanStalled: an active plan with a positive rate that has not advanced
// in StalledAfterDays in-world days. Measured from the last advance, or from
// the day it started when it has never advanced. A warning, not an error —
// the world may simply have been busy elsewhere — but a plan that claims to
// move and does not deserves a look.
func checkPlanStalled(snap *Snapshot) []Finding {
	var out []Finding
	for _, p := range snap.FactionPlans {
		if p.Status != PlanActive || p.RatePerDay <= 0 {
			continue
		}
		ref := p.LastAdvanced
		if ref == nil {
			ref = p.StartedDay
		}
		if ref == nil {
			continue // never started counting; nothing to measure against
		}
		if snap.Clock-*ref < StalledAfterDays {
			continue
		}
		out = append(out, Finding{
			Check: CheckPlanStalled, Severity: SeverityWarn,
			RecordKind: "faction_plan", RecordID: p.ID,
			Message: fmt.Sprintf("plan %q is active at %s/day but has not advanced since day %d (the clock is at %d)",
				p.Name, formatRate(p.RatePerDay), *ref, snap.Clock),
		})
	}
	return out
}

// checkFactionNoAntagonist: an active plan whose faction has no enemy_of
// edge and no step carrying an opposing precondition. Nobody is in its way,
// which is either a boring world or a missing edge — an info either way.
func checkFactionNoAntagonist(snap *Snapshot) []Finding {
	var out []Finding
	for _, p := range snap.FactionPlans {
		if p.Status != PlanActive || p.HasEnemyRequirement {
			continue
		}
		blocked := false
		for _, r := range snap.Relationships {
			if r.RelType != "enemy_of" {
				continue
			}
			if r.FromEntity == p.FactionEntity || r.ToEntity == p.FactionEntity {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		out = append(out, Finding{
			Check: CheckFactionNoAntagonist, Severity: SeverityInfo,
			RecordKind: "faction_plan", RecordID: p.ID,
			Message: fmt.Sprintf("plan %q is active with no enemy in its way; add an enemy_of edge or an opposing step precondition, or it will simply happen",
				p.Name),
		})
	}
	return out
}

// formatRate renders a rate without trailing zeros for finding prose.
func formatRate(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

/* ---------- the quest graph checks (MAD-369) ---------- */

// checkQuestStateUnreachable: a declared state with no path from the initial
// one. A warning, not an error — a DM may be mid-edit — but a state the
// machine can never reach is either a leftover or a missing edge, and both
// deserve a look before the generators (MAD-371) plan scenes against it.
func checkQuestStateUnreachable(snap *Snapshot) []Finding {
	var out []Finding
	for _, q := range snap.Quests {
		reach := q.Machine.Reachable(q.Machine.Initial)
		// Deterministic order: declaration order, not map iteration.
		for _, st := range q.Machine.States {
			if st.Key == q.Machine.Initial || reach[st.Key] {
				continue
			}
			out = append(out, Finding{
				Check: CheckQuestStateUnreachable, Severity: SeverityWarn,
				RecordKind: "quest", RecordID: q.ID,
				Message: fmt.Sprintf("state %q of quest %q has no path from the initial state %q",
					st.Key, q.Name, q.Machine.Initial),
			})
		}
	}
	return out
}

// checkQuestDeadEnd: a non-terminal state with no outgoing edge. A terminal
// state is an ending and may close; a passing state the machine cannot leave
// traps the quest mid-story.
func checkQuestDeadEnd(snap *Snapshot) []Finding {
	var out []Finding
	for _, q := range snap.Quests {
		outgoing := map[string]bool{}
		for _, e := range q.Machine.Edges {
			outgoing[e.From] = true
		}
		for _, st := range q.Machine.States {
			if st.Terminal != TerminalNone || outgoing[st.Key] {
				continue
			}
			out = append(out, Finding{
				Check: CheckQuestDeadEnd, Severity: SeverityWarn,
				RecordKind: "quest", RecordID: q.ID,
				Message: fmt.Sprintf("state %q of quest %q is not an ending and has no outgoing edge; mark it terminal or add the branch out",
					st.Key, q.Name),
			})
		}
	}
	return out
}

// checkQuestNoEnding: no terminal state reachable from where the quest sits.
// The party may be in a branch that can no longer conclude — the machine
// drifted under its own history, or the endings were never authored.
func checkQuestNoEnding(snap *Snapshot) []Finding {
	var out []Finding
	for _, q := range snap.Quests {
		if q.Status != QuestActive {
			continue // a finished or abandoned quest no longer owes an ending
		}
		reach := q.Machine.Reachable(q.CurrentState)
		anyEnding := false
		for key := range reach {
			if q.Machine.IsTerminal(key) {
				anyEnding = true
				break
			}
		}
		if anyEnding {
			continue
		}
		out = append(out, Finding{
			Check: CheckQuestNoEnding, Severity: SeverityWarn,
			RecordKind: "quest", RecordID: q.ID,
			Message: fmt.Sprintf("quest %q has no ending reachable from its current state %q",
				q.Name, q.CurrentState),
		})
	}
	return out
}

// checkQuestTransitionUngrounded: a recorded move along an edge whose
// requires names a fact the party holds no granting stance on. The join is
// against the awareness table, not a model: "the party" is the party knower
// and the player characters, exactly the knowers the player scopes read.
// A required fact that does not exist at all is ungrounded too — nothing can
// hold a stance on it.
func checkQuestTransitionUngrounded(snap *Snapshot) []Finding {
	pcs := map[string]bool{}
	for _, e := range snap.Entities {
		if e.Kind == KindPC && e.Status != StatusDeleted {
			pcs[e.ID] = true
		}
	}
	held := map[string]bool{}
	for _, a := range snap.Awareness {
		if a.Knower != PartyKnower && !pcs[a.Knower] {
			continue
		}
		switch a.Stance {
		case "knows", "suspects", "believes_false":
			held[a.FactID] = true
		}
	}
	var out []Finding
	for _, t := range snap.QuestTransitions {
		var q *Quest
		for i := range snap.Quests {
			if snap.Quests[i].ID == t.QuestID {
				q = &snap.Quests[i]
				break
			}
		}
		if q == nil {
			continue
		}
		edge, ok := q.Machine.Edge(t.FromState, t.ToState)
		if !ok || len(edge.Requires) == 0 {
			continue
		}
		var missing []string
		for _, fid := range edge.Requires {
			if !held[fid] {
				missing = append(missing, fid)
			}
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		out = append(out, Finding{
			Check: CheckQuestTransitionUngrounded, Severity: SeverityWarn,
			RecordKind: "quest_transition", RecordID: t.ID,
			Message: fmt.Sprintf("quest %q moved %s -> %s along an edge requiring %s, which the party holds no granting stance on",
				q.Name, t.FromState, t.ToState, strings.Join(missing, ", ")),
		})
	}
	return out
}
