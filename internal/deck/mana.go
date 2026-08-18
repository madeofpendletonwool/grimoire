package deck

import (
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
)

// Mana math: given the pip demands of a draft, how many of each basic land
// should the mana base run? Pure functions, unit-testable, no I/O. The
// standard heuristic: each color's share of the land slots mirrors its share
// of the total pip weight, with devotion (pips on cheap cards count more)
// nudging the split.

// ManaCost parses a mana cost string like "{2}{W}{W}{B}" into generic mana
// and colored pip counts. Hybrid ({W/U}) pips count half toward each color.
// A pip with a colorless escape hatch ({2/W}, {W/P}) counts half toward its
// color: the deck can pay it without the source.
func ManaCost(cost string) (generic int, pips map[string]float64) {
	pips = map[string]float64{}
	for _, sym := range splitSymbols(cost) {
		switch {
		case sym == "":
			continue
		case isDigits(sym):
			n := 0
			for _, c := range sym {
				n = n*10 + int(c-'0')
			}
			generic += n
		case len(sym) == 1 && isColorLetter(sym[0]):
			pips[sym] += 1
		case strings.Contains(sym, "/"):
			options := strings.Split(sym, "/")
			var colored []string
			escaped := false
			for _, o := range options {
				if len(o) == 1 && isColorLetter(o[0]) {
					colored = append(colored, o)
				} else {
					escaped = true // "2" or "P": a colorless way to pay
				}
			}
			if len(colored) == 0 {
				generic++
				continue
			}
			credit := 1 / float64(len(colored))
			if escaped {
				credit /= 2
			}
			for _, c := range colored {
				pips[c] += credit
			}
		case sym == "X" || sym == "Y" || sym == "Z" || sym == "S" || sym == "C":
			generic++
		}
	}
	return generic, pips
}

func isDigits(sym string) bool {
	if sym == "" {
		return false
	}
	for _, c := range sym {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isColorLetter(c byte) bool {
	return c == 'W' || c == 'U' || c == 'B' || c == 'R' || c == 'G'
}

// splitSymbols extracts the inner text of each {...} group.
func splitSymbols(cost string) []string {
	if cost == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(cost, " // ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		for _, chunk := range strings.FieldsFunc(part, func(r rune) bool { return r == '{' || r == '}' }) {
			chunk = strings.TrimSpace(chunk)
			if chunk != "" {
				out = append(out, chunk)
			}
		}
	}
	return out
}

// ManaNeeds is a draft's aggregate mana demand: per-color pip weight (devotion
// weighted) and total mana symbols.
type ManaNeeds struct {
	Pips      map[string]float64
	TotalPips float64
	Cards     int
}

// WeighPips computes the weighted pip demand over the given cards, each
// repeated by its count. Cards costing 1–2 mana are played early and their
// pips count double (devotion weighting); 3+ count face value. Lands
// contribute nothing.
func WeighPips(entries []Entry, lookup func(name string) (*carddb.Card, bool)) ManaNeeds {
	needs := ManaNeeds{Pips: map[string]float64{}}
	for _, e := range entries {
		if e.Board != "" && e.Board != "main" {
			continue
		}
		card, ok := lookup(e.Name)
		if !ok {
			continue
		}
		if card.IsLand() {
			continue
		}
		weight := 1.0
		if card.ManaValue <= 2 {
			weight = 2.0
		}
		_, pips := ManaCost(card.ManaCost)
		for color, n := range pips {
			needs.Pips[color] += n * weight * float64(e.Count)
		}
	}
	for _, n := range needs.Pips {
		needs.TotalPips += n
	}
	return needs
}

// LandCounts suggests how many basic lands of each color a deck should run,
// given its pip demand and the number of land slots to fill. Colors with no
// pips get zero; rounding is largest-remainder so the counts sum exactly to
// slots (when any color has demand).
func LandCounts(needs ManaNeeds, slots int) map[string]int {
	if slots < 0 {
		slots = 0
	}
	out := map[string]int{}
	if needs.TotalPips <= 0 || slots == 0 {
		return out
	}

	// Sort colors for deterministic largest-remainder rounding.
	colors := make([]string, 0, len(needs.Pips))
	for c, w := range needs.Pips {
		if w > 0 {
			colors = append(colors, c)
		}
	}
	if len(colors) == 0 {
		return out
	}
	sort.Strings(colors)

	// Exact shares, then floor each.
	used := 0
	floors := map[string]float64{}
	for _, c := range colors {
		share := needs.Pips[c] / needs.TotalPips * float64(slots)
		floors[c] = share
		out[c] = int(share)
		used += out[c]
	}
	// Distribute the remainder by largest fractional part.
	remaining := slots - used
	for remaining > 0 {
		best := ""
		bestFrac := -1.0
		for _, c := range colors {
			frac := floors[c] - float64(out[c])
			if frac > bestFrac {
				bestFrac, best = frac, c
			}
		}
		if best == "" {
			break
		}
		out[best]++
		remaining--
	}
	return out
}

// SuggestLandCount suggests total lands for a Commander deck from its average
// mana value: 37 at the format baseline (avg MV ~2.5), sliding up for heavier
// curves and down for cheaper ones, clamped to a sane 32–42.
func SuggestLandCount(entries []Entry, lookup func(name string) (*carddb.Card, bool)) int {
	total := 0.0
	cards := 0.0
	for _, e := range entries {
		if e.Board == "commander" || e.Board == "sideboard" {
			continue
		}
		card, ok := lookup(e.Name)
		if !ok {
			continue
		}
		if card.IsLand() {
			continue
		}
		total += card.ManaValue * float64(e.Count)
		cards += float64(e.Count)
	}
	if cards == 0 {
		return 37
	}
	avg := total / cards
	lands := 37 + int((avg-2.5)*3+0.5)
	if lands < 32 {
		lands = 32
	}
	if lands > 42 {
		lands = 42
	}
	return lands
}
