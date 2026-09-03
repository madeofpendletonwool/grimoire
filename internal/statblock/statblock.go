// Package statblock models a monster statblock as data and computes its
// challenge rating the way the 2014 Dungeon Master's Guide prescribes
// (chapter 9, "Creating a Monster Stat Block", DMG p. 273–274) — in the open,
// with every adjustment named.
//
// The package is pure: no database, no network, no clock. Every input it
// needs arrives in a Statblock, every output leaves in a Rating, and every
// rule between them is a plain function a test can pin. The encounter
// catalog mirrors the SRD into this shape; the monster designer (and later
// the homebrew linter) feeds it hand-built statblocks and gets the same
// arithmetic an official statblock gets.
//
// Where the DMG is silent or its procedure fails to reproduce a printed CR,
// the code says so rather than quietly compensating: Rating carries a
// confidence, and docs/development/statblock.md states where the arithmetic
// disagrees with the printed SRD and why. The disagreement is a property of
// the source procedure, not a bug to tune away.
package statblock

import (
	"math"
	"strconv"
	"strings"
)

// Abilities is the six ability scores of a statblock.
type Abilities struct {
	Str int `json:"str"`
	Dex int `json:"dex"`
	Con int `json:"con"`
	Int int `json:"int"`
	Wis int `json:"wis"`
	Cha int `json:"cha"`
}

// Damage is one component of an attack's damage: the dice expression, the
// printed average, and the type. Most attacks carry one; some print a
// second after "plus".
type Damage struct {
	Dice string `json:"dice,omitempty"` // e.g. "2d6+5"
	Avg  int    `json:"avg"`            // the printed average, e.g. 12
	Type string `json:"type,omitempty"` // e.g. "slashing"
}

// Attack is one action parsed into structure. It exists so the CR
// arithmetic — and everything downstream of it — never has to read prose.
// A parsed Attack is complete in its own terms: an action the parser could
// not fully read is not returned at all (it stays unparsed with its prose
// intact), so a caller holding an Attack never half-trusts it.
//
// Kind is one of "melee", "ranged", "melee-or-ranged", "save" (damage
// delivered through a saving throw rather than an attack roll) and
// "multiattack" (the composite action; see Components). Area marks an
// effect that hits an area — a breath weapon, an explosion — which the DMG
// prices as striking two targets. Rider carries the on-hit text that
// follows the damage sentence ("...and the target must succeed on a DC 14
// Constitution saving throw or become diseased"), verbatim but trimmed.
type Attack struct {
	Name        string      `json:"name"`
	Kind        string      `json:"kind"`
	Area        bool        `json:"area,omitempty"`
	ToHit       int         `json:"to_hit,omitempty"`
	SaveDC      int         `json:"save_dc,omitempty"`
	SaveAbility string      `json:"save_ability,omitempty"` // "Wis", "Con", ...
	Reach       int         `json:"reach,omitempty"`        // feet, melee
	Range       int         `json:"range,omitempty"`        // feet, ranged normal range
	Targets     string      `json:"targets,omitempty"`      // "one target", "each creature in a 60-foot cone"
	Damage      []Damage    `json:"damage,omitempty"`
	Rider       string      `json:"rider,omitempty"`
	Components  []Component `json:"components,omitempty"` // multiattack only
}

// Parsed reports whether the attack carries any mechanical content. A
// multiattack parses to no damage of its own; everything else is only worth
// calling an Attack if it can hit or deal damage.
func (a Attack) Parsed() bool {
	if a.Kind == KindMultiattack {
		return len(a.Components) > 0
	}
	return a.Kind != "" || a.SaveDC > 0 || len(a.Damage) > 0
}

// Avg returns the attack's total average damage per use, doubling area
// effects when area is true — the DMG's rule that an area effect is
// assumed to catch two targets.
func (a Attack) Avg(areaDouble bool) int {
	total := 0
	for _, d := range a.Damage {
		total += d.Avg
	}
	if areaDouble && a.Area {
		total *= 2
	}
	return total
}

// Component is one part of a multiattack round. Most components name one
// action used Count times ("two with its claws"). When the prose offers a
// choice ("using Bladed Arm or Fiery Bolt in any combination"), Options
// carries the alternatives: the creature makes Count attacks choosing among
// them, and the calculator prices the strongest option — the damage the
// creature can actually deal.
type Component struct {
	Count   int      `json:"count"`
	Name    string   `json:"name,omitempty"`
	Options []string `json:"options,omitempty"`
}

// Action kinds. Deliberately a small, closed vocabulary: the CR arithmetic
// only distinguishes how damage arrives, not the flavour.
const (
	KindMelee         = "melee"
	KindRanged        = "ranged"
	KindMeleeOrRanged = "melee-or-ranged"
	KindSave          = "save"
	KindMultiattack   = "multiattack"
)

// Action is one action of a statblock: the prose as printed, plus the
// structured Attack the parser produced from it — or, when Parsed is false,
// nothing but the prose. This is the never-half-parsed contract: exactly
// one of the two states holds, and an unparsed action keeps its full text
// so nothing is silently dropped.
type Action struct {
	Name  string `json:"name"`
	Desc  string `json:"desc"`
	Kind  string `json:"kind,omitempty"`  // ACTION, LEGENDARY_ACTION, BONUS_ACTION, REACTION
	Usage string `json:"usage,omitempty"` // "recharge 5-6", "2/day", "at will"; empty = every round
	Cost  int    `json:"cost,omitempty"`  // legendary action cost, 1 when unset

	Parsed  bool   `json:"parsed"`
	Attack  Attack `json:"attack,omitempty"`
	Unparse string `json:"unparse,omitempty"` // why the prose was not parsed, when it wasn't
}

// Legendary reports whether the action is a legendary action.
func (a Action) Legendary() bool { return a.Kind == "LEGENDARY_ACTION" }

// Statblock is the machine-readable face of a monster: what the CR
// arithmetic needs and nothing it doesn't. It is deliberately a plain
// value — the encounter mirror fills it from the SRD, a designer fills it
// from a draft.
type Statblock struct {
	Name string `json:"name"`
	Size string `json:"size,omitempty"`
	// Type is the creature's SRD type word ("undead", "dragon") —
	// informational, the way Speeds and Senses are: the arithmetic never
	// reads it, the rendering and the homebrew overlay do.
	Type      string    `json:"type,omitempty"`
	AC        int       `json:"ac"`
	HP        int       `json:"hp"`
	HitDice   string    `json:"hit_dice,omitempty"` // fallback when HP is absent
	Abilities Abilities `json:"abilities,omitempty"`

	Saves     map[string]int `json:"saves,omitempty"`  // "str"..."cha" -> total bonus
	Skills    map[string]int `json:"skills,omitempty"` // squashed skill -> total bonus
	ProfBonus int            `json:"prof_bonus,omitempty"`

	Resist     []string `json:"resist,omitempty"` // damage types, lower case
	Immune     []string `json:"immune,omitempty"`
	Vulnerable []string `json:"vulnerable,omitempty"`

	Speeds map[string]int `json:"speeds,omitempty"`

	Legendary    bool `json:"legendary,omitempty"`
	Lair         bool `json:"lair,omitempty"`         // informational: lair actions are not priced
	Spellcasting bool `json:"spellcasting,omitempty"` // informational: spell damage is not parsed

	Traits  []Action `json:"traits,omitempty"`  // prose only; the parser does not read traits
	Actions []Action `json:"actions,omitempty"` // each parsed or explicitly unparsed
}

// Confidence grades how much the rating can be trusted.
//
//   - High: every action parsed or legibly not-an-attack, damage found,
//     defensive numbers present.
//   - Medium: the parse is complete but a known blind spot applies (the
//     statblock casts spells whose damage is not priced, or a multiattack
//     resolved only partially).
//   - Low: the parse was incomplete — some action prose could not be read —
//     or the statblock lacks the numbers the arithmetic needs. A low rating
//     is a diagnosis, not an answer.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Adjustment is one correction the arithmetic applied, with its reason. The
// DMG's procedure is a stack of small shifts; a rating that reports "your
// CR 7 boss computes to CR 4" is only useful when it shows the shifts.
type Adjustment struct {
	Kind   string `json:"kind"` // "resistance", "immunity", "vulnerability", "regeneration", "armor", "attack bonus"
	Detail string `json:"detail"`
	Value  int    `json:"value"` // effective-HP delta, or CR half-steps for the AC/AB shifts
}

// Rating is the full result of ComputeCR: the two halves the DMG averages,
// the numbers that produced them, every adjustment with its reason, and how
// much the whole thing can be trusted.
type Rating struct {
	Defensive   float64 `json:"defensive"` // effective CR from hit points and AC
	Offensive   float64 `json:"offensive"` // effective CR from damage per round and attack bonus
	CR          float64 `json:"cr"`        // final: the average of the two, snapped to a table CR
	Label       string  `json:"label"`     // the printed form of CR: "1/4", "2", "13"
	EffectiveHP int     `json:"effective_hp"`
	HP          int     `json:"hp"`
	AC          int     `json:"ac"`
	DPR         int     `json:"dpr"`
	AttackBonus int     `json:"attack_bonus"` // the attack bonus (or save DC - 8) priced
	SaveBased   bool    `json:"save_based"`   // the priced attack uses a save DC

	Adjustments []Adjustment `json:"adjustments,omitempty"`
	Notes       []string     `json:"notes,omitempty"`
	Confidence  Confidence   `json:"confidence"`
}

/* ---------- the DMG tables ---------- */

// crLevels are the challenge ratings the Monster Manual prints, in order.
// Every effective CR the arithmetic produces is snapped to the nearest of
// these before it is labelled.
var crLevels = []float64{0, 0.125, 0.25, 0.5, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10,
	11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30}

// defensiveHP is, per crLevels entry, the lowest effective hit points that
// earns that defensive CR (DMG p. 274, "Challenge Rating by Effective Hit
// Points and Armor Class": 1–6 → 0, 7–35 → 1/8, ... 806–850 → 30).
var defensiveHP = []int{1, 7, 36, 50, 71, 86, 101, 116, 131, 146, 161, 176,
	191, 206, 221, 236, 251, 266, 281, 296, 311, 326, 341, 356, 401, 446,
	491, 536, 581, 626, 671, 716, 761, 806}

// defensiveAC is the assumed armor class for each CR on the same table.
var defensiveAC = []int{13, 13, 13, 13, 13, 13, 13, 13, 15, 15, 15, 16, 16,
	17, 17, 17, 18, 18, 18, 18, 19, 19, 19, 19, 19, 19, 21, 21, 21, 21, 21,
	21, 21, 21}

// offensiveDPR is, per crLevels entry, the lowest damage per round that
// earns that offensive CR (DMG p. 274, "Challenge Rating by Effective
// Damage Per Round and Attack Bonus").
var offensiveDPR = []int{0, 2, 4, 6, 9, 15, 21, 27, 33, 39, 45, 51, 57, 63,
	69, 75, 81, 87, 93, 99, 105, 111, 117, 123, 141, 159, 177, 195, 213,
	231, 249, 267, 285, 303}

// offensiveAB is the assumed attack bonus for each CR on the same table.
var offensiveAB = []int{3, 3, 3, 3, 3, 3, 3, 4, 4, 4, 4, 4, 5, 5, 6, 6, 6,
	6, 7, 7, 7, 7, 8, 8, 9, 9, 9, 9, 10, 10, 10, 10, 10, 10}

// offensiveDC is the assumed save DC for each CR on the same table — the
// column a save-based attacker is read against instead of the attack bonus.
var offensiveDC = []int{13, 13, 13, 13, 13, 13, 13, 14, 14, 14, 14, 15, 15,
	15, 16, 16, 16, 16, 17, 17, 17, 17, 18, 18, 18, 19, 19, 19, 19, 20, 20,
	20, 21, 21}

/* ---------- table arithmetic ---------- */

// Level reports the table index of a printed CR value.
func Level(cr float64) int {
	for i, v := range crLevels {
		if v == cr {
			return i
		}
	}
	return -1
}

// Label renders a challenge rating the way the books print it: 0.125 ->
// "1/8", 0.25 -> "1/4", 0.5 -> "1/2", whole numbers bare. It mirrors the
// encounter package's CRLabel — the pure package cannot import the mirror,
// so the three line-function lives here too.
func Label(cr float64) string {
	switch cr {
	case 0.125:
		return "1/8"
	case 0.25:
		return "1/4"
	case 0.5:
		return "1/2"
	}
	if cr == math.Trunc(cr) {
		return strconv.Itoa(int(cr))
	}
	return strconv.FormatFloat(cr, 'f', -1, 64)
}

// snapCR rounds an effective CR to the nearest printed CR value. Halves
// round down: the arithmetic errs against the monster, never for it.
func snapCR(cr float64) float64 {
	if cr <= 0 {
		return 0
	}
	if cr >= crLevels[len(crLevels)-1] {
		return crLevels[len(crLevels)-1]
	}
	for i, v := range crLevels {
		if v >= cr {
			prev := crLevels[i-1]
			if v-cr <= cr-prev {
				return v
			}
			return prev
		}
	}
	return crLevels[len(crLevels)-1]
}

// crFromHP reads the defensive CR index for an effective hit-point total.
func crFromHP(hp int) int {
	idx := 0
	for i, min := range defensiveHP {
		if hp >= min {
			idx = i
		}
	}
	return idx
}

// crFromDPR reads the offensive CR index for a damage-per-round figure.
func crFromDPR(dpr int) int {
	idx := 0
	for i, min := range offensiveDPR {
		if dpr >= min {
			idx = i
		}
	}
	return idx
}

// modifier renders a D&D ability modifier: floor((score - 10) / 2).
func modifier(score int) int {
	return int(math.Floor(float64(score-10) / 2))
}

// ProficiencyFor derives the proficiency bonus the DMG's Monster Statistics
// table assigns to a challenge rating (+2 up to CR 4, +3 at 5–8, +4 at
// 9–12, and so on). Open5e leaves the field null on the 2014 SRD, so the
// bridge derives it from the printed CR when the API does not carry it.
func ProficiencyFor(cr float64) int {
	switch {
	case cr < 5:
		return 2
	case cr < 9:
		return 3
	case cr < 13:
		return 4
	case cr < 17:
		return 5
	case cr < 21:
		return 6
	case cr < 25:
		return 7
	default:
		return 8
	}
}

// hitDiceAvg returns the average hit points a hit-dice expression implies,
// the way the books print averages: floor of the expected roll. "18d10+36"
// -> 135. A "" or unreadable expression returns 0.
func hitDiceAvg(expr string) int {
	expr = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(expr)), " ", "")
	if expr == "" {
		return 0
	}
	bonus := 0
	if i := strings.IndexAny(expr, "+-"); i >= 0 {
		b, err := strconv.Atoi(expr[i:])
		if err != nil {
			return 0
		}
		bonus = b
		expr = expr[:i]
	}
	parts := strings.SplitN(expr, "d", 2)
	if len(parts) != 2 {
		return 0
	}
	n, err1 := strconv.Atoi(parts[0])
	sides, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || n <= 0 || n > 100 || sides <= 0 {
		return 0
	}
	avg := float64(n) * float64(sides+1) / 2
	return int(math.Floor(avg)) + bonus
}
