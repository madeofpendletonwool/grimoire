package odds

// The derived-library reads and the draw-odds answer (MAD-336): with a
// deck attached (MAD-329) and the log recording every card drawn,
// played, milled or exiled, the remaining library is the fold's
// composition bookkeeping — derived, never guessed. DrawOdds turns a
// category over that composition into an exact hypergeometric with a
// receipt.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/deck"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// Lookup resolves a card name to its carddb row — *carddb.Store.Get's
// shape; nil-lookup callers can still ask exact-name questions.
type Lookup func(name string) (*carddb.Card, bool)

// Library is a seat's remaining library as it may honestly be
// described: a multiset, whether it is exact, and its size. There is
// no order field — the shape is the refusal.
type Library struct {
	Known  bool           `json:"known"`
	Exact  bool           `json:"exact,omitempty"`
	N      int            `json:"n,omitempty"`
	Counts map[string]int `json:"counts,omitempty"`
}

// Of derives a seat's remaining library from the fold. A seat without
// an attached deck is an honest Known=false, never an empty library in
// disguise.
func Of(st *engine.State, seat int) (Library, error) {
	if st == nil {
		return Library{}, fmt.Errorf("odds: no game state")
	}
	p, ok := st.Seats[seat]
	if !ok {
		return Library{}, fmt.Errorf("odds: seat %d is not in this game", seat)
	}
	if len(p.LibraryComp) == 0 {
		return Library{Known: false}, nil
	}
	counts := make(map[string]int, len(p.LibraryComp))
	n := 0
	for name, c := range p.LibraryComp {
		if c > 0 {
			counts[name] = c
			n += c
		}
	}
	return Library{Known: true, Exact: p.LibraryExact, N: n, Counts: counts}, nil
}

/* ---------- categories over the composition ---------- */

// Category names the vocabularies a draw question may ask for. The
// role categories reuse internal/deck's classification (RoleOf) —
// there is no second scoring scheme here — and the wipe category is
// the oracle-text shape every table recognises.
const (
	CatLands       = "lands"
	CatWipes       = "board_wipes"
	CatRamp        = "ramp"
	CatDraw        = "draw"
	CatInteraction = "interaction"
	CatAnyCard     = "any_card"
	CatCard        = "card" // a named card: Card field carries the name
)

// MatchCategory builds the category matcher. CatCard answers every
// name (a named-card question counts its own copies without a lookup);
// the rest need the card data a Lookup carries, because "a land" is a
// type line and "ramp" is oracle text — neither lives in the
// composition.
func MatchCategory(category string) func(*carddb.Card) bool {
	switch category {
	case CatCard, CatAnyCard:
		return func(*carddb.Card) bool { return true }
	case CatLands:
		return func(c *carddb.Card) bool { return c != nil && c.IsLand() }
	case CatWipes:
		return func(c *carddb.Card) bool {
			if c == nil {
				return false
			}
			t := strings.ToLower(c.OracleText)
			return strings.Contains(t, "destroy all") ||
				strings.Contains(t, "exile all") ||
				strings.Contains(t, "damage to each") ||
				strings.Contains(t, "each creature and") ||
				strings.Contains(t, "sacrifice all")
		}
	case CatRamp:
		return func(c *carddb.Card) bool { return c != nil && deck.RoleOf(c) == "ramp" }
	case CatDraw:
		return func(c *carddb.Card) bool { return c != nil && deck.RoleOf(c) == "draw" }
	case CatInteraction:
		return func(c *carddb.Card) bool { return c != nil && deck.RoleOf(c) == "interaction" }
	}
	return nil
}

/* ---------- the answer ---------- */

// Answer is one exact draw probability over the remaining library. The
// rational string is the receipt: any two runs of the same question
// over the same composition produce byte-identical answers.
type Answer struct {
	Question string `json:"question"`
	Category string `json:"category,omitempty"`
	Card     string `json:"card,omitempty"`

	// N is the library size the hypergeometric drew from, K the hits
	// in it, Draws the horizon asked about, AtLeast the least number
	// of hits asked for.
	N       int `json:"n"`
	K       int `json:"k_hits"`
	Draws   int `json:"draws"`
	AtLeast int `json:"at_least"`

	Probability float64 `json:"probability"`
	Rational    string  `json:"rational"` // exact, lowest terms
	Percent     float64 `json:"percent"`

	// Hits names the cards the K counted, with their remaining copies —
	// the out-loud version of the match, so nobody has to trust a label.
	Hits map[string]int `json:"hits,omitempty"`

	// Bounded is set when unidentified cards have left a known library:
	// the composition is then an upper bound per name and the float is
	// an estimate built on that bound, which is worth saying.
	Bounded bool `json:"bounded,omitempty"`
	// Note carries the caveats — the bounded case, the by-turn draw
	// assumption, the missing card data. An answer without its caveats
	// is a guess wearing numbers.
	Note string `json:"note,omitempty"`
}

// DrawOdds answers "the chance of drawing at least k of this category
// in the next n cards" over a derived library: exact hypergeometric,
// reproducible, with the hit multiset named. match nil + Card set
// counts the copies of that card alone.
func DrawOdds(question string, lib Library, category, card string, match func(*carddb.Card) bool, lookup Lookup, n, k int) (*Answer, error) {
	if !lib.Known {
		return nil, fmt.Errorf("odds: this seat's library composition is unknown — attach a deck to make it known")
	}
	if n < 0 {
		return nil, fmt.Errorf("odds: draws must not be negative")
	}
	if k <= 0 {
		k = 1
	}
	// Asking for more cards than remain is the "rest of the game"
	// case: cap at the library and say so.
	capped := false
	if n > lib.N {
		n = lib.N
		capped = true
	}
	a := &Answer{Question: question, Category: category, Card: card, N: lib.N, Draws: n, AtLeast: k, Bounded: !lib.Exact}
	K := 0
	hits := map[string]int{}
	for name, count := range lib.Counts {
		if card != "" {
			// A named card counts its own copies: case-insensitive
			// over the composition, no lookup needed.
			if strings.EqualFold(name, card) {
				K += count
				hits[name] = count
			}
			continue
		}
		c, _ := lookup(name)
		if match != nil && !match(c) {
			continue
		}
		K += count
		hits[name] = count
	}
	a.K = K
	if len(hits) > 0 {
		a.Hits = hits
	}
	p, err := AtLeast(lib.N, K, n, k)
	if err != nil {
		return nil, err
	}
	a.Probability, _ = p.Float64()
	a.Rational = ratString(p)
	a.Percent = a.Probability * 100
	if capped {
		a.Note = appendNote(a.Note,
			fmt.Sprintf("capped at the %d cards the library still holds", lib.N))
	}
	if a.Bounded {
		a.Note = appendNote(a.Note,
			"unidentified cards have left this library, so the composition is an upper bound per name — these odds are built on that bound")
	}
	return a, nil
}

// appendNote joins a caveat onto whatever caveats already stand.
func appendNote(note, add string) string {
	if note == "" {
		return add
	}
	return note + "; " + add
}

// DrawsByTurn is the draw count a "by turn t" question means: one draw
// per own turn from the current turn through t. The assumption is
// stated in the answer's note — extra draws (or missed draws) are the
// table's reality and this is the arithmetic under one-draw-per-turn.
func DrawsByTurn(current, t int) (int, error) {
	if t < 1 {
		return 0, fmt.Errorf("odds: turns start at 1")
	}
	if t < current {
		return 0, fmt.Errorf("odds: turn %d is behind the game's current turn %d", t, current)
	}
	if current < 1 {
		current = 1 // a game not yet into its first turn counts from turn 1
	}
	return t - current + 1, nil
}

// SortedHitNames gives the hit multiset a deterministic order for
// rendering — name order, the engine's own convention for walks.
func SortedHitNames(hits map[string]int) []string {
	out := make([]string, 0, len(hits))
	for name := range hits {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
