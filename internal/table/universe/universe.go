// Package universe is the Magic table's known-card universe (MAD-329,
// stage 4 of MAD-321): the few hundred cards in the attached decks a
// spoken or typed name is matched against before the ~28,000-card global
// index is ever consulted. Matching a mumbled name against the whole
// index is error-prone; matching it against the cards actually on the
// table is close to exact, and loading decklists is what collapses the
// identification problem — one paste per player per game.
//
// Resolution walks from the most certain scope to the loosest, per the
// issue's contract: exact match in the speaking seat's deck → fuzzy
// match in that deck → any other attached deck → the carddb global →
// unresolved. The fuzzy tiers reuse the credibility gates the rest of
// Grimoire already trusts — cards.NameMatches' word-subset and
// close-spelling rules — rather than inventing a second scoring scheme,
// and the global tier is carddb.Resolve's own 0.86 bar, unchanged.
//
// Decks are strongly encouraged but optional. A game without them
// resolves through the global tier only, which works and is simply
// worse; nothing here blocks setup.
//
// Library order is never modelled, here or anywhere: the library read
// this package offers is a composition (a multiset), and the exported
// ErrLibraryOrder is the out-loud refusal every order-dependent question
// gets — docs/table/interaction.md's "refused, rather than answered
// plausibly".
package universe

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// Method is which universe a resolution came from — the
// mtg_name_resolutions method column's vocabulary. The empty method is
// the in-memory marker for unresolved; unresolved names are never
// cached, so the column never holds one.
type Method string

const (
	MethodDeckExact Method = "deck_exact"
	MethodDeckFuzzy Method = "deck_fuzzy"
	MethodGlobal    Method = "global"
	MethodManual    Method = "manual"
	// MethodLLM is the model fallback's tier (MAD-331): the grammar
	// refused the utterance and the model identified the name against
	// the known-card universe. Distinguishable in the cache because an
	// audit trail that says how a name was found is only honest if the
	// model tier is not dressed up as a deck tier or a human's word.
	MethodLLM Method = "llm"
)

// Scope is where the winning match lived. The tiers of the resolution
// order are observable through it: the speaking seat's copy wins over
// another seat's, which wins over the index.
type Scope string

const (
	ScopeOwn    Scope = "own"    // the speaking seat's attached deck
	ScopeTable  Scope = "table"  // another seat's attached deck
	ScopeGlobal Scope = "global" // the card index
	ScopeCache  Scope = "cache"  // the per-game resolution cache
	ScopeManual Scope = "manual" // a human correction
)

// The confidence bands. These feed the confirmation ladder (MAD-331):
// anything below the exact band is a `confirm` candidate at most, and
// the ambiguity penalty is what pushes a genuinely two-way mumble toward
// a question rather than a silent write. Exact matches carry full
// confidence wherever they land — a verbatim name is not made more
// certain by being in a deck; the deck's value is that shorthand and
// mishearing resolve credibly at all.
const (
	ConfExact         = 1.0  // a verbatim (normalized) name, any scope
	ConfOwnFuzzy      = 0.95 // shorthand or a typo against the seat's own deck
	ConfOwnSpelling   = 0.90
	ConfOtherExact    = 0.95 // verbatim, but in someone else's list
	ConfOtherFuzzy    = 0.85
	ConfOtherSpelling = 0.80
	ConfGlobalExact   = 0.95 // verbatim against the index
	ConfGlobalFuzzy   = 0.80 // carddb's fuzzy bar, passed but not trusted
	ConfManual        = 1.0  // a human said what they meant
	// ConfLLM is the ceiling on a model identification (MAD-331): the
	// model is a fallback parser, never the front door, so nothing it
	// names is trusted above the fuzzy tiers — the confirmation ladder
	// holds everything the model says at confirm-at-best.
	ConfLLM            = 0.80
	ConfAmbiguousDelta = 0.15 // a tied runner-up at the same quality
)

// Resolution is one spoken name's identity: the card it means, how the
// match was made, how much to trust it, and where it came from. An
// unresolved Resolution carries no card and no method — it is reported,
// never guessed past.
type Resolution struct {
	Spoken     string  `json:"spoken"`
	Card       string  `json:"card,omitempty"`
	Method     Method  `json:"method,omitempty"`
	Confidence float64 `json:"confidence"`
	Scope      Scope   `json:"scope,omitempty"`
}

// Resolved reports whether the name found a card at all.
func (r Resolution) Resolved() bool { return r.Card != "" }

// ErrLibraryOrder is the refusal every order-dependent library question
// gets. Library composition is known once a deck is attached; order
// never is, and a tracker that pretends otherwise is untrustworthy the
// moment someone checks. Callers that can receive an order-dependent
// question return this sentinel instead of a plausible answer.
var ErrLibraryOrder = errors.New("library order is never modelled")

// Global is the global-index tier: the resolve the deck scoping defers
// to last. *carddb.Store satisfies it; tests supply a fake.
type Global interface {
	Resolve(ctx context.Context, name string) (*carddb.Card, bool)
}

/* ---------- the universe ---------- */

// Universe is one game's known-card universe: each seat's attached deck
// with its commander folded in. It is built, never stored — the fold's
// GAME_STARTED echo is its source of truth, so a rewind costs nothing.
type Universe struct {
	decks map[int]map[string]int
	seats []int
}

// FromState builds the universe from the fold: each seat's attached deck
// as GAME_STARTED echoed it (full, not remaining — a card in the
// graveyard is still a card known to be in this game) plus its
// commander.
func FromState(st *engine.State) *Universe {
	u := &Universe{decks: map[int]map[string]int{}}
	if st == nil {
		return u
	}
	for seat, p := range st.Seats {
		u.attach(seat, p.Deck, p.Commander)
	}
	u.sortSeats()
	return u
}

// FromSeats builds the universe from the setup pane's seat rows — the
// same shape GAME_STARTED will echo, for a table still being set.
func FromSeats(seats []engine.SeatConfig) *Universe {
	u := &Universe{decks: map[int]map[string]int{}}
	for i := range seats {
		u.attach(seats[i].Seat, seats[i].Deck, seats[i].Commander)
	}
	u.sortSeats()
	return u
}

// attach folds one seat's deck and commander into the universe.
func (u *Universe) attach(seat int, deck map[string]int, commander string) {
	m := make(map[string]int, len(deck)+1)
	for name, n := range deck {
		if name != "" && n > 0 {
			m[name] = n
		}
	}
	if commander != "" {
		if m[commander] < 1 {
			m[commander] = 1
		}
	}
	if len(m) == 0 {
		return
	}
	u.decks[seat] = m
	if !containsSeat(u.seats, seat) {
		u.seats = append(u.seats, seat)
	}
}

func containsSeat(seats []int, seat int) bool {
	for _, s := range seats {
		if s == seat {
			return true
		}
	}
	return false
}

func (u *Universe) sortSeats() {
	for i := 1; i < len(u.seats); i++ {
		for j := i; j > 0 && u.seats[j] < u.seats[j-1]; j-- {
			u.seats[j], u.seats[j-1] = u.seats[j-1], u.seats[j]
		}
	}
}

// Attached reports how many seats carry a deck — the setup hint's fact.
// Zero is a working game with worse identification, never a blocker.
func (u *Universe) Attached() int { return len(u.decks) }

// Seats lists the seats carrying an attached deck, in seating order —
// the prompt's and the model gate's walk order over the known cards
// (MAD-331).
func (u *Universe) Seats() []int {
	out := make([]int, len(u.seats))
	copy(out, u.seats)
	return out
}

// Cards is one seat's known-card universe as a name → count multiset.
// The seat's own client and the cache's prompts read this; nobody may
// read an order out of it, because none exists.
func (u *Universe) Cards(seat int) map[string]int {
	src := u.decks[seat]
	out := make(map[string]int, len(src))
	for name, n := range src {
		out[name] = n
	}
	return out
}

// LibraryView is a seat's library as it may honestly be described: a
// composition and whether that composition is exact. There is no order
// field, no next-card field, no position field — the shape is the
// refusal.
type LibraryView struct {
	Known  bool           `json:"known"`
	Exact  bool           `json:"exact,omitempty"`
	Counts map[string]int `json:"counts,omitempty"`
}

// Library answers the one library question that has an answer: what is
// (still) in the seat's library, as a multiset. Remaining counts come
// from the fold's composition bookkeeping; the exact flag clears once
// unidentified cards have left a known library, and questions that need
// exactness must say so rather than guess. Order-dependent questions are
// not a parameter away from working — return ErrLibraryOrder to them.
func (u *Universe) Library(st *engine.State, seat int) LibraryView {
	if st == nil {
		return LibraryView{}
	}
	p, ok := st.Seats[seat]
	if !ok || len(p.LibraryComp) == 0 {
		return LibraryView{}
	}
	counts := make(map[string]int, len(p.LibraryComp))
	for name, n := range p.LibraryComp {
		if n > 0 {
			counts[name] = n
		}
	}
	return LibraryView{Known: true, Exact: p.LibraryExact, Counts: counts}
}

/* ---------- resolution ---------- */

// Resolve matches one spoken or typed name for one speaking seat,
// walking the tiers in the contract's order: exact in the seat's own
// deck, fuzzy in it, then any other attached deck, then the global
// index, then honest unresolved. A wrong match is worse than a miss, so
// every fuzzy tier is behind the same credibility gates the rest of
// Grimoire uses, and a tier with two equally good candidates still
// resolves — deterministically — but at reduced confidence, which is the
// ladder's business, not the resolver's.
func (u *Universe) Resolve(ctx context.Context, seat int, spoken string, global Global) Resolution {
	spoken = strings.TrimSpace(spoken)
	res := Resolution{Spoken: spoken}
	if spoken == "" {
		return res
	}
	// Tiers 1–2: the speaking seat's deck. The seat that said it owns
	// the tie: two decks containing the same name resolve to the
	// speaker's copy.
	if name, q, amb := u.matchDeck(spoken, seat); name != "" {
		return deckResolution(spoken, name, q, amb, ScopeOwn, tierConfs{
			exact: ConfExact, subset: ConfOwnFuzzy, spelling: ConfOwnSpelling})
	}
	// Tier 3: the rest of the table, in seating order.
	for _, s := range u.seats {
		if s == seat {
			continue
		}
		if name, q, amb := u.matchDeck(spoken, s); name != "" {
			return deckResolution(spoken, name, q, amb, ScopeTable, tierConfs{
				exact: ConfOtherExact, subset: ConfOtherFuzzy, spelling: ConfOtherSpelling})
		}
	}
	// Tier 4: the global index, behind its own gate. carddb.Resolve
	// already refuses what is not actually close; the deck tiers simply
	// ran first.
	if global != nil {
		if c, ok := global.Resolve(ctx, spoken); ok && c != nil && c.Name != "" {
			if carddb.NormalizeName(spoken) == carddb.NormalizeName(c.Name) {
				return Resolution{Spoken: spoken, Card: c.Name, Method: MethodGlobal,
					Confidence: ConfGlobalExact, Scope: ScopeGlobal}
			}
			return Resolution{Spoken: spoken, Card: c.Name, Method: MethodGlobal,
				Confidence: ConfGlobalFuzzy, Scope: ScopeGlobal}
		}
	}
	return res
}

// tierConfs is one deck tier's confidence ladder: what a verbatim, a
// word-subset and a close-spelling match are worth in that scope.
type tierConfs struct {
	exact    float64
	subset   float64
	spelling float64
}

// deckResolution stamps one deck-tier win with its method and confidence.
func deckResolution(spoken, card string, q quality, ambiguous bool, scope Scope, confs tierConfs) Resolution {
	res := Resolution{Spoken: spoken, Card: card, Scope: scope}
	switch q {
	case qExact:
		res.Method, res.Confidence = MethodDeckExact, confs.exact
	case qSubset:
		res.Method, res.Confidence = MethodDeckFuzzy, confs.subset
	default:
		res.Method, res.Confidence = MethodDeckFuzzy, confs.spelling
	}
	if ambiguous && q != qExact {
		res.Confidence -= ConfAmbiguousDelta
	}
	return res
}

// Candidates lists up to limit plausible canonical names for one spoken
// span, best first, walking the deck tiers in Resolve's order: the
// speaking seat's deck, then the table's decks in seating order. Only
// gated matches (word-subset and close-spelling, the same credibility
// rules Resolve uses) are candidates, and an exact match is not listed —
// an exact match is Resolve's answer, not a question. The global index
// is deliberately absent: a tappable answer set must be small and
// credible, and 28,000 cards are neither. This exists for the ladder's
// ask rung (MAD-331): a genuinely ambiguous name becomes a one-tap
// question whose options are the candidates themselves.
func (u *Universe) Candidates(seat int, spoken string, limit int) []string {
	spoken = strings.TrimSpace(spoken)
	if spoken == "" || limit <= 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	tiers := make([]int, 0, len(u.seats)+1)
	tiers = append(tiers, seat)
	for _, s := range u.seats {
		if s != seat {
			tiers = append(tiers, s)
		}
	}
	for _, s := range tiers {
		deck := u.decks[s]
		if len(deck) == 0 {
			continue
		}
		names := make([]string, 0, len(deck))
		for name := range deck {
			names = append(names, name)
		}
		sortNames(names)
		// Rank this tier's gated candidates: class first, score within
		// class, codepoint order as the deterministic tie-break.
		type scored struct {
			name  string
			q     quality
			score float64
		}
		var hits []scored
		for _, name := range names {
			q, score := classify(spoken, name)
			if q == qNone || q == qExact {
				continue
			}
			hits = append(hits, scored{name, q, score})
		}
		for i := 1; i < len(hits); i++ {
			for j := i; j > 0; j-- {
				a, b := hits[j], hits[j-1]
				if a.q > b.q || (a.q == b.q && (a.score > b.score || (a.score == b.score && a.name < b.name))) {
					hits[j], hits[j-1] = hits[j-1], hits[j]
					continue
				}
				break
			}
		}
		for _, h := range hits {
			if seen[h.name] {
				continue
			}
			seen[h.name] = true
			out = append(out, h.name)
			if len(out) == limit {
				return out
			}
		}
	}
	return out
}

// matchDeck scores one seat's deck against the spoken phrase. It returns
// the best name, the quality class it won by, and whether a runner-up
// tied — the ambiguity the confirmation ladder should hear about.
func (u *Universe) matchDeck(spoken string, seat int) (string, quality, bool) {
	deck := u.decks[seat]
	if len(deck) == 0 {
		return "", qNone, false
	}
	names := make([]string, 0, len(deck))
	for name := range deck {
		names = append(names, name)
	}
	sortNames(names)
	best, bestQ, bestScore := "", qNone, 0.0
	runnerUp := false
	for _, name := range names {
		q, score := classify(spoken, name)
		if q == qNone {
			continue
		}
		if q > bestQ || (q == bestQ && score > bestScore) {
			best, bestQ, bestScore = name, q, score
			runnerUp = false
			continue
		}
		if q == bestQ && score == bestScore && best != name {
			runnerUp = true
		}
	}
	return best, bestQ, runnerUp
}

// quality is a match's class, ordered from weakest to strongest. The
// classes are cards.NameMatches' two credibility rules, with the verbatim
// case pulled out above them.
type quality int

const (
	qNone     quality = iota
	qSpelling         // close spelling, within NameMatches' edit budget
	qSubset           // every spoken word is a word of the card ("Rhystic")
	qExact            // the normalized name, or a split card's front face
)

// classify scores one candidate name against the spoken phrase. Score
// orders candidates within a class; across classes, class alone orders.
func classify(spoken, card string) (quality, float64) {
	want, got := carddb.NormalizeName(spoken), carddb.NormalizeName(card)
	if want == "" || got == "" {
		return qNone, 0
	}
	if want == got {
		return qExact, 1
	}
	// A split or double-faced card spoken as one face is that face.
	if front, _, ok := strings.Cut(got, " // "); ok && front == want {
		return qExact, 1
	}
	// The shared credibility gate: word subset first ("Bolt" →
	// "Lightning Bolt"), then close spelling ("gient growth"), both from
	// cards.NameMatches. The local scoring only ranks gated candidates.
	if !cards.NameMatches(spoken, card) {
		return qNone, 0
	}
	sw, cw := nameWords(spoken), nameWords(card)
	if subsetOf(sw, cw) {
		return qSubset, float64(len(sw)) / float64(len(cw))
	}
	s, c := alnum(want), alnum(got)
	return qSpelling, editSimilarity(s, c)
}

// subsetOf reports whether every word of a is a word of b.
func subsetOf(a, b []string) bool {
	if len(a) == 0 {
		return false
	}
	set := make(map[string]bool, len(b))
	for _, w := range b {
		set[w] = true
	}
	for _, w := range a {
		if !set[w] {
			return false
		}
	}
	return true
}

// nameWords splits a name into its lowercased words — the same split
// cards.NameMatches word-subset rule uses.
func nameWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// alnum is the letters-and-digits-only fold the close-spelling rule
// compares under: spacing and punctuation stop mattering.
func alnum(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// editSimilarity is 1 − distance/longer over two folded names, for
// ranking within the spelling class only. The gate already ran.
func editSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	longer := len(a)
	if len(b) > longer {
		longer = len(b)
	}
	return 1 - float64(editDistance(a, b))/float64(longer)
}

// editDistance is Levenshtein distance with a two-row buffer.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			m := prev[j] + 1
			if v := curr[j-1] + 1; v < m {
				m = v
			}
			if v := prev[j-1] + cost; v < m {
				m = v
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// sortNames gives deck walks a deterministic order: plain codepoint
// order, the way the engine sorts seats.
func sortNames(names []string) {
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
}
