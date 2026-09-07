package leveling

// The leveling store's tests (MAD-424): the XP award off the encounter
// builder's oracle, the staged level-up's gate lifecycle, and the
// reconciliation pass — over a private migrated database with the full
// stack wired the way runServe wires it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// harness is one private database with the full stack wired: campaigns,
// knowledge, an offline canon engine, the ledger store, and the leveling
// store registered as the level-up and reconcile finalizer.
type harness struct {
	db        *sql.DB
	campaigns *campaign.Store
	canon     *canon.Store
	ledgers   *ledger.Store
	leveling  *Store
	campaign  string
	wizard    string // a pc carrying a 4th-level wizard sheet, 6500 xp
	fighter   string // a pc carrying an 8th-level fighter sheet
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testdb.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('keeper', 'keeper', 'x', 0, 0)`); err != nil {
		t.Fatalf("insert owner: %v", err)
	}

	campaigns, err := campaign.New(db)
	if err != nil {
		t.Fatalf("campaign store: %v", err)
	}
	knowledgeStore, err := knowledge.New(db)
	if err != nil {
		t.Fatalf("knowledge store: %v", err)
	}
	engine, err := canon.NewOffline(db)
	if err != nil {
		t.Fatalf("canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)
	ledgers, err := ledger.New(db, campaigns, engine)
	if err != nil {
		t.Fatalf("ledger store: %v", err)
	}
	engine = engine.WithRestFinalizer(ledgers)
	levelings, err := New(db, campaigns, engine, ledgers)
	if err != nil {
		t.Fatalf("leveling store: %v", err)
	}
	engine = engine.WithLevelUpFinalizer(levelings).WithReconcileFinalizer(levelings)

	ctx := context.Background()
	c, err := campaigns.CreateCampaign(ctx, "keeper", "The Ashen Court", "D&D 5e", "")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	mkPC := func(name string, s sheet.Sheet) string {
		e, err := campaigns.CreateEntity(ctx, c.ID, campaign.KindPC, name, "", campaign.WithSheet(nil, s))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := ledgers.SyncEntity(ctx, c.ID, e.ID); err != nil {
			t.Fatalf("sync %s: %v", name, err)
		}
		return e.ID
	}
	return &harness{
		db: db, campaigns: campaigns, canon: engine, ledgers: ledgers, leveling: levelings,
		campaign: c.ID,
		wizard: mkPC("Thalia", sheet.Sheet{
			XP:        6500,
			Classes:   []sheet.ClassLevel{{Class: "wizard", Level: 4}},
			Abilities: sheet.Abilities{INT: 17, CON: 16, DEX: 12},
			MaxHP:     32,
			Spellcasting: &sheet.Spellcasting{
				Ability: "int", DC: 13, AttackBonus: 5,
				Slots: map[string]int{"1": 4, "2": 3},
			},
		}),
		fighter: mkPC("Bram", sheet.Sheet{
			XP:        40000,
			Classes:   []sheet.ClassLevel{{Class: "fighter", Level: 8}},
			Abilities: sheet.Abilities{STR: 18, CON: 14, INT: 15},
			MaxHP:     79,
		}),
	}
}

// mkEncounter inserts a campaign-scoped encounter with the monsters the
// test prices. XP values are deliberately wrong in the row: the store
// re-derives them from the CR, and the test pins that it does.
func (h *harness) mkEncounter(t *testing.T, name string, monsters [][3]any) string {
	t.Helper()
	type monster struct {
		Name  string `json:"name"`
		CR    string `json:"cr"`
		XP    int    `json:"xp"`
		Count int    `json:"count"`
	}
	var list []monster
	for _, m := range monsters {
		list = append(list, monster{Name: m[0].(string), CR: m[1].(string), XP: -1, Count: m[2].(int)})
	}
	blob, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	id := "enc-" + name
	if _, err := h.db.Exec(`
		INSERT INTO encounters (id, owner_id, name, party, monsters, campaign_id, status, created_at, updated_at)
		VALUES (?, 'keeper', ?, '[4]', ?, ?, 'planned', 1, 1)`,
		id, name, string(blob), h.campaign); err != nil {
		t.Fatalf("insert encounter: %v", err)
	}
	return id
}

func (h *harness) sheetOf(t *testing.T, eid string) sheet.Sheet {
	t.Helper()
	e, err := h.campaigns.GetEntity(context.Background(), campaign.ScopeDM, h.campaign, eid)
	if err != nil {
		t.Fatalf("load %s: %v", eid, err)
	}
	s, has, err := sheet.FromPayload(e.Payload)
	if err != nil || !has {
		t.Fatalf("%s's sheet: %v %v", eid, has, err)
	}
	return s
}

/* ---------- the award ---------- */

func TestAwardPricesFromTheOracle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Four goblins (CR 1/4, 50 XP each): raw total 200, split two ways.
	eid := h.mkEncounter(t, "ambush", [][3]any{{"goblin", "1/4", 4}})
	result, err := h.leveling.Award(ctx, h.campaign, eid, []string{h.wizard, h.fighter}, "", "the ambush", "keeper")
	if err != nil {
		t.Fatalf("award: %v", err)
	}
	if result.TotalXP != 200 {
		t.Errorf("total = %d, want 200 (4 x 50, the oracle's raw value)", result.TotalXP)
	}
	if result.Shares[h.wizard] != 100 || result.Shares[h.fighter] != 100 {
		t.Errorf("shares = %v, want 100/100", result.Shares)
	}
	// The sheets carry their new totals.
	if xp := h.sheetOf(t, h.wizard).XP; xp != 6600 {
		t.Errorf("wizard xp = %d, want 6600", xp)
	}
	if xp := h.sheetOf(t, h.fighter).XP; xp != 40100 {
		t.Errorf("fighter xp = %d, want 40100", xp)
	}
	// The award log is the provenance.
	awards, err := h.leveling.Awards(ctx, h.campaign)
	if err != nil {
		t.Fatal(err)
	}
	if len(awards) != 2 {
		t.Fatalf("award log has %d rows, want 2", len(awards))
	}
	// The encounter is run: the award is the writer that status waited for.
	var status string
	if err := h.db.QueryRow(`SELECT status FROM encounters WHERE id = ?`, eid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "run" {
		t.Errorf("encounter status = %q, want run", status)
	}

	// Double-awarding the same encounter refuses.
	if _, err := h.leveling.Award(ctx, h.campaign, eid, nil, "", "", "keeper"); err == nil {
		t.Error("awarding the same encounter twice should refuse")
	}
}

func TestAwardAgainstTheDMGExamples(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// The DMG's own bugbear example: one bugbear (CR 1, 200 XP). The
	// builder's budget priced it against a party; the award prices the
	// monster itself.
	eid := h.mkEncounter(t, "bugbear", [][3]any{{"bugbear", "1", 1}})
	result, err := h.leveling.Award(ctx, h.campaign, eid, []string{h.wizard}, "", "", "keeper")
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalXP != 200 || result.Shares[h.wizard] != 200 {
		t.Errorf("bugbear award = %d (%v), want 200", result.TotalXP, result.Shares)
	}
}

func TestAwardRefusesMilestoneCampaigns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.campaigns.UpdateCampaign(ctx, "keeper", h.campaign,
		nil, nil, nil, nil, map[string]any{SettingsKey: map[string]any{"mode": ModeMilestone}}); err != nil {
		t.Fatal(err)
	}
	eid := h.mkEncounter(t, "skirmish", [][3]any{{"goblin", "1/4", 1}})
	if _, err := h.leveling.Award(ctx, h.campaign, eid, nil, "", "", "keeper"); err == nil {
		t.Fatal("a milestone campaign has no XP to award")
	}
}

/* ---------- the gated level-up ---------- */

func TestLevelUpGateIsTheGate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	row, batch, err := h.leveling.StageLevelUp(ctx, h.campaign, h.wizard, "wizard", "", "fifth circle", "keeper")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if row.Status != LevelUpStaged || batch.Status != canon.BatchOpen {
		t.Fatalf("staged row %s, batch %s", row.Status, batch.Status)
	}
	if row.Diff.ToClass != 5 || row.Diff.SlotsNote != "" || len(row.Diff.Slots) != 1 || row.Diff.Slots[0].Level != 3 {
		t.Fatalf("diff = %+v", row.Diff)
	}

	// Nothing moved: the sheet is level 4, the review item is open.
	if lvl := h.sheetOf(t, h.wizard).TotalLevel(); lvl != 4 {
		t.Fatalf("the gate is the gate: sheet is level %d before any decision", lvl)
	}
	full, err := h.canon.GetBatch(ctx, h.campaign, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Items) != 1 || full.Items[0].Status != canon.ReviewOpen {
		t.Fatalf("batch items = %+v", full.Items)
	}

	// Accept through the batch surface — the same endpoint every
	// generator batch decides at.
	if _, err := h.canon.DecideBatch(ctx, h.campaign, batch.ID, canon.DecisionAccept, nil, "keeper"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	s := h.sheetOf(t, h.wizard)
	if s.TotalLevel() != 5 {
		t.Fatalf("level = %d, want 5 after acceptance", s.TotalLevel())
	}
	if s.MaxHP != 32+7 { // d6 average 4 + CON 3
		t.Errorf("max hp = %d, want 39", s.MaxHP)
	}
	if s.Spellcasting == nil || s.Spellcasting.Slots["3"] != 2 {
		t.Errorf("3rd-level slots = %+v, want 2", s.Spellcasting)
	}
	// The pools re-synced with the new maxima.
	balances, err := h.ledgers.Balances(ctx, h.campaign, h.wizard)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, b := range balances {
		found[b.Pool.Key()] = b.Pool.Size
	}
	if found["slot:3"] != 2 || found["hp:hp"] != 39 || found["hit_dice:hit_dice"] != 5 {
		t.Errorf("pool sizes after level-up = %v", found)
	}

	// The row is applied; deciding the decided batch again changes
	// nothing (idempotent by the never-resurrect rule and the finalizer's
	// recompute guard).
	if _, err := h.canon.DecideBatch(ctx, h.campaign, batch.ID, canon.DecisionAccept, nil, "keeper"); err != nil {
		t.Fatalf("re-decide: %v", err)
	}
	if s2 := h.sheetOf(t, h.wizard); s2.TotalLevel() != 5 || s2.MaxHP != 39 {
		t.Errorf("re-deciding moved something: level %d hp %d", s2.TotalLevel(), s2.MaxHP)
	}
	rows, err := h.leveling.LevelUps(ctx, h.campaign)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != LevelUpApplied {
		t.Fatalf("level-up rows = %+v", rows)
	}
}

func TestLevelUpDismissedWritesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	_, batch, err := h.leveling.StageLevelUp(ctx, h.campaign, h.wizard, "wizard", "", "", "keeper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.canon.DecideBatch(ctx, h.campaign, batch.ID, canon.DecisionDismiss, nil, "keeper"); err != nil {
		t.Fatal(err)
	}
	if lvl := h.sheetOf(t, h.wizard).TotalLevel(); lvl != 4 {
		t.Fatalf("dismissed level-up wrote anyway: level %d", lvl)
	}
	rows, err := h.leveling.LevelUps(ctx, h.campaign)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != LevelUpDiscarded {
		t.Fatalf("rows = %+v, want one discarded", rows)
	}
}

func TestLevelUpMilestoneNeedsNoXP(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.campaigns.UpdateCampaign(ctx, "keeper", h.campaign,
		nil, nil, nil, nil, map[string]any{SettingsKey: map[string]any{"mode": ModeMilestone}}); err != nil {
		t.Fatal(err)
	}
	// The fighter at 0 xp levels by decree.
	if err := setXP(h, h.fighter, 0); err != nil {
		t.Fatal(err)
	}
	row, _, err := h.leveling.StageLevelUp(ctx, h.campaign, h.fighter, "fighter", "", "", "keeper")
	if err != nil {
		t.Fatalf("milestone staging should not ask for xp: %v", err)
	}
	if row.Diff.Mode != ModeMilestone {
		t.Errorf("diff mode = %q", row.Diff.Mode)
	}
}

func TestLevelUpXPModeEnforcesEligibility(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := setXP(h, h.wizard, 6499); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.leveling.StageLevelUp(ctx, h.campaign, h.wizard, "wizard", "", "", "keeper"); err == nil {
		t.Fatal("one xp short of 5th should refuse staging in xp mode")
	}
}

func TestLevelUpMulticlassAddsNewClass(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Bram (fighter 8) takes a first wizard level: a new class entry, the
	// saves, the level-1 features, seeded slots — the golden-filed path.
	if err := setXP(h, h.fighter, 48000); err != nil { // level 9's threshold
		t.Fatal(err)
	}
	row, batch, err := h.leveling.StageLevelUp(ctx, h.campaign, h.fighter, "wizard", "", "", "keeper")
	if err != nil {
		t.Fatalf("stage multiclass: %v", err)
	}
	if !row.Diff.NewClass || row.Diff.ToTotal != 9 {
		t.Fatalf("diff = %+v", row.Diff)
	}
	if _, err := h.canon.DecideBatch(ctx, h.campaign, batch.ID, canon.DecisionAccept, nil, "keeper"); err != nil {
		t.Fatal(err)
	}
	s := h.sheetOf(t, h.fighter)
	if len(s.Classes) != 2 || s.Classes[1].Class != "wizard" || s.Classes[1].Level != 1 {
		t.Fatalf("classes = %+v", s.Classes)
	}
	if s.TotalLevel() != 9 || s.MaxHP != 79+6 { // d6 average 4 + CON 2
		t.Errorf("level %d hp %d", s.TotalLevel(), s.MaxHP)
	}
	// The saves landed, once each.
	saves := map[string]bool{}
	for _, sv := range s.Proficiencies.Saves {
		saves[sv] = true
	}
	if !saves["int"] || !saves["wis"] {
		t.Errorf("saves = %v", saves)
	}
}

// setXP rewrites a pc's sheet XP directly — the test's hand of the DM's
// sheet PUT.
func setXP(h *harness, eid string, xp int) error {
	ctx := context.Background()
	e, err := h.campaigns.GetEntity(ctx, campaign.ScopeDM, h.campaign, eid)
	if err != nil {
		return err
	}
	s, has, err := sheet.FromPayload(e.Payload)
	if err != nil || !has {
		return fmt.Errorf("sheet of %s", eid)
	}
	s.XP = xp
	_, err = h.campaigns.UpdateEntity(ctx, h.campaign, eid, nil, nil, nil, campaign.WithSheet(e.Payload, s))
	return err
}
