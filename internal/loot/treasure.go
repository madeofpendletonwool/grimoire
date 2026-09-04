package loot

// The DMG's treasure arithmetic (MAD-384). This file is the arithmetic
// floor the issue asks for: the amount of coin, gems and art objects is
// the book's, verbatim, and only the choice of magic items is
// campaign-aware.
//
// Source: Dungeon Master's Guide (2014) p.133, "Treasure Hoards" — the
// four hoard tables by challenge band, transcribed whole. The SRD corpus
// Grimoire mirrors does not carry these tables, so they are declared here
// as data, the way internal/encounter declares the DMG's encounter
// multipliers and internal/statblock declares the DMG p.274 procedure.
// Every band a test asserts is computed from this table, not hardcoded
// beside it.
//
// One deliberate adaptation, stated rather than hidden: the DMG's magic
// item tables A–I are lists of specific items Grimoire's corpus has no
// membership for. Each table here rides its printed rarity profile
// (A–E are the DMG's minor tables, F–I its major — the split XGE p.135
// names explicitly), and the actual pick comes from the item catalog.
// treasure.go owns only the profile; hoard.go owns the pick.

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
)

/* ---------- tiers ---------- */

// Tier is one of the game's four tiers of play. A tier is a level band
// first (1-4, 5-10, 11-16, 17-20) and a treasure band second: every hoard
// table in this file is keyed by it.
type Tier int

const (
	Tier1 Tier = 1
	Tier2 Tier = 2
	Tier3 Tier = 3
	Tier4 Tier = 4
)

// TierForLevels derives the tier from the party's declared levels, the
// way the encounter builder bands difficulty: the party's average level
// names the band. No usable level means no tier — the caller decides
// whether that is an error or a degraded mode.
func TierForLevels(levels []int) (Tier, bool) {
	if len(levels) == 0 {
		return 0, false
	}
	sum := 0
	for _, l := range levels {
		sum += l
	}
	return tierForLevel(float64(sum) / float64(len(levels)))
}

func tierForLevel(avg float64) (Tier, bool) {
	switch {
	case avg >= 17 && avg <= 20:
		return Tier4, true
	case avg >= 11:
		return Tier3, true
	case avg >= 5:
		return Tier2, true
	case avg >= 1:
		return Tier1, true
	default:
		return 0, false
	}
}

// Label is the band as the book spells it, for the arithmetic readout.
func (t Tier) Label() string {
	switch t {
	case Tier1:
		return "levels 1-4"
	case Tier2:
		return "levels 5-10"
	case Tier3:
		return "levels 11-16"
	case Tier4:
		return "levels 17-20"
	default:
		return "?"
	}
}

/* ---------- dice ---------- */

// Dice is one printed dice expression with its multiplier: the "6d6 × 100"
// of a coin line or the "2d6" of a gem count. Value arithmetic stays in
// copper pieces (the smallest unit on the table) so every comparison the
// tests make is exact integer arithmetic, never float drift.
type Dice struct {
	N     int // number of dice
	Sides int // sides per die
	Mult  int // multiplier, in the printed units (see the line that carries it)
}

// Roll returns the rolled total in the expression's printed units.
func (d Dice) Roll(r *rand.Rand) int {
	if d.N <= 0 || d.Sides <= 0 {
		return 0
	}
	total := 0
	for i := 0; i < d.N; i++ {
		total += 1 + r.Intn(d.Sides)
	}
	return total * d.Mult
}

// Avg returns the expression's average in its printed units.
func (d Dice) Avg() int { return d.N * (d.Sides + 1) / 2 * d.Mult }

// Min and Max return the expression's extremes in its printed units.
func (d Dice) Min() int { return d.N * d.Mult }
func (d Dice) Max() int { return d.N * d.Sides * d.Mult }

// String renders the expression as printed, multiplier spelled out.
func (d Dice) String() string {
	s := fmt.Sprintf("%dd%d", d.N, d.Sides)
	switch {
	case d.Mult > 1:
		s += " × " + comma(d.Mult)
	case d.Mult == 1:
		// no multiplier clause
	}
	return s
}

// comma inserts thousands separators, so readouts read like the book's.
func comma(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

/* ---------- coins ---------- */

// Coin values in copper pieces. The DMG's exchange: 10 cp = 1 sp,
// 10 sp = 1 gp, 2 ep = 1 gp, 10 gp = 1 pp.
var coinValueCP = map[string]int{
	"copper": 1, "silver": 10, "electrum": 50, "gold": 100, "platinum": 1000,
}

// coinLine is one row of a hoard table's coin block. Zero dice means the
// row's band rolls none of this coin.
type coinLine struct {
	coin string
	dice Dice // Mult carries "× 100" — the coins-per-roll multiplier
}

// valueCP converts one rolled line to copper pieces: the roll is a number
// of coins, so value = coins × per-coin value.
func (c coinLine) valueCP(coins int) int { return coins * coinValueCP[c.coin] }

// hoardCoins is the coin block of each DMG hoard table, verbatim
// (DMG p.133): copper, silver, electrum, gold, platinum, in that order.
var hoardCoins = map[Tier][]coinLine{
	Tier1: {
		{"copper", Dice{6, 6, 100}},
		{"silver", Dice{3, 6, 100}},
		{"gold", Dice{2, 6, 10}},
	},
	Tier2: {
		{"copper", Dice{2, 6, 100}},
		{"silver", Dice{2, 6, 1000}},
		{"gold", Dice{6, 6, 100}},
		{"platinum", Dice{3, 6, 10}},
	},
	Tier3: {
		{"silver", Dice{4, 6, 1000}},
		{"gold", Dice{1, 6, 1000}},
		{"platinum", Dice{1, 4, 100}},
	},
	Tier4: {
		{"gold", Dice{12, 6, 1000}},
		{"platinum", Dice{8, 6, 1000}},
	},
}

/* ---------- the d100 hoard rows ---------- */

// MundaneBundle is the gems-or-art-objects half of a hoard row, as
// printed: a dice count of a fixed-value treasure. The DMG's per-gem
// valuation variance (its d6 up/down pricing, p.134) is deliberately out:
// the printed row values keep the band arithmetic auditable.
type MundaneBundle struct {
	Kind      string // "gems" or "art objects"
	Count     Dice   // Mult 1 — how many pieces
	UnitValue int    // gp value of one piece, as the row prints it
}

// Roll returns (pieces, value in copper) for the bundle.
func (m MundaneBundle) Roll(r *rand.Rand) (int, int) {
	pieces := m.Count.Roll(r)
	return pieces, pieces * m.UnitValue * 100
}

// String renders the bundle as the row prints it: "2d6 50 gp gems".
func (m MundaneBundle) String() string {
	return fmt.Sprintf("%s %s gp %s", m.Count.String(), comma(m.UnitValue), m.Kind)
}

// MagicRoll is the magic-items half of a hoard row: roll this many times
// on the named DMG table. Table letters are the book's; the rarity each
// letter rides is tableProfile below.
type MagicRoll struct {
	Table string // "A".."I"
	Count Dice   // Mult 1
}

// hoardRows is each DMG hoard table's d100 block, verbatim (DMG p.133).
// A row with no mundane bundle and no magic roll is the book's "—" row.
var hoardRows = map[Tier][]hoardRow{
	Tier1: {
		{lo: 1, hi: 6},
		{lo: 7, hi: 16, mundane: MundaneBundle{"gems", Dice{2, 6, 1}, 10}},
		{lo: 17, hi: 26, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}},
		{lo: 27, hi: 36, mundane: MundaneBundle{"gems", Dice{2, 6, 1}, 50}},
		{lo: 37, hi: 44, mundane: MundaneBundle{"gems", Dice{2, 6, 1}, 10}, magic: []MagicRoll{{"A", Dice{1, 6, 1}}}},
		{lo: 45, hi: 52, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}, magic: []MagicRoll{{"A", Dice{1, 6, 1}}}},
		{lo: 53, hi: 60, mundane: MundaneBundle{"gems", Dice{2, 6, 1}, 50}, magic: []MagicRoll{{"A", Dice{1, 6, 1}}}},
		{lo: 61, hi: 65, mundane: MundaneBundle{"gems", Dice{2, 6, 1}, 10}, magic: []MagicRoll{{"B", Dice{1, 4, 1}}}},
		{lo: 66, hi: 70, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}, magic: []MagicRoll{{"B", Dice{1, 4, 1}}}},
		{lo: 71, hi: 75, mundane: MundaneBundle{"gems", Dice{2, 6, 1}, 50}, magic: []MagicRoll{{"B", Dice{1, 4, 1}}}},
		{lo: 76, hi: 78, mundane: MundaneBundle{"gems", Dice{2, 6, 1}, 10}, magic: []MagicRoll{{"C", Dice{1, 4, 1}}}},
		{lo: 79, hi: 80, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}, magic: []MagicRoll{{"C", Dice{1, 4, 1}}}},
		{lo: 81, hi: 85, mundane: MundaneBundle{"gems", Dice{2, 6, 1}, 50}, magic: []MagicRoll{{"C", Dice{1, 4, 1}}}},
		{lo: 86, hi: 92, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}, magic: []MagicRoll{{"F", Dice{1, 4, 1}}}},
		{lo: 93, hi: 97, mundane: MundaneBundle{"gems", Dice{2, 6, 1}, 50}, magic: []MagicRoll{{"F", Dice{1, 4, 1}}}},
		{lo: 98, hi: 99, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}, magic: []MagicRoll{{"G", Dice{1, 1, 1}}}},
		{lo: 100, hi: 100, mundane: MundaneBundle{"gems", Dice{2, 6, 1}, 50}, magic: []MagicRoll{{"G", Dice{1, 1, 1}}}},
	},
	Tier2: {
		{lo: 1, hi: 4},
		{lo: 5, hi: 10, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}},
		{lo: 11, hi: 16, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 50}},
		{lo: 17, hi: 22, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 100}},
		{lo: 23, hi: 28, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}},
		{lo: 29, hi: 32, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}, magic: []MagicRoll{{"A", Dice{1, 6, 1}}}},
		{lo: 33, hi: 36, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 50}, magic: []MagicRoll{{"A", Dice{1, 6, 1}}}},
		{lo: 37, hi: 40, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 100}, magic: []MagicRoll{{"A", Dice{1, 6, 1}}}},
		{lo: 41, hi: 44, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"A", Dice{1, 6, 1}}}},
		{lo: 45, hi: 49, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}, magic: []MagicRoll{{"B", Dice{1, 4, 1}}}},
		{lo: 50, hi: 54, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 50}, magic: []MagicRoll{{"B", Dice{1, 4, 1}}}},
		{lo: 55, hi: 59, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 100}, magic: []MagicRoll{{"B", Dice{1, 4, 1}}}},
		{lo: 60, hi: 63, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"B", Dice{1, 4, 1}}}},
		{lo: 64, hi: 66, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}, magic: []MagicRoll{{"C", Dice{1, 4, 1}}}},
		{lo: 67, hi: 69, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 50}, magic: []MagicRoll{{"C", Dice{1, 4, 1}}}},
		{lo: 70, hi: 72, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 100}, magic: []MagicRoll{{"C", Dice{1, 4, 1}}}},
		{lo: 73, hi: 74, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"C", Dice{1, 4, 1}}}},
		{lo: 75, hi: 76, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}, magic: []MagicRoll{{"D", Dice{1, 1, 1}}}},
		{lo: 77, hi: 78, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 50}, magic: []MagicRoll{{"D", Dice{1, 1, 1}}}},
		{lo: 79, hi: 79, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 100}, magic: []MagicRoll{{"D", Dice{1, 1, 1}}}},
		{lo: 80, hi: 80, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"D", Dice{1, 1, 1}}}},
		{lo: 81, hi: 84, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 25}, magic: []MagicRoll{{"F", Dice{1, 4, 1}}}},
		{lo: 85, hi: 88, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 50}, magic: []MagicRoll{{"F", Dice{1, 4, 1}}}},
		{lo: 89, hi: 91, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 100}, magic: []MagicRoll{{"F", Dice{1, 4, 1}}}},
		{lo: 92, hi: 94, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"F", Dice{1, 4, 1}}}},
		{lo: 95, hi: 96, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 100}, magic: []MagicRoll{{"G", Dice{1, 4, 1}}}},
		{lo: 97, hi: 98, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"G", Dice{1, 4, 1}}}},
		{lo: 99, hi: 99, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 100}, magic: []MagicRoll{{"H", Dice{1, 1, 1}}}},
		{lo: 100, hi: 100, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"H", Dice{1, 1, 1}}}},
	},
	Tier3: {
		{lo: 1, hi: 3},
		{lo: 4, hi: 6, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}},
		{lo: 7, hi: 9, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 750}},
		{lo: 10, hi: 12, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 500}},
		{lo: 13, hi: 15, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}},
		{lo: 16, hi: 19, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"A", Dice{1, 4, 1}}, {"B", Dice{1, 6, 1}}}},
		{lo: 20, hi: 23, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 750}, magic: []MagicRoll{{"A", Dice{1, 4, 1}}, {"B", Dice{1, 6, 1}}}},
		{lo: 24, hi: 26, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 500}, magic: []MagicRoll{{"A", Dice{1, 4, 1}}, {"B", Dice{1, 6, 1}}}},
		{lo: 27, hi: 29, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"A", Dice{1, 4, 1}}, {"B", Dice{1, 6, 1}}}},
		{lo: 30, hi: 35, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"C", Dice{1, 6, 1}}}},
		{lo: 36, hi: 40, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 750}, magic: []MagicRoll{{"C", Dice{1, 6, 1}}}},
		{lo: 41, hi: 45, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 500}, magic: []MagicRoll{{"C", Dice{1, 6, 1}}}},
		{lo: 46, hi: 50, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"C", Dice{1, 6, 1}}}},
		{lo: 51, hi: 54, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"D", Dice{1, 4, 1}}}},
		{lo: 55, hi: 58, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 750}, magic: []MagicRoll{{"D", Dice{1, 4, 1}}}},
		{lo: 59, hi: 62, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 500}, magic: []MagicRoll{{"D", Dice{1, 4, 1}}}},
		{lo: 63, hi: 66, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"D", Dice{1, 4, 1}}}},
		{lo: 67, hi: 68, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"E", Dice{1, 1, 1}}}},
		{lo: 69, hi: 70, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 750}, magic: []MagicRoll{{"E", Dice{1, 1, 1}}}},
		{lo: 71, hi: 72, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 500}, magic: []MagicRoll{{"E", Dice{1, 1, 1}}}},
		{lo: 73, hi: 74, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"E", Dice{1, 1, 1}}}},
		{lo: 75, hi: 76, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"F", Dice{1, 1, 1}}, {"G", Dice{1, 4, 1}}}},
		{lo: 77, hi: 78, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 750}, magic: []MagicRoll{{"F", Dice{1, 1, 1}}, {"G", Dice{1, 4, 1}}}},
		{lo: 79, hi: 80, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 500}, magic: []MagicRoll{{"F", Dice{1, 1, 1}}, {"G", Dice{1, 4, 1}}}},
		{lo: 81, hi: 82, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"F", Dice{1, 1, 1}}, {"G", Dice{1, 4, 1}}}},
		{lo: 83, hi: 85, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"H", Dice{1, 4, 1}}}},
		{lo: 86, hi: 88, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 750}, magic: []MagicRoll{{"H", Dice{1, 4, 1}}}},
		{lo: 89, hi: 90, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 500}, magic: []MagicRoll{{"H", Dice{1, 4, 1}}}},
		{lo: 91, hi: 92, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"H", Dice{1, 4, 1}}}},
		{lo: 93, hi: 94, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 250}, magic: []MagicRoll{{"I", Dice{1, 1, 1}}}},
		{lo: 95, hi: 96, mundane: MundaneBundle{"art objects", Dice{2, 4, 1}, 750}, magic: []MagicRoll{{"I", Dice{1, 1, 1}}}},
		{lo: 97, hi: 98, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 500}, magic: []MagicRoll{{"I", Dice{1, 1, 1}}}},
		{lo: 99, hi: 100, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"I", Dice{1, 1, 1}}}},
	},
	Tier4: {
		{lo: 1, hi: 2},
		{lo: 3, hi: 5, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"C", Dice{1, 8, 1}}}},
		{lo: 6, hi: 8, mundane: MundaneBundle{"art objects", Dice{1, 10, 1}, 2500}, magic: []MagicRoll{{"C", Dice{1, 8, 1}}}},
		{lo: 9, hi: 11, mundane: MundaneBundle{"art objects", Dice{1, 4, 1}, 7500}, magic: []MagicRoll{{"C", Dice{1, 8, 1}}}},
		{lo: 12, hi: 14, mundane: MundaneBundle{"gems", Dice{1, 8, 1}, 5000}, magic: []MagicRoll{{"C", Dice{1, 8, 1}}}},
		{lo: 15, hi: 22, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"D", Dice{1, 6, 1}}}},
		{lo: 23, hi: 30, mundane: MundaneBundle{"art objects", Dice{1, 10, 1}, 2500}, magic: []MagicRoll{{"D", Dice{1, 6, 1}}}},
		{lo: 31, hi: 38, mundane: MundaneBundle{"art objects", Dice{1, 4, 1}, 7500}, magic: []MagicRoll{{"D", Dice{1, 6, 1}}}},
		{lo: 39, hi: 46, mundane: MundaneBundle{"gems", Dice{1, 8, 1}, 5000}, magic: []MagicRoll{{"D", Dice{1, 6, 1}}}},
		{lo: 47, hi: 52, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"E", Dice{1, 6, 1}}}},
		{lo: 53, hi: 58, mundane: MundaneBundle{"art objects", Dice{1, 10, 1}, 2500}, magic: []MagicRoll{{"E", Dice{1, 6, 1}}}},
		{lo: 59, hi: 63, mundane: MundaneBundle{"art objects", Dice{1, 4, 1}, 7500}, magic: []MagicRoll{{"E", Dice{1, 6, 1}}}},
		{lo: 64, hi: 68, mundane: MundaneBundle{"gems", Dice{1, 8, 1}, 5000}, magic: []MagicRoll{{"E", Dice{1, 6, 1}}}},
		{lo: 69, hi: 69, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"G", Dice{1, 4, 1}}}},
		{lo: 70, hi: 70, mundane: MundaneBundle{"art objects", Dice{1, 10, 1}, 2500}, magic: []MagicRoll{{"G", Dice{1, 4, 1}}}},
		{lo: 71, hi: 71, mundane: MundaneBundle{"art objects", Dice{1, 4, 1}, 7500}, magic: []MagicRoll{{"G", Dice{1, 4, 1}}}},
		{lo: 72, hi: 72, mundane: MundaneBundle{"gems", Dice{1, 8, 1}, 5000}, magic: []MagicRoll{{"G", Dice{1, 4, 1}}}},
		{lo: 73, hi: 74, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"H", Dice{1, 4, 1}}}},
		{lo: 75, hi: 76, mundane: MundaneBundle{"art objects", Dice{1, 10, 1}, 2500}, magic: []MagicRoll{{"H", Dice{1, 4, 1}}}},
		{lo: 77, hi: 78, mundane: MundaneBundle{"art objects", Dice{1, 4, 1}, 7500}, magic: []MagicRoll{{"H", Dice{1, 4, 1}}}},
		{lo: 79, hi: 80, mundane: MundaneBundle{"gems", Dice{1, 8, 1}, 5000}, magic: []MagicRoll{{"H", Dice{1, 4, 1}}}},
		{lo: 81, hi: 85, mundane: MundaneBundle{"gems", Dice{3, 6, 1}, 1000}, magic: []MagicRoll{{"I", Dice{1, 4, 1}}}},
		{lo: 86, hi: 90, mundane: MundaneBundle{"art objects", Dice{1, 10, 1}, 2500}, magic: []MagicRoll{{"I", Dice{1, 4, 1}}}},
		{lo: 91, hi: 95, mundane: MundaneBundle{"art objects", Dice{1, 4, 1}, 7500}, magic: []MagicRoll{{"I", Dice{1, 4, 1}}}},
		{lo: 96, hi: 100, mundane: MundaneBundle{"gems", Dice{1, 8, 1}, 5000}, magic: []MagicRoll{{"I", Dice{1, 4, 1}}}},
	},
}

// hoardRow is one d100 row: its inclusive range and what it carries.
type hoardRow struct {
	lo, hi  int
	mundane MundaneBundle // zero Kind when the row carries none
	magic   []MagicRoll
}

// weight is the row's d100 width.
func (h hoardRow) weight() int { return h.hi - h.lo + 1 }

// pickRow rolls the d100 for a tier's table and returns the row it
// landed on together with the roll itself, so the readout can show it.
func pickRow(t Tier, r *rand.Rand) (hoardRow, int) {
	rows := hoardRows[t]
	roll := 1 + r.Intn(100)
	for _, row := range rows {
		if roll >= row.lo && roll <= row.hi {
			return row, roll
		}
	}
	// Unreachable while the rows tile 1..100, which a test asserts.
	return rows[len(rows)-1], roll
}

/* ---------- the value band ---------- */

// Band is the arithmetic envelope of a tier's hoard: the smallest and
// largest total value the book's tables can produce, and their expected
// value — coins, gems and art objects in gold pieces. Magic items are not
// priced; the DMG gives no prices, and this surface is not a gold-piece
// economy.
type Band struct {
	MinGP int `json:"min_gp"`
	MaxGP int `json:"max_gp"`
	AvgGP int `json:"avg_gp"`
}

// TierBand computes the band from the table itself, so the tests assert
// against arithmetic that cannot drift from the data.
func TierBand(t Tier) Band {
	rows := hoardRows[t]
	coins := hoardCoins[t]
	var minTotal, maxTotal, avgTotal int
	first := true
	for _, row := range rows {
		rowMin, rowMax, rowAvg := 0, 0, 0
		for _, c := range coins {
			rowMin += c.valueCP(c.dice.Min())
			rowMax += c.valueCP(c.dice.Max())
			rowAvg += c.valueCP(c.dice.Avg())
		}
		if row.mundane.Kind != "" {
			rowMin += row.mundane.Count.Min() * row.mundane.UnitValue * 100
			rowMax += row.mundane.Count.Max() * row.mundane.UnitValue * 100
			rowAvg += row.mundane.Count.Avg() * row.mundane.UnitValue * 100
		}
		if first || rowMin < minTotal {
			minTotal = rowMin
		}
		if first || rowMax > maxTotal {
			maxTotal = rowMax
		}
		avgTotal += row.weight() * rowAvg
		first = false
	}
	// avgTotal accumulates copper × d100 weight: divide by the d100's
	// total weight and by the copper-to-gold hundred, in one step.
	return Band{MinGP: minTotal / 100, MaxGP: maxTotal / 100, AvgGP: avgTotal / 10000}
}

/* ---------- the DMG tables' rarity profiles ---------- */

// tableProfile is the one adaptation in this file, declared: what rarity
// each DMG magic item table rides in Grimoire's catalog. The book's
// tables are item lists the corpus has no membership for; their rarity
// shape is not in dispute (A leans common and uncommon, I is legendary),
// and it is the shape — not the membership — that a tier-aware pick
// needs. A–E are the DMG's minor tables and F–I its major ones, the split
// XGE p.135 names.
var tableProfile = map[string][]string{
	"A": {"Common", "Uncommon"},
	"B": {"Uncommon"},
	"C": {"Uncommon", "Rare"},
	"D": {"Rare"},
	"E": {"Rare"},
	"F": {"Rare", "Very Rare"},
	"G": {"Very Rare"},
	"H": {"Very Rare", "Legendary"},
	"I": {"Legendary"},
}

// profileFor returns a table's rarity profile, empty when the letter is
// not one of the book's.
func profileFor(table string) []string {
	return tableProfile[table]
}
