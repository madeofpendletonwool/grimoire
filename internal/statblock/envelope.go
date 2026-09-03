// The target envelope (MAD-382): what the DMG's own tables say a creature of
// a given challenge rating looks like, before anything is asked of a model.
//
// The monster designer hands this to the model as fact — the expected armor
// class, hit points, damage per round, attack bonus and save DC bands, the
// proficiency bonus, and the legendary-action budget the calculator prices —
// so the draft is designed inside the numbers its CR will be checked
// against, rather than near them. The bands are read from the same tables
// ComputeCR reads; there is one source of the arithmetic and it is this
// package.

package statblock

import (
	"strconv"
	"strings"
)

// Band is one number's target window: where the table value sits (Assumed)
// and the range that costs at most one CR step against it (Min, Max). A
// band is guidance, not a gate — ComputeCR, not the envelope, is the check.
type Band struct {
	Min     int `json:"min"`
	Assumed int `json:"assumed"`
	Max     int `json:"max"`
}

// Envelope is the DMG p. 274 band a creature of a given CR lands in: the
// defensive half (effective hit points against armor class), the offensive
// half (damage per round against attack bonus or save DC), the proficiency
// bonus the Monster Statistics table assigns, and the legendary-action
// budget the calculator prices (three points a round — what
// ComputeCR's legendary arithmetic assumes a legendary monster uses).
type Envelope struct {
	CR        float64 `json:"cr"`
	Label     string  `json:"label"`
	ProfBonus int     `json:"prof_bonus"`

	HP          Band `json:"hp"`           // effective hit points (after resistances and the like)
	AC          Band `json:"ac"`           // armor class
	DPR         Band `json:"dpr"`          // damage per round
	AttackBonus Band `json:"attack_bonus"` // to-hit the damage is delivered at
	SaveDC      Band `json:"save_dc"`      // the DC, when offense is save-based

	// LegendaryPoints is the legendary-action budget, in points per round,
	// the calculator prices for a creature this size. A non-legendary
	// design simply leaves it unspent.
	LegendaryPoints int `json:"legendary_points"`
}

// hpBandMax and dprBandMax are the upper edges the DMG prints for the last
// table row (CR 30): 806–850 effective hit points, 303+ damage per round.
// The tables here stop at the row minima, so the top row's width is stated
// once instead of implied.
const (
	hpBandMax  = 850
	dprBandMax = 335
)

// EnvelopeFor returns the target envelope for a challenge rating. Values
// below 0 or above 30 clamp to the table's ends — a requested CR outside
// the books is still a number the tables can speak to, at their edge.
func EnvelopeFor(cr float64) Envelope {
	if cr < 0 {
		cr = 0
	}
	if cr > crLevels[len(crLevels)-1] {
		cr = crLevels[len(crLevels)-1]
	}
	// The table index of the requested CR itself.
	idx := Level(cr)
	if idx < 0 {
		idx = 0
	}

	hp := Band{Min: defensiveHP[idx], Assumed: assumedHP(idx), Max: maxOf(idx, defensiveHP, hpBandMax)}
	ac := Band{Min: defensiveAC[idx] - 2, Assumed: defensiveAC[idx], Max: defensiveAC[idx] + 2}
	dpr := Band{Min: offensiveDPR[idx], Assumed: assumedDPR(idx), Max: maxOf(idx, offensiveDPR, dprBandMax)}
	ab := Band{Min: offensiveAB[idx] - 2, Assumed: offensiveAB[idx], Max: offensiveAB[idx] + 2}
	dc := Band{Min: offensiveDC[idx] - 2, Assumed: offensiveDC[idx], Max: offensiveDC[idx] + 2}
	if ab.Min < 0 {
		ab.Min = 0
	}
	if dc.Min < 10 {
		dc.Min = 10
	}
	return Envelope{
		CR: cr, Label: Label(cr), ProfBonus: ProficiencyFor(cr),
		HP: hp, AC: ac, DPR: dpr, AttackBonus: ab, SaveDC: dc,
		LegendaryPoints: 3,
	}
}

// assumedHP is the midpoint of a row's effective-hit-point band — the
// "design here" figure the prompt leads with.
func assumedHP(idx int) int {
	lo := defensiveHP[idx]
	hi := maxOf(idx, defensiveHP, hpBandMax)
	return (lo + hi) / 2
}

func assumedDPR(idx int) int {
	lo := offensiveDPR[idx]
	hi := maxOf(idx, offensiveDPR, dprBandMax)
	return (lo + hi) / 2
}

// maxOf is a table row's upper edge: the next row's minimum minus one, or
// the stated cap on the last row.
func maxOf(idx int, table []int, last int) int {
	if idx+1 < len(table) {
		return table[idx+1] - 1
	}
	return last
}

// ParseLabel reads a challenge rating the way the books print it — "7",
// "1/4", "0.5" — and reports whether it is a CR the tables carry.
func ParseLabel(s string) (float64, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "1/8":
		return 0.125, true
	case "1/4":
		return 0.25, true
	case "1/2":
		return 0.5, true
	case "":
		return 0, false
	}
	if cr, err := strconv.ParseFloat(s, 64); err == nil {
		if Level(cr) >= 0 {
			return cr, true
		}
		return 0, false
	}
	return 0, false
}

/* ---------- the shortfall, in words ---------- */

// Shortfall compares a computed rating against a requested CR and names the
// miss specifically: which half is off, by how many CR steps, and what raw
// number closes it. An empty result means the maths agrees. This is the
// text that goes back to the model (and, unresolved, to the DM) — "offensive
// CR 4 against a defensive CR 9; damage per round is 22 short", not "wrong".
func Shortfall(requested float64, r Rating) []string {
	var out []string
	target := Level(requested)
	if target < 0 {
		return nil
	}

	defDelta := target - Level(r.Defensive)
	switch {
	case defDelta > 0:
		hpShort := defensiveHP[target] - r.EffectiveHP
		line := "defensive CR " + Label(r.Defensive) + " is " +
			strconv.Itoa(defDelta) + " step(s) below CR " + Label(requested) +
			"; effective hit points are " + strconv.Itoa(hpShort) + " short (need at least " +
			strconv.Itoa(defensiveHP[target]) + " at the assumed AC " + strconv.Itoa(defensiveAC[target]) + ")"
		if hpShort <= 0 {
			line += " — the hit points are there, so the armor class is dragging the rating down"
		}
		out = append(out, line)
	case defDelta < 0:
		out = append(out, "defensive CR "+Label(r.Defensive)+" is "+
			strconv.Itoa(-defDelta)+" step(s) above CR "+Label(requested)+
			"; effective hit points run "+strconv.Itoa(r.EffectiveHP-defensiveHP[target+1])+
			" past the band (the CR "+Label(requested)+" row tops out at "+
			strconv.Itoa(maxOf(target, defensiveHP, hpBandMax))+")")
	}

	offDelta := target - Level(r.Offensive)
	switch {
	case offDelta > 0:
		dprShort := offensiveDPR[target] - r.DPR
		line := "offensive CR " + Label(r.Offensive) + " is " +
			strconv.Itoa(offDelta) + " step(s) below CR " + Label(requested) +
			"; damage per round is " + strconv.Itoa(dprShort) + " short (need at least " +
			strconv.Itoa(offensiveDPR[target]) + ")"
		if dprShort <= 0 {
			// The damage is on target; the delivery is what is priced low.
			if r.SaveBased {
				line += " — the damage is there, so the save DC is dragging the rating down (assumed DC " +
					strconv.Itoa(offensiveDC[target]) + ")"
			} else {
				line += " — the damage is there, so the attack bonus is dragging the rating down (assumed +" +
					strconv.Itoa(offensiveAB[target]) + ")"
			}
		}
		out = append(out, line)
	case offDelta < 0:
		out = append(out, "offensive CR "+Label(r.Offensive)+" is "+
			strconv.Itoa(-offDelta)+" step(s) above CR "+Label(requested)+
			"; damage per round runs "+strconv.Itoa(r.DPR-offensiveDPR[target+1])+
			" past the band (the CR "+Label(requested)+" row tops out at "+
			strconv.Itoa(maxOf(target, offensiveDPR, dprBandMax))+")")
	}
	return out
}
