package canon

// The monster designer's tests (MAD-382): the loop's two branches asserted
// with the model faked — a miss that the revision pass corrects, and a miss
// that stands, shown — plus the campaign material the brief plays against
// and the placement that stages without writing.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// monsterFill builds one valid model fill for the declared schema. The
// damage and to-hit are the dials the CR tests turn: 47 at +4 is the CR 7
// row exactly; 27 is the CR 4 row.
func monsterFill(legendary bool) map[string]any {
	fill := map[string]any{
		"name": "Vashk, the Grave Marshal", "size": "Medium", "type": "undead",
		"ac": 15, "hp": 168, "hit_dice": "21d8+74", "speed": "30 ft.",
		"str": 18, "dex": 14, "con": 18, "int": 12, "wis": 14, "cha": 16,
		"trait1_name":  "Marshal of the Dead",
		"trait1_desc":  "Undead soldiers within 30 feet of Vashk add proficiency to their attacks.",
		"action1_name": "Graveblade",
		"action1_desc": "Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 47 (6d8 + 20) slashing damage.",
		"tactics":      "Opens with the blade against the line's anchor. Calls the ranks to close on whoever healed last. Fights to the death — the death already happened.",
		"lore":         "A battlefield commander who kept commanding after the war killed her. The Dead Bell rings, and the ranks form.",
		"role":         "boss",
	}
	if legendary {
		fill["legend1_name"] = "Grave Strike"
		fill["legend1_desc"] = "Vashk makes one attack with a fallen soldier's blade."
		fill["legend1_cost"] = 1
		fill["legend2_name"] = "The Ranks Close"
		fill["legend2_desc"] = "Vashk moves up to half a corpse's speed without provoking opportunity attacks."
		fill["legend2_cost"] = 1
	}
	return fill
}

// offTarget rewrites the fill's damage dial down to the CR 4 row.
func offTarget(fill map[string]any) map[string]any {
	fill["action1_desc"] = "Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 27 (4d8 + 9) slashing damage."
	return fill
}

func monsterJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The corrected branch: the first draft misses the offensive half, the
// revision carries the calculator's wording back, and the model lands it.
func TestGenerateMonster_Corrected(t *testing.T) {
	s, campaignID, _ := batchStore(t)

	off := offTarget(monsterFill(true))
	on := monsterFill(true)

	model := &fakeModel{responses: []string{
		monsterJSON(t, off),
		monsterJSON(t, on),
	}}
	s.model = model

	res, err := s.GenerateMonster(context.Background(), MonsterDesignInput{
		CampaignID: campaignID, Brief: "A CR 7 undead boss that fights like a battlefield commander",
		CR: "7", Legendary: true, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("GenerateMonster: %v", err)
	}
	if !res.Revised {
		t.Fatal("the revision pass must report that it ran")
	}
	if len(res.Shortfall) != 0 {
		t.Fatalf("shortfall after revision = %v, want the maths to agree", res.Shortfall)
	}
	if res.Rating.Label != "7" || res.Rating.CR != 7 {
		t.Fatalf("computed = %s (%f), want 7", res.Rating.Label, res.Rating.CR)
	}
	// The revision prompt carried the specific shortfall, not a shrug.
	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want draft + revision", len(model.calls))
	}
	if !strings.Contains(model.calls[1], "damage per round is 18 short") {
		t.Fatalf("revision prompt = %q, want the calculator's specific wording", model.calls[1])
	}
	// The draft carries the deterministic parts: the server's proficiency
	// bonus, the derived legendary flag, the parsed action.
	if res.Statblock.ProfBonus != 3 {
		t.Fatalf("prof bonus = %d, want the envelope's +3 for CR 7", res.Statblock.ProfBonus)
	}
	if !res.Statblock.Legendary {
		t.Fatal("legendary actions present but the flag was not derived")
	}
	if len(res.Statblock.Actions) == 0 || !res.Statblock.Actions[0].Parsed {
		t.Fatalf("actions = %+v, want the parsed Graveblade", res.Statblock.Actions)
	}
	if res.Role != "boss" || res.Tactics == "" || res.Lore == "" {
		t.Fatalf("designer prose missing: %+v", res)
	}
}

// The reported branch: the revision still misses, and the draft is
// surfaced with its disagreement shown — never an error, never a silent
// retune.
func TestGenerateMonster_Reported(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	off := offTarget(monsterFill(false))
	model := &fakeModel{responses: []string{
		monsterJSON(t, off),
		monsterJSON(t, off),
	}}
	s.model = model

	res, err := s.GenerateMonster(context.Background(), MonsterDesignInput{
		CampaignID: campaignID, Brief: "A CR 7 undead boss", CR: "7", CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("a persistent miss must surface, not fail: %v", err)
	}
	if !res.Revised || len(res.Shortfall) == 0 {
		t.Fatalf("revised=%v shortfall=%v — the disagreement must be shown", res.Revised, res.Shortfall)
	}
	if res.Rating.Label == "7" {
		t.Fatal("the label must be the calculator's, not the ask's")
	}
	if res.Rating.Offensive != 4 {
		t.Fatalf("offensive = %f, want the CR 4 the damage prices", res.Rating.Offensive)
	}
}

// The envelope is the server's, before any model call: the first prompt
// carries the bands, and a campaign-scoped brief carries the campaign's
// own material to write against.
func TestGenerateMonster_EnvelopeAndCampaignMaterial(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	fill := monsterFill(true)
	model := &fakeModel{responses: []string{monsterJSON(t, fill)}}
	s.model = model

	if _, err := s.GenerateMonster(context.Background(), MonsterDesignInput{
		CampaignID: campaignID, Brief: "a soldier of the duke's faction", CR: "7", Legendary: true,
	}); err != nil {
		t.Fatalf("GenerateMonster: %v", err)
	}
	prompt := model.calls[0]
	for _, want := range []string{
		"161", "175", // the CR 7 effective-hit-point band
		"45", "50", // the CR 7 damage-per-round band
		"Duke Aldric", "Blackwater", // the campaign's own material, by name
		"three legendary-action points",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// The ask must read as a CR the tables carry; an offline store refuses.
func TestGenerateMonster_ValidatesTheAsk(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	if _, err := s.GenerateMonster(context.Background(), MonsterDesignInput{
		CampaignID: campaignID, Brief: "x", CR: "eleven",
	}); err == nil || !strings.Contains(err.Error(), "CR") {
		t.Fatalf("unreadable CR err = %v", err)
	}
	if _, err := s.GenerateMonster(context.Background(), MonsterDesignInput{
		CampaignID: campaignID, CR: "7",
	}); err == nil {
		t.Fatal("a brief is required")
	}
	offline, err := NewOffline(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := offline.GenerateMonster(context.Background(), MonsterDesignInput{
		Brief: "x", CR: "7",
	}); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("offline err = %v", err)
	}
}

// The placement stages one creature entity behind the review gate and
// writes nothing until the batch is decided.
func TestPlaceMonster_StagesAndWritesNothing(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()

	countEntities := func() int {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entities WHERE campaign_id = ?`, campaignID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countEntities()

	batch, err := s.PlaceMonster(ctx, MonsterPlaceInput{
		CampaignID: campaignID, HomebrewID: "hb-1", Name: "Vashk, the Grave Marshal",
		Summary: "A battlefield commander who kept commanding after dying.",
		CRLabel: "7", Lore: "The Dead Bell rings, and the ranks form.", CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("PlaceMonster: %v", err)
	}
	if batch.Status != BatchOpen || batch.ItemCount != 1 {
		t.Fatalf("batch = %s with %d items, want open with the one entity", batch.Status, batch.ItemCount)
	}
	if countEntities() != before {
		t.Fatal("staging a placement wrote to the graph")
	}

	// Deciding the batch lands the creature entity.
	if _, err := s.DecideBatch(ctx, campaignID, batch.ID, DecisionAccept, nil, "dm"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	after := countEntities()
	if after != before+1 {
		t.Fatalf("entities = %d, want %d", after, before+1)
	}
	var name, kind string
	if err := s.db.QueryRowContext(ctx,
		`SELECT name, kind FROM entities WHERE campaign_id = ? AND name LIKE ?`, campaignID, "Vashk%").Scan(&name, &kind); err != nil {
		t.Fatal(err)
	}
	if kind != campaign.KindCreature {
		t.Fatalf("placed kind = %s, want a creature", kind)
	}
}

// A planned encounter using the campaign's own homebrew stops being a
// stat_block_unresolved finding: LoadSnapshot folds the campaign's
// homebrew names into the bestiary the check resolves against.
func TestLoadSnapshotResolvesCampaignHomebrew(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()

	// The mirror must carry something for "unresolved" to mean anything.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO bestiary (key, name, cr, cr_num, xp, type, data, synced_at)
		 VALUES ('goblin', 'Goblin', '1/4', 0.25, 50, 'humanoid', '{}', 0)`); err != nil {
		t.Fatal(err)
	}

	insertEncounter := func(monsters string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO encounters (id, owner_id, name, party, monsters, campaign_id, status, created_at, updated_at)
			VALUES ('enc-hb', 'dm', 'The bell tower', '[7,7,7,7]', ?, ?, 'planned', 1, 1)`,
			monsters, campaignID); err != nil {
			t.Fatal(err)
		}
	}
	plantHomebrew := func(name string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO homebrew_monsters (id, owner_id, campaign_id, name, slug, statblock,
				requested_cr, computed_cr, computed_detail, tactics, lore, encounter_role, source, created_at, updated_at)
			VALUES ('hb-1', 'dm', ?, ?, 'vashkthegravemarshal', '{}', '7', '7', '{}', '', '', 'boss', 'designed', 1, 1)`,
			campaignID, name); err != nil {
			t.Fatal(err)
		}
	}

	// Without the homebrew row, the campaign's own monster does not resolve.
	insertEncounter(`[{"name":"Vashk, the Grave Marshal","cr":"7","xp":2900,"count":1}]`)
	snap, err := LoadSnapshot(ctx, s.db, campaignID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if n := has(CheckSnapshot(snap, DefaultCheckOptions()), CheckStatBlockUnresolved, "enc-hb"); n != 1 {
		t.Fatalf("before homebrew: %d findings, want the unresolved one", n)
	}

	// The same campaign designs the monster: the finding must vanish.
	plantHomebrew("Vashk, the Grave Marshal")
	snap, err = LoadSnapshot(ctx, s.db, campaignID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if n := has(CheckSnapshot(snap, DefaultCheckOptions()), CheckStatBlockUnresolved, "enc-hb"); n != 0 {
		t.Fatalf("after homebrew: %d findings, want none — the campaign's own design resolves its own encounter", n)
	}

	// Another campaign's homebrew must not leak across the scope.
	if _, err := s.db.ExecContext(ctx, `UPDATE homebrew_monsters SET campaign_id = 'camp-other'`); err != nil {
		t.Fatal(err)
	}
	snap, err = LoadSnapshot(ctx, s.db, campaignID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if n := has(CheckSnapshot(snap, DefaultCheckOptions()), CheckStatBlockUnresolved, "enc-hb"); n != 1 {
		t.Fatalf("foreign homebrew: %d findings, want the unresolved one back", n)
	}
}
