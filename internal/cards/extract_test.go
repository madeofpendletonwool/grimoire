package cards

import (
	"reflect"
	"testing"
)

func TestExtractCandidates(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "double-quoted",
			in:   `what does "Lightning Bolt" do?`,
			want: []string{"Lightning Bolt"},
		},
		{
			name: "bracketed",
			in:   "tell me about [Sol Ring]",
			want: []string{"Sol Ring"},
		},
		{
			name: "title case multi-word",
			in:   "How does Lightning Bolt interact with Ward?",
			want: []string{"Lightning Bolt", "Ward"},
		},
		{
			name: "title case with connectors",
			in:   "Is Wrath of God destroyed by regeneration?",
			want: []string{"Wrath of God"},
		},
		{
			name: "single capitalized word mid-sentence",
			in:   "what does Counterspell target?",
			want: []string{"Counterspell"},
		},
		{
			name: "named pattern",
			in:   "what does the card named Lightning Bolt do?",
			want: []string{"Lightning Bolt"},
		},
		{
			name: "multiple distinct cards",
			in:   `does "Sol Ring" tap for more than "Birds of Paradise"?`,
			want: []string{"Sol Ring", "Birds of Paradise"},
		},
		{
			name: "no card mentioned, just rules",
			in:   "what does ward do and what is the rule number",
			want: nil,
		},
		{
			name: "stopword lone capital dropped",
			in:   "What is the rule for vigilance",
			want: nil,
		},
		{
			name: "lowercase card name not auto-detected",
			in:   "what does bolt do",
			want: nil,
		},
		{
			name: "quote overrides case",
			in:   `tell me about "lightning bolt"`,
			want: []string{"lightning bolt"},
		},
		{
			// Regression: a comma-separated list used to collapse into one
			// bogus candidate ("Humility Opalescence"), so neither card was
			// ever looked up.
			name: "comma separated list splits",
			in:   "You control Humility, Opalescence, and Enchanted Evening. Your opponent controls a Blood Moon.",
			want: []string{"Humility", "Opalescence", "Enchanted Evening", "Blood Moon"},
		},
		{
			// "Board" is emitted too — a lone capital costs one cached
			// Scryfall miss, which is cheaper than adding words like it to the
			// stopword list and breaking real names ("Board the Weatherlight").
			name: "colon and semicolon are boundaries",
			in:   "Board: Sol Ring; Lightning Bolt",
			want: []string{"Board", "Sol Ring", "Lightning Bolt"},
		},
		{
			name: "slash separates names",
			in:   "Does Sol Ring/Lightning Bolt matter?",
			want: []string{"Sol Ring", "Lightning Bolt"},
		},
		{
			// MAD-113: single-quoted card names are extracted just like
			// double-quoted ones, so 'prizefight' is pulled out even from a
			// sentence with no Title Case to signal it. The contraction
			// "It's" must NOT also be emitted as a lone capital.
			name: "single quoted name",
			in:   "It's 'prizefight'",
			want: []string{"prizefight"},
		},
		{
			// A single quote at the very start of the text is still a
			// delimiter (the left boundary is start-of-string).
			name: "single quoted name at start",
			in:   "'Lightning Bolt' is good",
			want: []string{"Lightning Bolt"},
		},
		{
			// Contractions must not be read as quoted phrases: the apostrophe
			// in "It's" is glued to letters, so it is not a delimiter, and as
			// a contraction of the stopword "it" it is not a card name either.
			name: "contraction is not a quote",
			in:   "It's a rules question",
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractCandidates(c.in)
			// Order is not guaranteed across extraction sources; compare as
			// sets by sorting both via a membership check.
			if !sameSet(got, c.want) {
				t.Errorf("ExtractCandidates(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	// order-insensitive compare
	want := map[string]bool{}
	for _, x := range b {
		want[x] = true
	}
	for _, x := range a {
		if !want[x] {
			return false
		}
	}
	return true
}

func TestExtractCandidates_DedupCaseInsensitive(t *testing.T) {
	got := ExtractCandidates(`"Lightning Bolt" and Lightning Bolt`)
	want := []string{"Lightning Bolt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractCandidates_Capped(t *testing.T) {
	// Many quoted names should be capped, not all emitted.
	in := `"A" "B" "C" "D" "E" "F" "G" "H"`
	got := ExtractCandidates(in)
	if len(got) > maxCandidates {
		t.Errorf("got %d candidates, max %d", len(got), maxCandidates)
	}
}
