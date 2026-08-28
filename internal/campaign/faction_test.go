package campaign

import (
	"reflect"
	"testing"
)

func TestFactionAgentOfDecodesAndCleans(t *testing.T) {
	e := &Entity{Kind: KindFaction, Payload: map[string]any{
		"founded": 1203,
		"agent": map[string]any{
			"public_face":   "  A  burial society ",
			"private_truth": "the root's own church",
			"goals":         []any{"reopen the crypt", "  ", "sink the roots"},
			"military":      2,
			"economic":      4,
			"reach":         3,
		},
	}}
	a := FactionAgentOf(e)
	if a.PublicFace != "A burial society" {
		t.Errorf("public face: %q", a.PublicFace)
	}
	if len(a.Goals) != 2 || a.Goals[0] != "reopen the crypt" || a.Goals[1] != "sink the roots" {
		t.Errorf("goals order/content: %v", a.Goals)
	}
	if a.Military != 2 || a.Economic != 4 || a.Reach != 3 {
		t.Errorf("scores: %+v", a)
	}

	// Missing block, wrong shape, nil entity: all yield the zero agent.
	zero := FactionAgent{}
	if !reflect.DeepEqual(FactionAgentOf(&Entity{}), zero) {
		t.Errorf("missing block must be the zero agent")
	}
	if !reflect.DeepEqual(FactionAgentOf(&Entity{Payload: map[string]any{"agent": 7}}), zero) {
		t.Errorf("wrong shape must be the zero agent")
	}
	if !reflect.DeepEqual(FactionAgentOf(nil), zero) {
		t.Errorf("nil entity must be the zero agent")
	}
}

func TestWithFactionAgentPreservesOtherKeys(t *testing.T) {
	payload := map[string]any{"founded": 1203, "notes": "robed"}
	out := WithFactionAgent(payload, FactionAgent{
		PrivateTruth:      "the root's own church",
		Goals:             []string{"a", "", "b"},
		InternalConflicts: []string{"the crypt faction", " "},
	})
	if out["founded"] != 1203 || out["notes"] != "robed" {
		t.Fatalf("other payload keys clobbered: %v", out)
	}
	agent := FactionAgentOf(&Entity{Payload: out})
	if agent.PrivateTruth != "the root's own church" || len(agent.Goals) != 2 || len(agent.InternalConflicts) != 1 {
		t.Fatalf("agent round trip: %+v", agent)
	}
	if _, exists := payload["agent"]; exists {
		t.Fatalf("input payload was mutated")
	}
}

func TestFactionEdgesOfCategorizes(t *testing.T) {
	cult, shrine, tomb, elara, duke, order, party := "cult", "shrine", "tomb", "elara", "duke", "order", "party"
	rels := []Relationship{
		{ID: "r1", FromEntity: cult, RelType: "owns", ToEntity: shrine},
		{ID: "r2", FromEntity: tomb, RelType: "owned_by", ToEntity: cult},
		{ID: "r3", FromEntity: cult, RelType: "leads", ToEntity: elara},
		{ID: "r4", FromEntity: duke, RelType: "member_of", ToEntity: cult},
		{ID: "r5", FromEntity: order, RelType: "enemy_of", ToEntity: cult},
		{ID: "r6", FromEntity: cult, RelType: "secretly_controls", ToEntity: duke},
		{ID: "r7", FromEntity: party, RelType: "knows", ToEntity: elara}, // not dossier material
		{ID: "r8", FromEntity: elara, RelType: "knows", ToEntity: party},
	}
	got := FactionEdgesOf(cult, rels)
	if len(got.Territory) != 2 || got.Territory[0] != shrine || got.Territory[1] != tomb {
		t.Errorf("territory: %v", got.Territory)
	}
	if len(got.Leaders) != 1 || got.Leaders[0] != elara {
		t.Errorf("leaders: %v", got.Leaders)
	}
	if len(got.Members) != 1 || got.Members[0] != duke {
		t.Errorf("members: %v", got.Members)
	}
	if len(got.Enemies) != 1 || got.Enemies[0] != order {
		t.Errorf("enemies: %v", got.Enemies)
	}
	if len(got.Puppets) != 1 || got.Puppets[0] != duke {
		t.Errorf("puppets: %v", got.Puppets)
	}
	if len(got.Allies) != 0 {
		t.Errorf("allies: %v", got.Allies)
	}
}

// TestFactionEdgesReadLiveFromTheGraph is the read-from-graph rule the
// dossier exists to keep: a new owns edge changes territory with no write to
// the faction entity. Exercised through the store over a real database —
// the assertion is on the entity row, which must not move.
func TestFactionEdgesReadLiveFromTheGraph(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	c := seedCampaign(t, s)

	cult, err := s.CreateEntity(ctx, c.ID, KindFaction, "Cult of the Root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	shrine, err := s.CreateEntity(ctx, c.ID, KindLocation, "The Northern Shrine", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetEntity(ctx, ScopeDM, c.ID, cult.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateRelationship(ctx, c.ID, cult.ID, "owns", shrine.ID, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	rels, err := s.RelationshipsOf(ctx, ScopeDM, c.ID, cult.ID)
	if err != nil {
		t.Fatal(err)
	}
	edges := FactionEdgesOf(cult.ID, rels)
	if len(edges.Territory) != 1 || edges.Territory[0] != shrine.ID {
		t.Fatalf("adding an owns edge changes territory: %v", edges.Territory)
	}

	// And the entity row never moved.
	after, err := s.GetEntity(ctx, ScopeDM, c.ID, cult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("the faction entity was written: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
	if !reflect.DeepEqual(after.Payload, before.Payload) {
		t.Fatalf("the faction payload was written: %v -> %v", before.Payload, after.Payload)
	}
}
