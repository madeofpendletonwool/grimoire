// Package grammar is the Magic table's deterministic intent parser (MAD-330,
// stage 4 of MAD-321): table talk in, a typed engine.Action plus a
// confidence out — never applied state. ADR 10's "grammar before model":
// the phrasings a Commander table actually says are a small, fixed
// vocabulary, and matching them by hand is instant, free, offline,
// consistent and unit-testable — everything a model is not.
//
// The contract, from the issue and docs/table/interaction.md:
//
//   - Every parse resolves against the **current game state** — whose turn
//     it is, which step, what is on the battlefield — because that is what
//     disambiguates most table talk ("sac this" is only a sentence against
//     a board).
//   - Output is Action plus confidence, and the action carries
//     Source "grammar" so the log's audit trail says how it got entered.
//   - Unparseable input returns no-parse cleanly. **A wrong parse is worse
//     than no parse**: the LLM fallback (MAD-331) only ever sees input
//     this package refused, so a guess here would never be corrected.
//
// Card names resolve through the Names seam so the deck-scoped
// known-card universe (MAD-329) does the identification — the grammar
// never matches against the card index itself. Pass nil and spoken names
// resolve to nothing; the structural shapes still work, and every
// name-dependent parse degrades to no-parse rather than a guess.
package grammar

import (
	"context"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

// Confidence bands. These feed the confirmation ladder (MAD-331): the
// auto tier is the certain and reference bands, confirm is everything
// resolved below them, and the unresolved band exists so an explicit
// shape with an unrecognized name is applied optimistically-highlighted
// or asked about — never silently written as though it were certain.
const (
	// ConfCertain: pure structure, nothing referenced that could be
	// misheard ("pass", "-3", "draw two").
	ConfCertain = 1.0
	// ConfReference: a referenced seat or a verbatim-resolved card
	// ("Bob takes 3", "cast Cultivate").
	ConfReference = 0.95
	// ConfDerived: state-derived disambiguation ("play X" ruled a land
	// because one with that name is on the battlefield).
	ConfDerived = 0.90
	// ConfMulti: several objects matched a spoken name; a deterministic
	// tie-break chose, and the ladder should watch it.
	ConfMulti = 0.80
	// ConfReferent: "it"/"this"/"that" — the referent is inferred from
	// recency on the speaker's own board.
	ConfReferent = 0.70
	// ConfUnresolved: the shape is explicit but the name did not resolve
	// ("cast blorptidious"). The name is recorded as spoken, never
	// guessed to be a real card.
	ConfUnresolved = 0.30
)

// Result is one parse. OK false is a clean no-parse — not an error, not a
// guess — and it is the signal the LLM fallback (MAD-331) waits for. When
// OK is true, Action carries the confidence and the "grammar" source
// stamp; the caller may override the source (voice-transcribed grammar
// output is still voice entry) before submitting.
type Result struct {
	OK     bool
	Action engine.Action
}

// NameInfo is what the name seam learned about one spoken span: the
// canonical card when it resolved, the card's type-line words when the
// source knows them (the card index does; decklists do not), and the
// resolution confidence from universe's tiers.
type NameInfo struct {
	Card       string
	Types      []string
	Confidence float64
}

// Names is the card-name resolution seam. The grammar never touches the
// card index directly — identification is MAD-329's resolved-order job
// (own deck → table decks → global), and this interface is the whole
// dependency. See FromUniverse for the standard wiring.
type Names interface {
	ResolveName(ctx context.Context, seat int, spoken string) NameInfo
}

// UniverseNames adapts the deck-scoped resolver. The global tier is left
// out (nil): the grammar runs offline by contract, and the install that
// wants index-backed types and matching wires its own Names with carddb
// behind it — 4c's job, not the parser's.
type UniverseNames struct {
	U *universe.Universe
}

// ResolveName runs the universe's tier order with no global index.
func (n UniverseNames) ResolveName(ctx context.Context, seat int, spoken string) NameInfo {
	if n.U == nil {
		return NameInfo{}
	}
	r := n.U.Resolve(ctx, seat, spoken, nil)
	return NameInfo{Card: r.Card, Confidence: r.Confidence}
}

// FromUniverse is the standard offline wiring.
func FromUniverse(u *universe.Universe) Names { return UniverseNames{U: u} }

// Parse turns one utterance by one seat into an Action, or refuses it.
// st may be nil (a setup-phase table); names may be nil (nothing
// resolves). Parse is total: it never returns an error, never panics on
// garbage, and never mutates the state it is handed.
func Parse(ctx context.Context, seat int, utterance string, st *engine.State, names Names) Result {
	if st == nil {
		st = engine.NewState()
	}
	toks := tokenize(utterance)
	if len(toks) == 0 {
		return Result{}
	}
	if isQuestion(toks) {
		return Result{}
	}
	toks = stripIntent(toks)
	if len(toks) == 0 {
		return Result{}
	}
	g := &game{ctx: ctx, st: st, seat: seat, names: names}
	for _, rule := range []ruleFunc{
		g.parseFlow,        // pass · go · resolves · take it · no response
		g.parseCounterFlag, // counters, energy, poison, monarch, initiative
		g.parseToken,       // make two Treasures · three 1/1 soldiers
		g.parseTapUntap,    // tap Sol Ring · untap everything
		g.parseZone,        // sac this · exile it · bounce it · draw · mill · concede
		g.parseLandCast,    // play a Forest · land: Forest · cast Rhystic · play my Study
		g.parseDamageLife,  // -3 · Bob takes 3 · attack Sarah for six · Bolt Alice for 3
		g.parseCombat,      // attack Bob with everything · Atraxa blocks Krenko
		g.parseBareName,    // Rhystic · Forest · Lightning Bolt Alice
	} {
		if a, ok := rule(toks); ok {
			if a.Seat == 0 {
				a.Seat = seat
			}
			if a.Source == "" {
				a.Source = "grammar"
			}
			if a.Confidence == 0 {
				a.Confidence = ConfCertain
			}
			return Result{OK: true, Action: a}
		}
	}
	return Result{}
}

// ruleFunc is one shape family: consume the tokens or decline.
type ruleFunc func(toks []string) (engine.Action, bool)
