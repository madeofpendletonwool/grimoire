package deck

import (
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
)

// The analyzer: deterministic health checks over a resolved deck — color
// identity, curve, ratios, salt — producing the report the model critiques
// the draft with and the UI renders. Pure functions over card data; no I/O.

// Analysis is the full report for one deck.
type Analysis struct {
	Commander    string          `json:"commander"`
	Identity     string          `json:"identity"`
	TotalMain    int             `json:"total_main"`
	Lands        int             `json:"lands"`
	Curve        map[int]int     `json:"curve"` // mana value -> card count
	AvgMV        float64         `json:"avg_mv"`
	Ratios       Ratios          `json:"ratios"`
	Saltiest     []SaltEntry     `json:"saltiest"`
	Warnings     []string        `json:"warnings"`
	IdentityBad  []IdentityIssue `json:"identity_violations"`
	GameChangers int             `json:"game_changers"`
}

// Ratios are the deck-shape counts against rough Commander norms.
type Ratios struct {
	Ramp        int `json:"ramp"`
	Draw        int `json:"draw"`
	Interaction int `json:"interaction"`
	Lands       int `json:"lands"`
}

// SaltEntry is one of the saltiest cards in the deck.
type SaltEntry struct {
	Name string  `json:"name"`
	Salt float64 `json:"salt"`
}

// IdentityIssue reports a card whose color identity breaks the deck's.
type IdentityIssue struct {
	Name     string `json:"name"`
	Identity string `json:"identity"`
}

// Analyze computes the report. lookup resolves names to card rows; unresolved
// names produce a warning, never a silent drop.
func Analyze(commander string, entries []Entry, lookup func(name string) (*carddb.Card, bool)) Analysis {
	a := Analysis{Commander: commander, Curve: map[int]int{}}

	identity := ""
	if c, ok := lookup(commander); ok {
		identity = c.ColorIdentity
		a.Identity = identity
	}

	var salt []SaltEntry
	symbols := 0.0
	mvcards := 0.0
	roles := map[string]int{}
	var unresolved []string

	for _, e := range entries {
		if e.Board == "sideboard" {
			continue
		}
		if e.Board == "commander" || strings.EqualFold(e.Name, commander) {
			continue
		}
		c, ok := lookup(e.Name)
		if !ok {
			unresolved = append(unresolved, e.Name)
			continue
		}
		a.TotalMain += e.Count

		if c.IsLand() {
			a.Lands += e.Count
			a.Ratios.Lands += e.Count
		} else {
			mv := int(c.ManaValue)
			if mv > 10 {
				mv = 10
			}
			a.Curve[mv] += e.Count
			symbols += c.ManaValue * float64(e.Count)
			mvcards += float64(e.Count)
		}

		if identity != "" && !c.IdentityAllowed(carddb.ColorMask(identity)) {
			a.IdentityBad = append(a.IdentityBad, IdentityIssue{Name: e.Name, Identity: c.ColorIdentity})
		}
		if c.EDHRECSaltiness > 0 {
			salt = append(salt, SaltEntry{Name: e.Name, Salt: c.EDHRECSaltiness})
		}
		if c.GameChanger {
			a.GameChangers += e.Count
		}
		roles[RoleOf(c)] += e.Count
	}
	a.Ratios.Ramp = roles["ramp"]
	a.Ratios.Draw = roles["draw"]
	a.Ratios.Interaction = roles["interaction"]
	if mvcards > 0 {
		a.AvgMV = symbols / mvcards
	}

	sort.Slice(salt, func(i, j int) bool { return salt[i].Salt > salt[j].Salt })
	if len(salt) > 5 {
		salt = salt[:5]
	}
	a.Saltiest = salt

	if len(unresolved) > 0 {
		a.Warnings = append(a.Warnings, "could not verify: "+strings.Join(unresolved, ", "))
	}
	if a.TotalMain < 99 && a.TotalMain > 0 {
		a.Warnings = append(a.Warnings, itoa(99-a.TotalMain)+" cards short of 99")
	}
	if a.TotalMain > 99 {
		a.Warnings = append(a.Warnings, itoa(a.TotalMain-99)+" cards over 99")
	}
	if a.Lands > 0 && a.Lands < 30 {
		a.Warnings = append(a.Warnings, "only "+itoa(a.Lands)+" lands — most Commander decks want 32-42")
	}
	if roles["interaction"] < 6 {
		a.Warnings = append(a.Warnings, "little interaction — consider more removal and counters")
	}
	if roles["ramp"] < 8 {
		a.Warnings = append(a.Warnings, "light on ramp — most decks want 8-12 ramp spells")
	}
	if roles["draw"] < 6 {
		a.Warnings = append(a.Warnings, "light on card draw")
	}
	return a
}

// Diff computes added/removed card lines between two entry lists.
type Diff struct {
	Added   []Entry `json:"added"`
	Removed []Entry `json:"removed"`
}

// DiffEntries compares two card lists by name (case-insensitive), returning
// what was added and removed with counts.
func DiffEntries(before, after []Entry) Diff {
	counts := map[string]Entry{}
	order := map[string]int{}
	for i, e := range before {
		key := strings.ToLower(e.Name)
		counts[key] = Entry{Name: e.Name, Count: e.Count}
		order[key] = i
	}
	var d Diff
	for _, e := range after {
		key := strings.ToLower(e.Name)
		old, ok := counts[key]
		if !ok {
			d.Added = append(d.Added, e)
			continue
		}
		if e.Count > old.Count {
			d.Added = append(d.Added, Entry{Name: e.Name, Count: e.Count - old.Count})
		} else if e.Count < old.Count {
			d.Removed = append(d.Removed, Entry{Name: e.Name, Count: old.Count - e.Count})
		}
		delete(counts, key)
	}
	for _, e := range counts {
		d.Removed = append(d.Removed, e)
	}
	sortEntries(d.Added)
	sortEntries(d.Removed)
	return d
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
