package cards

import (
	"context"
	"strings"
	"unicode"
)

// maxLookups bounds how many Scryfall calls one question may spend. Repeats
// are served from the service cache, so this is a ceiling on distinct names.
const maxLookups = 30

// maxSpanWords bounds the span length tried when segmenting a phrase. Longer
// names are still reachable: the whole phrase is always tried first.
const maxSpanWords = 8

// Looker resolves a name to a card. *Service satisfies it; tests supply a fake.
type Looker interface {
	Lookup(ctx context.Context, name string) (*Card, error)
}

// Resolution is the outcome of resolving one question's card candidates.
type Resolution struct {
	// Cards are the cards that resolved, in the order they were mentioned.
	Cards []*Card
	// Unresolved holds multi-word phrases that produced no card at all. They
	// are surfaced rather than dropped so a lookup miss is visible instead of
	// looking like "no cards were mentioned".
	Unresolved []string
}

// Resolve turns extracted candidate phrases into real cards.
//
// A phrase is tried whole first. If that misses, the phrase is segmented
// greedily longest-first, which is what recovers names run together without
// punctuation ("Kokusho the Evening Star Sakashima the Impostor" -> two
// cards). Every hit is checked with nameMatches so Scryfall's lenient fuzzy
// matching can't attach an over-long span to the wrong card.
func Resolve(ctx context.Context, l Looker, phrases []string) Resolution {
	var res Resolution
	if l == nil {
		return res
	}
	budget := maxLookups
	seen := map[string]bool{}

	// try looks a span up and returns the card only if the match is credible.
	try := func(name string) *Card {
		if budget <= 0 {
			return nil
		}
		budget--
		c, err := l.Lookup(ctx, name)
		if err != nil || c == nil || !nameMatches(name, c.Name) {
			return nil
		}
		return c
	}
	add := func(c *Card) {
		k := strings.ToLower(c.Name)
		if seen[k] {
			return
		}
		seen[k] = true
		res.Cards = append(res.Cards, c)
	}

	for _, phrase := range phrases {
		toks := strings.Fields(phrase)
		if len(toks) == 0 {
			continue
		}
		if c := try(phrase); c != nil {
			add(c)
			continue
		}
		matched := false
		for i := 0; i < len(toks); {
			n := len(toks) - i
			if n > maxSpanWords {
				n = maxSpanWords
			}
			hit := false
			for j := n; j >= 1; j-- {
				if i == 0 && j == len(toks) {
					continue // already tried as the whole phrase
				}
				if !spanWorthTrying(toks[i : i+j]) {
					continue
				}
				if c := try(strings.Join(toks[i:i+j], " ")); c != nil {
					add(c)
					i += j
					hit, matched = true, true
					break
				}
			}
			if !hit {
				i++
			}
		}
		if !matched && len(toks) > 1 {
			res.Unresolved = append(res.Unresolved, phrase)
		}
	}
	return res
}

// spanWorthTrying drops spans that cannot be a whole card name: ones that
// start or end on a connector or common word ("Evening Star Sakashima the").
// Pruning these keeps the segmentation inside its lookup budget.
func spanWorthTrying(toks []string) bool {
	if len(toks) == 0 {
		return false
	}
	first, last := strings.ToLower(toks[0]), strings.ToLower(toks[len(toks)-1])
	if connectorWords[first] || isStopwordLower(first) {
		return false
	}
	return !connectorWords[last] && !isStopwordLower(last)
}

// nameMatches reports whether a card is a credible match for the span we
// searched: every word of the span must appear in the card's name. That keeps
// a shorthand ("Bolt" -> "Lightning Bolt") while rejecting a fuzzy match onto
// a span carrying extra words ("Evening Star Sakashima the Impostor").
func nameMatches(span, cardName string) bool {
	have := map[string]bool{}
	for _, w := range nameWords(cardName) {
		have[w] = true
	}
	words := nameWords(span)
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		if !have[w] {
			return false
		}
	}
	return true
}

// nameWords lowercases a name and splits it on everything that isn't a letter
// or digit, so punctuation differences ("Kokusho, the Evening Star") don't
// matter.
func nameWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
