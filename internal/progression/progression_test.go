package progression

// The leveling tables' tests (MAD-424): every constant pinned against the
// 2014 PHB. These tests are the oracle — the tables are data, and a wrong
// number fails here before it ever fails a character.

import (
	"reflect"
	"testing"
)

func TestXPForLevelSpotValues(t *testing.T) {
	cases := []struct {
		level int
		xp    int
	}{
		{1, 0}, {2, 300}, {3, 900}, {4, 2700}, {5, 6500},
		{6, 14000}, {7, 23000}, {8, 34000}, {9, 48000}, {10, 64000},
		{11, 85000}, {12, 100000}, {13, 120000}, {14, 140000}, {15, 165000},
		{16, 195000}, {17, 225000}, {18, 265000}, {19, 305000}, {20, 355000},
	}
	for _, c := range cases {
		got, err := XPForLevel(c.level)
		if err != nil {
			t.Fatalf("XPForLevel(%d): %v", c.level, err)
		}
		if got != c.xp {
			t.Errorf("XPForLevel(%d) = %d, want %d", c.level, got, c.xp)
		}
	}
	if _, err := XPForLevel(0); err == nil {
		t.Error("XPForLevel(0) should be an error")
	}
	if _, err := XPForLevel(21); err == nil {
		t.Error("XPForLevel(21) should be an error")
	}
}

func TestLevelForXP(t *testing.T) {
	cases := []struct {
		xp    int
		level int
	}{
		{0, 1}, {299, 1}, {300, 2}, {899, 2}, {900, 3}, {6499, 4},
		{6500, 5}, {355000, 20}, {999999, 20}, {-5, 1},
	}
	for _, c := range cases {
		if got := LevelForXP(c.xp); got != c.level {
			t.Errorf("LevelForXP(%d) = %d, want %d", c.xp, got, c.level)
		}
	}
}

func TestXPToNext(t *testing.T) {
	if need, ok := XPToNext(4, 2700); !ok || need != 3800 {
		t.Errorf("XPToNext(4, 2700) = %d, %v; want 3800, true", need, ok)
	}
	if need, ok := XPToNext(4, 6500); !ok || need != 0 {
		t.Errorf("XPToNext(4, 6500) = %d, %v; want 0, true (eligible)", need, ok)
	}
	if _, ok := XPToNext(20, 999999); ok {
		t.Error("XPToNext(20, ...) should report no next level")
	}
}

func TestCanonical(t *testing.T) {
	for _, c := range []string{"wizard", "Wizard", " WIZARD ", "Ranger", "Fighter"} {
		if _, err := Canonical(c); err != nil {
			t.Errorf("Canonical(%q): %v", c, err)
		}
	}
	if _, err := Canonical("blood hunter"); err == nil {
		t.Error("a homebrew class should be an error, never a guess")
	}
	if got, _ := Canonical("Paladin"); got != "paladin" {
		t.Errorf("Canonical(Paladin) = %q, want paladin", got)
	}
}

func TestClassRoster(t *testing.T) {
	if len(Classes) != 12 {
		t.Fatalf("the 2014 roster is twelve classes, got %d", len(Classes))
	}
	if got := ClassNames(); len(got) != 12 {
		t.Errorf("ClassNames() = %v", got)
	}
}

func TestHitDiceAndSaves(t *testing.T) {
	cases := map[string]struct {
		die   int
		saves []string
	}{
		"barbarian": {12, []string{"str", "con"}},
		"bard":      {8, []string{"dex", "cha"}},
		"cleric":    {8, []string{"wis", "cha"}},
		"druid":     {8, []string{"int", "wis"}},
		"fighter":   {10, []string{"str", "con"}},
		"monk":      {8, []string{"str", "dex"}},
		"paladin":   {10, []string{"wis", "cha"}},
		"ranger":    {10, []string{"str", "dex"}},
		"rogue":     {8, []string{"dex", "int"}},
		"sorcerer":  {6, []string{"con", "cha"}},
		"warlock":   {8, []string{"wis", "cha"}},
		"wizard":    {6, []string{"int", "wis"}},
	}
	for name, c := range cases {
		cl := Classes[name]
		if cl.HitDie != c.die {
			t.Errorf("%s hit die = %d, want %d", name, cl.HitDie, c.die)
		}
		if !reflect.DeepEqual(cl.Saves, c.saves) {
			t.Errorf("%s saves = %v, want %v", name, cl.Saves, c.saves)
		}
	}
}

func TestCastingKinds(t *testing.T) {
	cases := map[string]string{
		"bard": CastingFull, "cleric": CastingFull, "druid": CastingFull,
		"sorcerer": CastingFull, "wizard": CastingFull,
		"paladin": CastingHalf, "ranger": CastingHalf,
		"warlock":   CastingPact,
		"barbarian": CastingNone, "fighter": CastingNone,
		"monk": CastingNone, "rogue": CastingNone,
	}
	for name, want := range cases {
		if got := Classes[name].Casting; got != want {
			t.Errorf("%s casting = %q, want %q", name, got, want)
		}
	}
}

func TestFullCasterSlots(t *testing.T) {
	// The five full casters share one table; pin it at the levels a
	// golden diff will exercise plus the boundaries.
	cases := []struct {
		level int
		want  map[int]int
	}{
		{1, map[int]int{1: 2}},
		{5, map[int]int{1: 4, 2: 3, 3: 2}},
		{9, map[int]int{1: 4, 2: 3, 3: 3, 4: 3, 5: 1}},
		{17, map[int]int{1: 4, 2: 3, 3: 3, 4: 3, 5: 2, 6: 1, 7: 1, 8: 1, 9: 1}},
		{20, map[int]int{1: 4, 2: 3, 3: 3, 4: 3, 5: 3, 6: 2, 7: 2, 8: 1, 9: 1}},
	}
	for _, caster := range []string{"bard", "cleric", "druid", "sorcerer", "wizard"} {
		for _, c := range cases {
			if got := SlotsAt(caster, c.level); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s slots at %d = %v, want %v", caster, c.level, got, c.want)
			}
		}
	}
}

func TestHalfCasterSlots(t *testing.T) {
	// The paladin starts one level late and trails by one slot through
	// 4th; from 5th the two tables are identical.
	if got := SlotsAt("paladin", 1); len(got) != 0 {
		t.Errorf("paladin level 1 carries no slots, got %v", got)
	}
	if got := SlotsAt("paladin", 2); !reflect.DeepEqual(got, map[int]int{1: 2}) {
		t.Errorf("paladin slots at 2 = %v", got)
	}
	if got := SlotsAt("ranger", 4); !reflect.DeepEqual(got, map[int]int{1: 4}) {
		t.Errorf("ranger slots at 4 = %v", got)
	}
	for lvl := 5; lvl <= 20; lvl++ {
		p, r := SlotsAt("paladin", lvl), SlotsAt("ranger", lvl)
		if !reflect.DeepEqual(p, r) {
			t.Errorf("paladin and ranger tables diverge at %d: %v vs %v", lvl, p, r)
		}
	}
	if got := SlotsAt("ranger", 19); !reflect.DeepEqual(got, map[int]int{1: 4, 2: 3, 3: 3, 4: 3, 5: 1}) {
		t.Errorf("ranger slots at 19 = %v", got)
	}
}

func TestPactSlots(t *testing.T) {
	cases := []struct {
		level     int
		count     int
		slotLevel int
	}{
		{1, 1, 1}, {2, 2, 1}, {3, 2, 2}, {5, 2, 3}, {9, 2, 5},
		{11, 3, 5}, {17, 4, 5}, {20, 4, 5},
	}
	for _, c := range cases {
		count, lvl := PactSlotsAt("warlock", c.level)
		if count != c.count || lvl != c.slotLevel {
			t.Errorf("pact slots at warlock %d = %d x %d, want %d x %d",
				c.level, count, lvl, c.count, c.slotLevel)
		}
	}
	if count, lvl := PactSlotsAt("wizard", 5); count != 0 || lvl != 0 {
		t.Errorf("a wizard has no pact slots, got %d x %d", count, lvl)
	}
}

func TestFeaturesAt(t *testing.T) {
	if got := FeaturesAt("wizard", 1); !reflect.DeepEqual(got, []string{"Arcane Recovery", "Spellcasting"}) {
		t.Errorf("wizard level 1 features = %v", got)
	}
	// ASI rides along on the schedule.
	got := FeaturesAt("wizard", 4)
	if len(got) != 1 || got[0] != "Ability Score Improvement" {
		t.Errorf("wizard level 4 features = %v, want the ASI alone", got)
	}
	// The fighter's denser schedule and the rogue's.
	if got := FeaturesAt("fighter", 6); len(got) != 1 || got[0] != "Ability Score Improvement" {
		t.Errorf("fighter level 6 features = %v, want the ASI alone", got)
	}
	if got := FeaturesAt("rogue", 10); len(got) != 1 || got[0] != "Ability Score Improvement" {
		t.Errorf("rogue level 10 features = %v, want the ASI alone", got)
	}
	if got := FeaturesAt("barbarian", 5); !reflect.DeepEqual(got, []string{"Extra Attack", "Fast Movement"}) {
		t.Errorf("barbarian level 5 features = %v", got)
	}
	if got := FeaturesAt("bard", 5); !reflect.DeepEqual(got, []string{"Bardic Inspiration (d8)", "Font of Inspiration"}) {
		t.Errorf("bard level 5 features = %v", got)
	}
	// A level with nothing gains nothing.
	if got := FeaturesAt("wizard", 7); len(got) != 0 {
		t.Errorf("wizard level 7 features = %v, want none", got)
	}
}

func TestASISchedules(t *testing.T) {
	if got := ASILevels["fighter"]; !reflect.DeepEqual(got, []int{4, 6, 8, 12, 14, 16, 19}) {
		t.Errorf("fighter ASI schedule = %v", got)
	}
	if got := ASILevels["rogue"]; !reflect.DeepEqual(got, []int{4, 8, 10, 12, 16, 19}) {
		t.Errorf("rogue ASI schedule = %v", got)
	}
	for name, levels := range ASILevels {
		if len(levels) == 0 {
			t.Errorf("%s has no ASI schedule", name)
		}
		for _, l := range levels {
			if l < 4 || l > 19 {
				t.Errorf("%s ASI at %d is outside 4..19", name, l)
			}
		}
	}
}

func TestHitPointsGained(t *testing.T) {
	cases := []struct {
		die, con, want int
	}{
		{6, 0, 4}, {8, 0, 5}, {10, 0, 6}, {12, 0, 7},
		{6, 3, 7}, {8, -1, 4}, {10, 2, 8}, {12, -2, 5},
	}
	for _, c := range cases {
		if got := HitPointsGained(c.die, c.con); got != c.want {
			t.Errorf("HitPointsGained(%d, %+d) = %d, want %d", c.die, c.con, got, c.want)
		}
	}
}

func TestSlotTablesComplete(t *testing.T) {
	// Every caster table covers exactly the levels it should, once each.
	for _, name := range []string{"bard", "cleric", "druid", "sorcerer", "wizard", "warlock", "ranger"} {
		seen := map[int]bool{}
		for _, r := range Classes[name].Slots {
			if seen[r.Level] {
				t.Errorf("%s slot table has two rows at level %d", name, r.Level)
			}
			seen[r.Level] = true
		}
		for l := 1; l <= 20; l++ {
			if !seen[l] {
				t.Errorf("%s slot table is missing level %d", name, l)
			}
		}
	}
	seen := map[int]bool{}
	for _, r := range Classes["paladin"].Slots {
		if seen[r.Level] {
			t.Errorf("paladin slot table has two rows at level %d", r.Level)
		}
		seen[r.Level] = true
	}
	for l := 2; l <= 20; l++ {
		if !seen[l] {
			t.Errorf("paladin slot table is missing level %d", l)
		}
	}
	if seen[1] {
		t.Error("paladin slot table should not have a level-1 row")
	}
}
