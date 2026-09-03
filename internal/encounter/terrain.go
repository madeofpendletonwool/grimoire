package encounter

// Terrain and hazards make the objective true. A `reach` objective on a flat
// empty room is a footrace; the ground the fight happens on is generated
// *with* the objective, not decorated on afterwards.
//
// Everything here is a declared vocabulary: the server picks which features
// and hazards the fight carries, and every one of them declares what it does
// mechanically — so the tactical analysis issue can reason over the values
// rather than re-read prose. The model names and describes what it was
// handed; it never sets a number. Hazards are priced the way the DMG prices
// traps, by tier, which is the part that keeps them honest.
//
// Pure, like the rest of the package: no clock, no network, no randomness.

import (
	"fmt"
	"regexp"
	"strings"
)

// Terrain is the structured battlefield an encounter is generated with.
// Features change how the fight moves; hazards hurt on a save. Both are
// structured values, never prose.
type Terrain struct {
	Features []Feature `json:"features,omitempty"`
	Hazards  []Hazard  `json:"hazards,omitempty"`
}

// Feature is one piece of terrain: its kind from the declared vocabulary,
// the mechanical effect the kind carries, and where on the board it sits.
type Feature struct {
	Kind   string `json:"kind"`
	Effect string `json:"effect"` // what it does mechanically, in the game's own words
	Area   string `json:"area"`   // where it sits relative to the objective
}

// Hazard is one dangerous piece of the battlefield, priced the way the DMG
// prices traps: a save DC and a damage expression that fall inside the
// party tier's band.
type Hazard struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	SaveAbility string `json:"save_ability"` // str | dex | con | int | wis | cha
	DC          int    `json:"dc"`
	Damage      string `json:"damage"`      // a dice expression, e.g. "4d10"
	DamageType  string `json:"damage_type"` // e.g. "bludgeoning"
	Trigger     string `json:"trigger"`     // when the save is called for
	Area        string `json:"area"`        // where on the board it bites
}

// featureEffects is the declared vocabulary of terrain features. The key is
// the kind; the value is what the kind does mechanically. A feature without
// an effect here cannot be generated, and one that arrives from outside
// without a matching kind is refused by Validate — the mechanical effect is
// never free prose.
var featureEffects = map[string]string{
	"cover":            "half cover while behind it: +2 AC and +2 Dexterity saving throws against attacks and effects that cross it",
	"elevation":        "ranged attackers up there ignore half cover and can see the whole board; reaching them means the climb",
	"difficult_ground": "every foot of movement through it costs 2 feet",
	"chokepoint":       "one creature wide: whoever holds it decides who crosses",
	"concealment":      "lightly obscured: disadvantage on Wisdom (Perception) checks to see across it",
	"water":            "waist-deep: difficult ground, and a DC 10 Strength (Athletics) check to swim rather than wade when pushed",
	"darkness":         "heavily obscured: only darkvision sees across it — everyone else is blinded beyond 5 feet",
}

// featureLabels renders each feature kind the way the readout does.
var featureLabels = map[string]string{
	"cover":            "Cover",
	"elevation":        "Elevation",
	"difficult_ground": "Difficult ground",
	"chokepoint":       "Chokepoint",
	"concealment":      "Concealment",
	"water":            "Water",
	"darkness":         "Darkness",
}

// hazardKinds is the declared vocabulary of hazards. Each kind fixes the
// save, the damage type and the trigger; the tier tables below fix the DC
// and the dice. Nothing about a hazard is improvised.
var hazardKinds = map[string]struct {
	label, save, damageType, trigger string
}{
	"rockfall": {"Rockfall", "dex", "bludgeoning", "a creature starts its turn under the failing span"},
	"fire":     {"Fire on the ground", "dex", "fire", "a creature ends its turn in the burning ground"},
	"spikes":   {"Spikes", "dex", "piercing", "a creature falls or is pushed in"},
	"surge":    {"Arcane surge", "con", "force", "a creature starts its turn inside it"},
}

// saveAbilities is the six the game has; a hazard save outside it is
// structurally invalid.
var saveAbilities = map[string]bool{
	"str": true, "dex": true, "con": true, "int": true, "wis": true, "cha": true,
}

// trapDCs is the DMG's Trap Save DCs table (chapter 5): rows are the level
// tiers — 1-4, 5-10, 11-16, 17-20 — and the columns are the easy, moderate
// and hard DCs the book prints for each.
var trapDCs = [4][3]int{
	{10, 15, 20},
	{11, 16, 21},
	{12, 17, 22},
	{13, 18, 23},
}

// trapDamage is the DMG's Damage Severity by Level table, same tiers, with
// the book's setback, dangerous and deadly damage columns.
var trapDamage = [4][3]string{
	{"1d10", "2d10", "4d10"},
	{"2d10", "4d10", "10d10"},
	{"4d10", "10d10", "18d10"},
	{"10d10", "18d10", "24d10"},
}

// hazardSeverity maps an encounter band onto the DMG trap columns: the DC
// column reads easy/moderate/hard and the damage column setback/dangerous/
// deadly, so an easy fight's hazards bruise and a deadly one's maim. Hard
// and Deadly share the hardest column — the table has no fifth step.
func hazardSeverity(band string) int {
	switch band {
	case BandEasy:
		return 0
	case BandMedium:
		return 1
	default:
		return 2
	}
}

// hazardTier maps the party's average level onto the DMG's trap tiers.
func hazardTier(avgLevel float64) int {
	switch {
	case avgLevel >= 17:
		return 3
	case avgLevel >= 11:
		return 2
	case avgLevel >= 5:
		return 1
	default:
		return 0
	}
}

// diceRE is the grammar a hazard's damage expression must speak: NdN with an
// optional flat modifier, in the dice the game actually has.
var diceRE = regexp.MustCompile(`^\d{1,2}d(4|6|8|10|12|20)([+-]\d{1,3})?$`)

// TerrainFor generates the battlefield an objective is true on: the feature
// kinds the objective needs, each with its declared mechanical effect and
// its place on the board, and — where the objective's story calls for one —
// a hazard priced from the DMG's tier tables for this party and band. The
// same objective, party and band always generate the same battlefield.
func TerrainFor(o Objective, band string, avgLevel float64) Terrain {
	var t Terrain
	place := func(kind, area string) {
		t.Features = append(t.Features, Feature{Kind: kind, Effect: featureEffects[kind], Area: area})
	}
	hazard := func(kind, area string) {
		k := hazardKinds[kind]
		tier := hazardTier(avgLevel)
		sev := hazardSeverity(band)
		t.Hazards = append(t.Hazards, Hazard{
			Kind:        kind,
			Name:        k.label,
			SaveAbility: k.save,
			DC:          trapDCs[tier][sev],
			Damage:      trapDamage[tier][sev],
			DamageType:  k.damageType,
			Trigger:     k.trigger,
			Area:        area,
		})
	}

	switch o.Kind {
	case Survive:
		// The party holds; the waves cross ground that slows and hides them.
		place("chokepoint", "the ground the party holds")
		place("difficult_ground", "the approaches the waves cross")
		place("concealment", "where the waves come from")
	case Reach:
		// The crossing itself is the fight: slow ground, one gap, a span
		// coming down.
		place("difficult_ground", "the crossing")
		place("chokepoint", "the gap on the far side")
		hazard("rockfall", "the stretch the crossing passes under")
	case Protect:
		// A line, cover for what is protected, and burning ground in front
		// of it.
		place("cover", "behind the thing being protected")
		place("chokepoint", "the approach to it")
		place("difficult_ground", "the ground in front of the line")
		hazard("fire", "the ground around the thing being protected")
	case Stop:
		// The clock stands somewhere the party has to find and reach, and
		// it bites.
		place("concealment", "behind which the clock stands")
		place("cover", "where the clock's keepers hold")
		hazard("surge", "the circle around the clock")
	case Retrieve:
		// The prize is hidden, up, and trapped.
		place("concealment", "where the prize rests")
		place("elevation", "above the prize")
		hazard("spikes", "the approach to the prize")
	case Escape:
		// The way out is dark, slow, and wet. The pursuit is the hazard;
		// the ground is not also out to kill the party twice.
		place("darkness", "the way out")
		place("difficult_ground", "the route")
		place("water", "the flooded stretch")
	case Defeat, "":
		// No objective, no terrain: the fight the builder has always made.
		return Terrain{}
	}
	return t
}

// Validate rejects terrain that does not speak the declared vocabulary.
// Every feature kind must carry its mechanical effect, every hazard must be
// saveable, damaging and placed in the game's own grammar — a structured
// value, not prose.
func (t Terrain) Validate() error {
	if len(t.Features) > len(featureEffects) {
		return fmt.Errorf("%w: terrain is limited to %d features", ErrInvalid, len(featureEffects))
	}
	for _, f := range t.Features {
		if _, ok := featureEffects[f.Kind]; !ok {
			return fmt.Errorf("%w: unknown terrain feature kind %q", ErrInvalid, f.Kind)
		}
	}
	if len(t.Hazards) > len(hazardKinds) {
		return fmt.Errorf("%w: terrain is limited to %d hazards", ErrInvalid, len(hazardKinds))
	}
	for _, h := range t.Hazards {
		k, ok := hazardKinds[h.Kind]
		if !ok {
			return fmt.Errorf("%w: unknown hazard kind %q", ErrInvalid, h.Kind)
		}
		if !saveAbilities[h.SaveAbility] {
			return fmt.Errorf("%w: hazard %q saves on unknown ability %q", ErrInvalid, k.label, h.SaveAbility)
		}
		if h.DC < 1 || h.DC > 30 {
			return fmt.Errorf("%w: hazard %q DC %d is outside the game's range", ErrInvalid, k.label, h.DC)
		}
		if !diceRE.MatchString(h.Damage) {
			return fmt.Errorf("%w: hazard %q damage %q is not a dice expression", ErrInvalid, k.label, h.Damage)
		}
		if strings.TrimSpace(h.Trigger) == "" || strings.TrimSpace(h.Area) == "" {
			return fmt.Errorf("%w: hazard %q needs a trigger and an area", ErrInvalid, k.label)
		}
		if h.DamageType == "" {
			return fmt.Errorf("%w: hazard %q needs a damage type", ErrInvalid, k.label)
		}
	}
	return nil
}

// FeatureLabel renders a feature kind for the readout; unknown kinds render
// as themselves.
func FeatureLabel(kind string) string {
	if l, ok := featureLabels[kind]; ok {
		return l
	}
	return kind
}
