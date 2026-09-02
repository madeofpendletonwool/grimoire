package knowledge

import (
	"context"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/place"
)

/*
The location dossier's leak test, in the shape leak_test.go cuts: the same
seeded campaign, a location with an authored interior and live graph
material, and the two reads over it — the DM's snapshot and the party's
scoped snapshot. The player dossier must carry no secret fact, no private
truth and no NPC the party has not met, and must carry them the honest way:
the rows were never loaded into the snapshot at all. A secret the party has
deliberately been granted is planted too — the PlayerView rule drops even
granted secrets, and this test holds that line for the dossier.
*/

// plantLocation authors Blackwater: the place block (public half plus a
// private truth), a route out, resident NPCs (one met, one not), an item,
// secrets about the place (one granted to the party anyway), a witnessed
// event sited here, and quests sited here (one public, one secret).
func plantLocation(t *testing.T, s *Store, fx *campaign.Fixture) {
	t.Helper()
	ctx := context.Background()
	cid := fx.Campaign.ID
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}

	town, err := cs.GetEntity(ctx, campaign.ScopeDM, cid, fx.Blackwater)
	if err != nil {
		t.Fatal(err)
	}
	payload := campaign.WithPlace(town.Payload, campaign.Place{
		Kind: "town", Scale: "large village", Population: "about 900",
		Government: "a merchant council", Services: []string{"inn", "market"},
		Defences: "a palisade and a watch of twelve", Climate: "temperate",
		Senses: []string{"gull noise", "damp wool"}, State: "flooding after the rains",
		Danger: 2, PrivateTruth: "the root's tendrils reach the town well",
	})
	payload["travel"] = map[string]any{"routes": []map[string]any{
		{"to": fx.Monastery, "days": 1, "terrain": "road"},
	}}
	if _, err := cs.UpdateEntity(ctx, cid, fx.Blackwater, nil, nil, nil, payload); err != nil {
		t.Fatal(err)
	}

	// Tom is met: a public fact about him the party holds makes him a
	// visible entity, and his seed located_in edge then reads structurally.
	tomFact, err := cs.CreateFact(ctx, cid, fx.Tom, "keeps", "", "the Waystone",
		"Tom the innkeeper keeps the Waystone in Blackwater.",
		campaign.ConfidenceCanon, campaign.VisibilityPublic, "keeper",
		[]campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored, Quote: "the lamp is always lit"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAwareness(ctx, cid, campaign.PartyKnower, tomFact.ID, StanceKnows, 1, "", ""); err != nil {
		t.Fatal(err)
	}

	// The hermit is not met: located here, invisible to the party.
	hermit, err := cs.CreateEntity(ctx, cid, campaign.KindNPC, "The Hermit of the Falls", "Seen only by the falls.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CreateRelationship(ctx, cid, hermit.ID, "located_in", fx.Blackwater, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	t.Helper()

	// The Silver Key is here.
	if _, err := cs.CreateRelationship(ctx, cid, fx.Key, "located_in", fx.Blackwater, 0, "", ""); err != nil {
		t.Fatal(err)
	}

	// Secrets about the town — one the party has been granted, which must
	// still not reach the player dossier (the PlayerView rule).
	mkSecret := func(predicate, object, statement string) *campaign.Fact {
		t.Helper()
		f, err := cs.CreateFact(ctx, cid, fx.Blackwater, predicate, "", object, statement,
			campaign.ConfidenceCanon, campaign.VisibilitySecret, "keeper",
			[]campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored}})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	granted := mkSecret("hides", "a root-choked crypt", "Blackwater hides a root-choked crypt beneath the well.")
	if _, err := s.SetAwareness(ctx, cid, campaign.PartyKnower, granted.ID, StanceKnows, 1, "", ""); err != nil {
		t.Fatal(err)
	}
	mkSecret("floods", "every spring", "Blackwater floods every spring because the river is dammed upstream.")

	// Quests sited here: one public, one secret.
	machine := campaign.StateMachine{
		Initial: "start",
		States:  []campaign.State{{Key: "start"}, {Key: "done", Terminal: campaign.TerminalSuccess}},
		Edges:   []campaign.StateEdge{{From: "start", To: "done"}},
	}
	public, err := cs.CreateQuest(ctx, cid, campaign.QuestInput{
		Name: "The Wrecked Caravan", Summary: "Find the caravan.", Machine: machine,
		Visibility: campaign.QuestVisibilityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	secretQuest, err := cs.CreateQuest(ctx, cid, campaign.QuestInput{
		Name: "Silence the Hermit", Summary: "The cult's own errand.", Machine: machine,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{public.ID, secretQuest.ID} {
		if _, err := cs.AddQuestEntity(ctx, cid, q, fx.Blackwater, campaign.QuestRoleSite); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocationDossierTwoSnapshotsOneFunction(t *testing.T) {
	s, fx, _ := seeded(t)
	plantLocation(t, s, fx)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// The DM dossier: assembled over campaign.LoadSnapshot, complete.
	dmSnap, err := campaign.LoadSnapshot(ctx, campaign.ScopeDM, s.db, cid)
	if err != nil {
		t.Fatal(err)
	}
	dm, ok := place.Dossier(dmSnap, fx.Blackwater)
	if !ok {
		t.Fatal("the dm dossier must assemble")
	}
	if dm.Place.PrivateTruth == "" || len(dm.Routes) != 1 || len(dm.Secrets) != 2 {
		t.Fatalf("dm dossier incomplete: %+v", dm.Place)
	}
	if len(dm.Present) != 2 { // Tom and the hermit
		t.Fatalf("dm present: %+v", dm.Present)
	}
	if len(dm.Quests) != 2 { // public and secret alike
		t.Fatalf("dm quests: %+v", dm.Quests)
	}
	if len(dm.Events) != 1 || dm.Events[0].ID != fx.EventAmbush {
		t.Fatalf("dm events: %+v", dm.Events)
	}
	if len(dm.Items) != 1 {
		t.Fatalf("dm items: %+v", dm.Items)
	}

	// The player dossier: the same function over the scoped snapshot.
	view, err := s.PlayerViewOf(ScopeParty)
	if err != nil {
		t.Fatal(err)
	}
	psnap, err := view.PlaceSnapshot(ctx, cid, fx.Blackwater)
	if err != nil {
		t.Fatal(err)
	}
	pd, ok := place.Dossier(psnap, fx.Blackwater)
	if !ok {
		t.Fatal("the player dossier must assemble: the town is met (the ambush happened there)")
	}

	// The leak rules, asserted structurally.
	if len(pd.Secrets) != 0 {
		t.Fatalf("LEAK: secret facts in the player dossier: %+v", pd.Secrets)
	}
	if pd.Place.PrivateTruth != "" {
		t.Fatalf("LEAK: private truth in the player dossier: %q", pd.Place.PrivateTruth)
	}
	for _, e := range append(append([]campaign.Entity{}, pd.Present...), pd.Items...) {
		if e.ID != fx.Tom && e.ID != fx.Key {
			t.Fatalf("LEAK: unmet entity %s (%s) in the player dossier", e.Name, e.ID)
		}
	}
	if len(pd.Present) != 1 || pd.Present[0].ID != fx.Tom {
		t.Fatalf("only the met resident is present: %+v", pd.Present)
	}
	for _, q := range pd.Quests {
		if q.Name == "Silence the Hermit" {
			t.Fatalf("LEAK: the secret quest reached the player dossier: %+v", q)
		}
	}
	if len(pd.Quests) != 1 {
		t.Fatalf("player quests: %+v", pd.Quests)
	}

	// The public half is what crossed: the block's face, not its interior.
	if pd.Place.Kind != "town" || pd.Place.Climate != "temperate" || len(pd.Place.Services) != 2 {
		t.Fatalf("the public half of the block crossed: %+v", pd.Place)
	}
	// Routes are DM payload material: the travel block never crossed.
	if len(pd.Routes) != 0 {
		t.Fatalf("the travel block must not cross a player scope: %+v", pd.Routes)
	}
	// And the witnessed event at the town is the party's own history.
	if len(pd.Events) != 1 || pd.Events[0].ID != fx.EventAmbush {
		t.Fatalf("player events: %+v", pd.Events)
	}
}
