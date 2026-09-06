package sheet

// Quick rolls (MAD-420, stage 3 of MAD-417): the roll bar's one-tap
// formulas, derived from the sheet's own numbers — the Stage 1 dependency
// the dice stage names. Every formula is computed here, server-side, so
// no surface ever re-derives proficiency bonuses in client code and the
// quick-roll chips can never disagree with the sheet.

import (
	"fmt"
	"sort"
	"strconv"
)

// QuickRoll is one tappable line: a label for the chip and the formula
// the roller bar fills.
type QuickRoll struct {
	Label   string `json:"label"`
	Formula string `json:"formula"`
	Group   string `json:"group"` // checks | saves | skills | combat | spellcasting
}

// skillAbilities is the 2014 skill-to-ability map, keyed by the squashed
// vocabulary Proficiencies.Skills uses (homebrew.Abilities' spelling).
var skillAbilities = map[string]string{
	"acrobatics": "dex", "animalhandling": "wis", "arcana": "int",
	"athletics": "str", "deception": "cha", "history": "int",
	"insight": "wis", "intimidation": "cha", "investigation": "int",
	"medicine": "wis", "nature": "int", "perception": "wis",
	"performance": "cha", "persuasion": "cha", "religion": "int",
	"sleightofhand": "dex", "stealth": "dex", "survival": "wis",
}

var abilityOrder = []string{"str", "dex", "con", "int", "wis", "cha"}

var abilityNames = map[string]string{
	"str": "STR", "dex": "DEX", "con": "CON", "int": "INT", "wis": "WIS", "cha": "CHA",
}

// score reads one ability score, 0 when undeclared.
func (s Sheet) score(ab string) int {
	switch ab {
	case "str":
		return s.Abilities.STR
	case "dex":
		return s.Abilities.DEX
	case "con":
		return s.Abilities.CON
	case "int":
		return s.Abilities.INT
	case "wis":
		return s.Abilities.WIS
	case "cha":
		return s.Abilities.CHA
	}
	return 0
}

// Mod is one ability's modifier, (score-10)/2, 0 when the score is
// undeclared — a blank sheet rolls no better than no sheet.
func (s Sheet) Mod(ab string) int {
	if v := s.score(ab); v > 0 {
		return (v - 10) / 2
	}
	return 0
}

// ProficiencyBonus is the 2014 PB for the total level (1-4: 2 … 17-20:
// 6); a sheet with no levels has none, and quick rolls omit it rather
// than guess.
func (s Sheet) ProficiencyBonus() int {
	lvl := s.TotalLevel()
	if lvl < 1 {
		return 0
	}
	return 2 + (lvl-1)/4
}

// signed renders a modifier for a formula: "+5", "-1".
func signed(n int) string {
	if n < 0 {
		return strconv.Itoa(n)
	}
	return "+" + strconv.Itoa(n)
}

// labelWith renders "Stealth +7" style labels.
func labelWith(label string, mod int) string {
	return label + " " + signed(mod)
}

// QuickRolls derives the one-tap roll list from the sheet's own numbers:
// the six ability checks, proficient saves, proficient skills,
// initiative, and the spell attack. Only what the sheet declares — no
// invented proficiencies, no default skill list — and always in a stable
// order so the chip row does not reshuffle between reads.
func QuickRolls(s Sheet) []QuickRoll {
	var out []QuickRoll
	pb := s.ProficiencyBonus()

	for _, ab := range abilityOrder {
		if s.score(ab) == 0 {
			continue
		}
		out = append(out, QuickRoll{
			Label:   labelWith(abilityNames[ab]+" check", s.Mod(ab)),
			Formula: "1d20" + signed(s.Mod(ab)),
			Group:   "checks",
		})
	}

	if s.score("dex") > 0 {
		out = append(out, QuickRoll{
			Label:   labelWith("Initiative", s.Mod("dex")),
			Formula: "1d20" + signed(s.Mod("dex")),
			Group:   "combat",
		})
	}

	proficient := func(list []string, want string) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}
	for _, ab := range abilityOrder {
		if !proficient(s.Proficiencies.Saves, ab) {
			continue
		}
		mod := s.Mod(ab)
		if mod == 0 && s.score(ab) == 0 {
			continue // save proficiency without a score: nothing to roll yet
		}
		out = append(out, QuickRoll{
			Label:   labelWith(abilityNames[ab]+" save", mod+pb),
			Formula: "1d20" + signed(mod+pb),
			Group:   "saves",
		})
	}

	skills := append([]string(nil), s.Proficiencies.Skills...)
	sort.Strings(skills)
	for _, skill := range skills {
		ab, ok := skillAbilities[skill]
		if !ok {
			continue // not a 2014 skill; validation owns that complaint
		}
		mod := s.Mod(ab)
		out = append(out, QuickRoll{
			Label:   labelWith(titleize(skill), mod+pb),
			Formula: "1d20" + signed(mod+pb),
			Group:   "skills",
		})
	}

	if s.Spellcasting != nil && s.Spellcasting.AttackBonus != 0 {
		out = append(out, QuickRoll{
			Label:   labelWith("Spell attack", s.Spellcasting.AttackBonus),
			Formula: "1d20" + signed(s.Spellcasting.AttackBonus),
			Group:   "spellcasting",
		})
	}
	return out
}

// titleize turns a squashed skill name into its printed spelling:
// "sleightofhand" → "Sleight of Hand".
func titleize(squash string) string {
	words := map[string]string{
		"animalhandling": "Animal Handling", "sleightofhand": "Sleight of Hand",
	}
	if w, ok := words[squash]; ok {
		return w
	}
	if squash == "" {
		return ""
	}
	return fmt.Sprintf("%c%s", squash[0]-32, squash[1:])
}
