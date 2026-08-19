package deck

import (
	"strings"
	"unicode"
)

// Decklist annotations. Every site that exports a Commander list decorates the
// card name with where the printing came from: Archidekt writes "[40K]",
// Moxfield writes "(LTC) 123" and appends "*F*" for foil, Arena writes
// "(LTC) 123", Archidekt's full export adds "{noDeck}" flags and a category in
// a second bracket. None of that is part of the card's name, and leaving it on
// is what turned a 100-card paste into 47 matches — "Command Tower [40K]" is
// not a card, so it fell through to a fuzzy search that found something else
// entirely. CleanCardName strips the decoration back to the name MTGJSON knows.

// CleanCardName removes printing annotations from a decklist card name and
// returns the bare name. It is deliberately conservative with parentheses:
// only a set-code-shaped group is stripped, so "B.F.M. (Big Furry Monster)"
// survives intact.
func CleanCardName(name string) string {
	s := strings.TrimSpace(name)
	for {
		trimmed := stripTrailingAnnotation(s)
		if trimmed == s {
			break
		}
		s = strings.TrimSpace(trimmed)
	}
	return strings.TrimSpace(strings.Trim(s, "-–— \t"))
}

// stripTrailingAnnotation removes at most one trailing annotation group and
// reports the result. Returns the input unchanged when the tail is part of the
// name.
func stripTrailingAnnotation(s string) string {
	s = strings.TrimRight(s, " \t")
	if s == "" {
		return s
	}
	// "*F*", "*E*" — foil / etched markers.
	if strings.HasSuffix(s, "*") {
		if i := strings.LastIndex(s[:len(s)-1], "*"); i >= 0 {
			return s[:i]
		}
	}
	// "#123" — a collector number written with a hash.
	if i := strings.LastIndexByte(s, '#'); i > 0 && isDigits(strings.TrimSpace(s[i+1:])) {
		return s[:i]
	}
	// Bracketed groups: "[40K]", "[Ramp]", "<LTC>", "{noDeck}". A card name
	// never ends in one, so all three are safe to drop wholesale.
	for _, pair := range [][2]byte{{'[', ']'}, {'<', '>'}, {'{', '}'}} {
		if s[len(s)-1] == pair[1] {
			if i := strings.LastIndexByte(s, pair[0]); i > 0 {
				return s[:i]
			}
		}
	}
	// "(LTC)" — only when the contents look like a set code, so parenthesised
	// name suffixes are left alone.
	if s[len(s)-1] == ')' {
		if i := strings.LastIndexByte(s, '('); i > 0 && looksLikeSetCode(s[i+1:len(s)-1]) {
			return s[:i]
		}
	}
	// A bare collector number, but only when it trails a set code —
	// "(LTC) 123" — so a name is never truncated on its own last word.
	if fields := strings.Fields(s); len(fields) > 1 && isDigits(fields[len(fields)-1]) {
		head := strings.TrimSpace(strings.TrimSuffix(s, fields[len(fields)-1]))
		if strings.HasSuffix(head, ")") || strings.HasSuffix(head, "]") {
			return head
		}
	}
	return s
}

// looksLikeSetCode reports whether a parenthesised group is a set code rather
// than part of the card's name: two to six characters, alphanumeric, no space.
func looksLikeSetCode(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 2 || len(s) > 6 {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
