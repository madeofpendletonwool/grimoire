package cards

import "strings"

// dictMaxWindow bounds how many consecutive words of a question are joined and
// looked up as one card name. Eight covers the longest realistic names someone
// would type unquoted; the handful of absurdly long Un-set names are what the
// autocomplete tab is for.
const dictMaxWindow = 8

// Dictionary is an in-memory index of known MTG card names. It spots mentions
// the heuristic extractor misses — lowercase, unquoted, no Title Case — by
// matching against real names (MTGJSON AtomicCards). Matching is case- and
// spacing-insensitive: "prize fight" matches "Prizefight", and "lightning
// bolt" matches "Lightning Bolt".
type Dictionary struct {
	norm map[string]string // normalized alnum -> canonical name
}

// NewDictionary indexes a set of card names. Names that normalize to fewer
// than three characters are dropped: one- and two-letter tokens are noise that
// rarely aid detection and often collide with ordinary prose.
func NewDictionary(names []string) *Dictionary {
	d := &Dictionary{norm: make(map[string]string, len(names))}
	for _, n := range names {
		key := alnum(n)
		if len(key) < 3 {
			continue
		}
		// First write of a normalized form wins; duplicates do not clobber a
		// canonical spelling already stored.
		if _, ok := d.norm[key]; !ok {
			d.norm[key] = n
		}
	}
	return d
}

// Size returns the number of distinct, indexable card names.
func (d *Dictionary) Size() int {
	if d == nil {
		return 0
	}
	return len(d.norm)
}

// Mentions returns the canonical card names found in text, in the order they
// first appear. At each position the longest dictionary match wins (greedy),
// so "giant growth" resolves to Giant Growth rather than two spurious
// single-word hits, and consumed words are not re-matched.
func (d *Dictionary) Mentions(text string) []string {
	if d == nil || len(d.norm) == 0 {
		return nil
	}
	words := nameWords(text) // lowercased alnum tokens
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(words); {
		matched := false
		maxN := dictMaxWindow
		if len(words)-i < maxN {
			maxN = len(words) - i
		}
		for n := maxN; n >= 1; n-- {
			key := strings.Join(words[i:i+n], "")
			if name, ok := d.norm[key]; ok {
				if !seen[key] {
					seen[key] = true
					out = append(out, name)
				}
				i += n
				matched = true
				break
			}
		}
		if !matched {
			i++
		}
	}
	return out
}
