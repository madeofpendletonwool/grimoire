package ledger

// The ledger's row-level tests (MAD-419): sync, apply, the live rest, and
// the review-gated rest, over a private migrated database. The pure grammar
// has its own tests; these pin the store's half of the contract — pools
// survive re-sync by id, transactions survive pool deletion by key, the
// clock moves exactly once per decided rest, and a dismissed proposal
// writes nothing.

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// harness is one private database with the full stack wired the way
// runServe wires it: campaigns, knowledge, an offline canon engine, and the
// ledger store registered as the rest finalizer.
type harness struct {
	db        *sql.DB
	campaigns *campaign.Store
	canon     *canon.Store
	ledgers   *Store
	campaign  string
	wizard    string // a pc with a wizard sheet
	warlock   string // a pc with a warlock sheet
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
	ledgers, err := New(db, campaigns, engine)
	if err != nil {
		t.Fatalf("ledger store: %v", err)
	}
	engine = engine.WithRestFinalizer(ledgers)

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
	wizardSheet := sheet.Sheet{
		Classes:      []sheet.ClassLevel{{Class: "Wizard", Level: 5}},
		Spellcasting: &sheet.Spellcasting{Slots: map[string]int{"1": 4, "2": 3, "3": 2}},
		MaxHP:        32, Currency: sheet.Currency{GP: 130},
	}
	warlockSheet := sheet.Sheet{
		Classes:      []sheet.ClassLevel{{Class: "Warlock", Level: 5}},
		Spellcasting: &sheet.Spellcasting{Slots: map[string]int{"3": 2}},
		MaxHP:        33,
	}
	return &harness{
		db: db, campaigns: campaigns, canon: engine, ledgers: ledgers, campaign: c.ID,
		wizard: mkPC("Velren", wizardSheet), warlock: mkPC("Nyx", warlockSheet),
	}
}

/* ---------- sync ---------- */

func TestSyncSeedsPoolsPerSheet(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	pools, err := h.ledgers.Pools(ctx, h.campaign, h.wizard)
	if err != nil {
		t.Fatalf("pools: %v", err)
	}
	slot3 := poolByKey(t, pools, "slot:3")
	if slot3.Recovery != RecoveryLong || slot3.Size != 2 || slot3.Source != "sheet" {
		t.Fatalf("wizard slot:3 = %+v", slot3)
	}
	if p := poolByKey(t, pools, "currency:gp"); p.Size != 130 {
		t.Fatalf("gold = %+v", p)
	}

	warlockPools, err := h.ledgers.Pools(ctx, h.campaign, h.warlock)
	if err != nil {
		t.Fatalf("pools: %v", err)
	}
	if p := poolByKey(t, warlockPools, "slot:3"); p.Recovery != RecoveryShort {
		t.Fatalf("warlock slot:3 recovery = %q, want short (pact magic)", p.Recovery)
	}
}

func TestSyncUpdatesSizesAndKeepsIdsAndDMPools(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	before := poolByKey(t, mustPools(t, h, h.wizard), "slot:1")
	if _, err := h.ledgers.CreatePool(ctx, h.campaign, h.wizard, Pool{
		Kind: KindItem, Name: "arrows", Label: "arrows", Size: 20, Recovery: RecoveryManual,
	}); err != nil {
		t.Fatalf("register arrows: %v", err)
	}

	// The wizard levels: one more 1st-level slot.
	e, err := h.campaigns.GetEntity(ctx, campaign.ScopeDM, h.campaign, h.wizard)
	if err != nil {
		t.Fatalf("load wizard: %v", err)
	}
	s, _, _ := sheet.FromPayload(e.Payload)
	s.Spellcasting.Slots["1"] = 5
	if _, err := h.campaigns.UpdateEntity(ctx, h.campaign, h.wizard, nil, nil, nil, campaign.WithSheet(e.Payload, s)); err != nil {
		t.Fatalf("update sheet: %v", err)
	}
	if err := h.ledgers.SyncEntity(ctx, h.campaign, h.wizard); err != nil {
		t.Fatalf("sync: %v", err)
	}

	pools := mustPools(t, h, h.wizard)
	after := poolByKey(t, pools, "slot:1")
	if after.Size != 5 {
		t.Fatalf("slot:1 size = %d, want 5", after.Size)
	}
	if after.ID != before.ID {
		t.Fatalf("slot:1 changed row id on re-sync (%s -> %s); transactions would orphan", before.ID, after.ID)
	}
	if p := poolByKey(t, pools, "item:arrows"); p.Size != 20 {
		t.Fatalf("sync disturbed a DM-registered pool: %+v", p)
	}
}

func TestSyncDropsStalePoolsButHistorySurvives(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	pools := mustPools(t, h, h.wizard)
	gold := poolByKey(t, pools, "currency:gp")
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, gold.ID,
		TxnInput{Kind: TxnSpend, Amount: 30, Note: "inn keep"}, "dm"); err != nil {
		t.Fatalf("spend gold: %v", err)
	}

	// The purse is removed from the sheet: the pool must go, the log stay.
	e, err := h.campaigns.GetEntity(ctx, campaign.ScopeDM, h.campaign, h.wizard)
	if err != nil {
		t.Fatalf("load wizard: %v", err)
	}
	s, _, _ := sheet.FromPayload(e.Payload)
	s.Currency = sheet.Currency{}
	if _, err := h.campaigns.UpdateEntity(ctx, h.campaign, h.wizard, nil, nil, nil, campaign.WithSheet(e.Payload, s)); err != nil {
		t.Fatalf("update sheet: %v", err)
	}
	if err := h.ledgers.SyncEntity(ctx, h.campaign, h.wizard); err != nil {
		t.Fatalf("sync: %v", err)
	}
	for _, p := range mustPools(t, h, h.wizard) {
		if p.Key() == "currency:gp" {
			t.Fatalf("stale pool survived the sync")
		}
	}
	history, err := h.ledgers.History(ctx, h.campaign, h.wizard, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 || history[0].Pool != "currency:gp" || history[0].Amount != 30 {
		t.Fatalf("history did not survive the pool deletion: %+v", history)
	}
}

/* ---------- transactions ---------- */

func TestApplySpendRegainSet(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pools := mustPools(t, h, h.wizard)
	slot3 := poolByKey(t, pools, "slot:3").ID

	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, slot3, TxnInput{Kind: TxnSpend, Amount: 2}, "player"); err != nil {
		t.Fatalf("spend: %v", err)
	}
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, slot3, TxnInput{Kind: TxnSpend, Amount: 1}, "player"); err == nil {
		t.Fatalf("overspend accepted (2 of 2 spent)")
	}
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, slot3, TxnInput{Kind: TxnRegain, Amount: 1}, "player"); err != nil {
		t.Fatalf("regain: %v", err)
	}
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, slot3, TxnInput{Kind: TxnSet, Amount: 2}, "dm"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, slot3, TxnInput{Kind: TxnSet, Amount: 3}, "dm"); err == nil {
		t.Fatalf("set above size accepted on a bounded pool")
	}
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, slot3, TxnInput{Kind: "reset"}, "dm"); err == nil {
		t.Fatalf("hand-written reset accepted (resets are a rest's to write)")
	}
	if got := currentOf(t, h, h.wizard, "slot:3"); got != 2 {
		t.Fatalf("slot:3 = %d, want 2", got)
	}
}

// balanceOf finds one pool's derived current value.
func currentOf(t *testing.T, h *harness, entity, key string) int {
	t.Helper()
	balances, err := h.ledgers.Balances(context.Background(), h.campaign, entity)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	for _, b := range balances {
		if b.Pool.Key() == key {
			return b.Current
		}
	}
	t.Fatalf("no balance %s for %s", key, entity)
	return 0
}

func mustPools(t *testing.T, h *harness, entity string) []Pool {
	t.Helper()
	pools, err := h.ledgers.Pools(context.Background(), h.campaign, entity)
	if err != nil {
		t.Fatalf("pools: %v", err)
	}
	return pools
}

/* ---------- the live rest ---------- */

func TestLiveLongRestAdvancesClockOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	slot3 := poolByKey(t, mustPools(t, h, h.wizard), "slot:3").ID
	hd := poolByKey(t, mustPools(t, h, h.wizard), "hit_dice:hit_dice").ID
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, slot3, TxnInput{Kind: TxnSpend, Amount: 2}, "player"); err != nil {
		t.Fatalf("spend slots: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, hd, TxnInput{Kind: TxnSpend, Amount: 1}, "player"); err != nil {
			t.Fatalf("spend hit die: %v", err)
		}
	}

	c, err := h.campaigns.GetCampaign(ctx, h.campaign)
	if err != nil {
		t.Fatalf("campaign: %v", err)
	}
	from := c.Clock
	rest, plans, err := h.ledgers.Rest(ctx, h.campaign, []string{h.wizard}, RestLong, "", "camp", "dm")
	if err != nil {
		t.Fatalf("rest: %v", err)
	}
	if rest.Status != RestApplied || rest.AdvanceID == "" {
		t.Fatalf("rest row wrong: %+v", rest)
	}
	c, err = h.campaigns.GetCampaign(ctx, h.campaign)
	if err != nil {
		t.Fatalf("campaign: %v", err)
	}
	if c.Clock != from+1 {
		t.Fatalf("clock at %d, want %d (one long-rest day)", c.Clock, from+1)
	}
	var reason string
	if err := h.db.QueryRow(`SELECT reason FROM clock_advances WHERE id = ?`, rest.AdvanceID).Scan(&reason); err != nil {
		t.Fatalf("read advance: %v", err)
	}
	if reason != campaign.AdvanceRest {
		t.Fatalf("advance reason = %q, want rest", reason)
	}
	if currentOf(t, h, h.wizard, "slot:3") != 2 {
		t.Fatalf("slots not restored by the long rest")
	}
	if got := currentOf(t, h, h.wizard, "hit_dice:hit_dice"); got != 4 { // 5 - 3 spent + 2 half-regain
		t.Fatalf("hit dice = %d, want 4 (half-regain)", got)
	}
	if len(plans[h.wizard]) == 0 {
		t.Fatalf("no plan returned")
	}

	// A short rest does not touch the clock or the wizard's slots.
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, slot3, TxnInput{Kind: TxnSpend, Amount: 1}, "player"); err != nil {
		t.Fatalf("spend slot: %v", err)
	}
	before := c.Clock
	if _, _, err := h.ledgers.Rest(ctx, h.campaign, []string{h.wizard}, RestShort, "", "", "dm"); err != nil {
		t.Fatalf("short rest: %v", err)
	}
	c, _ = h.campaigns.GetCampaign(ctx, h.campaign)
	if c.Clock != before {
		t.Fatalf("short rest moved the clock")
	}
	if got := currentOf(t, h, h.wizard, "slot:3"); got != 1 {
		t.Fatalf("short rest restored regular slots — the false positive: %d", got)
	}
}

/* ---------- the proposed rest ---------- */

func TestProposedRestAppliesOnAcceptOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	slot3 := poolByKey(t, mustPools(t, h, h.warlock), "slot:3").ID
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.warlock, slot3, TxnInput{Kind: TxnSpend, Amount: 2}, "player"); err != nil {
		t.Fatalf("spend pact slots: %v", err)
	}
	c, _ := h.campaigns.GetCampaign(ctx, h.campaign)
	from := c.Clock

	rest, batch, plans, err := h.ledgers.StageRest(ctx, h.campaign, []string{h.warlock}, RestLong, "", "transcribed: we make camp", "model")
	if err != nil {
		t.Fatalf("stage rest: %v", err)
	}
	if rest.Status != RestStaged || batch.Status != canon.BatchOpen {
		t.Fatalf("staged rest wrong: %+v / %+v", rest, batch.Status)
	}
	if len(plans[h.warlock]) == 0 {
		t.Fatalf("the staged plan is empty though slots are spent")
	}
	// Nothing has moved yet: the gate is the gate.
	if got := currentOf(t, h, h.warlock, "slot:3"); got != 0 {
		t.Fatalf("staging applied the rest: %d", got)
	}
	c, _ = h.campaigns.GetCampaign(ctx, h.campaign)
	if c.Clock != from {
		t.Fatalf("staging moved the clock")
	}

	if _, err := h.canon.DecideBatch(ctx, h.campaign, batch.ID, "accept", nil, "dm"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got := currentOf(t, h, h.warlock, "slot:3"); got != 2 {
		t.Fatalf("accepted rest did not restore pact slots: %d", got)
	}
	c, _ = h.campaigns.GetCampaign(ctx, h.campaign)
	if c.Clock != from+1 {
		t.Fatalf("accepted long rest did not advance the clock: %d -> %d", from, c.Clock)
	}
	var restAdvances int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ? AND reason = 'rest'`, h.campaign).Scan(&restAdvances); err != nil {
		t.Fatalf("count advances: %v", err)
	}
	if restAdvances != 1 {
		t.Fatalf("rest advances = %d, want exactly 1", restAdvances)
	}
	var status string
	if err := h.db.QueryRow(`SELECT status FROM rests WHERE id = ?`, rest.ID).Scan(&status); err != nil {
		t.Fatalf("read rest: %v", err)
	}
	if status != RestApplied {
		t.Fatalf("rest status = %q, want applied", status)
	}

	// Deciding the batch again (a re-read) changes nothing — idempotent.
	if _, err := h.canon.DecideBatch(ctx, h.campaign, batch.ID, "accept", nil, "dm"); err != nil {
		t.Fatalf("re-decide: %v", err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ? AND reason = 'rest'`, h.campaign).Scan(&restAdvances); err != nil {
		t.Fatalf("count advances: %v", err)
	}
	if restAdvances != 1 {
		t.Fatalf("a re-decided batch advanced the clock again: %d", restAdvances)
	}
}

func TestProposedRestDismissedWritesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	slot3 := poolByKey(t, mustPools(t, h, h.warlock), "slot:3").ID
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.warlock, slot3, TxnInput{Kind: TxnSpend, Amount: 2}, "player"); err != nil {
		t.Fatalf("spend: %v", err)
	}
	c, _ := h.campaigns.GetCampaign(ctx, h.campaign)
	from := c.Clock

	rest, batch, _, err := h.ledgers.StageRest(ctx, h.campaign, []string{h.warlock}, RestLong, "", "", "model")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := h.canon.DecideBatch(ctx, h.campaign, batch.ID, "dismiss", nil, "dm"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if got := currentOf(t, h, h.warlock, "slot:3"); got != 0 {
		t.Fatalf("dismissed rest restored slots: %d", got)
	}
	c, _ = h.campaigns.GetCampaign(ctx, h.campaign)
	if c.Clock != from {
		t.Fatalf("dismissed rest moved the clock")
	}
	var status string
	if err := h.db.QueryRow(`SELECT status FROM rests WHERE id = ?`, rest.ID).Scan(&status); err != nil {
		t.Fatalf("read rest: %v", err)
	}
	if status != RestDiscarded {
		t.Fatalf("rest status = %q, want discarded", status)
	}
}

/* ---------- byte stability over the rows ---------- */

// TestBalancesByteStableOverRows pins the acceptance line at the store
// level: the same log, re-read and re-folded, renders the same bytes.
func TestBalancesByteStableOverRows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pools := mustPools(t, h, h.wizard)
	for _, key := range []string{"slot:1", "slot:2", "slot:3"} {
		id := poolByKey(t, pools, key).ID
		if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, id, TxnInput{Kind: TxnSpend, Amount: 1}, "player"); err != nil {
			t.Fatalf("spend %s: %v", key, err)
		}
	}
	first, err := json.Marshal(h.balances(t, h.wizard))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 3; i++ {
		again, err := json.Marshal(h.balances(t, h.wizard))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(first) != string(again) {
			t.Fatalf("row-derived balances not byte-stable on read %d", i)
		}
	}
}

func (h *harness) balances(t *testing.T, entity string) []Balance {
	t.Helper()
	balances, err := h.ledgers.Balances(context.Background(), h.campaign, entity)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	return balances
}
