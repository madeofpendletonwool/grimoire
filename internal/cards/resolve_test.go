package cards

import (
	"context"
	"strings"
	"testing"
)

// fakeLooker resolves a fixed set of card names the way Scryfall's fuzzy
// endpoint does: exact match, or a unique name that contains every word of the
// query. Everything else is ErrNotFound.
type fakeLooker struct {
	names []string
	calls int
}

func (f *fakeLooker) Lookup(_ context.Context, name string) (*Card, error) {
	f.calls++
	want := nameWords(name)
	if len(want) == 0 {
		return nil, ErrNotFound
	}
	var hits []string
	for _, n := range f.names {
		have := map[string]bool{}
		for _, w := range nameWords(n) {
			have[w] = true
		}
		ok := true
		for _, w := range want {
			if !have[w] {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, n)
		}
	}
	if len(hits) != 1 {
		return nil, ErrNotFound // missing or ambiguous
	}
	return &Card{Name: hits[0], OracleText: "text of " + hits[0]}, nil
}

func resolvedNames(r Resolution) []string {
	var out []string
	for _, c := range r.Cards {
		out = append(out, c.Name)
	}
	return out
}

func TestResolve_SplitsRunTogetherNames(t *testing.T) {
	// Regression for MAD-102: the question lists cards without separators, so
	// extraction hands over one phrase spanning several names. Nothing
	// resolved before, and the model answered from memory.
	l := &fakeLooker{names: []string{
		"Kokusho, the Evening Star", "Sakashima the Impostor", "Sundial of the Infinite", "Teysa Karlov",
	}}
	got := Resolve(context.Background(), l, []string{
		"Kokusho the Evening Star Sakashima the Impostor",
		"Kokusho Sundial of the Infinite",
		"Teysa Karlov",
	})
	want := []string{"Kokusho, the Evening Star", "Sakashima the Impostor", "Sundial of the Infinite", "Teysa Karlov"}
	if !sameSet(resolvedNames(got), want) {
		t.Errorf("resolved %v, want %v", resolvedNames(got), want)
	}
	if len(got.Unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", got.Unresolved)
	}
}

func TestResolve_WholePhraseFirst(t *testing.T) {
	// A phrase that is itself a card costs exactly one lookup.
	l := &fakeLooker{names: []string{"Wrath of God"}}
	got := Resolve(context.Background(), l, []string{"Wrath of God"})
	if !sameSet(resolvedNames(got), []string{"Wrath of God"}) {
		t.Fatalf("resolved %v", resolvedNames(got))
	}
	if l.calls != 1 {
		t.Errorf("calls = %d, want 1", l.calls)
	}
}

func TestResolve_DedupesAcrossPhrases(t *testing.T) {
	l := &fakeLooker{names: []string{"Lightning Bolt"}}
	got := Resolve(context.Background(), l, []string{"Lightning Bolt", "Lightning Bolt"})
	if len(got.Cards) != 1 {
		t.Errorf("cards = %v, want one entry", resolvedNames(got))
	}
}

func TestResolve_ReportsUnresolvedPhrases(t *testing.T) {
	// A multi-word phrase that resolves to nothing must be reported, not
	// silently dropped — a lookup miss should never look like "no card was
	// mentioned".
	l := &fakeLooker{names: []string{"Lightning Bolt"}}
	got := Resolve(context.Background(), l, []string{"Not A Card"})
	if len(got.Cards) != 0 {
		t.Errorf("cards = %v, want none", resolvedNames(got))
	}
	if len(got.Unresolved) != 1 || got.Unresolved[0] != "Not A Card" {
		t.Errorf("unresolved = %v, want [Not A Card]", got.Unresolved)
	}
}

func TestResolve_StaysWithinLookupBudget(t *testing.T) {
	l := &fakeLooker{names: []string{"Lightning Bolt"}}
	long := strings.Repeat("Zzz ", 20)
	Resolve(context.Background(), l, []string{long, long + "Qqq"})
	if l.calls > maxLookups {
		t.Errorf("calls = %d, exceeds budget %d", l.calls, maxLookups)
	}
}

func TestNameMatches(t *testing.T) {
	cases := []struct {
		span, card string
		want       bool
	}{
		{"Bolt", "Lightning Bolt", true},                                            // shorthand
		{"Kokusho the Evening Star", "Kokusho, the Evening Star", true},             // punctuation
		{"Evening Star Sakashima the Impostor", "Kokusho, the Evening Star", false}, // over-long span
		{"Humility Opalescence", "Humility", false},
		{"", "Humility", false},
		// Spacing differences (MAD-113): a space the user added or dropped
		// must not defeat a Scryfall fuzzy match.
		{"prize fight", "Prizefight", true},
		{"Prizefight", "Prize Fight", true}, // hypothetical reverse spacing
		{"lightningbolt", "Lightning Bolt", true},
		// Minor misspellings (MAD-113): Scryfall's fuzzy matcher corrects
		// these; NameMatches must accept its answer.
		{"gient growth", "Giant Growth", true},
		{"counterspel", "Counterspell", true},
		// Two unrelated names must never fuzzy onto each other.
		{"Completely Different Thing", "Lightning Bolt", false},
	}
	for _, c := range cases {
		if got := NameMatches(c.span, c.card); got != c.want {
			t.Errorf("NameMatches(%q, %q) = %v, want %v", c.span, c.card, got, c.want)
		}
	}
}

func TestResolve_NilLooker(t *testing.T) {
	got := Resolve(context.Background(), nil, []string{"Lightning Bolt"})
	if len(got.Cards) != 0 || len(got.Unresolved) != 0 {
		t.Errorf("nil looker should resolve nothing, got %+v", got)
	}
}

// mappingLooker returns a fixed card for an exact (case-insensitive) query,
// modeling how Scryfall's fuzzy /cards/named endpoint resolves a slightly
// mis-spaced or misspelled name to the real card. The fakeLooker above can't
// express that, because it matches on whole-word containment.
type mappingLooker struct {
	byQuery map[string]*Card
	calls   int
}

func (m *mappingLooker) Lookup(_ context.Context, name string) (*Card, error) {
	m.calls++
	if c, ok := m.byQuery[strings.ToLower(name)]; ok {
		return c, nil
	}
	return nil, ErrNotFound
}

func TestResolve_ToleratesSpacingAndTypos(t *testing.T) {
	// Regression for MAD-113: Scryfall resolves "prize fight" to Prizefight
	// (one word). Before the fix, NameMatches rejected that match because
	// "prize" and "fight" are not whole words of "Prizefight", so the card
	// came back unresolved and the model answered from memory.
	l := &mappingLooker{byQuery: map[string]*Card{
		"prize fight":  {Name: "Prizefight", OracleText: "fight text"},
		"gient growth": {Name: "Giant Growth", OracleText: "+3/+3"},
	}}
	got := Resolve(context.Background(), l, []string{"Prize Fight", "Gient Growth"})
	want := []string{"Prizefight", "Giant Growth"}
	if !sameSet(resolvedNames(got), want) {
		t.Errorf("resolved %v, want %v", resolvedNames(got), want)
	}
	if len(got.Unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", got.Unresolved)
	}
}
