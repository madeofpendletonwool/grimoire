package universe

import (
	"context"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// pod is the standing test table: Collin and Bob carry decks, Alice is
// deckless — decks are optional, and her seat exercises the fallthrough.
func pod() []engine.SeatConfig {
	return []engine.SeatConfig{
		{Seat: 1, Name: "Collin", Commander: "Atraxa, Praetors' Voice", Deck: map[string]int{
			"Rhystic Study": 1, "Sol Ring": 1, "Kodama's Reach": 1,
			"Wear // Tear": 1, "Island": 20,
		}},
		{Seat: 2, Name: "Bob", Commander: "Krenko, Mob Boss", Deck: map[string]int{
			"Sol Ring": 1, "Rhystic Cave": 1, "Mountain": 20,
		}},
		{Seat: 3, Name: "Alice", Commander: "Urza, Lord High Artificer"},
	}
}

// started folds a START_GAME over the pod — the log's GAME_STARTED echo
// is the universe's source of truth, so tests build it the honest way.
func started(t *testing.T, seats []engine.SeatConfig) *engine.State {
	t.Helper()
	evs, err := engine.Apply(engine.NewState(), engine.Action{Kind: engine.ActionStartGame, Seats: seats})
	if err != nil {
		t.Fatalf("start game: %v", err)
	}
	return engine.Fold(evs)
}

// fakeGlobal is the index tier's stand-in: it counts calls, because the
// deck tiers' whole point is that the index is never reached when a deck
// already answered.
type fakeGlobal struct {
	calls int
	by    map[string]string
}

func (f *fakeGlobal) Resolve(ctx context.Context, name string) (*carddb.Card, bool) {
	f.calls++
	if card, ok := f.by[carddb.NormalizeName(name)]; ok {
		return &carddb.Card{Name: card}, true
	}
	return nil, false
}

func resolve(t *testing.T, st *engine.State, seat int, spoken string, global Global) Resolution {
	t.Helper()
	return FromState(st).Resolve(context.Background(), seat, spoken, global)
}

func TestOwnDeckExact(t *testing.T) {
	r := resolve(t, started(t, pod()), 1, "Sol Ring", nil)
	if !r.Resolved() || r.Card != "Sol Ring" {
		t.Fatalf("Sol Ring → %+v", r)
	}
	if r.Method != MethodDeckExact || r.Scope != ScopeOwn {
		t.Fatalf("method/scope = %s/%s, want deck_exact/own", r.Method, r.Scope)
	}
	if r.Confidence != ConfExact {
		t.Fatalf("confidence = %v, want %v", r.Confidence, ConfExact)
	}
}

// The deck-scope win: shorthand that would be ambiguous against the
// index is near-exact against one deck.
func TestOwnDeckShorthand(t *testing.T) {
	r := resolve(t, started(t, pod()), 1, "Rhystic", nil)
	if r.Card != "Rhystic Study" {
		t.Fatalf("Rhystic → %q", r.Card)
	}
	if r.Method != MethodDeckFuzzy || r.Scope != ScopeOwn || r.Confidence != ConfOwnFuzzy {
		t.Fatalf("got %+v", r)
	}
}

// The other deck's Rhystic must not steal the speaker's shorthand, and
// the speaker's own copy wins when both decks hold the name.
func TestSpeakingSeatCopyWins(t *testing.T) {
	st := started(t, pod())
	// Both decks contain Sol Ring verbatim: the speaking seat's copy is
	// the one that answers.
	r := resolve(t, st, 2, "Sol Ring", nil)
	if r.Card != "Sol Ring" || r.Scope != ScopeOwn {
		t.Fatalf("seat 2 Sol Ring → %+v, want scope own", r)
	}
	// The fuzzy conflict: "Rhystic" matches Collin's Study and Bob's
	// Cave; the speaker resolves against their own list first.
	r = resolve(t, st, 2, "Rhystic", nil)
	if r.Card != "Rhystic Cave" || r.Scope != ScopeOwn {
		t.Fatalf("seat 2 Rhystic → %+v, want own Rhystic Cave", r)
	}
	// Spoken by the deckless seat, it is another seat's card.
	r = resolve(t, st, 3, "Rhystic Cave", nil)
	if r.Card != "Rhystic Cave" || r.Scope != ScopeTable || r.Method != MethodDeckExact {
		t.Fatalf("seat 3 Rhystic Cave → %+v, want table deck_exact", r)
	}
}

func TestTableDeckFuzzy(t *testing.T) {
	r := resolve(t, started(t, pod()), 3, "Krenko", nil)
	if r.Card != "Krenko, Mob Boss" || r.Scope != ScopeTable {
		t.Fatalf("Krenko → %+v", r)
	}
	if r.Method != MethodDeckFuzzy || r.Confidence != ConfOtherFuzzy {
		t.Fatalf("got %+v", r)
	}
}

// A split card spoken as one face is that face — the exact class, the
// same rule carddb.Resolve applies at the index tier.
func TestSplitCardFrontFace(t *testing.T) {
	r := resolve(t, started(t, pod()), 1, "Wear", nil)
	if r.Card != "Wear // Tear" || r.Method != MethodDeckExact || r.Confidence != ConfExact {
		t.Fatalf("Wear → %+v", r)
	}
}

// Punctuation and accents fold at the exact class: "Kodamas Reach" and
// "Kodama's Reach" normalize alike — the same rule carddb.Resolve
// applies at the index tier.
func TestPunctuationFolds(t *testing.T) {
	r := resolve(t, started(t, pod()), 1, "Kodamas Reach", nil)
	if r.Card != "Kodama's Reach" || r.Scope != ScopeOwn {
		t.Fatalf("Kodamas Reach → %+v", r)
	}
	if r.Method != MethodDeckExact || r.Confidence != ConfExact {
		t.Fatalf("got %+v", r)
	}
}

// The commander is part of the seat's universe: "cast Atraxa" resolves
// even though she is not in the library composition.
func TestCommanderInUniverse(t *testing.T) {
	r := resolve(t, started(t, pod()), 1, "Atraxa", nil)
	if r.Card != "Atraxa, Praetors' Voice" || r.Scope != ScopeOwn || r.Method != MethodDeckFuzzy {
		t.Fatalf("Atraxa → %+v", r)
	}
}

// A deck tier answers before the index is ever consulted — the
// materially-better-rate property, as a count.
func TestDeckScopeBeatsGlobal(t *testing.T) {
	global := &fakeGlobal{by: map[string]string{"rhystic": "Rhystic Deluge"}}
	r := resolve(t, started(t, pod()), 1, "Rhystic", global)
	if r.Card != "Rhystic Study" || r.Scope != ScopeOwn {
		t.Fatalf("Rhystic → %+v, want own Study", r)
	}
	if global.calls != 0 {
		t.Fatalf("global consulted %d times behind a deck answer", global.calls)
	}
}

func TestGlobalFallback(t *testing.T) {
	global := &fakeGlobal{by: map[string]string{
		"cyclonic rift": "Cyclonic Rift", "cylonic rift": "Cyclonic Rift"}}
	st := started(t, pod())
	// Verbatim against the index: the deckless seat still resolves.
	r := resolve(t, st, 3, "Cyclonic Rift", global)
	if r.Card != "Cyclonic Rift" || r.Method != MethodGlobal || r.Scope != ScopeGlobal {
		t.Fatalf("Cyclonic Rift → %+v", r)
	}
	if r.Confidence != ConfGlobalExact {
		t.Fatalf("confidence = %v, want %v", r.Confidence, ConfGlobalExact)
	}
	// A mumble the index repairs: passed its bar, but not trusted like a
	// deck answer.
	r = resolve(t, st, 3, "cylonic rift", global)
	if r.Card != "Cyclonic Rift" || r.Confidence != ConfGlobalFuzzy {
		t.Fatalf("cylonic rift → %+v", r)
	}
}

func TestUnresolved(t *testing.T) {
	r := resolve(t, started(t, pod()), 1, "Blorple Warp", &fakeGlobal{})
	if r.Resolved() || r.Method != "" || r.Confidence != 0 || r.Scope != "" {
		t.Fatalf("Blorple Warp → %+v, want unresolved", r)
	}
}

// A two-way tie inside one deck still resolves deterministically, at
// reduced confidence — the ladder's `confirm`, not a silent write.
func TestAmbiguityLowersConfidence(t *testing.T) {
	seats := pod()
	seats[0].Deck["Sol Talisman"] = 1
	r := resolve(t, started(t, seats), 1, "Sol", nil)
	if !r.Resolved() {
		t.Fatalf("Sol → unresolved")
	}
	if r.Card != "Sol Ring" && r.Card != "Sol Talisman" {
		t.Fatalf("Sol → %q", r.Card)
	}
	if r.Confidence != ConfOwnFuzzy-ConfAmbiguousDelta && !almostEqual(r.Confidence, ConfOwnFuzzy-ConfAmbiguousDelta) {
		t.Fatalf("confidence = %v, want %v", r.Confidence, ConfOwnFuzzy-ConfAmbiguousDelta)
	}
}

// almostEqual compares confidences at float tolerance — 0.95 − 0.15 is
// not bit-exact 0.8, and the ladder cares about bands, not bits.
func almostEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// The tier ladder, end to end: confidence never rises as scope loosens.
func TestConfidenceOrdering(t *testing.T) {
	st := started(t, pod())
	global := &fakeGlobal{by: map[string]string{"mystic remora": "Mystic Remora"}}
	cases := []struct {
		spoken string
		seat   int
		card   string
		method Method
		scope  Scope
		conf   float64
	}{
		{"Sol Ring", 1, "Sol Ring", MethodDeckExact, ScopeOwn, ConfExact},
		{"Rhystic", 1, "Rhystic Study", MethodDeckFuzzy, ScopeOwn, ConfOwnFuzzy},
		{"Kodamas Reach", 1, "Kodama's Reach", MethodDeckExact, ScopeOwn, ConfExact},
		{"Rhystic Cave", 1, "Rhystic Cave", MethodDeckExact, ScopeTable, ConfOtherExact},
		{"Krenko", 1, "Krenko, Mob Boss", MethodDeckFuzzy, ScopeTable, ConfOtherFuzzy},
		{"mystic remora", 3, "Mystic Remora", MethodGlobal, ScopeGlobal, ConfGlobalExact},
	}
	for _, c := range cases {
		r := resolve(t, st, c.seat, c.spoken, global)
		if r.Card != c.card || r.Method != c.method || r.Scope != c.scope || r.Confidence != c.conf {
			t.Errorf("%q → {%s %s %s %v}, want {%s %s %s %v}",
				c.spoken, r.Card, r.Method, r.Scope, r.Confidence, c.card, c.method, c.scope, c.conf)
		}
	}
}

func TestFromSeats(t *testing.T) {
	u := FromSeats(pod())
	// Alice carries no deck, but her commander is public fact in the
	// command zone, so her seat still contributes one known card.
	if u.Attached() != 3 {
		t.Fatalf("attached = %d, want 3", u.Attached())
	}
	cards := u.Cards(1)
	if cards["Rhystic Study"] != 1 || cards["Atraxa, Praetors' Voice"] != 1 || cards["Island"] != 20 {
		t.Fatalf("seat 1 universe = %v", cards)
	}
	if u.Cards(3)["Urza, Lord High Artificer"] != 1 {
		t.Fatalf("seat 3 universe = %v, want its commander", u.Cards(3))
	}
}

// The universe is the full deck, not the remaining library: play and
// draw shrink the composition, and the universe keeps every name.
func TestUniverseOutlivesTheLibrary(t *testing.T) {
	seats := pod()
	st := started(t, seats)
	// Draw two, both named: the composition loses both, exactness holds.
	evs, err := engine.Apply(st, engine.Action{Kind: engine.ActionDraw, Seat: 1, Count: 2, Cards: []string{"Rhystic Study", "Island"}})
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	st.FoldInto(evs)
	p := st.Seats[1]
	if p.LibraryComp["Rhystic Study"] != 0 || p.LibraryComp["Island"] != 19 {
		t.Fatalf("composition = %v", p.LibraryComp)
	}
	if p.Deck["Rhystic Study"] != 1 || p.Deck["Island"] != 20 {
		t.Fatalf("deck echo = %v, want the full attached list", p.Deck)
	}
	u := FromState(st)
	if u.Cards(1)["Rhystic Study"] != 1 {
		t.Fatalf("universe lost a card the table has seen")
	}
	view := u.Library(st, 1)
	if !view.Known || !view.Exact || view.Counts["Island"] != 19 {
		t.Fatalf("library view = %+v, want known, exact, 19 islands", view)
	}
	// An unnamed draw ends exactness: the composition is an upper bound
	// from here on, and the view says so instead of guessing.
	evs, err = engine.Apply(st, engine.Action{Kind: engine.ActionDraw, Seat: 1, Count: 1})
	if err != nil {
		t.Fatalf("unnamed draw: %v", err)
	}
	st.FoldInto(evs)
	if view = FromState(st).Library(st, 1); view.Exact {
		t.Fatalf("library view = %+v, want inexact after an unnamed draw", view)
	}
}

// Library order is refused out loud, and the view has no order to leak.
func TestLibraryOrderRefused(t *testing.T) {
	st := started(t, pod())
	view := FromState(st).Library(st, 1)
	if len(view.Counts) == 0 || !view.Known {
		t.Fatalf("view = %+v, want a composition", view)
	}
	if err := ErrLibraryOrder; err == nil || err.Error() != "library order is never modelled" {
		t.Fatalf("refusal = %v", err)
	}
}

// A game with no decks at all works: everything falls to the index tier.
func TestDecklessGame(t *testing.T) {
	seats := []engine.SeatConfig{
		{Seat: 1, Name: "Collin"}, {Seat: 2, Name: "Bob"},
	}
	global := &fakeGlobal{by: map[string]string{"sol ring": "Sol Ring"}}
	r := resolve(t, started(t, seats), 1, "sol ring", global)
	if r.Card != "Sol Ring" || r.Method != MethodGlobal {
		t.Fatalf("deckless sol ring → %+v", r)
	}
}

/* ---------- the ask rung's candidate list (MAD-331) ---------- */

// Candidates walks the deck tiers in Resolve's order and lists the
// gated matches an ambiguous span could mean — the one-tap question's
// options. An exact match is not a candidate: it is Resolve's answer.
func TestCandidates(t *testing.T) {
	st := started(t, pod())
	u := FromState(st)
	// "Rhystic" is gated shorthand for both the Study (own deck) and
	// the Cave (Bob's): exactly the ambiguity a question resolves.
	got := u.Candidates(1, "Rhystic", 3)
	if len(got) != 2 || got[0] != "Rhystic Study" || got[1] != "Rhystic Cave" {
		t.Fatalf("candidates = %v, want [Rhystic Study Rhystic Cave]", got)
	}
	// The speaker's deck leads regardless of who else matches.
	if got := u.Candidates(2, "Rhystic", 3); got[0] != "Rhystic Cave" {
		t.Fatalf("from seat 2 = %v, want the Cave first", got)
	}
	// The limit is the one-tap rule's small set.
	if got := u.Candidates(1, "Rhystic", 1); len(got) != 1 || got[0] != "Rhystic Study" {
		t.Fatalf("capped candidates = %v", got)
	}
	// An exact match is Resolve's business, not a question.
	if got := u.Candidates(1, "Sol Ring", 3); len(got) != 0 {
		t.Fatalf("exact-match candidates = %v, want none", got)
	}
	// Nothing gated, nothing listed — and the question parks.
	if got := u.Candidates(1, "blorptidious", 3); len(got) != 0 {
		t.Fatalf("unmatched candidates = %v, want none", got)
	}
}

// Seats lists the attached decks in seating order — the prompt's and
// the gate's walk order.
func TestSeats(t *testing.T) {
	got := FromState(started(t, pod())).Seats()
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("seats = %v, want [1 2 3] (seat 3 carries its commander)", got)
	}
}
