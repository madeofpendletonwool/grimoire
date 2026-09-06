package sheet

// QuickRolls (MAD-420) is a deterministic derivation from the sheet's own
// numbers: the exact chips the roll bar offers, computed here so no
// surface re-derives proficiency bonuses of its own. The test pins the
// PB curve, the save/skill proficiency math, and that nothing is invented
// for a sheet that declares nothing.

import "testing"

func quickFixture() Sheet {
	return Sheet{
		Classes: []ClassLevel{{Class: "rogue", Level: 5}},
		Abilities: Abilities{
			STR: 8, DEX: 17, CON: 14, INT: 12, WIS: 10, CHA: 13,
		},
		Proficiencies: Proficiencies{
			Saves:  []string{"dex", "int"},
			Skills: []string{"stealth", "perception", "acrobatics"},
		},
		Spellcasting: &Spellcasting{AttackBonus: 5, DC: 14},
	}
}

func TestQuickRollsDeriveFromTheSheet(t *testing.T) {
	s := quickFixture()
	rolls := QuickRolls(s)
	byLabel := map[string]QuickRoll{}
	for _, q := range rolls {
		byLabel[q.Label] = q
	}

	// PB 3 at rogue 5: DEX 17 (+3) saves +3 = 1d20+6; checks without PB.
	want := map[string]string{
		"STR check -1":    "1d20-1",
		"DEX check +3":    "1d20+3",
		"WIS check +0":    "1d20+0",
		"Initiative +3":   "1d20+3",
		"DEX save +6":     "1d20+6",
		"INT save +4":     "1d20+4", // INT 12 (+1) + PB 3
		"Stealth +6":      "1d20+6", // DEX +3 + PB 3
		"Perception +3":   "1d20+3", // WIS +0 + PB 3
		"Spell attack +5": "1d20+5",
	}
	for label, formula := range want {
		got, ok := byLabel[label]
		if !ok {
			t.Fatalf("no quick roll labeled %q; have %+v", label, rolls)
		}
		if got.Formula != formula {
			t.Errorf("%s = %s, want %s", label, got.Formula, formula)
		}
	}
	// A save the sheet is not proficient in is not offered.
	if _, ok := byLabel["WIS save +3"]; ok {
		t.Error("WIS save offered without proficiency")
	}
	if _, ok := byLabel["CHA save +4"]; ok {
		t.Error("CHA save offered without proficiency")
	}
}

func TestQuickRollsProficiencyCurve(t *testing.T) {
	cases := []struct {
		level, pb int
	}{
		{1, 2}, {4, 2}, {5, 3}, {8, 3}, {9, 4}, {12, 4},
		{13, 5}, {16, 5}, {17, 6}, {20, 6},
	}
	for _, c := range cases {
		s := Sheet{Classes: []ClassLevel{{Class: "fighter", Level: c.level}}}
		if got := s.ProficiencyBonus(); got != c.pb {
			t.Errorf("level %d: PB %d, want %d", c.level, got, c.pb)
		}
	}
	if (Sheet{}).ProficiencyBonus() != 0 {
		t.Error("a sheet with no levels has no PB — nothing to add")
	}
}

func TestQuickRollsInventNothing(t *testing.T) {
	// The zero sheet offers nothing — no guessed scores, no default skills.
	if rolls := QuickRolls(Sheet{}); len(rolls) != 0 {
		t.Fatalf("zero sheet offered %d quick rolls: %+v", len(rolls), rolls)
	}
	// Undeclared abilities contribute no checks; declared ones do.
	partial := QuickRolls(Sheet{Abilities: Abilities{DEX: 16}})
	if len(partial) != 2 { // DEX check + Initiative
		t.Fatalf("DEX-only sheet offered %+v", partial)
	}
	for _, q := range partial {
		if q.Formula != "1d20+3" {
			t.Errorf("%s = %s, want 1d20+3", q.Label, q.Formula)
		}
	}
	// A skill outside the 2014 map is validation's complaint, not a chip.
	odd := QuickRolls(Sheet{
		Abilities:     Abilities{STR: 10},
		Proficiencies: Proficiencies{Skills: []string{"spacemanship"}},
	})
	for _, q := range odd {
		if q.Group == "skills" {
			t.Errorf("invented a chip for a non-5e skill: %+v", q)
		}
	}
}
