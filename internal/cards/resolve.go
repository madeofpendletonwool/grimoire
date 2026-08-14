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
// cards). Every hit is checked with NameMatches so Scryfall's lenient fuzzy
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
		if err != nil || c == nil || !NameMatches(name, c.Name) {
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

// NameMatches reports whether a card is a credible match for the span we
// searched. It is the shared credibility check behind Grimoire's lookup: the
// D&D/Open5e resolver reuses it for entity names so fuzzy tolerance is
// consistent across corpora.
//
// Scryfall's fuzzy matcher bridges two kinds of user error that we must not
// reject:
//
//   - Spacing: "prize fight" vs "Prizefight", or "lightningbolt" vs
//     "Lightning Bolt".
//   - Minor misspellings: "gient growth" vs "Giant Growth".
//
// But it must still reject an over-long span — several card names run
// together — attaching to a single card ("Evening Star Sakashima the
// Impostor" -> "Kokusho, the Evening Star"). A match is accepted on either:
//
//  1. Word subset — every word of the span is a word of the card name. Keeps
//     shorthand ("Bolt" -> "Lightning Bolt") and ignores punctuation.
//  2. Close spelling — the alphanumeric forms of the two are within a small
//     edit distance, and the span is not longer than the card by more than
//     that budget (the run-on guard).
func NameMatches(span, cardName string) bool {
	spanWords := nameWords(span)
	if len(spanWords) == 0 {
		return false
	}
	cardWordSet := map[string]bool{}
	for _, w := range nameWords(cardName) {
		cardWordSet[w] = true
	}
	subset := true
	for _, w := range spanWords {
		if !cardWordSet[w] {
			subset = false
			break
		}
	}
	if subset {
		return true
	}

	s, c := alnum(span), alnum(cardName)
	if s == "" || c == "" {
		return false
	}
	budget := editBudget(s, c)
	// Run-on guard: a span carrying a lot of extra content is probably two
	// card names jammed together, not a fuzzy spelling of one card.
	if len(s)-len(c) > budget {
		return false
	}
	return levenshtein(s, c, budget) <= budget
}

// alnum returns the lowercased letters and digits of s with everything else
// dropped, so spacing and punctuation no longer matter: "prize fight",
// "Prizefight", and "Prize-Fight" all collapse to "prizefight".
func alnum(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// editBudget is the fuzzy tolerance for a span/card pair: a couple of edits
// for short names, growing gently with length. Scryfall's fuzzy matcher only
// returns a card for genuinely close input, so a small budget is enough.
func editBudget(s, c string) int {
	longer := len(s)
	if len(c) > longer {
		longer = len(c)
	}
	b := longer / 4
	if b < 2 {
		b = 2
	}
	if b > 4 {
		b = 4
	}
	return b
}

// levenshtein returns the edit distance between a and b, returning max+1 as
// soon as the distance is known to exceed max. Inputs are short normalized
// card names, so the bounded DP is cheap.
func levenshtein(a, b string, max int) int {
	if absInt(len(a)-len(b)) > max {
		return max + 1
	}
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d := prev[j-1] + cost
			if v := prev[j] + 1; v < d {
				d = v
			}
			if v := cur[j-1] + 1; v < d {
				d = v
			}
			cur[j] = d
			if d < rowMin {
				rowMin = d
			}
		}
		if rowMin > max {
			return max + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// nameWords lowercases a name and splits it on everything that isn't a letter
// or digit, so punctuation differences ("Kokusho, the Evening Star") don't
// matter.
func nameWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
