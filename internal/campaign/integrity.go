package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Severity of an integrity finding. Errors mean the graph contradicts its own
// rules; reviews mean a human should look; warnings are soft signals (none
// exist yet — awareness-dependent checks land with the knowledge layer).
type Severity string

const (
	SeverityError  Severity = "error"
	SeverityReview Severity = "review"
	SeverityWarn   Severity = "warning"
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
	CheckFactWithoutProvenance  = "fact_without_provenance"
	CheckDanglingReference      = "dangling_reference"
	CheckCauseAfterEffect       = "cause_after_effect"
	CheckQuestTransitionInvalid = "quest_transition_invalid"
	CheckContradictoryFacts     = "contradictory_facts"
	CheckEntityMergeCandidate   = "entity_merge_candidate"
	CheckDuplicateFact          = "duplicate_fact"
)

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
}

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

	// Attach links to events for the cause_after_effect check.
	for i := range s.Events {
		s.Events[i].Links = eventLinks[s.Events[i].ID]
	}
	return s, nil
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
