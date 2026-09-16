package odds

// The question gate's tests (MAD-336): the mandated order-dependent
// refusal rule first — the phrases a table says that depend on an
// order the engine refuses to model are declined with
// universe.ErrLibraryOrder, never answered plausibly — then the
// shapes the gate does parse, then the honest no-parse.

import (
	"errors"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

func TestParseRefusesOrderDependentQuestions(t *testing.T) {
	// Every one of these asks about a position in an order that is
	// never modelled: identity, timing, or arrangement.
	refused := []string{
		"what's my next card?",
		"is my next card a land",
		"what's my next draw",
		"what's the top card of my library",
		"what is on top of my library",
		"chance the top card is a land",
		"when will I draw a Wrath of God",
		"when will I draw my next land",
		"how many cards until I find a counterspell",
		"how many draws until I hit a land",
		"what's the first card I'll draw",
		"will the third card be a land",
		"do I draw a land before a counterspell",
		"what position is Rhystic Study in my library",
		"what's the order of my library",
		"if I shuffle, what comes next",
		"what does my scry put on top",
		"look at the top three — what are they",
		"reveal the top card of my library",
		"how deep is my board wipe",
	}
	for _, q := range refused {
		_, err := Parse(q)
		if !errors.Is(err, ErrOrderDependent) {
			t.Errorf("%q: err = %v, want ErrOrderDependent", q, err)
		}
		if !errors.Is(err, universe.ErrLibraryOrder) {
			t.Errorf("%q: err is not the universe sentinel: %v", q, err)
		}
	}
}

func TestParseAggregateQuestionsAreNotRefused(t *testing.T) {
	// "Next" over a horizon of several draws is composition, not
	// order — these must parse, not drown in the refusal.
	accepted := []string{
		"chance of a land in the next three",
		"what are my odds of drawing a land in the next 2 cards?",
		"chance of finding a board wipe by turn nine",
	}
	for _, q := range accepted {
		if _, err := Parse(q); err != nil {
			t.Errorf("%q: unexpected error %v", q, err)
		}
	}
}

func TestParseOddsShapes(t *testing.T) {
	cases := []struct {
		q    string
		want Query
	}{
		{
			q:    "chance of a land in the next three",
			want: Query{Kind: "odds", Category: CatLands, AtLeast: 1, Draws: 3},
		},
		{
			q:    "what are the odds of drawing a land in the next 7 cards?",
			want: Query{Kind: "odds", Category: CatLands, AtLeast: 1, Draws: 7},
		},
		{
			q:    "chance of a board wipe in the next 4 draws",
			want: Query{Kind: "odds", Category: CatWipes, AtLeast: 1, Draws: 4},
		},
		{
			q:    "chance of finding a board wipe by turn nine",
			want: Query{Kind: "odds", Category: CatWipes, AtLeast: 1, ByTurn: 9},
		},
		{
			q:    "odds of a wrath by turn 12",
			want: Query{Kind: "odds", Category: CatWipes, AtLeast: 1, ByTurn: 12},
		},
		{
			q:    "probability of drawing Rhystic Study in the next five cards",
			want: Query{Kind: "odds", Category: CatCard, Card: "Rhystic Study", AtLeast: 1, Draws: 5},
		},
		{
			q:    "chance of drawing removal in the next 3",
			want: Query{Kind: "odds", Category: CatInteraction, AtLeast: 1, Draws: 3},
		},
		{
			q:    "odds of drawing a land",
			want: Query{Kind: "odds", Category: CatLands, AtLeast: 1, Draws: 1},
		},
	}
	for _, tc := range cases {
		got, err := Parse(tc.q)
		if err != nil {
			t.Errorf("%q: %v", tc.q, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %+v, want %+v", tc.q, got, tc.want)
		}
	}
}

func TestParseOutsShapes(t *testing.T) {
	cases := []struct {
		q       string
		want    string
		wantNil bool
	}{
		{q: "what are my outs against that enchantment", want: "that enchantment"},
		{q: "outs against Rhystic Study", want: "Rhystic Study"},
		{q: "any outs for an artifact?", want: "an artifact"},
		{q: "what answers an enchantment", want: "enchantment"},
		{q: "what kills a creature", want: "creature"},
		{q: "what deals with a planeswalker?", want: "planeswalker"},
		{q: "what are my outs", wantNil: true}, // against what?
	}
	for _, tc := range cases {
		got, err := Parse(tc.q)
		if tc.wantNil {
			if !errors.Is(err, ErrNoParse) {
				t.Errorf("%q: err = %v, want ErrNoParse", tc.q, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.q, err)
			continue
		}
		if got.Kind != "outs" || got.Target != tc.want {
			t.Errorf("%q: got %+v, want outs %q", tc.q, got, tc.want)
		}
	}
}

func TestParseNoParseIsHonest(t *testing.T) {
	// Not vocabulary, not a name, not a shape: declined, never guessed.
	for _, q := range []string{
		"",
		"hello",
		"what should I do",
		"chance of a frog in the next three", // lowercase non-vocabulary
	} {
		if _, err := Parse(q); !errors.Is(err, ErrNoParse) {
			t.Errorf("%q: err = %v, want ErrNoParse", q, err)
		}
	}
}
