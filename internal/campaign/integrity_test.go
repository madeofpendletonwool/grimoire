package campaign

import (
	"context"
	"testing"
)

// The pure checks run over snapshots, so most of this file builds snapshots
// in memory — no database, which is exactly the property that makes every
// rule unit-testable.

func entity(id, name, status string) Entity {
	return Entity{ID: id, CampaignID: "c1", Kind: KindNPC, Name: name, Status: status}
}

func fact(id, subject, predicate, objectEntity, objectLiteral, confidence string) Fact {
	return Fact{ID: id, CampaignID: "c1", SubjectEntity: subject, Predicate: predicate,
		ObjectEntity: objectEntity, ObjectLiteral: objectLiteral, Statement: "stmt " + id,
		Confidence: confidence, Visibility: VisibilityPublic}
}

func TestCheckFactWithoutProvenance(t *testing.T) {
	snap := &Snapshot{
		CampaignID: "c1",
		Facts: []Fact{
			fact("f1", "e1", "owns", "e2", "", ConfidenceCanon),
			fact("f2", "e1", "knows", "", "someone", ConfidenceCanon),
		},
		ProvenanceCount: map[string]int{"f1": 2},
	}
	got := checkFactWithoutProvenance(snap)
	if len(got) != 1 || got[0].RecordID != "f2" || got[0].Severity != SeverityError {
		t.Fatalf("want one error on f2: %+v", got)
	}
}

func TestCheckDanglingReferenceTable(t *testing.T) {
	deleted := entity("e1", "The Duke", StatusDeleted)
	alive := entity("e2", "The Mines", StatusActive)

	cases := []struct {
		name string
		snap *Snapshot
		want int // number of findings
	}{
		{
			name: "fact on a deleted subject",
			snap: &Snapshot{Entities: []Entity{deleted, alive},
				Facts: []Fact{fact("f1", "e1", "owns", "e2", "", ConfidenceCanon)}},
			want: 1,
		},
		{
			name: "fact on a deleted object",
			snap: &Snapshot{Entities: []Entity{deleted, alive},
				Facts: []Fact{fact("f1", "e2", "owns", "e1", "", ConfidenceCanon)}},
			want: 1,
		},
		{
			name: "fact on a missing subject",
			snap: &Snapshot{Entities: []Entity{alive},
				Facts: []Fact{fact("f1", "ghost", "owns", "e2", "", ConfidenceCanon)}},
			want: 1,
		},
		{
			name: "event at a deleted location",
			snap: &Snapshot{Entities: []Entity{deleted},
				Events: []Event{{ID: "v1", CampaignID: "c1", Summary: "s", LocationEntity: "e1"}}},
			want: 1,
		},
		{
			name: "relationship to a deleted entity",
			snap: &Snapshot{Entities: []Entity{deleted, alive},
				Relationships: []Relationship{{ID: "r1", FromEntity: "e2", RelType: "knows", ToEntity: "e1"}}},
			want: 1,
		},
		{
			name: "relationship justified by a missing fact",
			snap: &Snapshot{Entities: []Entity{alive, entity("e3", "X", StatusActive)},
				Relationships: []Relationship{{ID: "r1", FromEntity: "e2", RelType: "knows", ToEntity: "e3", JustifiedByFact: "gone"}}},
			want: 1,
		},
		{
			name: "superseded by a missing fact",
			snap: &Snapshot{Entities: []Entity{alive},
				Facts: []Fact{fact("f1", "e2", "owns", "", "x", ConfidenceRetconned)}},
			// f1.SupersededBy is empty here, so plant it below.
			want: 0,
		},
		{
			name: "transition tied to a missing event",
			snap: &Snapshot{Entities: []Entity{alive},
				Quests: []Quest{{ID: "q1", CampaignID: "c1", Name: "q", CurrentState: "b",
					Machine: StateMachine{Initial: "a", States: []string{"a", "b"},
						Edges: []StateEdge{{From: "a", To: "b"}}}}},
				QuestTransitions: []QuestTransition{{ID: "t1", QuestID: "q1", FromState: "a", ToState: "b", EventID: "gone"}}},
			want: 1,
		},
		{
			name: "clean graph",
			snap: &Snapshot{Entities: []Entity{alive, entity("e3", "Y", StatusActive)},
				Facts: []Fact{fact("f1", "e2", "knows", "e3", "", ConfidenceCanon)}},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkDanglingReference(tc.snap)
			if len(got) != tc.want {
				t.Fatalf("want %d finding(s), got %d: %+v", tc.want, len(got), got)
			}
			for _, f := range got {
				if f.Check != CheckDanglingReference || f.Severity != SeverityError {
					t.Fatalf("wrong code/severity: %+v", f)
				}
			}
		})
	}

	// The superseded-by case needs the field planted.
	snap := &Snapshot{Entities: []Entity{alive},
		Facts: []Fact{fact("f1", "e2", "owns", "", "x", ConfidenceRetconned)}}
	snap.Facts[0].SupersededBy = "gone"
	if got := checkDanglingReference(snap); len(got) != 1 {
		t.Fatalf("superseded by a missing fact must dangle: %+v", got)
	}
}

func TestCheckCauseAfterEffectTable(t *testing.T) {
	day := func(v int64) *int64 { return &v }
	link := func(from string, clockFrom, clockTo *int64, linkKind string) *Snapshot {
		return &Snapshot{
			CampaignID: "c1",
			Events: []Event{
				{ID: from, CampaignID: "c1", Summary: "cause", ClockAt: clockFrom,
					Links: []EventLinkRef{{FromEvent: from, ToEvent: "effect", Link: linkKind}}},
				{ID: "effect", CampaignID: "c1", Summary: "effect", ClockAt: clockTo},
			},
		}
	}
	cases := []struct {
		name string
		snap *Snapshot
		want int
	}{
		{"cause before effect", link("v1", day(1), day(2), LinkCaused), 0},
		{"cause after effect", link("v1", day(3), day(2), LinkCaused), 1},
		{"same day", link("v1", day(2), day(2), LinkCaused), 0},
		{"undated cause", link("v1", nil, day(2), LinkCaused), 0},
		{"undated effect", link("v1", day(3), nil, LinkCaused), 0},
		// Enabled and revealed are not causation for this check.
		{"enabled after", link("v1", day(3), day(2), LinkEnabled), 0},
		{"revealed after", link("v1", day(3), day(2), LinkRevealed), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkCauseAfterEffect(tc.snap)
			if len(got) != tc.want {
				t.Fatalf("want %d finding(s), got %d: %+v", tc.want, len(got), got)
			}
		})
	}
}

func TestCheckQuestTransitionInvalidTable(t *testing.T) {
	machine := StateMachine{
		Initial: "a",
		States:  []string{"a", "b", "c"},
		Edges:   []StateEdge{{From: "a", To: "b"}, {From: "b", To: "c"}},
	}
	quest := Quest{ID: "q1", CampaignID: "c1", Name: "q", CurrentState: "b", Machine: machine}

	cases := []struct {
		name string
		t    QuestTransition
		want bool
	}{
		{"legal move", QuestTransition{ID: "t1", QuestID: "q1", FromState: "a", ToState: "b"}, false},
		{"invented edge", QuestTransition{ID: "t2", QuestID: "q1", FromState: "a", ToState: "c"}, true},
		{"undeclared state", QuestTransition{ID: "t3", QuestID: "q1", FromState: "a", ToState: "zzz"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &Snapshot{Quests: []Quest{quest}, QuestTransitions: []QuestTransition{tc.t}}
			got := checkQuestTransitionInvalid(snap)
			if tc.want && (len(got) != 1 || got[0].Severity != SeverityError) {
				t.Fatalf("want one error, got %+v", got)
			}
			if !tc.want && len(got) != 0 {
				t.Fatalf("want no findings, got %+v", got)
			}
		})
	}
}

func TestCheckContradictoryFactsTable(t *testing.T) {
	cases := []struct {
		name string
		snap *Snapshot
		want int
	}{
		{
			name: "two canon facts disagree",
			snap: &Snapshot{
				ProvenanceCount: map[string]int{},
				Facts: []Fact{
					fact("f1", "e1", "visited", "", "the mines", ConfidenceCanon),
					fact("f2", "e1", "visited", "", "nowhere", ConfidenceCanon),
				}},
			want: 1,
		},
		{
			name: "canon vs derived still counts",
			snap: &Snapshot{
				ProvenanceCount: map[string]int{},
				Facts: []Fact{
					fact("f1", "e1", "visited", "", "the mines", ConfidenceCanon),
					fact("f2", "e1", "visited", "", "nowhere", ConfidenceDerived),
				}},
			want: 1,
		},
		{
			name: "proposed facts are invisible, not contradictory",
			snap: &Snapshot{
				ProvenanceCount: map[string]int{},
				Facts: []Fact{
					fact("f1", "e1", "visited", "", "the mines", ConfidenceCanon),
					fact("f2", "e1", "visited", "", "nowhere", ConfidenceProposed),
				}},
			want: 0,
		},
		{
			name: "contested facts already registered",
			snap: &Snapshot{
				ProvenanceCount: map[string]int{},
				CoveredFacts:    map[string]bool{"f1": true, "f2": true},
				Facts: []Fact{
					fact("f1", "e1", "visited", "", "the mines", ConfidenceContested),
					fact("f2", "e1", "visited", "", "nowhere", ConfidenceContested),
				}},
			want: 0,
		},
		{
			name: "superseded history may disagree with the present",
			snap: &Snapshot{
				ProvenanceCount: map[string]int{},
				Facts: []Fact{
					fact("f1", "e1", "visited", "", "the mines", ConfidenceCanon),
					fact("f2", "e1", "visited", "", "nowhere", ConfidenceCanon),
				}},
			want: 0, // f2 is planted below as retconned via SupersededBy
		},
		{
			name: "different predicates never conflict",
			snap: &Snapshot{
				ProvenanceCount: map[string]int{},
				Facts: []Fact{
					fact("f1", "e1", "visited", "", "the mines", ConfidenceCanon),
					fact("f2", "e1", "fled", "", "nowhere", ConfidenceCanon),
				}},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "superseded history may disagree with the present" {
				tc.snap.Facts[1].SupersededBy = "f9"
			}
			got := checkContradictoryFacts(tc.snap)
			if len(got) != tc.want {
				t.Fatalf("want %d finding(s), got %d: %+v", tc.want, len(got), got)
			}
			for _, f := range got {
				if f.Severity != SeverityReview {
					t.Fatalf("contradictions are review severity: %+v", f)
				}
			}
		})
	}
}

func TestCheckEntityMergeCandidateTable(t *testing.T) {
	cases := []struct {
		name  string
		names []EntityName
		want  int
	}{
		{"distinct names", []EntityName{
			{EntityID: "e1", Name: "Tom"}, {EntityID: "e2", Name: "Bran"}}, 0},
		{"same name two entities", []EntityName{
			{EntityID: "e1", Name: "Tom"}, {EntityID: "e2", Name: "tom"}}, 1},
		{"same name one entity twice", []EntityName{
			{EntityID: "e1", Name: "Tom"}, {EntityID: "e1", Name: "Tom"}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &Snapshot{
				CampaignID: "c1",
				Entities:   []Entity{entity("e1", "A", StatusActive), entity("e2", "B", StatusActive)},
				Names:      tc.names,
			}
			got := checkEntityMergeCandidate(snap)
			if len(got) != tc.want {
				t.Fatalf("want %d finding(s), got %d: %+v", tc.want, len(got), got)
			}
		})
	}

	// A deleted entity no longer merges with anything.
	snap := &Snapshot{
		CampaignID: "c1",
		Entities:   []Entity{entity("e1", "Tom", StatusDeleted), entity("e2", "Tom", StatusActive)},
		Names:      []EntityName{{EntityID: "e1", Name: "Tom"}, {EntityID: "e2", Name: "Tom"}},
	}
	if got := checkEntityMergeCandidate(snap); len(got) != 0 {
		t.Fatalf("deleted entities must not merge: %+v", got)
	}
}

func TestCheckDuplicateFactTable(t *testing.T) {
	snap := &Snapshot{
		CampaignID: "c1",
		Facts: []Fact{
			fact("f1", "e1", "owns", "e2", "", ConfidenceCanon),
			fact("f2", "e1", "owns", "e2", "", ConfidenceDerived), // exact triple duplicate
			fact("f3", "e1", "owns", "e2", "", ConfidenceContested),
		},
	}
	// Three copies of one triple should report once, not three times.
	got := checkDuplicateFact(snap)
	if len(got) != 1 || got[0].Severity != SeverityReview {
		t.Fatalf("want one review finding: %+v", got)
	}

	// Superseded copies stop counting.
	snap2 := &Snapshot{
		CampaignID: "c1",
		Facts: []Fact{
			fact("f1", "e1", "owns", "e2", "", ConfidenceCanon),
			fact("f2", "e1", "owns", "e2", "", ConfidenceCanon),
		},
	}
	snap2.Facts[1].SupersededBy = "f1"
	if got := checkDuplicateFact(snap2); len(got) != 0 {
		t.Fatalf("superseded duplicates are history: %+v", got)
	}

	// Same triple shape but literal vs entity object is not a duplicate.
	snap3 := &Snapshot{
		CampaignID: "c1",
		Facts: []Fact{
			fact("f1", "e1", "owns", "e2", "", ConfidenceCanon),
			fact("f2", "e1", "owns", "", "the mines", ConfidenceCanon),
		},
	}
	if got := checkDuplicateFact(snap3); len(got) != 0 {
		t.Fatalf("different object kinds are different facts: %+v", got)
	}
}

func TestCheckOrdersFindingsStably(t *testing.T) {
	snap := &Snapshot{
		CampaignID: "c1",
		Entities:   []Entity{entity("e1", "A", StatusActive)},
		Facts: []Fact{
			fact("f2", "e1", "knows", "", "x", ConfidenceCanon),
			fact("f1", "e1", "knows", "", "x", ConfidenceCanon), // duplicate with f2
		},
		ProvenanceCount: map[string]int{"f1": 1, "f2": 0}, // f2 missing provenance
	}
	got := Check(snap)
	if len(got) != 2 {
		t.Fatalf("want duplicate + provenance findings: %+v", got)
	}
	if got[0].Check != CheckDuplicateFact || got[1].Check != CheckFactWithoutProvenance {
		t.Fatalf("findings sort by check code: %+v", got)
	}
}

/* ---------- the DB-backed path ---------- */

func TestIntegrityOverDatabase(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)
	duke, _ := s.CreateEntity(ctx, c.ID, KindNPC, "The Duke", "", nil)
	mines, _ := s.CreateEntity(ctx, c.ID, KindLocation, "The Mines", "", nil)

	if _, err := s.CreateFact(ctx, c.ID, duke.ID, "owns", mines.ID, "",
		"The Duke owns the mines.", ConfidenceCanon, VisibilityPublic, "keeper",
		[]ProvenanceInput{{Method: MethodDMAuthored}}); err != nil {
		t.Fatalf("create fact: %v", err)
	}

	findings, err := Integrity(ctx, ScopeDM, s.db, c.ID)
	if err != nil {
		t.Fatalf("integrity: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("a healthy graph must be clean: %+v", findings)
	}

	// Soft-delete the mines: the fact's object now dangles.
	if err := s.DeleteEntity(ctx, c.ID, mines.ID); err != nil {
		t.Fatalf("delete mines: %v", err)
	}
	findings, err = Integrity(ctx, ScopeDM, s.db, c.ID)
	if err != nil || len(findings) != 1 {
		t.Fatalf("want one dangling reference: %v %+v", err, findings)
	}
	if findings[0].Check != CheckDanglingReference || findings[0].Severity != SeverityError {
		t.Fatalf("wrong finding: %+v", findings[0])
	}

	// Strip a fact's provenance with raw SQL: the write path would never
	// allow it, and integrity must catch it.
	facts, _ := s.ListFacts(ctx, ScopeDM, c.ID, FactFilter{})
	if len(facts) != 1 {
		t.Fatalf("want the one fact: %+v", facts)
	}
	if _, err := s.db.Exec(`DELETE FROM fact_provenance WHERE fact_id = ?`, facts[0].ID); err != nil {
		t.Fatalf("strip provenance: %v", err)
	}
	findings, err = Integrity(ctx, ScopeDM, s.db, c.ID)
	if err != nil {
		t.Fatalf("integrity: %v", err)
	}
	codes := map[string]int{}
	for _, f := range findings {
		codes[f.Check]++
	}
	if codes[CheckDanglingReference] != 1 || codes[CheckFactWithoutProvenance] != 1 {
		t.Fatalf("want one dangling + one provenance finding: %+v", findings)
	}
}
