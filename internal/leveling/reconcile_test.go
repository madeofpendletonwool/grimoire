package leveling

// The reconciliation pass's tests (MAD-424): the e2e fixture the issue's
// acceptance names — seeded discrepancies caught, corrections proposed
// through the review gate, and nothing auto-applied. The pass reads the
// fold and the award log; the seeds are direct row inserts, the drift a
// month of hand-edited sheets would produce.

import (
	"context"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
)

// seedOverspend inserts a spend no validated path could have written —
// the hp pool driven below zero — the classic "HP math that does not
// close".
func (h *harness) seedOverspend(t *testing.T, eid string) {
	t.Helper()
	var poolID string
	var size int
	if err := h.db.QueryRow(
		`SELECT id, size FROM resource_pools WHERE campaign_id = ? AND entity_id = ? AND kind = 'hp'`,
		h.campaign, eid).Scan(&poolID, &size); err != nil {
		t.Fatalf("find hp pool: %v", err)
	}
	if _, err := h.db.Exec(`
		INSERT INTO resource_transactions (id, campaign_id, entity_id, pool_id, pool, kind, amount, actor, note, clock_day, created_at)
		VALUES ('seed-overspend', ?, ?, ?, 'hp:hp', 'spend', ?, 'gremlin', 'seeded discrepancy', 1, 1)`,
		h.campaign, eid, poolID, size+8); err != nil {
		t.Fatalf("seed overspend: %v", err)
	}
}

func TestReconcileCatchesAndNeverAutoApplies(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Seed both discrepancy kinds: an impossible overspend on the
	// wizard's hp pool, and an award log the sheet no longer carries.
	h.seedOverspend(t, h.wizard)
	eid := h.mkEncounter(t, "awarded", [][3]any{{"goblin", "1/4", 2}}) // 100 xp
	if _, err := h.leveling.Award(ctx, h.campaign, eid, []string{h.wizard}, "", "", "keeper"); err != nil {
		t.Fatal(err)
	}
	if err := setXP(h, h.wizard, 6100); err != nil { // below the awarded 6600
		t.Fatal(err)
	}

	result, err := h.leveling.Reconcile(ctx, h.campaign, "keeper", "post-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 2 {
		t.Fatalf("findings = %+v, want the overspend and the xp drift", result.Findings)
	}
	kinds := map[string]Finding{}
	for _, f := range result.Findings {
		kinds[f.Kind] = f
	}
	overspend, ok := kinds["pool_bounds"]
	if !ok || overspend.Entity != h.wizard || overspend.Pool != "hp:hp" || overspend.Have != -8 || overspend.Proposed != 0 {
		t.Fatalf("pool_bounds finding = %+v", overspend)
	}
	drift, ok := kinds["sheet_xp"]
	if !ok || drift.Have != 6100 || drift.Proposed != 6600 {
		t.Fatalf("sheet_xp finding = %+v", drift)
	}
	if result.Batch == nil || result.Batch.Status != canon.BatchOpen {
		t.Fatalf("batch = %+v, want an open proposal batch", result.Batch)
	}

	// NEVER auto-applies: the books still disagree until the decision.
	hp := h.balanceOf(t, h.wizard, "hp:hp")
	if hp != -8 {
		t.Fatalf("reconcile auto-applied a fix: hp = %d, want -8", hp)
	}
	if xp := h.sheetOf(t, h.wizard).XP; xp != 6100 {
		t.Fatalf("reconcile auto-applied a fix: xp = %d, want 6100", xp)
	}

	// Accept through the batch surface: the corrections land as the
	// visible, attributed writes they are.
	if _, err := h.canon.DecideBatch(ctx, h.campaign, result.Batch.ID, canon.DecisionAccept, nil, "keeper"); err != nil {
		t.Fatal(err)
	}
	if hp := h.balanceOf(t, h.wizard, "hp:hp"); hp != 0 {
		t.Fatalf("corrected hp = %d, want 0", hp)
	}
	if xp := h.sheetOf(t, h.wizard).XP; xp != 6600 {
		t.Fatalf("corrected xp = %d, want 6600", xp)
	}
	// The correction is a set transaction in the log — visible, proven.
	var kind string
	if err := h.db.QueryRow(`
		SELECT kind FROM resource_transactions
		 WHERE campaign_id = ? AND entity_id = ? AND note LIKE 'reconciliation%'`,
		h.campaign, h.wizard).Scan(&kind); err != nil {
		t.Fatalf("correction txn: %v", err)
	}
	if kind != "set" {
		t.Errorf("correction txn kind = %q, want set", kind)
	}
}

func TestReconcileDismissalFixesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.seedOverspend(t, h.fighter)
	result, err := h.leveling.Reconcile(ctx, h.campaign, "keeper", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %+v", result.Findings)
	}
	if _, err := h.canon.DecideBatch(ctx, h.campaign, result.Batch.ID, canon.DecisionDismiss, nil, "keeper"); err != nil {
		t.Fatal(err)
	}
	if hp := h.balanceOf(t, h.fighter, "hp:hp"); hp != -8 {
		t.Fatalf("dismissed batch applied a fix anyway: hp = %d", hp)
	}
}

func TestReconcileCleanBooksStageNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	result, err := h.leveling.Reconcile(ctx, h.campaign, "keeper", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 0 || result.Batch != nil {
		t.Fatalf("clean books staged %+v / %+v", result.Findings, result.Batch)
	}
}

func (h *harness) balanceOf(t *testing.T, eid, key string) int {
	t.Helper()
	balances, err := h.ledgers.Balances(context.Background(), h.campaign, eid)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range balances {
		if b.Pool.Key() == key {
			return b.Current
		}
	}
	t.Fatalf("no %s pool for %s", key, eid)
	return 0
}
