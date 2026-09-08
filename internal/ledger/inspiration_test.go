package ledger

// Inspiration's ledger semantics (MAD-428): awarded by the DM, held at
// most one, spent for advantage — and the acceptance pair the issue
// names. Inspiration itself never refills on a rest (2014: recovery
// manual, it lasts until spent), while a custom pool registered on the
// same grammar rests exactly like the built-ins its recovery matches —
// no special case anywhere in the engine.

import (
	"context"
	"errors"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

func TestInspirationAwardAndSpend(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if held, ok := h.ledgers.InspirationBalance(ctx, h.campaign, h.wizard); ok || held != 0 {
		t.Fatalf("before any award: held=%d ok=%v, want 0/false (no pool)", held, ok)
	}
	if _, err := h.ledgers.TrySpendInspiration(ctx, h.campaign, h.wizard, "", "keeper"); err == nil {
		t.Fatal("spending with no pool awarded must fail")
	} else if !errors.Is(err, campaign.ErrInvalid) {
		t.Fatalf("spend with no pool: %v", err)
	}

	if _, _, err := h.ledgers.AwardInspiration(ctx, h.campaign, h.wizard, "the goblin plan", "keeper"); err != nil {
		t.Fatalf("award: %v", err)
	}
	if held, ok := h.ledgers.InspirationBalance(ctx, h.campaign, h.wizard); !ok || held != 1 {
		t.Fatalf("after award: held=%d ok=%v, want 1/true", held, ok)
	}

	// 2014: it does not stack.
	if _, _, err := h.ledgers.AwardInspiration(ctx, h.campaign, h.wizard, "", "keeper"); err == nil {
		t.Fatal("a second award must refuse (inspiration does not stack)")
	} else if !errors.Is(err, campaign.ErrInvalid) {
		t.Fatalf("second award: %v", err)
	}

	txn, err := h.ledgers.TrySpendInspiration(ctx, h.campaign, h.wizard, "longbow attack", "keeper")
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if txn.Pool != InspirationKey || txn.Kind != TxnSpend || txn.Amount != 1 {
		t.Fatalf("spend txn = %+v", txn.Transaction)
	}
	if held, ok := h.ledgers.InspirationBalance(ctx, h.campaign, h.wizard); !ok || held != 0 {
		t.Fatalf("after spend: held=%d ok=%v, want 0/true", held, ok)
	}
	if _, err := h.ledgers.TrySpendInspiration(ctx, h.campaign, h.wizard, "", "keeper"); err == nil {
		t.Fatal("spending an empty pool must fail")
	}

	// A re-award after the spend is legal — the DM's word again.
	if _, _, err := h.ledgers.AwardInspiration(ctx, h.campaign, h.wizard, "", "keeper"); err != nil {
		t.Fatalf("re-award after spend: %v", err)
	}
	if held, _ := h.ledgers.InspirationBalance(ctx, h.campaign, h.wizard); held != 1 {
		t.Fatalf("after re-award: held=%d, want 1", held)
	}
}

// TestInspirationRestNeverRefills pins the 2014 rule against both rest
// kinds: inspiration is recovery manual — a rest neither grants it nor
// takes it away, in either direction.
func TestInspirationRestNeverRefills(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.ledgers.AwardInspiration(ctx, h.campaign, h.wizard, "", "keeper"); err != nil {
		t.Fatalf("award: %v", err)
	}
	if _, err := h.ledgers.TrySpendInspiration(ctx, h.campaign, h.wizard, "", "keeper"); err != nil {
		t.Fatalf("spend: %v", err)
	}
	if _, _, err := h.ledgers.Rest(ctx, h.campaign, []string{h.wizard}, RestLong, "", "", "keeper"); err != nil {
		t.Fatalf("long rest: %v", err)
	}
	if held, _ := h.ledgers.InspirationBalance(ctx, h.campaign, h.wizard); held != 0 {
		t.Fatalf("a long rest must not refill inspiration: held=%d", held)
	}

	if _, _, err := h.ledgers.AwardInspiration(ctx, h.campaign, h.warlock, "", "keeper"); err != nil {
		t.Fatalf("award: %v", err)
	}
	if _, _, err := h.ledgers.Rest(ctx, h.campaign, []string{h.warlock}, RestShort, "", "", "keeper"); err != nil {
		t.Fatalf("short rest: %v", err)
	}
	if held, _ := h.ledgers.InspirationBalance(ctx, h.campaign, h.warlock); held != 1 {
		t.Fatalf("a short rest must not take held inspiration away: held=%d", held)
	}
}

// TestCustomPoolsRestLikeBuiltIns is the acceptance: a DM-registered
// pool rests by its recovery grammar exactly as the sheet's own pools
// do — a short-recovery custom pool resets beside pact magic, a
// manual-recovery one never moves, all in the same batch as the
// built-ins.
func TestCustomPoolsRestLikeBuiltIns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	luck, err := h.ledgers.CreatePool(ctx, h.campaign, h.warlock, Pool{
		Kind: KindFeature, Name: "luck", Label: "Luck points", Size: 3, Recovery: RecoveryShort,
	})
	if err != nil {
		t.Fatalf("register luck: %v", err)
	}
	sorrow, err := h.ledgers.CreatePool(ctx, h.campaign, h.warlock, Pool{
		Kind: KindFeature, Name: "sorrow", Label: "Sorrow dice", Size: 2, Recovery: RecoveryManual,
	})
	if err != nil {
		t.Fatalf("register sorrow: %v", err)
	}
	pools := mustPools(t, h, h.warlock)
	spend := func(key string, n int) {
		t.Helper()
		for _, p := range pools {
			if p.Key() == key {
				for i := 0; i < n; i++ {
					if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.warlock, p.ID, TxnInput{Kind: TxnSpend, Amount: 1}, "keeper"); err != nil {
						t.Fatalf("spend %s: %v", key, err)
					}
				}
				return
			}
		}
		t.Fatalf("no pool %s", key)
	}
	spend("feature:luck", 2)
	spend("feature:sorrow", 1)
	spend("slot:3", 2) // pact magic, the built-in short-recovery fixture

	if _, _, err := h.ledgers.Rest(ctx, h.campaign, []string{h.warlock}, RestShort, "", "", "keeper"); err != nil {
		t.Fatalf("short rest: %v", err)
	}
	want := map[string]int{
		"feature:luck":   3, // short recovery: reset, like the pact slots
		"slot:3":         2, // the built-in the custom pool must match
		"feature:sorrow": 1, // manual: untouched
	}
	for _, b := range mustBalances(t, h, h.warlock) {
		if w, ok := want[b.Pool.Key()]; ok && b.Current != w {
			t.Fatalf("after short rest %s = %d, want %d", b.Pool.Key(), b.Current, w)
		}
	}

	// A long rest resets short and long both; manual still never moves.
	spend("feature:luck", 1)
	spend("feature:sorrow", 1)
	if _, _, err := h.ledgers.Rest(ctx, h.campaign, []string{h.warlock}, RestLong, "", "", "keeper"); err != nil {
		t.Fatalf("long rest: %v", err)
	}
	longWant := map[string]int{"feature:luck": 3, "feature:sorrow": 0}
	for _, b := range mustBalances(t, h, h.warlock) {
		if w, ok := longWant[b.Pool.Key()]; ok && b.Current != w {
			t.Fatalf("after long rest %s = %d, want %d", b.Pool.Key(), b.Current, w)
		}
	}
	if luck.Size != 3 || sorrow.Recovery != RecoveryManual {
		t.Fatalf("registration lost grammar: %+v %+v", *luck, *sorrow)
	}
}

// TestLinkTxnEvent pins the provenance hook the roll flow uses: the
// spend written before the roll existed gets pointed at the session
// event the roll left behind.
func TestLinkTxnEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.ledgers.AwardInspiration(ctx, h.campaign, h.wizard, "", "keeper"); err != nil {
		t.Fatalf("award: %v", err)
	}
	txn, err := h.ledgers.TrySpendInspiration(ctx, h.campaign, h.wizard, "", "keeper")
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO game_sessions (id, campaign_id, ordinal, name, status, created_at, updated_at)
		 VALUES ('sess-1', ?, 1, 'The Belltoll Job', 'done', 0, 0)`, h.campaign); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO session_events (id, session_id, seq, kind, summary, payload, created_at)
		 VALUES ('ev-1', 'sess-1', 1, 'roll', 'Velren — 1d20+5 (advantage) → 17', '{}', 0)`); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := h.ledgers.LinkTxnEvent(ctx, txn.ID, "ev-1", "sess-1"); err != nil {
		t.Fatalf("link: %v", err)
	}
	rows, err := h.ledgers.History(ctx, h.campaign, h.wizard, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, row := range rows {
		if row.ID == txn.ID {
			if row.EventID != "ev-1" || row.SessionID != "sess-1" {
				t.Fatalf("linked txn = event %q session %q, want ev-1/sess-1", row.EventID, row.SessionID)
			}
			return
		}
	}
	t.Fatal("spend txn missing from history")
}

// mustBalances folds one character's balances or fails the test.
func mustBalances(t *testing.T, h *harness, eid string) []Balance {
	t.Helper()
	balances, err := h.ledgers.Balances(context.Background(), h.campaign, eid)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	return balances
}
