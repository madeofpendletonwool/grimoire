package odds

// The outs search (MAD-336): "what are my outs?" — the remaining
// library filtered to the cards that answer a specified board object,
// reusing carddb's FTS over type line and oracle text to find the
// candidates and internal/deck's categorisation to label them. Every
// out returned is a real card from the actual remaining library: the
// graveyard, the hand and the battlefield are not outs, and neither is
// a card the search merely wishes were still in the deck.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/deck"
)

// ErrNoCardData reports an install without the card index: outs
// search needs type lines and oracle text, and saying so beats
// answering from names alone.
var ErrNoCardData = fmt.Errorf("odds: outs search needs the card index, which this install does not have")

// Searcher is the FTS seam — *carddb.Store satisfies it through
// SearchText, the same cards_fts index the deck builder searches with
// the weights turned toward rules text instead of names.
type Searcher interface {
	SearchText(ctx context.Context, q string, limit int) ([]*carddb.Card, error)
}

// Target is the board object the outs must answer: its card name when
// known, its card types (creature, enchantment, ...), and whether it
// is a spell on the stack (counters answer those; nothing else does).
type Target struct {
	Card    string   `json:"card,omitempty"`
	Types   []string `json:"types,omitempty"`
	OnStack bool     `json:"on_stack,omitempty"`
	Text    string   `json:"text,omitempty"`
}

// TargetFromCard builds the target from a carddb row: its type line
// split into the type words the answer matching works over.
func TargetFromCard(c *carddb.Card, onStack bool) Target {
	return Target{Card: c.Name, Types: typeWords(c.TypeLine), OnStack: onStack}
}

// TargetFromCardName resolves an object's card name through the
// lookup and builds the target from its row — the live-board path the
// outs endpoint takes. An unresolvable name is reported, never
// guessed past.
func TargetFromCardName(name string, onStack bool, lookup Lookup) (Target, error) {
	if name == "" {
		return Target{}, fmt.Errorf("odds: that object has no card identity to answer")
	}
	if lookup == nil {
		return Target{}, ErrNoCardData
	}
	c, ok := lookup(name)
	if !ok || c == nil {
		return Target{}, fmt.Errorf("odds: %q is not in the card index", name)
	}
	return TargetFromCard(c, onStack), nil
}

// TargetFromText builds a target from free text: a card the index
// knows by that name (fuzzy through SearchText), else the type words
// the text itself carries.
func TargetFromText(ctx context.Context, text string, search Searcher, lookup Lookup) (Target, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Target{}, fmt.Errorf("odds: the outs search needs something to answer")
	}
	if search != nil {
		if c, ok := lookup(text); ok {
			return TargetFromCard(c, false), nil
		}
		// A name-shaped span the index resolves: SearchText's first
		// hit under the name weights, accepted only when it is close —
		// the deck builder's own discipline for written names.
		if hits, err := search.SearchText(ctx, text, 1); err == nil && len(hits) > 0 &&
			strings.Contains(strings.ToLower(hits[0].Name), strings.ToLower(strings.Fields(text)[0])) {
			return TargetFromCard(hits[0], false), nil
		}
	}
	return Target{Text: text, Types: typeWords(text)}, nil
}

// typeWords extracts the card-type vocabulary a type line (or free
// text) carries — the words answer matching keys on.
func typeWords(typeLine string) []string {
	words := map[string]bool{}
	for _, word := range strings.Fields(typeLine) {
		w := strings.ToLower(strings.Trim(word, "—-,."))
		switch w {
		case "creature", "artifacts", "artifact", "enchantments", "enchantment",
			"land", "lands", "planeswalker", "planeswalkers", "battle", "battles":
			if strings.HasSuffix(w, "s") && len(w) > 4 {
				w = w[:len(w)-1]
			}
			words[w] = true
		}
	}
	out := make([]string, 0, len(words))
	for w := range words {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// The answer verbs — the oracle-text shapes that remove, neutralise
// or counter a board object, with the reason fragment each renders.
type verb struct {
	word string // the oracle-text marker, lowercased
}

var permanentVerbs = []verb{
	{word: "destroy target"},
	{word: "exile target"},
	{word: "return target"},
	{word: "destroy all"},
	{word: "exile all"},
}
var stackVerbs = []verb{{word: "counter target"}}

// Out is one answer card from the remaining library.
type Out struct {
	Name     string   `json:"name"`
	Count    int      `json:"count"` // copies still in the library
	Role     string   `json:"role"`  // internal/deck's classification
	Reasons  []string `json:"reasons"`
	ManaCost string   `json:"mana_cost,omitempty"`
}

// OutsAnswer is the outs search's whole reply.
type OutsAnswer struct {
	Target  Target `json:"target"`
	Outs    []Out  `json:"outs"`
	K       int    `json:"k_hits"` // total out copies remaining
	Note    string `json:"note,omitempty"`
	Bounded bool   `json:"bounded,omitempty"`
	// Odds is the chance of drawing at least one out in the given
	// draws — the hypergeometric riding along, same exactness.
	Draws       int     `json:"draws"`
	Probability float64 `json:"probability"`
	Rational    string  `json:"rational"`
}

// Outs searches the remaining library for cards that answer the
// target. The FTS pass proposes candidates over type line and oracle
// text; the library composition vetoes every card not still in it;
// the local verification labels why each survivor answers. Deterministic
// order: copies descending, then name — reproducible by construction.
func Outs(ctx context.Context, lib Library, target Target, search Searcher, lookup Lookup, draws int) (*OutsAnswer, error) {
	if !lib.Known {
		return nil, fmt.Errorf("odds: this seat's library composition is unknown — attach a deck to make it known")
	}
	if search == nil || lookup == nil {
		return nil, ErrNoCardData
	}
	ans := &OutsAnswer{Target: target, Draws: draws, Bounded: !lib.Exact}

	// Candidates straight out of the index, text-weighted: the verbs
	// that answer this target plus its type words.
	terms := target.Terms()
	hits, err := search.SearchText(ctx, strings.Join(terms, " "), 400)
	if err != nil {
		return nil, fmt.Errorf("odds: card search failed: %w", err)
	}
	for _, c := range hits {
		count, ok := lib.Counts[c.Name]
		if !ok || count <= 0 {
			continue // not still in the library: not an out
		}
		reasons := answers(c, target)
		if len(reasons) == 0 {
			continue // the FTS proposed it; the text does not back it
		}
		ans.Outs = append(ans.Outs, Out{
			Name: c.Name, Count: count, Role: deck.RoleOf(c),
			Reasons: reasons, ManaCost: c.ManaCost,
		})
		ans.K += count
	}
	sort.Slice(ans.Outs, func(i, j int) bool {
		if ans.Outs[i].Count != ans.Outs[j].Count {
			return ans.Outs[i].Count > ans.Outs[j].Count
		}
		return ans.Outs[i].Name < ans.Outs[j].Name
	})

	if draws > 0 {
		if draws > lib.N {
			draws = lib.N
		}
		ans.Draws = draws
		if p, err := AtLeast(lib.N, ans.K, draws, 1); err == nil {
			ans.Probability, _ = p.Float64()
			ans.Rational = ratString(p)
		}
	}
	if ans.Bounded {
		ans.Note = "unidentified cards have left this library, so the composition is an upper bound per name"
	}
	return ans, nil
}

// Terms is the FTS query vocabulary for a target: the answer verbs
// plus the target's type words.
func (t Target) Terms() []string {
	verbs := permanentVerbs
	if t.OnStack {
		verbs = stackVerbs
	}
	var terms []string
	for _, v := range verbs {
		terms = append(terms, strings.Fields(v.word)...)
	}
	terms = append(terms, t.Types...)
	return terms
}

// massCovers reports whether a mass clause ("destroy all …", "exile
// all …") sweeps the target's types. The clause after the verb names
// its scope: "creatures" covers a creature, "permanents" covers
// everything, "nonland permanents" covers everything but a land. With
// no type constraint every mass answer covers.
func massCovers(text, verb string, types []string) bool {
	if len(types) == 0 {
		return true
	}
	idx := strings.Index(text, verb)
	if idx < 0 {
		return false
	}
	seg := text[idx+len(verb):]
	if len(seg) > 32 {
		seg = seg[:32]
	}
	for _, ty := range types {
		if strings.Contains(seg, "permanent") {
			// "nonland permanents" answers everything except a land.
			if ty == "land" && strings.Contains(seg, "nonland") {
				continue
			}
			return true
		}
		if strings.Contains(seg, ty) {
			return true
		}
	}
	return false
}

// answers verifies locally that a card's text answers the target and
// returns the reason fragments — the oracle-text shapes each hit was
// verified against. FTS proposes; this disposes — a candidate that
// mentions "destroy" but only ever destroys lands does not answer an
// enchantment.
func answers(c *carddb.Card, target Target) []string {
	if c == nil {
		return nil
	}
	text := strings.ToLower(c.OracleText + " " + c.TypeLine)
	reasons := map[string]bool{}
	add := func(how string) { reasons[how] = true }
	verbs := permanentVerbs
	if target.OnStack {
		verbs = stackVerbs
	}
	for _, v := range verbs {
		if !strings.Contains(text, v.word) {
			continue
		}
		// A mass answer ("destroy all") sweeps what its clause names:
		// "Destroy all creatures" does not answer an enchantment, and
		// a sweeper of nonland permanents does not answer a land.
		if strings.HasSuffix(v.word, " all") {
			if massCovers(text, v.word, target.Types) {
				add(v.word)
			}
			continue
		}
		// On the stack there is only one thing to answer: the spell.
		if target.OnStack {
			add(v.word)
			continue
		}
		if len(target.Types) == 0 {
			// No type constraint: any targeted answer counts.
			add(v.word)
			continue
		}
		for _, ty := range target.Types {
			if strings.Contains(text, v.word+" "+ty) || strings.Contains(text, v.word+" permanent") {
				add(v.word + " " + ty)
				break
			}
			if strings.Contains(text, ty) {
				add(v.word + " (" + ty + ")")
				break
			}
		}
	}
	// Creatures and planeswalkers also fall to damage — the pure
	// arithmetic the combat engine already owns — and creatures to
	// shrink effects.
	if !target.OnStack {
		for _, ty := range target.Types {
			var shapes []string
			switch ty {
			case "creature":
				shapes = []string{"damage to any target", "damage to target creature", "damage divided as", "gets -"}
			case "planeswalker":
				shapes = []string{"damage to any target", "damage to target player or planeswalker", "damage divided as"}
			}
			for _, shape := range shapes {
				if strings.Contains(text, shape) {
					add(shape + " (" + ty + ")")
					break
				}
			}
		}
	}
	out := make([]string, 0, len(reasons))
	for r := range reasons {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}
