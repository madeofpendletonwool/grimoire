package canon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/dungeon"
)

// The dungeon designer's canon tests (MAD-373): the dressing pass's
// topology lock — a fake client that returns an extra room, a missing
// room or a new edge is rejected, nothing written — and the placing
// pass's batch: the full room hierarchy with contains edges, decided
// through the gate, marking the dungeon placed.

// dungeonFixture is a wired canon store plus one designed dungeon.
type dungeonFixture struct {
	s *Store
	d *campaign.Dungeon
}

func openDungeonStore(t *testing.T) *dungeonFixture {
	t.Helper()
	s, campaignID, _ := batchStore(t)
	d, err := s.campaigns.CreateDungeon(context.Background(), campaignID, campaign.DungeonInput{
		Name:  "The Sunken Reliquary",
		Theme: "a drowned temple beneath the marsh",
		Params: dungeon.Params{
			Size: dungeon.SizeDelve, Level: 4, ExpectedSessions: 1,
			CombatDensity: 1, PuzzleDensity: 1, ExploreDensity: 1, Branchiness: 1,
		},
		Seed: 777,
	})
	if err != nil {
		t.Fatalf("create dungeon: %v", err)
	}
	return &dungeonFixture{s: s, d: d}
}

// dressFill scripts a complete, valid dress fill for the fixture's
// dungeon. Mutators tweak the map before it is marshalled, which is how
// the topology-mismatch tests fake a disobedient model.
func (f *dungeonFixture) dressFill(mutate func(m map[string]any)) string {
	m := map[string]any{
		"boss_name":        "Vesh of the Drowned Bell",
		"boss_motive":      "Vesh wants the bell rung again, whatever wakes.",
		"secret_statement": "The temple sank on purpose, to drown what slept under it.",
		"key_item_name":    "the Pale Key",
	}
	for _, r := range f.d.Rooms {
		m["room_"+r.Key+"_name"] = fmt.Sprintf("The %s of %s", r.Purpose, r.Key)
		m["room_"+r.Key+"_detail"] = "Water on the steps, and something counting them."
	}
	if mutate != nil {
		mutate(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func roomDetail(d *campaign.Dungeon, key string) string {
	for _, r := range d.Rooms {
		if r.Key == key {
			return r.Detail
		}
	}
	return ""
}

func TestDressDungeon_FillsRoomsAndPersists(t *testing.T) {
	f := openDungeonStore(t)
	f.s.model = &fakeModel{responses: []string{f.dressFill(nil)}}

	res, err := f.s.DressDungeon(context.Background(), DungeonDressInput{CampaignID: f.d.CampaignID, DungeonID: f.d.ID})
	if err != nil {
		t.Fatalf("DressDungeon: %v", err)
	}
	d := res.Dungeon
	if d.Status != campaign.DungeonDressed {
		t.Fatalf("status = %q, want dressed", d.Status)
	}
	if d.KeyItem != "the Pale Key" || d.BossName != "Vesh of the Drowned Bell" || d.Secret == "" {
		t.Fatalf("dress extras did not persist: key=%q boss=%q secret=%q", d.KeyItem, d.BossName, d.Secret)
	}
	for _, r := range d.Rooms {
		if r.Name == "" || r.Detail == "" {
			t.Fatalf("room %s undressed: %+v", r.Key, r)
		}
	}
	// The boss room's detail carries the motive, and the topology never
	// moved: same rooms, same edges as the pure layout computes.
	bossKey := ""
	for _, r := range d.Rooms {
		if r.Purpose == dungeon.PurposeBoss {
			bossKey = r.Key
		}
	}
	if !strings.Contains(roomDetail(d, bossKey), "wants the bell rung") {
		t.Fatalf("boss motive did not fold into the boss room: %q", roomDetail(d, bossKey))
	}
	graph, err := dungeon.Layout(f.d.Params, f.d.Seed)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Rooms) != len(graph.Rooms) || len(d.Edges) != len(graph.Edges) {
		t.Fatalf("dressing changed the topology: %d/%d vs %d/%d",
			len(d.Rooms), len(d.Edges), len(graph.Rooms), len(graph.Edges))
	}
}

// TestDressDungeon_RejectsTopologyChanges is the acceptance: a fake
// client returning an extra room, a missing room or a new edge is
// rejected, and the test asserts the rejection rather than a repair.
func TestDressDungeon_RejectsTopologyChanges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m map[string]any)
		want   string
	}{
		{
			name: "an extra room",
			mutate: func(m map[string]any) {
				m["room_r99_name"] = "The Room That Was Not There"
				m["room_r99_detail"] = "It was not there."
			},
			want: "room_r99_name",
		},
		{
			name: "a missing room",
			mutate: func(m map[string]any) {
				delete(m, "room_r1_name")
				delete(m, "room_r1_detail")
			},
			want: "room_r1_name",
		},
		{
			name: "a new edge",
			mutate: func(m map[string]any) {
				m["edge_r1_r9"] = "passage"
			},
			want: "edge_r1_r9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := openDungeonStore(t)
			// The fake returns the same disobedient response twice: the
			// harness's one repair retry gets the same answer, and the
			// whole exchange fails.
			fill := f.dressFill(tc.mutate)
			f.s.model = &fakeModel{responses: []string{fill, fill}}

			_, err := f.s.DressDungeon(context.Background(), DungeonDressInput{CampaignID: f.d.CampaignID, DungeonID: f.d.ID})
			if err == nil {
				t.Fatalf("a fill with %s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rejection = %q, want it to name %s", err.Error(), tc.want)
			}
			// Nothing was written: the dungeon is still a draft, its
			// rooms still unnamed — the rejection was not a repair.
			d, getErr := f.s.campaigns.GetDungeon(context.Background(), campaign.ScopeDM, f.d.CampaignID, f.d.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if d.Status != campaign.DungeonDraft {
				t.Fatalf("status = %q after a rejected dress; a rejection is not a repair", d.Status)
			}
			for _, r := range d.Rooms {
				if r.Name != "" {
					t.Fatalf("room %s was named by a rejected dress", r.Key)
				}
			}
		})
	}
}

// TestPlaceDungeon_StagesTheHierarchyAndPlacesOnDecision covers the
// acceptance: placing produces the full room hierarchy with correct
// contains edges, and a decided batch leaves no error-severity canon
// findings.
func TestPlaceDungeon_StagesTheHierarchyAndPlacesOnDecision(t *testing.T) {
	f := openDungeonStore(t)
	f.s.model = &fakeModel{responses: []string{f.dressFill(nil)}}
	if _, err := f.s.DressDungeon(context.Background(), DungeonDressInput{CampaignID: f.d.CampaignID, DungeonID: f.d.ID}); err != nil {
		t.Fatalf("dress: %v", err)
	}

	res, err := f.s.PlaceDungeon(context.Background(), DungeonPlaceInput{CampaignID: f.d.CampaignID, DungeonID: f.d.ID, CreatedBy: "keeper"})
	if err != nil {
		t.Fatalf("PlaceDungeon: %v", err)
	}
	batch := res.Batch
	if batch.Source != BatchSourceDungeon {
		t.Fatalf("source = %q", batch.Source)
	}
	// Every room has an entity item and a contains edge item off the
	// root, in dependency order. The batch-local ids live in the item
	// payloads; the review ids are minted at staging.
	rooms, contains := 0, 0
	for _, it := range batch.Items {
		var p map[string]any
		if err := json.Unmarshal([]byte(it.Detail), &p); err != nil {
			t.Fatalf("item payload: %v", err)
		}
		id, _ := p["local_id"].(string)
		if strings.HasPrefix(id, "room-") {
			rooms++
		}
		if strings.HasPrefix(id, "contains-") {
			contains++
			if p["rel_type"] != "contains" || p["from_entity"] != "dungeon-root" {
				t.Fatalf("contains edge payload: %+v", p)
			}
		}
	}
	if rooms != len(f.d.Rooms) || contains != len(f.d.Rooms) {
		t.Fatalf("rooms staged %d, contains %d, dungeon has %d", rooms, contains, len(f.d.Rooms))
	}

	// Nothing is written before the decision.
	d, err := f.s.campaigns.GetDungeon(context.Background(), campaign.ScopeDM, f.d.CampaignID, f.d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != campaign.DungeonDressed || d.LocationEntity != "" {
		t.Fatalf("staging wrote to the dungeon: %+v", d)
	}

	// Decide: the hierarchy lands, the dungeon is marked placed.
	decided, err := f.s.DecideBatch(context.Background(), f.d.CampaignID, batch.ID, DecisionAccept, nil, "keeper")
	if err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	if decided.Batch.Status != BatchAccepted {
		t.Fatalf("batch status = %q: %s", decided.Batch.Status, summarizeOutcomes(decided))
	}
	d, err = f.s.campaigns.GetDungeon(context.Background(), campaign.ScopeDM, f.d.CampaignID, f.d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != campaign.DungeonPlaced || d.LocationEntity == "" {
		t.Fatalf("the decided batch did not mark the dungeon placed: %+v", d)
	}
	for _, r := range d.Rooms {
		if r.EntityID == "" {
			t.Fatalf("room %s has no entity", r.Key)
		}
	}

	// The graph carries the hierarchy: the root contains every room.
	snap, err := LoadSnapshot(context.Background(), f.s.db, f.d.CampaignID)
	if err != nil {
		t.Fatal(err)
	}
	rootChildren := 0
	for _, rel := range snap.Relationships {
		if rel.FromEntity == d.LocationEntity && rel.RelType == "contains" {
			rootChildren++
		}
	}
	if rootChildren != len(d.Rooms) {
		t.Fatalf("root carries %d contains edges, want %d", rootChildren, len(d.Rooms))
	}

	// Canon check is clean of error-severity findings.
	for _, finding := range campaign.Check(snap.Snapshot) {
		if finding.Severity == campaign.SeverityError {
			t.Fatalf("error-severity finding after placement: %+v", finding)
		}
	}
}

// TestPlaceDungeon_PartialAcceptanceMarksNothing: a DM who dismisses
// part of the placement keeps the entities but the dungeon is not
// marked placed — it stays placeable again.
func TestPlaceDungeon_PartialAcceptanceMarksNothing(t *testing.T) {
	f := openDungeonStore(t)
	f.s.model = &fakeModel{responses: []string{f.dressFill(nil)}}
	if _, err := f.s.DressDungeon(context.Background(), DungeonDressInput{CampaignID: f.d.CampaignID, DungeonID: f.d.ID}); err != nil {
		t.Fatalf("dress: %v", err)
	}
	res, err := f.s.PlaceDungeon(context.Background(), DungeonPlaceInput{CampaignID: f.d.CampaignID, DungeonID: f.d.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Dismiss one room's entity: the contains edge and everything
	// depending on it is refused with it. The item's batch-local id
	// ("room-rN") lives in its payload; the decision names the review id.
	var target string
	for _, it := range res.Batch.Items {
		var p map[string]any
		if json.Unmarshal([]byte(it.Detail), &p) != nil {
			continue
		}
		if id, _ := p["local_id"].(string); strings.HasPrefix(id, "room-") {
			target = it.ID
			break
		}
	}
	if target == "" {
		t.Fatal("no room item found to dismiss")
	}
	decided, err := f.s.DecideBatch(context.Background(), f.d.CampaignID, res.Batch.ID, DecisionAccept,
		[]ItemDecision{{ItemID: target, Decision: DecisionDismiss}}, "keeper")
	if err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	if decided.Batch.Status == BatchAccepted {
		t.Fatal("a batch with a dismissed item was fully accepted")
	}
	d, err := f.s.campaigns.GetDungeon(context.Background(), campaign.ScopeDM, f.d.CampaignID, f.d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status == campaign.DungeonPlaced {
		t.Fatal("a partial placement marked the dungeon placed")
	}
}

func summarizeOutcomes(d *BatchDecision) string {
	var parts []string
	for _, it := range d.Items {
		if it.Status != ReviewAccepted && it.Status != ReviewModified {
			parts = append(parts, fmt.Sprintf("%s: %s (%s)", it.Kind, it.Status, it.Reason))
		}
	}
	if len(parts) == 0 {
		return "no failures recorded"
	}
	return strings.Join(parts, "; ")
}
