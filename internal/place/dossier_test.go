package place

import (
	"reflect"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// The assembly tests are pure data, the shape the integrity rules' tests
// follow: hand-built snapshots, no database. What each case asserts is the
// one rule the dossier exists to keep — every list is read from the graph
// live, and the zero cases stay zero rather than erroring.

func ent(id, kind, name string) campaign.Entity {
	return campaign.Entity{ID: id, CampaignID: "c1", Kind: kind, Name: name, Status: campaign.StatusActive}
}

func TestDossierAssemblesTheWholePlace(t *testing.T) {
	loc := ent("town", campaign.KindLocation, "Blackwater")
	loc.Payload = map[string]any{
		"place": map[string]any{
			"kind": "town", "scale": "large village", "climate": "temperate",
			"services": []any{"inn", "market"}, "private_truth": "the well is compromised",
		},
		"travel": map[string]any{"routes": []any{
			map[string]any{"to": "monastery", "days": float64(1), "terrain": "road"},
			map[string]any{"to": "mines", "days": float64(2)},
		}},
	}
	child := ent("monastery", campaign.KindLocation, "Greyfall Monastery")
	hermit := ent("hermit", campaign.KindNPC, "The Hermit")
	troll := ent("troll", campaign.KindCreature, "Bridge troll")
	key := ent("key", campaign.KindItem, "The Silver Key")
	day34 := int64(34)
	secret := campaign.Fact{
		ID: "f-secret", SubjectEntity: "town", Visibility: campaign.VisibilitySecret,
		Confidence: campaign.ConfidenceCanon, Statement: "The well is compromised.",
	}
	secretObject := campaign.Fact{
		ID: "f-key", SubjectEntity: "key", ObjectEntity: "town",
		Visibility: campaign.VisibilitySecret, Confidence: campaign.ConfidenceCanon,
		Statement: "The key opens something under Blackwater.",
	}
	snap := &campaign.Snapshot{
		CampaignID: "c1",
		Entities:   []campaign.Entity{loc, child, hermit, troll, key, ent("duke", campaign.KindNPC, "The Duke")},
		Facts: []campaign.Fact{
			secret, secretObject,
			{ID: "f-retconned", SubjectEntity: "town", Visibility: campaign.VisibilitySecret,
				Confidence: campaign.ConfidenceCanon, SupersededBy: "f-secret", Statement: "old"},
			{ID: "f-public", SubjectEntity: "town", Visibility: campaign.VisibilityPublic,
				Confidence: campaign.ConfidenceCanon, Statement: "Blackwater floods each spring."},
		},
		Relationships: []campaign.Relationship{
			{ID: "r1", FromEntity: "monastery", RelType: "located_in", ToEntity: "town"},
			{ID: "r2", FromEntity: "town", RelType: "contains", ToEntity: "mines-ghost"}, // far end not in the snapshot
			{ID: "r3", FromEntity: "hermit", RelType: "located_in", ToEntity: "town"},
			{ID: "r4", FromEntity: "town", RelType: "contains", ToEntity: "troll"},
			{ID: "r5", FromEntity: "key", RelType: "located_in", ToEntity: "town"},
			{ID: "r6", FromEntity: "duke", RelType: "enemy_of", ToEntity: "town"}, // not a placement edge
		},
		Events: []campaign.Event{
			{ID: "e2", LocationEntity: "town", Summary: "later", RealOrdinal: 2, ClockAt: &day34},
			{ID: "e1", LocationEntity: "town", Summary: "earlier", RealOrdinal: 1},
			{ID: "e3", LocationEntity: "monastery", Summary: "elsewhere", RealOrdinal: 3},
		},
		Quests: []campaign.Quest{
			{ID: "q1", Name: "The Wrecked Caravan", Status: campaign.QuestActive, Visibility: campaign.QuestVisibilityPublic},
			{ID: "q2", Name: "Unrelated", Status: campaign.QuestActive},
		},
		QuestEntities: []campaign.QuestEntity{
			{ID: "l1", QuestID: "q1", EntityID: "town", Role: campaign.QuestRoleSite},
			{ID: "l2", QuestID: "q2", EntityID: "hermit", Role: campaign.QuestRoleSite},  // sited elsewhere
			{ID: "l3", QuestID: "q2", EntityID: "town", Role: campaign.QuestRoleGiver},   // wrong role
			{ID: "l4", QuestID: "ghost", EntityID: "town", Role: campaign.QuestRoleSite}, // quest not in the snapshot
		},
	}

	d, ok := Dossier(snap, "town")
	if !ok {
		t.Fatal("the town must assemble")
	}
	if d.Place.Kind != "town" || d.Place.PrivateTruth != "the well is compromised" {
		t.Fatalf("block: %+v", d.Place)
	}
	if len(d.Routes) != 2 || d.Routes[0].To != "monastery" || d.Routes[0].Days != 1 || d.Routes[0].Terrain != "road" {
		t.Fatalf("routes: %+v", d.Routes)
	}
	if len(d.Children) != 1 || d.Children[0].ID != "monastery" {
		t.Fatalf("children: %+v", d.Children)
	}
	if len(d.Present) != 2 || d.Present[0].ID != "troll" || d.Present[1].ID != "hermit" {
		t.Fatalf("present (npc and creature, by name): %+v", d.Present)
	}
	if len(d.Items) != 1 || d.Items[0].ID != "key" {
		t.Fatalf("items: %+v", d.Items)
	}
	if len(d.Secrets) != 2 {
		t.Fatalf("secrets (subject or object, never retconned): %+v", d.Secrets)
	}
	if len(d.Events) != 2 || d.Events[0].ID != "e1" || d.Events[1].ID != "e2" {
		t.Fatalf("events (sited here, play order): %+v", d.Events)
	}
	if len(d.Quests) != 1 || d.Quests[0].ID != "q1" {
		t.Fatalf("quests (site role only): %+v", d.Quests)
	}
	if len(d.Rumours) != 0 {
		t.Fatalf("rumours stay empty until MAD-374: %+v", d.Rumours)
	}
}

func TestDossierZeroBlockAndRefusals(t *testing.T) {
	loc := ent("town", campaign.KindLocation, "Blackwater")
	deleted := ent("gone", campaign.KindLocation, "The Drowned Town")
	deleted.Status = campaign.StatusDeleted
	npc := ent("duke", campaign.KindNPC, "The Duke")
	snap := &campaign.Snapshot{
		CampaignID: "c1",
		Entities:   []campaign.Entity{loc, deleted, npc},
		// A malformed block — hand-edited JSON — must read as the zero block,
		// not take the dossier down.
	}
	snap.Entities[0].Payload = map[string]any{"place": "nonsense"}

	d, ok := Dossier(snap, "town")
	if !ok || !reflect.DeepEqual(d.Place, campaign.Place{}) {
		t.Fatalf("malformed block yields the zero block: ok=%v %+v", ok, d.Place)
	}
	for _, id := range []string{"missing", "gone", "duke"} {
		if _, ok := Dossier(snap, id); ok {
			t.Fatalf("%s must not assemble", id)
		}
	}
	if _, ok := Dossier(nil, "town"); ok {
		t.Fatal("a nil snapshot must not assemble")
	}
}

func TestDossierIsDeterministicUnderShuffledInput(t *testing.T) {
	loc := ent("town", campaign.KindLocation, "Blackwater")
	a, b := ent("b-npc", campaign.KindNPC, "Anna"), ent("a-npc", campaign.KindNPC, "Boris")
	base := func() *campaign.Snapshot {
		return &campaign.Snapshot{
			CampaignID: "c1",
			Entities:   []campaign.Entity{loc, a, b},
			Relationships: []campaign.Relationship{
				{ID: "r2", FromEntity: "b-npc", RelType: "located_in", ToEntity: "town"},
				{ID: "r1", FromEntity: "a-npc", RelType: "located_in", ToEntity: "town"},
			},
		}
	}
	first, _ := Dossier(base(), "town")
	again, _ := Dossier(base(), "town")
	if first.Present[0].ID != "b-npc" || first.Present[1].ID != "a-npc" {
		t.Fatalf("present is name-ordered (Anna before Boris): %+v", first.Present)
	}
	if len(again.Present) != 2 || again.Present[0].ID != first.Present[0].ID {
		t.Fatal("the same snapshot assembles identically")
	}
}
