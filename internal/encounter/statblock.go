package encounter

import (
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

// Statblock flattens a mirrored creature into the pure shape the CR
// calculator reads. It re-runs the same deterministic parse toCreature used,
// so the two always agree: an action that parsed once parses again, and an
// action that did not stays explicitly unparsed with its prose intact.
func (c Creature) Statblock() statblock.Statblock {
	s := statblock.Statblock{
		Name:         c.Name,
		Size:         c.Size,
		Type:         c.Type,
		AC:           c.AC,
		HP:           c.HP,
		HitDice:      c.HitDice,
		Saves:        c.Saves,
		Skills:       c.Skills,
		ProfBonus:    c.ProfBonus,
		Resist:       damageTypes(c.Resist),
		Immune:       damageTypes(c.Immune),
		Vulnerable:   damageTypes(c.Vulnerable),
		Speeds:       c.Speeds,
		Legendary:    hasKind(c.Actions, "LEGENDARY_ACTION"),
		Lair:         c.LairAction,
		Spellcasting: c.Spellcasting,
	}
	if c.Abilities != nil {
		s.Abilities = *c.Abilities
	}
	for _, t := range c.Traits {
		// Traits ride as prose; the parser does not read them. The CR
		// arithmetic reads the regeneration rate out of the text and nothing
		// else — the DMG prices no other trait in isolation.
		s.Traits = append(s.Traits, statblock.Action{Name: t.Name, Desc: t.Desc})
	}
	for _, a := range c.Actions {
		sa := statblock.Action{Name: a.Name, Desc: a.Desc, Kind: a.Kind, Usage: a.Usage, Cost: a.Cost}
		if atk, ok := statblock.ParseAttack(a.Name, a.Desc); ok {
			sa.Parsed = true
			sa.Attack = atk
		} else {
			sa.Unparse = "prose names no mechanic the parser reads"
		}
		s.Actions = append(s.Actions, sa)
	}
	return s
}

// damageTypes splits a display string the API prints ("bludgeoning,
// piercing, and slashing from nonmagical attacks; cold") into clauses the
// CR arithmetic can classify. The semicolon groups stay whole: the books
// print "acid; bludgeoning, piercing, and slashing from nonmagical attacks"
// and only the b/p/s group prices.
func damageTypes(display string) []string {
	display = strings.TrimSpace(display)
	if display == "" {
		return nil
	}
	var out []string
	for _, group := range strings.Split(display, ";") {
		if g := strings.TrimSpace(group); g != "" {
			out = append(out, g)
		}
	}
	return out
}

func hasKind(actions []NamedText, kind string) bool {
	for _, a := range actions {
		if a.Kind == kind {
			return true
		}
	}
	return false
}
