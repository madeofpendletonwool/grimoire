package ledger

// The recovery grammar's unit tests (MAD-419). The two load-bearing
// fixtures are the false-positive pair the issue names: pact magic —
// recovery short — MUST reset on a long rest as well as a short one, and
// regular slots — recovery long — must NOT reset on a short rest. A grammar
// that got only one direction right would quietly hand every warlock their
// slots back mid-dungeon or dock every wizard's mid-camp.

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

// warlockSheet is the pact-magic fixture: one class, one slot table.
func warlockSheet() sheet.Sheet {
	return sheet.Sheet{
		Classes: []sheet.ClassLevel{{Class: "Warlock", Level: 5}},
		Spellcasting: &sheet.Spellcasting{
			Ability: "cha",
			Slots:   map[string]int{"1": 2, "2": 2, "3": 2, "4": 1, "5": 1},
		},
		MaxHP: 33,
	}
}

// wizardSheet is the regular-caster fixture.
func wizardSheet() sheet.Sheet {
	return sheet.Sheet{
		Classes: []sheet.ClassLevel{{Class: "Wizard", Level: 5}},
		Spellcasting: &sheet.Spellcasting{
			Ability: "int",
			Slots:   map[string]int{"1": 4, "2": 3, "3": 2},
		},
		MaxHP: 32,
	}
}

// multiclassWarlockSheet is the known limit: a warlock beside another
// casting class reads as the combined table, long recovery.
func multiclassWarlockSheet() sheet.Sheet {
	return sheet.Sheet{
		Classes: []sheet.ClassLevel{{Class: "Wizard", Level: 4}, {Class: "Warlock", Level: 3}},
		Spellcasting: &sheet.Spellcasting{
			Ability: "int",
			Slots:   map[string]int{"1": 4, "2": 3},
		},
	}
}

func TestPoolsOfPactMagicIsShortRecovery(t *testing.T) {
	pools := PoolsOf(warlockSheet())
	slot3 := poolByKey(t, pools, "slot:3")
	if slot3.Recovery != RecoveryShort {
		t.Fatalf("warlock slot:3 recovery = %q, want short (pact magic)", slot3.Recovery)
	}
	if slot3.Size != 2 {
		t.Fatalf("warlock slot:3 size = %d, want 2", slot3.Size)
	}
	hd := poolByKey(t, pools, "hit_dice:hit_dice")
	if hd.Recovery != RecoveryManual {
		t.Fatalf("hit dice recovery = %q, want manual (the half-regain is a transaction)", hd.Recovery)
	}
	if hd.Size != 5 {
		t.Fatalf("hit dice size = %d, want 5", hd.Size)
	}
}

func TestPoolsOfRegularSlotsAreLongRecovery(t *testing.T) {
	pools := PoolsOf(wizardSheet())
	if r := poolByKey(t, pools, "slot:1").Recovery; r != RecoveryLong {
		t.Fatalf("wizard slot:1 recovery = %q, want long", r)
	}
	pools = PoolsOf(multiclassWarlockSheet())
	if r := poolByKey(t, pools, "slot:1").Recovery; r != RecoveryLong {
		t.Fatalf("multiclass warlock slot:1 recovery = %q, want long (the combined table)", r)
	}
}

func TestPoolsOfCurrency(t *testing.T) {
	s := wizardSheet()
	s.Currency = sheet.Currency{CP: 12, GP: 130}
	pools := PoolsOf(s)
	if p := poolByKey(t, pools, "currency:gp"); p.Size != 130 || p.Recovery != RecoveryManual || p.Bounded() {
		t.Fatalf("gold pool wrong: %+v", p)
	}
	for _, p := range pools {
		if p.Name == "pp" {
			t.Fatalf("zero coins seeded a pool: %+v", p)
		}
	}
}

// poolByKey finds a pool by its canonical key or fails the test.
func poolByKey(t *testing.T, pools []Pool, key string) Pool {
	t.Helper()
	for _, p := range pools {
		if p.Key() == key {
			return p
		}
	}
	t.Fatalf("no pool %s in %v", key, pools)
	return Pool{}
}

// The recovery cast: two owners, as at a table. The warlock carries pact
// magic (short), ki (short), and a dawn pool; the wizard carries regular
// slots (long), rage (long — a magic item, say), arrows and the purse
// (manual), and both carry hit dice. Pools are owner-scoped in the store;
// the pure fixture keeps them apart the same way.
func warlockFixturePools() []Pool {
	pools := PoolsOf(warlockSheet())
	pools = append(pools,
		Pool{Kind: KindFeature, Name: "ki", Label: "ki points", Size: 5, Recovery: RecoveryShort, Source: "dm"},
		Pool{Kind: KindFeature, Name: "mantle_of_dawn", Size: 2, Recovery: RecoveryDawn, Source: "dm"},
	)
	return pools
}

func wizardFixturePools() []Pool {
	pools := PoolsOf(wizardSheet())
	pools = append(pools,
		Pool{Kind: KindFeature, Name: "rage", Label: "rages", Size: 3, Recovery: RecoveryLong, Source: "dm"},
		Pool{Kind: KindItem, Name: "arrows", Size: 20, Recovery: RecoveryManual, Source: "dm"},
		Pool{Kind: KindCurrency, Name: "gp", Label: "gold", Size: 130, Recovery: RecoveryManual, Source: "sheet"},
	)
	return pools
}

// The day's spends, per owner.
func warlockSpends() []Transaction {
	return []Transaction{
		{Pool: "slot:1", Kind: TxnSpend, Amount: 1, Actor: "player"},
		{Pool: "slot:3", Kind: TxnSpend, Amount: 2, Actor: "player"},
		{Pool: "slot:5", Kind: TxnSpend, Amount: 1, Actor: "player"},
		{Pool: "hit_dice:hit_dice", Kind: TxnSpend, Amount: 4, Actor: "player"},
		{Pool: "feature:ki", Kind: TxnSpend, Amount: 4, Actor: "player"},
		{Pool: "feature:mantle_of_dawn", Kind: TxnSpend, Amount: 1, Actor: "player"},
	}
}

func wizardSpends() []Transaction {
	return []Transaction{
		{Pool: "slot:1", Kind: TxnSpend, Amount: 3, Actor: "player"},
		{Pool: "slot:2", Kind: TxnSpend, Amount: 2, Actor: "player"},
		{Pool: "slot:3", Kind: TxnSpend, Amount: 1, Actor: "player"},
		{Pool: "hit_dice:hit_dice", Kind: TxnSpend, Amount: 3, Actor: "player"},
		{Pool: "feature:rage", Kind: TxnSpend, Amount: 1, Actor: "player"},
		{Pool: "item:arrows", Kind: TxnSpend, Amount: 12, Actor: "player"},
		{Pool: "currency:gp", Kind: TxnSpend, Amount: 25, Actor: "player"},
	}
}

// TestRestRecoveryBothDirections is the acceptance fixture: a short rest
// resets pact magic but not regular slots; a long rest resets both — plus
// the dawn pool and the long-recovery features — and returns half the hit
// dice while leaving manual pools and the purse exactly as they were.
func TestRestRecoveryBothDirections(t *testing.T) {
	warlockPools, wizardPools := warlockFixturePools(), wizardFixturePools()
	warlockDay1, wizardDay1 := warlockSpends(), wizardSpends()

	// The short rest, on both casters.
	warlockShort := RestPlan(RestShort, warlockPools, Derive(warlockPools, warlockDay1))
	warlockBy := planByPool(warlockShort)
	if _, ok := warlockBy["slot:3"]; !ok {
		t.Fatalf("short rest did not reset pact magic — pact slots are short-recovery")
	}
	if _, ok := warlockBy["feature:ki"]; !ok {
		t.Fatalf("short rest did not reset ki (short-recovery feature)")
	}
	if _, ok := warlockBy["feature:mantle_of_dawn"]; ok {
		t.Fatalf("short rest reset a dawn pool")
	}
	if _, ok := warlockBy["hit_dice:hit_dice"]; ok {
		t.Fatalf("short rest touched hit dice")
	}

	wizardShort := RestPlan(RestShort, wizardPools, Derive(wizardPools, wizardDay1))
	wizardBy := planByPool(wizardShort)
	for key := range wizardBy {
		t.Fatalf("short rest reset %s — regular slots, rage, arrows and gold are not short-recovery; this is the false positive", key)
	}

	// The long rest resets everything a short rest would, too — proven
	// against the same spent state, not after the short rest refilled it.
	applyRest := func(txns []Transaction, plan []PlannedTxn) []Transaction {
		out := append([]Transaction(nil), txns...)
		for _, p := range plan {
			out = append(out, Transaction{Pool: p.Pool, Kind: p.Kind, Amount: p.Amount})
		}
		return out
	}
	warlockLong := RestPlan(RestLong, warlockPools, Derive(warlockPools, warlockDay1))
	warlockLongBy := planByPool(warlockLong)
	if _, ok := warlockLongBy["slot:3"]; !ok {
		t.Fatalf("long rest did not reset pact magic — a long rest resets everything a short rest would; this is the other false direction")
	}
	if _, ok := warlockLongBy["feature:mantle_of_dawn"]; !ok {
		t.Fatalf("long rest did not reset the dawn pool (the night crosses dawn)")
	}
	if p, ok := warlockLongBy["hit_dice:hit_dice"]; !ok || p.Kind != TxnRegain || p.Amount != 2 {
		t.Fatalf("long rest hit-dice regain wrong (burned 4 of 5, half of 5 is 2): %+v", p)
	}

	wizardAfterShort := applyRest(wizardSpends(), wizardShort)
	wizardDay2 := append(append([]Transaction(nil), wizardAfterShort...),
		Transaction{Pool: "slot:1", Kind: TxnSpend, Amount: 1, Actor: "player"})
	wizardLong := RestPlan(RestLong, wizardPools, Derive(wizardPools, wizardDay2))
	wizardLongBy := planByPool(wizardLong)
	for _, key := range []string{"slot:1", "slot:2", "slot:3", "feature:rage"} {
		if _, ok := wizardLongBy[key]; !ok {
			t.Fatalf("long rest did not reset %s (long-recovery)", key)
		}
	}
	if _, ok := wizardLongBy["item:arrows"]; ok {
		t.Fatalf("long rest reset arrows (manual recovery)")
	}
	if _, ok := wizardLongBy["currency:gp"]; ok {
		t.Fatalf("long rest touched the purse")
	}
	if p, ok := wizardLongBy["hit_dice:hit_dice"]; !ok || p.Amount != 2 {
		t.Fatalf("long rest hit-dice regain wrong (burned 3 of 5, half of 5 is 2): %+v", p)
	}
}

func planByPool(plan []PlannedTxn) map[string]PlannedTxn {
	out := make(map[string]PlannedTxn, len(plan))
	for _, p := range plan {
		out[p.Pool] = p
	}
	return out
}

func TestHitDiceRegain(t *testing.T) {
	cases := []struct {
		total, spent, want int
	}{
		{1, 1, 1}, // the minimum-one-die rule
		{2, 2, 1}, // half of two
		{5, 4, 2}, // half of five, floored
		{5, 1, 1}, // capped by what is spent
		{7, 0, 0}, // nothing spent, nothing returns
		{5, 7, 2}, // an over-spent log still caps at half of total
	}
	for _, c := range cases {
		if got := HitDiceRegain(c.total, c.spent); got != c.want {
			t.Fatalf("HitDiceRegain(%d, %d) = %d, want %d", c.total, c.spent, got, c.want)
		}
	}
}

// TestDeriveByteStable pins the derivation's contract: the same pools and
// the same log produce the same bytes, whatever order the pools arrived in
// and however many times the fold runs. The log's own order is the truth
// and changing it changes the answer.
func TestDeriveByteStable(t *testing.T) {
	pools := wizardFixturePools()
	txns := wizardSpends()

	first, err := json.Marshal(Derive(pools, txns))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := json.Marshal(Derive(pools, txns))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(first) != string(again) {
			t.Fatalf("derivation not byte-stable on re-run %d:\n%s\n%s", i, first, again)
		}
	}

	// Pool order must not leak into the output.
	shuffled := append([]Pool(nil), pools...)
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	reordered, err := json.Marshal(Derive(shuffled, txns))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(reordered) {
		t.Fatalf("pool order changed the derivation's bytes")
	}

	// Transaction order is the truth: a set before a spend is not the same
	// ledger as a set after it.
	ordered := []Transaction{{Pool: "slot:1", Kind: TxnSet, Amount: 1}, {Pool: "slot:1", Kind: TxnSpend, Amount: 1}}
	reversed := []Transaction{{Pool: "slot:1", Kind: TxnSpend, Amount: 1}, {Pool: "slot:1", Kind: TxnSet, Amount: 1}}
	if string(mustJSON(t, Derive(pools, ordered))) == string(mustJSON(t, Derive(pools, reversed))) {
		t.Fatalf("log order does not matter to the fold — it must")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return blob
}

func TestTransactionValidation(t *testing.T) {
	slot := Pool{Kind: KindSlot, Name: "3", Size: 2, Recovery: RecoveryLong}
	if err := (Transaction{Kind: TxnSpend, Amount: 3}).Validate(slot, 2); err == nil {
		t.Fatalf("overspend accepted")
	}
	if err := (Transaction{Kind: TxnSpend, Amount: 2}).Validate(slot, 2); err != nil {
		t.Fatalf("legal spend refused: %v", err)
	}
	if err := (Transaction{Kind: TxnRegain, Amount: 1}).Validate(slot, 2); err == nil {
		t.Fatalf("overfill regain accepted on a bounded pool")
	}
	gold := Pool{Kind: KindCurrency, Name: "gp", Size: 130, Recovery: RecoveryManual}
	if err := (Transaction{Kind: TxnRegain, Amount: 500}).Validate(gold, 130); err != nil {
		t.Fatalf("unbounded pool refused an overfill regain: %v", err)
	}
	if err := (Transaction{Kind: TxnSpend, Amount: 0}).Validate(gold, 130); err == nil {
		t.Fatalf("zero-amount spend accepted")
	}
	if err := ValidateSet(slot, 3); err == nil {
		t.Fatalf("set above size accepted on a bounded pool")
	}
	if err := ValidateSet(gold, 999); err != nil {
		t.Fatalf("set above size refused on an unbounded pool: %v", err)
	}
	if err := ValidateSet(slot, -1); err == nil {
		t.Fatalf("negative set accepted")
	}
}

/* ---------- the golden file ---------- */

var updateGoldens = flag.Bool("update-golden", false, "rewrite the recovery golden file")

// TestRecoveryGolden pins the whole recovery story as one fixture: two
// casters, a day of spends each, a short rest, a second day, a long rest —
// each stage's plan and derived balances rendered to bytes and compared
// against testdata/recovery_golden.json. Regenerate with -update-golden
// after an intentional grammar change; a diff you did not intend is a
// regression.
func TestRecoveryGolden(t *testing.T) {
	applyRest := func(txns []Transaction, plan []PlannedTxn) []Transaction {
		out := append([]Transaction(nil), txns...)
		for _, p := range plan {
			out = append(out, Transaction{Pool: p.Pool, Kind: p.Kind, Amount: p.Amount})
		}
		return out
	}

	warlockPools, wizardPools := warlockFixturePools(), wizardFixturePools()
	warlockDay1, wizardDay1 := warlockSpends(), wizardSpends()
	warlockShort := RestPlan(RestShort, warlockPools, Derive(warlockPools, warlockDay1))
	wizardShort := RestPlan(RestShort, wizardPools, Derive(wizardPools, wizardDay1))

	warlockAfterShort := applyRest(warlockDay1, warlockShort)
	wizardAfterShort := applyRest(wizardDay1, wizardShort)

	warlockDay2 := append(append([]Transaction(nil), warlockAfterShort...),
		Transaction{Pool: "feature:ki", Kind: TxnSpend, Amount: 2, Actor: "player"},
		Transaction{Pool: "slot:3", Kind: TxnSpend, Amount: 1, Actor: "player"},
	)
	wizardDay2 := append(append([]Transaction(nil), wizardAfterShort...),
		Transaction{Pool: "slot:1", Kind: TxnSpend, Amount: 1, Actor: "player"},
		Transaction{Pool: "item:arrows", Kind: TxnSpend, Amount: 5, Actor: "player"},
		Transaction{Pool: "item:arrows", Kind: TxnRegain, Amount: 11, Actor: "dm", Note: "quiver refilled from the goblins"},
		Transaction{Pool: "currency:gp", Kind: TxnSet, Amount: 40, Actor: "dm", Note: "purse recounted"},
	)

	warlockLong := RestPlan(RestLong, warlockPools, Derive(warlockPools, warlockDay2))
	wizardLong := RestPlan(RestLong, wizardPools, Derive(wizardPools, wizardDay2))

	got := map[string]any{
		"warlock": map[string]any{
			"pools":          warlockPools,
			"day1_spends":    warlockDay1,
			"day1_balances":  Derive(warlockPools, warlockDay1),
			"short_rest":     warlockShort,
			"day2_spends":    warlockDay2[len(warlockAfterShort):],
			"day2_balances":  Derive(warlockPools, warlockDay2),
			"long_rest":      warlockLong,
			"final_balances": Derive(warlockPools, applyRest(warlockDay2, warlockLong)),
		},
		"wizard": map[string]any{
			"pools":          wizardPools,
			"day1_spends":    wizardDay1,
			"day1_balances":  Derive(wizardPools, wizardDay1),
			"short_rest":     wizardShort,
			"day2_spends":    wizardDay2[len(wizardAfterShort):],
			"day2_balances":  Derive(wizardPools, wizardDay2),
			"long_rest":      wizardLong,
			"final_balances": Derive(wizardPools, applyRest(wizardDay2, wizardLong)),
		},
	}
	gotBytes := mustJSON(t, got)

	golden := filepath.Join("testdata", "recovery_golden.json")
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(golden, append(gotBytes, '\n'), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run -update-golden once): %v", err)
	}
	if string(want) != string(append(gotBytes, '\n')) {
		gotPath := filepath.Join(t.TempDir(), "got.json")
		_ = os.WriteFile(gotPath, gotBytes, 0o644)
		t.Fatalf("recovery golden differs; got %s, want %s", gotPath, golden)
	}
}
