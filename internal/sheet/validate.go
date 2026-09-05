package sheet

// Validation (MAD-418): the structural checks a sheet must pass before it is
// stored. This is the mirror image of the statblock machinery's grammar
// (MAD-379) and the homebrew linter's declared vocabularies (MAD-385): same
// rule, opposite direction — the linter reviews a generated design, the
// validator gates a stored definition. Both cite the game's own sets and
// name the window a value fell outside of, because an error that does not
// say what is wrong is a rejected sheet and a confused DM.
//
// Everything here is pure: same sheet in, same problems out, no model, no
// network. The vocabulary half of the contract is pinned by a golden file
// (testdata/vocab_golden.json) so a vocabulary change is a deliberate,
// visible commit, the same discipline the CR harness holds the parser to.

import (
	"fmt"

	"github.com/madeofpendletonwool/grimoire/internal/homebrew"
)

// Problem is one field of one sheet that validation refuses. Field is the
// dotted path into the sheet JSON ("abilities.str", "spellcasting.slots.3",
// "classes[1].level"); Detail says what is wrong and, where a vocabulary or
// window governs, names it.
type Problem struct {
	Field  string `json:"field"`
	Detail string `json:"detail"`
}

// Error is the sentinel WriteSheet paths report when validation failed; the
// problems themselves travel on it.
type Error struct{ Problems []Problem }

func (e *Error) Error() string {
	if len(e.Problems) == 0 {
		return "sheet validation failed"
	}
	return fmt.Sprintf("sheet validation failed: %s: %s", e.Problems[0].Field, e.Problems[0].Detail)
}

// windows the 2014 game itself bounds. Deliberately generous at the edges
// (epic boons exist; house rules exist) but finite: a validator that
// accepts anything is not a validator.
const (
	abilityMin, abilityMax = 1, 30
	acMin, acMax           = 1, 40
	hpMin, hpMax           = 1, 999
	speedMin, speedMax     = 1, 120
	levelMin, levelMax     = 1, 20
	dcMin, dcMax           = 5, 35
	atkMin, atkMax         = -10, 20
	slotMax                = 12
	qtyMax                 = 999
)

// Validate checks a normalized sheet and returns every problem found — one
// pass, all fields, so the DM fixes the whole sheet in one round trip. An
// empty sheet (nothing declared) is valid: it is the pc the DM has not
// written numbers for, the same state the party block always tolerated.
func Validate(s Sheet) []Problem {
	var probs []Problem
	bad := func(field, format string, args ...any) {
		probs = append(probs, Problem{Field: field, Detail: fmt.Sprintf(format, args...)})
	}

	if s.Version != 0 && s.Version != Version {
		bad("version", "sheet version %d is not supported; this build writes version %d", s.Version, Version)
	}

	for ab, v := range map[string]int{
		"str": s.Abilities.STR, "dex": s.Abilities.DEX, "con": s.Abilities.CON,
		"int": s.Abilities.INT, "wis": s.Abilities.WIS, "cha": s.Abilities.CHA,
	} {
		if v != 0 && (v < abilityMin || v > abilityMax) {
			bad("abilities."+ab, "ability score %d is outside %d-%d", v, abilityMin, abilityMax)
		}
	}
	if s.AC != 0 && (s.AC < acMin || s.AC > acMax) {
		bad("ac", "ac %d is outside %d-%d", s.AC, acMin, acMax)
	}
	if s.MaxHP != 0 && (s.MaxHP < hpMin || s.MaxHP > hpMax) {
		bad("max_hp", "max_hp %d is outside %d-%d", s.MaxHP, hpMin, hpMax)
	}
	if s.XP < 0 {
		bad("xp", "xp %d is negative", s.XP)
	}

	for k, v := range s.Speeds {
		if !inSet(k, homebrew.SpeedWords) {
			bad("speeds."+k, "%q is not a movement mode; the game's own set is %s", k, setList(homebrew.SpeedWords))
			continue
		}
		if v < speedMin || v > speedMax {
			bad("speeds."+k, "speed %d is outside %d-%d feet", v, speedMin, speedMax)
		}
	}

	for i, c := range s.Classes {
		if c.Class == "" {
			bad(fmt.Sprintf("classes[%d].class", i), "class name is required")
		}
		if c.Level < levelMin || c.Level > levelMax {
			bad(fmt.Sprintf("classes[%d].level", i), "class level %d is outside %d-%d", c.Level, levelMin, levelMax)
		}
	}
	seen := map[string]int{}
	for i, c := range s.Classes {
		if c.Class != "" {
			if j, dup := seen[c.Class]; dup {
				bad(fmt.Sprintf("classes[%d].class", i), "class %q is listed twice (entry %d); merge the levels into one entry", c.Class, j)
			} else {
				seen[c.Class] = i
			}
		}
	}
	if total := s.TotalLevel(); total > levelMax {
		bad("classes", "total level %d is above %d; no character in the 2014 game is", total, levelMax)
	}

	for i, save := range s.Proficiencies.Saves {
		if !inSet(save, homebrew.Abilities) {
			bad(fmt.Sprintf("proficiencies.saves[%d]", i), "%q is not an ability; the save vocabulary is %s", save, setList(homebrew.Abilities))
		}
	}
	for i, skill := range s.Proficiencies.Skills {
		if !inSet(skill, homebrew.Skills) {
			bad(fmt.Sprintf("proficiencies.skills[%d]", i), "%q is not a 5e skill; the skill vocabulary is %s", skill, setList(homebrew.Skills))
		}
	}

	for _, list := range []struct {
		field  string
		values []string
	}{
		{"resistances", s.Resistances},
		{"immunities", s.Immunities},
		{"vulnerabilities", s.Vulnerabilities},
	} {
		for i, v := range list.values {
			if !inSet(v, homebrew.DamageTypes) {
				bad(fmt.Sprintf("%s[%d]", list.field, i), "%q is not a damage type; the game's thirteen are %s", v, setList(homebrew.DamageTypes))
			}
		}
	}

	if sc := s.Spellcasting; sc != nil {
		if sc.Ability != "" && !inSet(sc.Ability, homebrew.Abilities) {
			bad("spellcasting.ability", "%q is not an ability; the casting vocabulary is %s", sc.Ability, setList(homebrew.Abilities))
		}
		if sc.DC != 0 && (sc.DC < dcMin || sc.DC > dcMax) {
			bad("spellcasting.dc", "save dc %d is outside %d-%d", sc.DC, dcMin, dcMax)
		}
		if sc.AttackBonus != 0 && (sc.AttackBonus < atkMin || sc.AttackBonus > atkMax) {
			bad("spellcasting.attack_bonus", "attack bonus %d is outside %d to +%d", sc.AttackBonus, atkMin, atkMax)
		}
		for lvl, n := range sc.Slots {
			if !inSet(lvl, slotLevels) {
				bad("spellcasting.slots."+lvl, "slot level %q is not one of 1-9", lvl)
				continue
			}
			if n < 0 || n > slotMax {
				bad("spellcasting.slots."+lvl, "%d slots is outside 0-%d", n, slotMax)
			}
		}
		for i, e := range sc.Known {
			if e.Name == "" {
				bad(fmt.Sprintf("spellcasting.known[%d].name", i), "spell name is required")
			}
		}
		for i, e := range sc.Prepared {
			if e.Name == "" {
				bad(fmt.Sprintf("spellcasting.prepared[%d].name", i), "spell name is required")
			}
		}
	}

	for i, it := range s.Inventory {
		if it.Name == "" {
			bad(fmt.Sprintf("inventory[%d].name", i), "item name is required")
		}
		if it.Qty < 0 || it.Qty > qtyMax {
			bad(fmt.Sprintf("inventory[%d].qty", i), "quantity %d is outside 0-%d", it.Qty, qtyMax)
		}
	}
	if attuned := len(s.AttunedItems()); attuned > s.AttunementLimit() {
		bad("inventory", "%d items are attuned but the limit is %d; unattune %d of them or raise attunement_max", attuned, s.AttunementLimit(), attuned-s.AttunementLimit())
	}
	if s.AttunementMax < 0 || s.AttunementMax > 10 {
		bad("attunement_max", "attunement_max %d is outside 0-10", s.AttunementMax)
	}

	if s.Currency.CP < 0 || s.Currency.SP < 0 || s.Currency.EP < 0 || s.Currency.GP < 0 || s.Currency.PP < 0 {
		bad("currency", "coin counts cannot be negative")
	}

	return probs
}

// Valid is Validate reduced to its verdict.
func Valid(s Sheet) bool { return len(Validate(s)) == 0 }

// slotLevels is the spell-slot key vocabulary, "1".."9", in order.
var slotLevels = []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}

func inSet(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

func setList(set []string) string {
	out := ""
	for i, v := range set {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

// Vocabulary is the golden-file shape: every declared set the validator
// enforces against, plus the numeric windows, rendered once so a change to
// any of them is a reviewable diff (testdata/vocab_golden.json).
type Vocabulary struct {
	Abilities       []string `json:"abilities"`
	Skills          []string `json:"skills"`
	DamageTypes     []string `json:"damage_types"`
	SpeedWords      []string `json:"speed_words"`
	SlotLevels      []string `json:"slot_levels"`
	AbilityWindow   [2]int   `json:"ability_window"`
	ACWindow        [2]int   `json:"ac_window"`
	HPWindow        [2]int   `json:"hp_window"`
	SpeedWindow     [2]int   `json:"speed_window"`
	LevelWindow     [2]int   `json:"level_window"`
	DCWindow        [2]int   `json:"dc_window"`
	AttackWindow    [2]int   `json:"attack_bonus_window"`
	AttunementFloor int      `json:"attunement_default"`
}

// VocabularyOf collects the enforced sets and windows in their golden order.
func VocabularyOf() Vocabulary {
	return Vocabulary{
		Abilities:       homebrew.Abilities,
		Skills:          homebrew.Skills,
		DamageTypes:     homebrew.DamageTypes,
		SpeedWords:      homebrew.SpeedWords,
		SlotLevels:      slotLevels,
		AbilityWindow:   [2]int{abilityMin, abilityMax},
		ACWindow:        [2]int{acMin, acMax},
		HPWindow:        [2]int{hpMin, hpMax},
		SpeedWindow:     [2]int{speedMin, speedMax},
		LevelWindow:     [2]int{levelMin, levelMax},
		DCWindow:        [2]int{dcMin, dcMax},
		AttackWindow:    [2]int{atkMin, atkMax},
		AttunementFloor: 3,
	}
}
