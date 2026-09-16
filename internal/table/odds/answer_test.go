package odds

// The draw-odds and outs tests (MAD-336): exactness over a derived
// composition, the bounded-honesty rule, outs coming only from the
// remaining library, and reproducibility — the same question over the
// same composition answering byte-identically twice.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// fixture is a small deterministic card set with the oracle-text
// shapes the matchers key on.
var fixture = map[string]*carddb.Card{
	"Forest":        {Name: "Forest", TypeLine: "Basic Land — Forest", OracleText: "{T}: Add {G}."},
	"Island":        {Name: "Island", TypeLine: "Basic Land — Island", OracleText: "{T}: Add {U}."},
	"Wrath of God":  {Name: "Wrath of God", ManaCost: "{2}{W}{W}", ManaValue: 4, TypeLine: "Sorcery", OracleText: "Destroy all creatures."},
	"Counterspell":  {Name: "Counterspell", ManaCost: "{U}{U}", ManaValue: 2, TypeLine: "Instant", OracleText: "Counter target spell."},
	"Beast Within":  {Name: "Beast Within", ManaCost: "{2}{G}", ManaValue: 3, TypeLine: "Instant", OracleText: "Destroy target permanent."},
	"Naturalize":    {Name: "Naturalize", ManaCost: "{1}{G}", ManaValue: 2, TypeLine: "Instant", OracleText: "Destroy target artifact or enchantment."},
	"Sol Ring":      {Name: "Sol Ring", ManaCost: "{1}", ManaValue: 1, TypeLine: "Artifact", OracleText: "{T}: Add {C}{C}."},
	"Cultivate":     {Name: "Cultivate", ManaCost: "{2}{G}", ManaValue: 3, TypeLine: "Sorcery", OracleText: "Search your library for up to two basic land cards, put one onto the battlefield and one into your hand."},
	"Harmonize":     {Name: "Harmonize", ManaCost: "{2}{G}", ManaValue: 3, TypeLine: "Sorcery", OracleText: "Draw three cards."},
	"Rhystic Study": {Name: "Rhystic Study", ManaCost: "{1}{U}{U}", ManaValue: 3, TypeLine: "Enchantment", OracleText: "Whenever an opponent casts a spell, you may draw a card unless they pay {1}."},
}

func fixtureLookup(name string) (*carddb.Card, bool) {
	c, ok := fixture[name]
	return c, ok
}

// libraryState builds a folded one-seat game whose seat 1 library is
// the given composition. All-seat setup through the engine itself, so
// the fold's own bookkeeping produced the composition.
func libraryState(comp map[string]int, exact bool) *engine.State {
	st := engine.NewState()
	st.Seats[1] = &engine.Player{Seat: 1, Name: "Collin", Alive: true,
		Deck: comp, LibraryComp: comp, LibraryExact: exact,
		Library: engine.KnownCount(countOf(comp))}
	st.Seats[2] = &engine.Player{Seat: 2, Name: "Bob", Alive: true}
	st.Order = []int{1, 2}
	st.Status = engine.StatusActive
	return st
}

func countOf(comp map[string]int) int {
	n := 0
	for _, c := range comp {
		n += c
	}
	return n
}

func TestOfDerivesFromTheFold(t *testing.T) {
	comp := map[string]int{"Forest": 30, "Wrath of God": 2, "Naturalize": 1}
	lib, err := Of(libraryState(comp, true), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !lib.Known || !lib.Exact || lib.N != 33 || lib.Counts["Forest"] != 30 {
		t.Fatalf("lib = %+v", lib)
	}
	// A deckless seat is unknown, not empty.
	lib, err = Of(libraryState(comp, true), 2)
	if err != nil {
		t.Fatal(err)
	}
	if lib.Known {
		t.Fatalf("deckless seat should be unknown, got %+v", lib)
	}
	// A missing seat is an error.
	if _, err := Of(libraryState(comp, true), 9); err == nil {
		t.Fatal("seat 9 should error")
	}
}

func TestDrawOddsExactAndReproducible(t *testing.T) {
	// 30 Forests in a 33-card library: the chance of at least one land
	// in 3 draws. Hand-computed: 1 − C(3,3)/C(33,3) = 1 − 1/5456.
	comp := map[string]int{"Forest": 30, "Wrath of God": 2, "Naturalize": 1}
	lib, _ := Of(libraryState(comp, true), 1)
	a1, err := DrawOdds("chance of a land in the next three", lib, CatLands, "",
		MatchCategory(CatLands), fixtureLookup, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a1.N != 33 || a1.K != 30 {
		t.Fatalf("N=%d K=%d, want 33/30", a1.N, a1.K)
	}
	if a1.Rational != "5455/5456" {
		t.Fatalf("rational = %s, want 5455/5456", a1.Rational)
	}
	if a1.Bounded {
		t.Fatal("an exact composition must not claim to be bounded")
	}
	// Reproducible: the same question twice is byte-identical.
	a2, _ := DrawOdds("chance of a land in the next three", lib, CatLands, "",
		MatchCategory(CatLands), fixtureLookup, 3, 1)
	if fmt.Sprintf("%+v", a1) != fmt.Sprintf("%+v", a2) {
		t.Fatalf("answers differ between runs:\n%+v\n%+v", a1, a2)
	}
}

func TestDrawOddsBoundedSaysSo(t *testing.T) {
	comp := map[string]int{"Forest": 30, "Wrath of God": 2}
	lib, _ := Of(libraryState(comp, false), 1) // unidentified cards left
	a, err := DrawOdds("chance of a land", lib, CatLands, "",
		MatchCategory(CatLands), fixtureLookup, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Bounded || a.Note == "" {
		t.Fatalf("bounded composition must say so: %+v", a)
	}
}

func TestDrawOddsNamedCard(t *testing.T) {
	comp := map[string]int{"Forest": 30, "Wrath of God": 2, "Naturalize": 1}
	lib, _ := Of(libraryState(comp, true), 1)
	// Case-insensitive over the composition, no lookup involved.
	a, err := DrawOdds("Wrath of God in the next five", lib, CatCard, "wrath of god",
		nil, fixtureLookup, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.K != 2 || a.Hits["Wrath of God"] != 2 {
		t.Fatalf("K=%d hits=%v", a.K, a.Hits)
	}
	// Hand-computed: 1 − C(31,5)/C(33,5) = 1 − (28·27)/(33·32) = 25/88.
	if a.Rational != "25/88" {
		t.Fatalf("rational = %s, want 25/88", a.Rational)
	}
}

func TestDrawOddsWipeCategory(t *testing.T) {
	comp := map[string]int{"Forest": 30, "Wrath of God": 2, "Naturalize": 1, "Counterspell": 2}
	lib, _ := Of(libraryState(comp, true), 1)
	a, err := DrawOdds("board wipe in the next nine", lib, CatWipes, "",
		MatchCategory(CatWipes), fixtureLookup, 9, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.K != 2 { // Wrath only: Naturalize is targeted, not a wipe
		t.Fatalf("K=%d hits=%v, want 2 (Wrath of God only)", a.K, a.Hits)
	}
}

func TestDrawOddsUnknownLibraryRefuses(t *testing.T) {
	if _, err := DrawOdds("land", Library{Known: false}, CatLands, "",
		MatchCategory(CatLands), fixtureLookup, 3, 1); err == nil {
		t.Fatal("an unknown library must not be answered")
	}
}

func TestDrawsByTurn(t *testing.T) {
	if n, _ := DrawsByTurn(3, 9); n != 7 {
		t.Fatalf("turns 3→9 = %d draws, want 7", n)
	}
	if n, _ := DrawsByTurn(9, 9); n != 1 {
		t.Fatalf("turn 9→9 = %d draws, want 1", n)
	}
	if _, err := DrawsByTurn(9, 3); err == nil {
		t.Fatal("a past turn must error")
	}
	if _, err := DrawsByTurn(1, 0); err == nil {
		t.Fatal("turn 0 must error")
	}
}

/* ---------- outs ---------- */

// fakeSearch stands in for carddb's FTS: it returns every fixture
// card whose text mentions any query word — the contract the real
// SearchText carries (OR-of-terms over type line and oracle text),
// deterministic for the tests.
type fakeSearch struct{}

func (fakeSearch) SearchText(_ context.Context, q string, limit int) ([]*carddb.Card, error) {
	terms := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(q)) {
		terms[w] = true
	}
	var out []*carddb.Card
	names := make([]string, 0, len(fixture))
	for name := range fixture {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := fixture[name]
		hay := strings.ToLower(c.OracleText + " " + c.TypeLine + " " + c.Name)
		for term := range terms {
			if strings.Contains(hay, term) {
				out = append(out, c)
				break
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func TestOutsComeOnlyFromTheRemainingLibrary(t *testing.T) {
	// The Wrath was already drawn: it is not an out, whatever the
	// index knows. Naturalize and Beast Within remain and both answer
	// an enchantment.
	comp := map[string]int{"Forest": 30, "Naturalize": 1, "Beast Within": 2, "Wrath of God": 0}
	delete(comp, "Wrath of God")
	lib, _ := Of(libraryState(comp, true), 1)
	ans, err := Outs(context.Background(), lib, Target{Types: []string{"enchantment"}},
		fakeSearch{}, fixtureLookup, 3)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, o := range ans.Outs {
		names[o.Name] = o.Count
	}
	if names["Naturalize"] != 1 || names["Beast Within"] != 2 {
		t.Fatalf("outs = %v", names)
	}
	if _, ok := names["Wrath of God"]; ok {
		t.Fatal("a drawn card is never an out")
	}
	if ans.K != 3 {
		t.Fatalf("K = %d, want 3", ans.K)
	}
	// Deterministic order: copies desc, then name.
	if ans.Outs[0].Name != "Beast Within" || ans.Outs[1].Name != "Naturalize" {
		t.Fatalf("order = %v, %v", ans.Outs[0].Name, ans.Outs[1].Name)
	}
}

func TestOutsVerifyAgainstType(t *testing.T) {
	// A card that only destroys creatures does not answer an
	// enchantment, even though the FTS proposed it.
	comp := map[string]int{"Forest": 30, "Counterspell": 2, "Naturalize": 1}
	lib, _ := Of(libraryState(comp, true), 1)
	ans, err := Outs(context.Background(), lib, Target{Types: []string{"enchantment"}},
		fakeSearch{}, fixtureLookup, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range ans.Outs {
		if o.Name == "Counterspell" {
			t.Fatal("Counterspell does not answer an enchantment")
		}
	}
	if len(ans.Outs) != 1 || ans.Outs[0].Name != "Naturalize" {
		t.Fatalf("outs = %+v", ans.Outs)
	}
	if len(ans.Outs[0].Reasons) == 0 {
		t.Fatal("every out carries why it answers")
	}
}

func TestOutsOnStackCountersOnly(t *testing.T) {
	comp := map[string]int{"Forest": 30, "Counterspell": 2, "Naturalize": 1}
	lib, _ := Of(libraryState(comp, true), 1)
	ans, err := Outs(context.Background(), lib, Target{Types: []string{"instant"}, OnStack: true},
		fakeSearch{}, fixtureLookup, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Outs) != 1 || ans.Outs[0].Name != "Counterspell" {
		t.Fatalf("stack outs = %+v", ans.Outs)
	}
}

func TestOutsNeedCardData(t *testing.T) {
	lib, _ := Of(libraryState(map[string]int{"Forest": 30}, true), 1)
	if _, err := Outs(context.Background(), lib, Target{Types: []string{"enchantment"}}, nil, nil, 1); err != ErrNoCardData {
		t.Fatalf("err = %v, want ErrNoCardData", err)
	}
}

func TestOutsUnknownLibraryRefuses(t *testing.T) {
	if _, err := Outs(context.Background(), Library{}, Target{Types: []string{"creature"}},
		fakeSearch{}, fixtureLookup, 1); err == nil {
		t.Fatal("an unknown library must not be searched")
	}
}
