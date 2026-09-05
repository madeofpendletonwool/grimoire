package sheet

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
)

// The vocabulary golden (MAD-418): every declared set and numeric window
// the validator enforces, pinned to a file so a change to any of them is a
// deliberate, reviewable diff — the same discipline the CR harness holds
// the statblock arithmetic to. Regenerate with:
//
//	go test ./internal/sheet -run TestVocabularyGolden -update-golden

var updateGolden = flag.Bool("update-golden", false, "rewrite golden files")

const vocabGoldenPath = "testdata/vocab_golden.json"

func TestVocabularyGolden(t *testing.T) {
	got, err := json.MarshalIndent(VocabularyOf(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *updateGolden {
		if err := os.WriteFile(vocabGoldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(vocabGoldenPath)
	if err != nil {
		t.Fatalf("golden file missing; run with -update-golden: %v", err)
	}
	if string(want) != string(got) {
		t.Fatalf(`vocabulary drifted from the golden file.
This is expected when the game's declared sets or the numeric windows
changed on purpose — regenerate deliberately and describe the shift:

    go test ./internal/sheet -run TestVocabularyGolden -update-golden`)
	}
}

// Nonsense is rejected with errors that say what is wrong and name the
// window or vocabulary the value fell outside of — an actionable message,
// not a bare "invalid".
func TestValidateRejectsNonsense(t *testing.T) {
	cases := []struct {
		name  string
		sheet Sheet
		field string
		wants []string
	}{
		{
			name:  "ability outside the window",
			sheet: Sheet{Abilities: Abilities{STR: 42}},
			field: "abilities.str",
			wants: []string{"outside 1-30"},
		},
		{
			name:  "ac nonsense",
			sheet: Sheet{AC: 99},
			field: "ac",
			wants: []string{"outside 1-40"},
		},
		{
			name:  "hp nonsense",
			sheet: Sheet{MaxHP: -5},
			field: "max_hp",
		},
		{
			name:  "class level beyond twenty",
			sheet: Sheet{Classes: []ClassLevel{{Class: "wizard", Level: 25}}},
			field: "classes[0].level",
			wants: []string{"outside 1-20"},
		},
		{
			name: "total level beyond twenty",
			sheet: Sheet{Classes: []ClassLevel{
				{Class: "fighter", Level: 12}, {Class: "wizard", Level: 12},
			}},
			field: "classes",
			wants: []string{"total level 24 is above 20"},
		},
		{
			name: "duplicate class entries",
			sheet: Sheet{Classes: []ClassLevel{
				{Class: "fighter", Level: 4}, {Class: "fighter", Level: 4},
			}},
			field: "classes[1].class",
			wants: []string{"listed twice"},
		},
		{
			name:  "damage type not in the game",
			sheet: Sheet{Resistances: []string{"spooky"}},
			field: "resistances[0]",
			wants: []string{"thirteen"},
		},
		{
			name:  "save not an ability",
			sheet: Sheet{Proficiencies: Proficiencies{Saves: []string{"luck"}}},
			field: "proficiencies.saves[0]",
			wants: []string{"str, dex, con, int, wis, cha"},
		},
		{
			name:  "skill not a skill",
			sheet: Sheet{Proficiencies: Proficiencies{Skills: []string{"lpturntableism"}}},
			field: "proficiencies.skills[0]",
		},
		{
			name:  "movement mode unknown",
			sheet: Sheet{Speeds: map[string]int{"tunnel": 30}},
			field: "speeds.tunnel",
			wants: []string{"walk, fly, swim, burrow, climb"},
		},
		{
			name:  "slot level not one through nine",
			sheet: Sheet{Spellcasting: &Spellcasting{Slots: map[string]int{"11": 1}}},
			field: "spellcasting.slots.11",
			wants: []string{"1-9"},
		},
		{
			name:  "casting ability not an ability",
			sheet: Sheet{Spellcasting: &Spellcasting{Ability: "broodiness"}},
			field: "spellcasting.ability",
		},
		{
			name:  "save dc nonsense",
			sheet: Sheet{Spellcasting: &Spellcasting{DC: 60}},
			field: "spellcasting.dc",
		},
		{
			name: "attunement over the limit",
			sheet: Sheet{Inventory: []Item{
				{Name: "one", Attuned: true}, {Name: "two", Attuned: true},
				{Name: "three", Attuned: true}, {Name: "four", Attuned: true},
			}},
			field: "inventory",
			wants: []string{"limit is 3"},
		},
		{
			name:  "negative coin",
			sheet: Sheet{Currency: Currency{GP: -10}},
			field: "currency",
			wants: []string{"negative"},
		},
		{
			name:  "item without a name",
			sheet: Sheet{Inventory: []Item{{Qty: 2}}},
			field: "inventory[0].name",
		},
		{
			name:  "unknown version",
			sheet: Sheet{Version: 99},
			field: "version",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := Validate(tc.sheet)
			found := false
			for _, p := range problems {
				if p.Field != tc.field {
					continue
				}
				found = true
				for _, want := range tc.wants {
					if !contains(p.Detail, want) {
						t.Errorf("problem detail %q does not mention %q", p.Detail, want)
					}
				}
			}
			if !found {
				t.Fatalf("no problem on %s; got %+v", tc.field, problems)
			}
		})
	}
}

func TestValidateAcceptsTheHonestSheets(t *testing.T) {
	// The empty sheet is valid: it is the pc nobody has written numbers
	// for, and rejecting it would break "every field is optional".
	if !Valid(Sheet{}) {
		t.Fatal("the zero sheet must validate")
	}
	// A real one, and a multiclass caster, both clean.
	fighter := Sheet{
		Version:   Version,
		Abilities: Abilities{STR: 17, DEX: 10, CON: 16, INT: 8, WIS: 13, CHA: 12},
		AC:        18, MaxHP: 49, Speeds: map[string]int{"walk": 25},
		Proficiencies: Proficiencies{Saves: []string{"str", "con"}, Skills: []string{"athletics", "intimidation"}},
		Classes:       []ClassLevel{{Class: "fighter", Subclass: "champion", Level: 8}},
		Resistances:   []string{"poison"},
		Inventory:     []Item{{Name: "flame tongue warhammer", Attuned: true, Equipped: true}},
		Currency:      Currency{SP: 12, GP: 55},
	}
	if problems := Validate(fighter); len(problems) > 0 {
		t.Fatalf("honest sheet rejected: %+v", problems)
	}
	eldritch := Sheet{
		Version: Version,
		Classes: []ClassLevel{{Class: "fighter", Level: 8}, {Class: "wizard", Level: 2}},
		Spellcasting: &Spellcasting{
			Ability: "int", DC: 12, AttackBonus: 4,
			Slots:    map[string]int{"1": 3},
			Known:    []Entry{{Name: "fire bolt"}},
			Prepared: []Entry{{Name: "shield"}},
		},
	}
	if problems := Validate(eldritch); len(problems) > 0 {
		t.Fatalf("multiclass caster rejected: %+v", problems)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
