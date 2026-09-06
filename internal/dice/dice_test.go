package dice

// The engine's contract (MAD-420): malformed formulas are errors, never
// guesses; advantage and disadvantage are one deterministic rewrite; the
// same (seed, nonce, formula) produce the same dice forever, which the
// golden file pins byte for byte.

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* ---------- the parser ---------- */

func TestParseNormalizes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1d20+5", "1d20 + 5"},
		{"2d6+3", "2d6 + 3"},
		{"d20", "1d20"},
		{"D20", "1d20"},
		{" 1d20 + 5 ", "1d20 + 5"},
		{"4d6kh3", "4d6kh3"},
		{"4D6KH3", "4d6kh3"},
		{"2d20kl1", "2d20kl1"},
		{"2d20kh", "2d20kh1"},
		{"8d6-2", "8d6 - 2"},
		{"1d8+2d6+3", "1d8 + 2d6 + 3"},
		{"10", "10"},
		{"1d100", "1d100"},
		{"-1d4+6", "-1d4 + 6"},
		{"2d6kh2", "2d6"}, // keep == count keeps every die: not a keep clause
	}
	for _, c := range cases {
		e, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got := e.String(); got != c.want {
			t.Errorf("Parse(%q).String() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"", "   ", "banana", "2d", "d", "d0", "0d6", "1d1", "1d1001", "101d6",
		"2d6kh0", "2d6kh3", "1d20++5", "1d20+)", "3..5",
		"2d6+", "1d20 5", "5 5", "1d6x3", "1d20*2", "3/2", "99999999999999999999",
		"2d6kh99999999999999999999", "1d20+-", "d-20",
	}
	for _, in := range bad {
		if e, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %s, want an error", in, e.String())
		}
	}
}

func TestParseErrorsNamePosition(t *testing.T) {
	_, err := Parse("1d20 + banana")
	if err == nil || err.Error() == "" {
		t.Fatalf("want an error naming the problem, got %v", err)
	}
}

/* ---------- advantage and disadvantage ---------- */

func TestWithMode(t *testing.T) {
	cases := []struct{ in, mode, want string }{
		{"1d20+5", ModeAdvantage, "2d20kh1 + 5"},
		{"1d20+5", ModeDisadvantage, "2d20kl1 + 5"},
		{"d20", ModeAdvantage, "2d20kh1"},
		{"2+1d20", ModeDisadvantage, "2 + 2d20kl1"},
		{"1d20", ModeNone, "1d20"},
	}
	for _, c := range cases {
		e, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		got, err := WithMode(e, c.mode)
		if err != nil {
			t.Fatalf("WithMode(%q, %s): %v", c.in, c.mode, err)
		}
		if got.String() != c.want {
			t.Errorf("WithMode(%q, %s) = %q, want %q", c.in, c.mode, got.String(), c.want)
		}
		// The original expression is untouched — mode is a read, not a mutation.
		if e.String() == c.want && c.in != c.want && c.mode != ModeNone {
			t.Errorf("WithMode mutated its input")
		}
	}
}

func TestWithModeRefuses(t *testing.T) {
	bad := []struct{ in, mode string }{
		{"2d6", ModeAdvantage},        // no d20
		{"2d20kh1", ModeAdvantage},    // already a keep clause
		{"1d20+1d20", ModeAdvantage},  // two d20 terms
		{"1d20+5", "luck"},            // not a mode
		{"2d20kl1", ModeDisadvantage}, // already disadvantage
	}
	for _, c := range bad {
		e, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if _, err := WithMode(e, c.mode); err == nil {
			t.Errorf("WithMode(%q, %s) succeeded, want an error", c.in, c.mode)
		}
	}
}

/* ---------- determinism ---------- */

func TestRollDeterministic(t *testing.T) {
	e, _ := Parse("4d6kh3 + 2")
	first := Roll(42, 7, e, "")
	for i := 0; i < 10; i++ {
		again := Roll(42, 7, e, "")
		got, _ := json.Marshal(again)
		want, _ := json.Marshal(first)
		if string(got) != string(want) {
			t.Fatalf("roll %d differs: %s vs %s", i, got, want)
		}
	}
	// A different nonce is a different stream.
	other := Roll(42, 8, e, "")
	got, _ := json.Marshal(other)
	want, _ := json.Marshal(first)
	if string(got) == string(want) {
		t.Fatalf("nonce 8 produced nonce 7's dice; the stream is not keyed on nonce")
	}
}

func TestRollTotals(t *testing.T) {
	// A flat-only formula: the total is the flats, modifier carries the sum.
	e, err := Parse("7+3-2")
	if err != nil {
		t.Fatal(err)
	}
	r := Roll(1, 1, e, "")
	if r.Total != 8 || r.Modifier != 8 {
		t.Fatalf("7+3-2 rolled total=%d modifier=%d, want 8/8", r.Total, r.Modifier)
	}
}

func TestRollKeepSemantics(t *testing.T) {
	// Advantage's whole drama: two dice, one kept, the dropped one still
	// on the record. Find a nonce where the two d20s differ.
	e, _ := Parse("2d20kh1")
	for nonce := int64(1); nonce < 200; nonce++ {
		r := Roll(3, nonce, e, "")
		dice := r.Terms[0].Dice
		if dice[0].Value == dice[1].Value {
			continue
		}
		kept, dropped := 0, 0
		for _, d := range dice {
			if d.Kept {
				kept = d.Value
			} else {
				dropped = d.Value
			}
		}
		if kept < dropped {
			t.Fatalf("nonce %d: kh kept %d and dropped %d", nonce, kept, dropped)
		}
		if r.Total != kept {
			t.Fatalf("nonce %d: total %d != kept %d", nonce, r.Total, kept)
		}
		return
	}
	t.Fatal("no differing 2d20 in 200 nonces — the stream is suspect")
}

func TestRollNaturalFlourishes(t *testing.T) {
	e20, _ := Parse("1d20+5")
	found20, found1 := false, false
	for nonce := int64(1); nonce < 400 && !(found20 && found1); nonce++ {
		r := Roll(5, nonce, e20, "")
		if r.Total == 25 {
			if !r.Natural20 {
				t.Fatalf("nonce %d: total 25 without the natural-20 flourish", nonce)
			}
			found20 = true
		}
		if r.Total == 6 {
			if !r.Natural1 {
				t.Fatalf("nonce %d: total 6 without the natural-1 flourish", nonce)
			}
			found1 = true
		}
	}
	if !found20 || !found1 {
		t.Fatalf("no natural 20/1 in 400 d20s (20=%v 1=%v) — the stream is suspect", found20, found1)
	}
	// 2d6 can never earn either flourish.
	e6, _ := Parse("2d6")
	for nonce := int64(1); nonce < 100; nonce++ {
		r := Roll(5, nonce, e6, "")
		if r.Natural20 || r.Natural1 {
			t.Fatal("2d6 earned a d20 flourish")
		}
	}
}

func TestRollEveryDieInRange(t *testing.T) {
	for _, formula := range []string{"1d2", "1d4", "1d6", "1d8", "1d10", "1d12", "1d20", "1d100", "3d6+2", "4d6kh3"} {
		e, err := Parse(formula)
		if err != nil {
			t.Fatalf("Parse(%q): %v", formula, err)
		}
		dt := e.Terms[0].Dice
		wantKept := dt.Count
		if dt.Keep > 0 {
			wantKept = dt.Keep
		}
		for nonce := int64(1); nonce < 60; nonce++ {
			r := Roll(99, nonce, e, "")
			kept := 0
			for _, d := range r.Terms[0].Dice {
				if d.Value < 1 || d.Value > dt.Sides {
					t.Fatalf("%s nonce %d: die %d outside 1..%d", formula, nonce, d.Value, dt.Sides)
				}
				if d.Kept {
					kept++
				}
			}
			if kept != wantKept {
				t.Fatalf("%s nonce %d: %d kept dice, want %d", formula, nonce, kept, wantKept)
			}
		}
	}
}

/* ---------- the golden file ---------- */

var updateGoldens = flag.Bool("update-golden", false, "rewrite the dice golden files")

// goldenCase is one pinned roll: the inputs that decide it and the whole
// result. The same (seed, nonce, formula, mode) must produce these bytes
// forever — that is the Stage 9 replay contract, asserted here at the
// engine and again at the store.
type goldenCase struct {
	Seed    int64      `json:"seed"`
	Nonce   int64      `json:"nonce"`
	Formula string     `json:"formula"`
	Mode    string     `json:"mode,omitempty"`
	Result  RollResult `json:"result"`
}

// TestRollGolden pins a spread of real rolls — attacks, advantage,
// disadvantage, stat rolls, damage, a flat, a negative term — against
// testdata/rolls_golden.json. Regenerate with -update-golden after an
// intentional change to the grammar, the stream or the result shape; a
// diff you did not intend is a regression someone's replay will feel.
func TestRollGolden(t *testing.T) {
	cases := []goldenCase{
		{Seed: 20260906, Nonce: 1, Formula: "1d20+5"},
		{Seed: 20260906, Nonce: 2, Formula: "1d20+5", Mode: ModeAdvantage},
		{Seed: 20260906, Nonce: 3, Formula: "1d20+3", Mode: ModeDisadvantage},
		{Seed: 20260906, Nonce: 4, Formula: "2d6+3"},
		{Seed: 20260906, Nonce: 5, Formula: "4d6kh3"},
		{Seed: 20260906, Nonce: 6, Formula: "8d6"},
		{Seed: 20260906, Nonce: 7, Formula: "1d12-2"},
		{Seed: 20260906, Nonce: 8, Formula: "10"},
		{Seed: 20260906, Nonce: 9, Formula: "1d100"},
		{Seed: 20260906, Nonce: 10, Formula: "2d20kh1+5"},
		{Seed: 20260906, Nonce: 11, Formula: "3d8+2d6+4"},
	}
	for i := range cases {
		e, err := Parse(cases[i].Formula)
		if err != nil {
			t.Fatalf("Parse(%q): %v", cases[i].Formula, err)
		}
		if cases[i].Mode != "" {
			e, err = WithMode(e, cases[i].Mode)
			if err != nil {
				t.Fatalf("WithMode: %v", err)
			}
		}
		cases[i].Result = *Roll(cases[i].Seed, cases[i].Nonce, e, cases[i].Mode)
	}
	gotBytes, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	gotPath := filepath.Join("testdata", "rolls_golden.json")
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(gotPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gotPath, append(gotBytes, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("read golden (run -update-golden once): %v", err)
	}
	if got, w := strings.TrimSpace(string(gotBytes)), strings.TrimSpace(string(want)); got != w {
		t.Fatalf("rolls golden differs; regenerate with -update-golden and read the diff before accepting")
	}
}
