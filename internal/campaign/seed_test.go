package campaign

import (
	"context"
	"testing"
)

// The fixture is the substrate later stages build on, so this test pins its
// promised shape: a dozen-plus entities, provenance on every fact, one
// registered contradiction with both sides contested, and a quest with a real
// state machine partway through.
func TestSeedFixture(t *testing.T) {
	db := openDB(t)
	userIDs(t, db, "keeper", "player-1")
	ctx := context.Background()

	fx, err := Seed(ctx, db, "keeper", "player-1")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, _ := New(db)
	cid := fx.Campaign.ID

	entities, err := s.ListEntities(ctx, ScopeDM, cid, "")
	if err != nil {
		t.Fatalf("list entities: %v", err)
	}
	if len(entities) < 12 {
		t.Fatalf("the fixture promises a dozen entities, got %d", len(entities))
	}

	// Every fact carries provenance, and at least one fact is secret.
	facts, err := s.ListFacts(ctx, ScopeDM, cid, FactFilter{})
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	if len(facts) < 4 {
		t.Fatalf("fixture should carry several facts: %d", len(facts))
	}
	secrets := 0
	for _, f := range facts {
		prov, err := s.FactProvenance(ctx, ScopeDM, cid, f.ID)
		if err != nil {
			t.Fatalf("provenance for %s: %v", f.ID, err)
		}
		if len(prov) == 0 {
			t.Fatalf("fact %s has no provenance; that is a bug in the fixture", f.ID)
		}
		if f.Visibility == VisibilitySecret {
			secrets++
		}
	}
	if secrets == 0 {
		t.Fatal("fixture should include at least one secret fact")
	}

	// The registered contradiction: both sides contested, linked, no winner.
	visited, err := s.GetFact(ctx, ScopeDM, cid, fx.FactDukeVisited)
	if err != nil {
		t.Fatalf("get visited: %v", err)
	}
	never, err := s.GetFact(ctx, ScopeDM, cid, fx.FactDukeNever)
	if err != nil {
		t.Fatalf("get never: %v", err)
	}
	if visited.Confidence != ConfidenceContested || never.Confidence != ConfidenceContested {
		t.Fatalf("both sides must be downgraded to contested: %s / %s",
			visited.Confidence, never.Confidence)
	}
	versions, err := s.FactVersions(ctx, ScopeDM, cid, fx.ContradictionID)
	if err != nil || len(versions) != 2 {
		t.Fatalf("the contradiction must hold both sides: %v %v", versions, err)
	}

	// The quest: a branching machine, moved twice, sitting on survivors_found.
	q, err := s.GetQuest(ctx, ScopeDM, cid, fx.QuestID)
	if err != nil {
		t.Fatalf("get quest: %v", err)
	}
	if len(q.Machine.Edges) < 6 {
		t.Fatalf("the fixture's machine must really branch: %+v", q.Machine)
	}
	if q.CurrentState != "survivors_found" {
		t.Fatalf("fixture quest should be partway through: %s", q.CurrentState)
	}
	moves, err := s.QuestTransitions(ctx, ScopeDM, cid, fx.QuestID)
	if err != nil || len(moves) != 2 {
		t.Fatalf("want two recorded moves: %v %v", moves, err)
	}

	// Dual ordering: the flashback happened earlier in-world but later in play.
	timeline, err := s.ListEvents(ctx, ScopeDM, cid)
	if err != nil || len(timeline) != 2 {
		t.Fatalf("fixture timeline: %v %v", timeline, err)
	}
	if timeline[0].ID != fx.EventAmbush || timeline[1].ID != fx.EventSurvivors {
		t.Fatalf("play order: %+v", timeline)
	}
	if timeline[0].ClockAt == nil || timeline[1].ClockAt == nil {
		t.Fatalf("fixture events must both be dated: %+v", timeline)
	}
	if !(*timeline[0].ClockAt > *timeline[1].ClockAt) {
		t.Fatalf("in-world order must deliberately diverge from play order: %d then %d",
			*timeline[0].ClockAt, *timeline[1].ClockAt)
	}

	// The alias resolves: Tom the Innkeeper is also Thomas Vane.
	hits, err := s.ResolveName(ctx, ScopeDM, cid, "Thomas Vane")
	if err != nil || len(hits) != 1 || hits[0].ID != fx.Tom {
		t.Fatalf("alias resolve: %v %v", hits, err)
	}

	// The player member is tied to their pc.
	if role, ok, _ := s.Role(ctx, cid, "player-1"); !ok || role != RolePlayer {
		t.Fatalf("player membership missing: %q %v", role, ok)
	}
	members, _ := s.Members(ctx, cid)
	var character string
	for _, m := range members {
		if m.UserID == "player-1" {
			character = m.CharacterID
		}
	}
	if character != fx.Thalia {
		t.Fatalf("player's character must be Thalia: %q", character)
	}

	// The whole fixture must pass its own integrity checks.
	findings, err := Integrity(ctx, ScopeDM, db, cid)
	if err != nil {
		t.Fatalf("integrity: %v", err)
	}
	for _, f := range findings {
		if f.Severity == SeverityError {
			t.Fatalf("fixture must be free of error findings: %+v", f)
		}
	}
}
