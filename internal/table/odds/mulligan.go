package odds

// The mulligan advisor (MAD-336): keep-or-mulligan over an opening
// hand and the known decklist, built on the analysis internal/deck
// already computes — curve, land count, ramp/draw/interaction roles —
// plus the same exact hypergeometric the draw questions use. There is
// deliberately no second scoring system: the numbers a deck report
// shows are the numbers this advice cites.
//
// Advice is a recommendation with its arithmetic shown, not an oracle:
// every reason carries the count or probability behind it, and the
// verdict is a deterministic function of those numbers. Opt-in per
// game is the server's gate (mtg_games.settings), not a judgement made
// here.

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/deck"
)

// Advice is one keep-or-mulligan verdict over an opening hand.
type Advice struct {
	Verdict  string `json:"verdict"` // "keep" | "mulligan"
	Lands    int    `json:"lands"`   // lands in the hand
	HandSize int    `json:"hand_size"`
	// LandPercentile is P(a random hand from this deck has ≤ this many
	// lands): where this hand's land count sits. The exact rational
	// rides along like every other answer's receipt.
	LandPercentile float64  `json:"land_percentile"`
	LandRational   string   `json:"land_rational"`
	Reasons        []string `json:"reasons"`
	// HandRoles labels each non-land card with internal/deck's role
	// classification — the same categories the deck report renders.
	HandRoles map[string]string `json:"hand_roles,omitempty"`
}

// Advise scores one opening hand against the decklist it came from.
// hand is the card names as drawn; decklist is the full multiset.
// Verdict rules, all cited in the reasons:
//
//   - 0, 1 or 6+ lands → mulligan
//   - 2 lands → keep only with ramp in hand or a land-heavy deck
//   - 3–5 lands → keep, unless nothing in the hand is castable early
//
// The percentile comes from the exact hypergeometric: P(X ≤ lands)
// for the deck's land count over its size.
func Advise(hand []string, decklist map[string]int, lookup Lookup) (*Advice, error) {
	if len(hand) == 0 {
		return nil, fmt.Errorf("odds: mulligan advice needs the hand")
	}
	if len(decklist) == 0 {
		return nil, fmt.Errorf("odds: mulligan advice needs the decklist")
	}
	a := &Advice{HandSize: len(hand), HandRoles: map[string]string{}}

	// The deck's own analysis: land count, curve, ramp/draw/interaction
	// ratios — one call, the same report the deck page renders.
	entries := make([]deck.Entry, 0, len(decklist))
	names := SortedHitNames(decklist)
	for _, name := range names {
		entries = append(entries, deck.Entry{Name: name, Count: decklist[name]})
	}
	report := deck.Analyze("", entries, func(name string) (*carddb.Card, bool) {
		if lookup == nil {
			return nil, false
		}
		return lookup(name)
	})

	// The hand, in two passes — sources first, so a spell's colors are
	// checked against every basic the hand holds, not just the ones
	// walked before it: lands and their colors, then roles and
	// early-castability.
	handLands := 0
	sources := map[string]bool{}
	for _, name := range hand {
		c, ok := lookup(name)
		if !ok || c == nil {
			continue
		}
		if c.IsLand() {
			handLands++
			if letter, basic := basicColor(c.Name); basic {
				sources[letter] = true
			}
		}
	}
	castableEarly := 0
	for _, name := range hand {
		c, ok := lookup(name)
		if !ok || c == nil {
			a.HandRoles[name] = "unknown"
			continue
		}
		if c.IsLand() {
			continue
		}
		a.HandRoles[name] = deck.RoleOf(c)
		if c.ManaValue <= 3 && colorsSatisfied(c.ManaCost, sources) {
			castableEarly++
		}
	}
	a.Lands = handLands

	// Where this hand's land count sits among the deck's hands —
	// P(X ≤ handLands), exact. TotalMain already includes the lands.
	total, libLands := report.TotalMain, report.Lands
	if total > 0 && libLands > 0 {
		percentile := new(big.Rat)
		for k := 0; k <= handLands; k++ {
			if p, err := PMF(total, libLands, len(hand), k); err == nil {
				percentile.Add(percentile, p)
			}
		}
		a.LandPercentile, _ = percentile.Float64()
		a.LandRational = ratString(percentile)
	}

	switch {
	case handLands <= 1:
		a.Verdict = "mulligan"
		a.Reasons = append(a.Reasons, fmt.Sprintf(
			"%d land%s in a %d-land deck: %.1f%% of %d-card hands have this few or fewer — the keep bar is not close",
			handLands, plural(handLands), libLands, a.LandPercentile*100, len(hand)))
		if handLands == 0 {
			a.Reasons = append(a.Reasons, "zero lands cannot curve out under any draw")
		}
	case handLands >= 6:
		a.Verdict = "mulligan"
		a.Reasons = append(a.Reasons, fmt.Sprintf(
			"%d lands is flooded: %.1f%% of hands have this many or more", handLands, (1-a.LandPercentile)*100))
	case handLands == 2:
		hasRamp := false
		for _, role := range a.HandRoles {
			if role == "ramp" {
				hasRamp = true
				break
			}
		}
		landShare := 0.0
		if total > 0 {
			landShare = float64(libLands) / float64(total)
		}
		if hasRamp {
			a.Verdict = "keep"
			a.Reasons = append(a.Reasons, "two lands plus ramp in hand — the hand plays the early turns")
		} else if landShare >= 0.40 {
			a.Verdict = "keep"
			a.Reasons = append(a.Reasons, fmt.Sprintf(
				"two lands in a land-heavy deck (%.0f%% lands) is near the median hand", landShare*100))
		} else {
			a.Verdict = "mulligan"
			a.Reasons = append(a.Reasons, fmt.Sprintf(
				"two lands with no ramp in a %.0f%%-land deck: %.1f%% of hands have more",
				landShare*100, (1-a.LandPercentile)*100))
		}
	default:
		a.Verdict = "keep"
		a.Reasons = append(a.Reasons, fmt.Sprintf(
			"%d lands sits inside the keep band (%.1f%% of hands have this few or fewer)",
			handLands, a.LandPercentile*100))
		if castableEarly == 0 {
			// A keepable land count with nothing to cast is the hand
			// that loses three games in a row quietly.
			a.Verdict = "mulligan"
			a.Reasons = append(a.Reasons, "nothing in hand is castable by turn three with the hand's own sources")
		} else {
			a.Reasons = append(a.Reasons, fmt.Sprintf("%d spell%s castable by turn three from the hand's sources",
				castableEarly, plural(castableEarly)))
		}
	}
	if report.AvgMV > 0 {
		a.Reasons = append(a.Reasons, fmt.Sprintf("deck averages %.1f mana value with %d ramp and %d interaction",
			report.AvgMV, report.Ratios.Ramp, report.Ratios.Interaction))
	}
	return a, nil
}

// basicColor maps a basic land's name to the color it produces.
func basicColor(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "plains":
		return "W", true
	case "island":
		return "U", true
	case "swamp":
		return "B", true
	case "mountain":
		return "R", true
	case "forest":
		return "G", true
	}
	return "", false
}

// colorsSatisfied reports whether a mana cost's colored pips are all
// among the sources — a v1 check over basic lands only, and the advice
// says so rather than pretending to know a nonbasic's production.
func colorsSatisfied(cost string, sources map[string]bool) bool {
	for _, pip := range costPips(cost) {
		if len(pip) == 1 && strings.ContainsAny(pip, "WUBRG") {
			if !sources[pip] {
				return false
			}
		}
	}
	return true
}

// costPips splits a mana cost into its {...} spans: "{2}{U}{U}" →
// ["2", "U", "U"].
func costPips(cost string) []string {
	var out []string
	for _, field := range strings.Split(cost, " ") {
		for len(field) > 0 && field[0] == '{' {
			end := strings.IndexByte(field, '}')
			if end < 1 {
				break
			}
			out = append(out, field[1:end])
			field = field[end+1:]
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
