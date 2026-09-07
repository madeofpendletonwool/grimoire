package leveling

// The pure leveling engine's tests (MAD-424): the award split, the mode
// config, and the level-up diff — the last pinned by golden files, one
// single-class path and one multiclass path, per the issue's acceptance.

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/progression"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

func TestSplit(t *testing.T) {
	// 300 XP across three characters: 100 each.
	got := Split(300, []string{"a", "b", "c"})
	want := map[string]int{"a": 100, "b": 100, "c": 100}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("Split(300)[%s] = %d, want %d", k, got[k], v)
		}
	}
	// The remainder distributes whole, deterministically in id order:
	// 100 XP across three is 34/33/33.
	got = Split(100, []string{"c", "a", "b"})
	if got["a"] != 34 || got["b"] != 33 || got["c"] != 33 {
		t.Fatalf("Split(100) = %v, want a=34 b=33 c=33", got)
	}
	// Determinism: same inputs, same bytes.
	again, _ := json.Marshal(Split(100, []string{"c", "a", "b"}))
	first, _ := json.Marshal(Split(100, []string{"a", "b", "c"}))
	if string(again) != string(first) {
		t.Fatal("Split must be order-independent in its output bytes")
	}
	if got := Split(0, []string{"a"}); got["a"] != 0 {
		t.Fatalf("Split(0) = %v", got)
	}
}

func TestConfig(t *testing.T) {
	if cfg := ConfigOf(nil); cfg.Mode != ModeXP {
		t.Errorf("absent settings default to xp, got %q", cfg.Mode)
	}
	cfg, err := Parse(map[string]any{"mode": "milestone"})
	if err != nil || cfg.Mode != ModeMilestone {
		t.Errorf("Parse(milestone) = %v, %v", cfg, err)
	}
	if _, err := Parse(map[string]any{"mode": "story"}); err == nil {
		t.Error("an unknown mode is an error, never a guess")
	}
	// The round trip the settings write depends on.
	back, err := Parse(cfg.SettingsValue())
	if err != nil || back != cfg {
		t.Errorf("SettingsValue does not round-trip: %v, %v", back, err)
	}
}

func TestComputeDiffUnknownClass(t *testing.T) {
	if _, err := ComputeDiff(DiffInput{Name: "Thalia", Class: "blood hunter"}); err == nil {
		t.Fatal("an unknown class is an error")
	}
}

func TestComputeDiffCap(t *testing.T) {
	s := sheet.Sheet{Classes: []sheet.ClassLevel{{Class: "wizard", Level: 20}}, MaxHP: 90}
	if _, err := ComputeDiff(DiffInput{Name: "Thalia", Sheet: s, Class: "wizard"}); err == nil {
		t.Fatal("level 21 is an error")
	}
}

func TestApplyDiffIdempotent(t *testing.T) {
	s := sheet.Sheet{
		Classes:   []sheet.ClassLevel{{Class: "wizard", Level: 4}},
		Abilities: sheet.Abilities{CON: 16},
		MaxHP:     32,
	}
	d, err := ComputeDiff(DiffInput{Entity: "e1", Name: "Thalia", Sheet: s, Class: "wizard", Mode: ModeXP})
	if err != nil {
		t.Fatal(err)
	}
	once := ApplyDiff(s, d)
	twice := ApplyDiff(once, d)
	b1, _ := json.Marshal(once)
	b2, _ := json.Marshal(twice)
	if string(b1) != string(b2) {
		t.Fatalf("ApplyDiff is not idempotent:\n%s\n%s", b1, b2)
	}
	if once.TotalLevel() != 5 || once.MaxHP != 32+d.HPGain {
		t.Fatalf("apply produced level %d hp %d, want 5 and %d", once.TotalLevel(), once.MaxHP, 32+d.HPGain)
	}
}

/* ---------- the golden files ---------- */

var updateGoldens = flag.Bool("update-golden", false, "rewrite the level-up diff golden files")

// goldenCase is one pinned diff: the sheet the character carries, the
// class being advanced, and the whole diff. The same (sheet, class) must
// produce these bytes forever — a diff a review showed is a diff the
// finalizer applies, and the golden is the contract between them.
type goldenCase struct {
	Name   string      `json:"name"`
	Mode   string      `json:"mode"`
	Sheet  sheet.Sheet `json:"sheet"`
	Class  string      `json:"class"`
	Entity string      `json:"entity"`
	Diff   Diff        `json:"diff"`
}

// TestDiffGolden pins one single-class path and one multiclass path —
// the issue's acceptance criterion. Regenerate with -update-golden after
// an intentional change to the tables or the diff shape; a diff you did
// not intend is a level a character did not earn.
func TestDiffGolden(t *testing.T) {
	cases := []goldenCase{
		{
			// The classic: a 4th-level wizard (CON 16, 32 hp) reaches
			// 5th — third-level slots arrive, the fixed average adds
			// 6 hp, and level 5 grants the wizard nothing else.
			Name:   "single-class wizard 4 -> 5",
			Mode:   ModeXP,
			Entity: "golden-wizard",
			Sheet: sheet.Sheet{
				Race:      "elf",
				XP:        7000,
				Classes:   []sheet.ClassLevel{{Class: "wizard", Level: 4}},
				Abilities: sheet.Abilities{INT: 17, CON: 16, DEX: 12},
				MaxHP:     32,
				Spellcasting: &sheet.Spellcasting{
					Ability: "int", DC: 13, AttackBonus: 5,
					Slots: map[string]int{"1": 4, "2": 3},
				},
			},
			Class: "wizard",
		},
		{
			// The multiclass path: a 8th-level fighter takes a first
			// wizard level — a new class entry, both saves, the level-1
			// features, first-level slots seeded on a sheet that had no
			// slot table, and a d6's average for the hp. The diff names
			// the combined-table question instead of computing a wrong
			// total.
			Name:   "multiclass fighter 8 adds wizard 1",
			Mode:   ModeXP,
			Entity: "golden-multiclass",
			Sheet: sheet.Sheet{
				Race:      "human",
				XP:        40000,
				Classes:   []sheet.ClassLevel{{Class: "fighter", Level: 8}},
				Abilities: sheet.Abilities{STR: 18, CON: 14, INT: 15},
				MaxHP:     79,
			},
			Class: "wizard",
		},
	}
	for i := range cases {
		diff, err := ComputeDiff(DiffInput{
			Entity: cases[i].Entity, Name: cases[i].Name,
			Sheet: cases[i].Sheet, Class: cases[i].Class, Mode: cases[i].Mode,
		})
		if err != nil {
			t.Fatalf("%s: %v", cases[i].Name, err)
		}
		cases[i].Diff = diff
	}
	got, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "levelup_diffs_golden.json")
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run go test ./internal/leveling -update-golden): %v", err)
	}
	if got, w := strings.TrimSpace(string(got)), strings.TrimSpace(string(want)); got != w {
		t.Errorf("level-up diff golden differs; regenerate with -update-golden and read the diff before accepting")
	}
}

func TestSummaryMentionsTheNumbers(t *testing.T) {
	s := sheet.Sheet{
		Classes:   []sheet.ClassLevel{{Class: "wizard", Level: 4}},
		Abilities: sheet.Abilities{CON: 16},
		MaxHP:     32,
	}
	d, err := ComputeDiff(DiffInput{Entity: "e1", Name: "Thalia", Sheet: s, Class: "wizard", Mode: ModeXP})
	if err != nil {
		t.Fatal(err)
	}
	sum := Summary(d)
	for _, want := range []string{"level 4 to 5", "total level 5", "max hp 32 -> 39", "3rd slots 0 -> 2"} {
		if !strings.Contains(sum, want) {
			t.Errorf("Summary missing %q: %s", want, sum)
		}
	}
}

func TestEligibleXP(t *testing.T) {
	need5, err := progression.XPForLevel(5)
	if err != nil {
		t.Fatal(err)
	}
	s := sheet.Sheet{
		Classes: []sheet.ClassLevel{{Class: "wizard", Level: 4}},
		XP:      need5 - 1,
	}
	d, err := ComputeDiff(DiffInput{Entity: "e1", Name: "Thalia", Sheet: s, Class: "wizard", Mode: ModeXP})
	if err != nil {
		t.Fatal(err)
	}
	if EligibleXP(s, d) {
		t.Error("one XP short of 5th is not eligible")
	}
	s.XP++
	if !EligibleXP(s, d) {
		t.Error("6500 XP is eligible for 5th")
	}
}
