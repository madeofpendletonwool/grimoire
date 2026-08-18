package encounter

import (
	"math"
	"testing"
)

// Every value asserted in this file was checked against the indexed
// rulebooks in dnd-books/: the DMG chapter 3 encounter-building text
// (including its worked examples) and Monster Manual stat blocks.

func TestXPByCRSpotValues(t *testing.T) {
	cases := map[string]int{
		"0": 10, "1/8": 25, "1/4": 50, "1/2": 100,
		"1": 200, "2": 450, "3": 700, "4": 1100, "5": 1800,
		"13": 10000, "16": 15000, "20": 25000, "30": 155000,
	}
	for cr, want := range cases {
		if got, ok := CRXP(cr); !ok || got != want {
			t.Errorf("CR %s = %d (ok=%v), want %d", cr, got, ok, want)
		}
	}
}

// The DMG names a manticore/owlbear (CR 3) as worth 700 XP and dire wolves
// (CR 1) as 200 XP each in its chapter 3 examples; the indexed Monster Manual
// prints Goblin CR 1/4 (50 XP), Bugbear CR 1 (200 XP), and CR 2 (450 XP) in
// the stat blocks themselves.
func TestXPForCRAcceptsBookForms(t *testing.T) {
	cases := map[string]int{
		"1/4": 50, "0.25": 50, "1": 200, "1.0": 200, "2": 450, "3": 700,
	}
	for cr, want := range cases {
		got, err := XPForCR(cr)
		if err != nil {
			t.Errorf("XPForCR(%q): %v", cr, err)
			continue
		}
		if got != want {
			t.Errorf("XPForCR(%q) = %d, want %d", cr, got, want)
		}
	}
	if _, err := XPForCR("legendary"); err == nil {
		t.Error("XPForCR should reject a non-rating")
	}
	if _, err := XPForCR("31"); err == nil {
		t.Error("XPForCR should reject a rating outside the table")
	}
}

func TestThresholdsSpotValues(t *testing.T) {
	// The DMG's worked example: three 3rd-level characters and one 2nd-level
	// character total Easy 275, Medium 550, Hard 825, Deadly 1,400 XP.
	party := []int{3, 3, 3, 2}
	v := Evaluate(party, nil)
	want := map[string]int{BandEasy: 275, BandMedium: 550, BandHard: 825, BandDeadly: 1400}
	for band, w := range want {
		if v.Thresholds[band] != w {
			t.Errorf("%s threshold = %d, want %d", band, v.Thresholds[band], w)
		}
	}
	// Rows printed legibly in the indexed table: levels 12, 13, 20.
	for _, tc := range []struct {
		level               int
		easy, med, hard, dl int
	}{
		{12, 1000, 2000, 3000, 4500},
		{13, 1100, 2200, 3400, 5100},
		{20, 2800, 5700, 8500, 12700},
	} {
		for band, w := range map[string]int{BandEasy: tc.easy, BandMedium: tc.med, BandHard: tc.hard, BandDeadly: tc.dl} {
			got, ok := ThresholdFor(tc.level, band)
			if !ok || got != w {
				t.Errorf("level %d %s = %d (ok=%v), want %d", tc.level, band, got, ok, w)
			}
		}
	}
}

func TestMultiplierTable(t *testing.T) {
	// The five-to-six character party the guidelines assume: the table as
	// printed for 3-5 characters, one rung down for 6+.
	cases := []struct {
		monsters, party int
		want            float64
	}{
		{1, 4, 1}, {2, 4, 1.5}, {3, 4, 2}, {6, 4, 2},
		{7, 4, 2.5}, {10, 4, 2.5}, {11, 4, 3}, {14, 4, 3}, {15, 4, 4}, {40, 4, 4},
		// Party Size note: fewer than three characters moves one rung up
		// (a single monster x1.5, fifteen-plus x5), six or more moves one
		// rung down (a single monster x0.5).
		{1, 2, 1.5}, {15, 2, 5}, {1, 6, 0.5}, {2, 6, 1},
	}
	for _, tc := range cases {
		if got := multiplierFor(tc.monsters, tc.party); got != tc.want {
			t.Errorf("multiplierFor(%d monsters, %d party) = %v, want %v", tc.monsters, tc.party, got, tc.want)
		}
	}
}

// The DMG's bugbear-and-hobgoblins example: 200 + 3x100 = 500 XP, x2 for
// four monsters = 1,000 adjusted, a hard encounter for the sample party.
func TestDMGBugbearExample(t *testing.T) {
	v := Evaluate([]int{3, 3, 3, 2}, []Monster{
		{Name: "Bugbear", CR: "1", XP: 200, Count: 1},
		{Name: "Hobgoblin", CR: "1/2", XP: 100, Count: 3},
	})
	if v.TotalXP != 500 {
		t.Errorf("total XP = %d, want 500", v.TotalXP)
	}
	if v.AdjustedXP != 1000 {
		t.Errorf("adjusted XP = %d, want 1000", v.AdjustedXP)
	}
	if v.Difficulty != BandHard {
		t.Errorf("difficulty = %q, want %q", v.Difficulty, BandHard)
	}
}

// The issue's acceptance case: four goblins (CR 1/4, 50 XP each) against
// five 1st-level characters. 200 XP raw, x2 for four monsters = 400
// adjusted; the party's Hard threshold is 5x75 = 375 and Deadly 5x100 = 500,
// so the encounter reads Hard.
func TestFourGoblinsVsFiveLevelOnes(t *testing.T) {
	v := Evaluate([]int{1, 1, 1, 1, 1}, []Monster{
		{Name: "Goblin", CR: "1/4", XP: 50, Count: 4},
	})
	if v.TotalXP != 200 || v.AdjustedXP != 400 {
		t.Fatalf("XP: total %d adjusted %d, want 200/400", v.TotalXP, v.AdjustedXP)
	}
	if v.Difficulty != BandHard {
		t.Errorf("difficulty = %q, want %q", v.Difficulty, BandHard)
	}
	if v.Margins[BandDeadly] != 100 {
		t.Errorf("deadly margin = %d, want 100 (500 - 400)", v.Margins[BandDeadly])
	}
}

func TestEvaluateBands(t *testing.T) {
	goblins := goblinGroup
	party := []int{1, 1, 1, 1, 1} // Easy 125 / Medium 250 / Hard 375 / Deadly 500
	cases := []struct {
		monsters []Monster
		want     string
	}{
		{nil, BandTrivial},        // nothing fielded
		{goblins(1), BandTrivial}, // 50 < Easy 125
		{goblins(2), BandEasy},    // 100 x1.5 = 150 >= 125
		{goblins(4), BandHard},    // 400
		{goblins(10), BandDeadly}, // 500 x2.5 = 1250 >= 500
	}
	for _, tc := range cases {
		if got := Evaluate(party, tc.monsters).Difficulty; got != tc.want {
			t.Errorf("Evaluate(%+v) = %q, want %q", tc.monsters, got, tc.want)
		}
	}
}

// goblinGroup builds a group of CR 1/4 goblins (50 XP each per the Monster
// Manual stat block).
func goblinGroup(n int) []Monster {
	return []Monster{{Name: "Goblin", CR: "1/4", XP: 50, Count: n}}
}

func TestEvaluateDegenerateParties(t *testing.T) {
	// Empty party: thresholds stay zero and no difficulty is claimed.
	v := Evaluate(nil, []Monster{{Name: "Goblin", CR: "1/4", XP: 50, Count: 4}})
	if v.Difficulty != "" {
		t.Errorf("empty party difficulty = %q, want none", v.Difficulty)
	}
	if v.Thresholds[BandEasy] != 0 {
		t.Errorf("empty party easy threshold = %d, want 0", v.Thresholds[BandEasy])
	}
	// Single PC vs one goblin: party of one moves the multiplier up a rung.
	v = Evaluate([]int{1}, []Monster{{Name: "Goblin", CR: "1/4", XP: 50, Count: 1}})
	if v.Multiplier != 1.5 {
		t.Errorf("single PC multiplier = %v, want 1.5", v.Multiplier)
	}
	// A lone 20th-level character against an ancient red dragon (CR 24).
	v = Evaluate([]int{20}, []Monster{{Name: "Ancient Red Dragon", CR: "24", XP: 62000, Count: 1}})
	if v.Difficulty != BandDeadly {
		t.Errorf("dragon vs one PC difficulty = %q, want %q", v.Difficulty, BandDeadly)
	}
	// Huge counts must not overflow: 200 goblins at 50 XP.
	v = Evaluate([]int{5, 5, 5, 5}, []Monster{{Name: "Goblin", CR: "1/4", XP: 50, Count: 200}})
	if v.TotalXP != 10000 || v.AdjustedXP != int(math.Round(10000*multiplierFor(200, 4))) {
		t.Errorf("huge count XP wrong: %+v", v)
	}
	// Out-of-range levels are skipped rather than panicking.
	v = Evaluate([]int{0, 25, 3}, goblinGroup(1))
	if v.PartySize != 3 || v.Thresholds[BandEasy] != 75 {
		t.Errorf("out-of-range levels mishandled: %+v", v)
	}
}
