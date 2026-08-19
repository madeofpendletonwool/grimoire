package deck

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseDecklist parses a pasted decklist: one card per line, with counts as
// either "2x Name", "2 Name", "Name x2", or "Name x 2". Commander-zone lines
// marked "Commander" (or a lone "1 Name" after a "Commander:" header) are
// tagged to the commander board. Comments (#, //), board headers, card-type
// section headers, and blank lines are skipped.
//
// Printing annotations are stripped from every name: the set code and
// collector number Archidekt, Moxfield and Arena decorate their exports with
// are not part of the card's name, and leaving them attached is what makes a
// real export fail to resolve. What remains is kept verbatim — resolution
// against the card database happens separately, so a typo'd name survives to
// be fuzzy-matched or flagged, never silently dropped.
func ParseDecklist(text string) []Entry {
	var entries []Entry
	board := "main"
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		// MTGO writes sideboard lines inline as "SB: 1 Sol Ring".
		if rest, ok := cutPrefixFold(line, "SB:"); ok {
			if e, ok := parseCardLine(rest, "sideboard"); ok {
				entries = append(entries, e)
			}
			continue
		}
		// A board header switches the following lines; a trailing "(15)" from
		// an exporter is part of the header, not a card.
		if b, ok := boardHeader(line); ok {
			board = b
			continue
		}
		// "Commander: Kaalia of the Vast" carries the name inline.
		if name, ok := commanderInline(line); ok {
			entries = append(entries, Entry{Name: CleanCardName(name), Count: 1, Board: "commander"})
			continue
		}
		// Type headers an exporter writes above each group ("Creatures (30)").
		// One of these closes the commander block: an export lists the
		// commander first and then groups the maindeck by type, so without
		// this every card after "Commander (1)" lands in the command zone.
		if sectionHeader(line) {
			if board == "commander" {
				board = "main"
			}
			continue
		}
		if e, ok := parseCardLine(line, board); ok {
			entries = append(entries, e)
		}
	}
	return entries
}

// parseCardLine turns one decklist line into an entry, or reports that the
// line names no card. Every line that does name a card is kept verbatim apart
// from its printing annotations — resolution against the card database
// happens separately, so a typo'd name survives to be matched or flagged,
// never silently dropped.
func parseCardLine(line, board string) (Entry, bool) {
	count, name := splitCount(line)
	name = CleanCardName(name)
	if name == "" {
		return Entry{}, false
	}
	// Prose tolerance: model-written drafts wrap the list in sentences. A bare
	// line (no leading count) that reads like prose — sentence punctuation, a
	// colon, or too many words — is skipped. Card names never end in ".", "!",
	// or "?" and never contain ":".
	if count == 1 && !hasLeadingCount(line) && looksLikeProse(name) {
		return Entry{}, false
	}
	return Entry{Name: name, Count: count, Board: board}, true
}

// boardHeader matches a line that names a board rather than a card, tolerating
// the "(15)" count exporters append. It returns the board the following lines
// belong to.
func boardHeader(line string) (string, bool) {
	head := strings.ToLower(stripTrailingCount(line))
	head = strings.TrimRight(head, ":")
	switch head {
	case "commander", "commanders", "command zone", "commandzone":
		return "commander", true
	case "sideboard", "side", "sb":
		return "sideboard", true
	case "mainboard", "maindeck", "main", "deck", "decklist":
		return "main", true
	}
	return "", false
}

// sectionHeader reports whether a line is one of the card-type group headings
// exporters write above each block ("Creatures (30)", "Lands"). Those are not
// cards, and counting them as one is how a pasted list grows phantom entries.
func sectionHeader(line string) bool {
	return sectionHeaders[strings.ToLower(strings.TrimRight(stripTrailingCount(line), ":"))]
}

var sectionHeaders = map[string]bool{
	"creature": true, "creatures": true, "land": true, "lands": true,
	"instant": true, "instants": true, "sorcery": true, "sorceries": true,
	"artifact": true, "artifacts": true, "enchantment": true, "enchantments": true,
	"planeswalker": true, "planeswalkers": true, "battle": true, "battles": true,
	"kindred": true, "tribal": true, "token": true, "tokens": true,
	"maybeboard": true, "considering": true, "companion": true,
	"spell": true, "spells": true, "ramp": true, "removal": true, "other": true,
}

// stripTrailingCount drops an exporter's "(30)" tally from a header line.
func stripTrailingCount(line string) string {
	s := strings.TrimSpace(line)
	if !strings.HasSuffix(s, ")") {
		return s
	}
	i := strings.LastIndexByte(s, '(')
	if i < 1 || !isDigits(strings.TrimSpace(s[i+1:len(s)-1])) {
		return s
	}
	return strings.TrimSpace(s[:i])
}

// cutPrefixFold strips a case-insensitive prefix, reporting whether it was
// there.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return strings.TrimSpace(s[len(prefix):]), true
}

// looksLikeProse reports whether a bare line reads as a sentence rather than
// a card name.
func looksLikeProse(name string) bool {
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, "!") || strings.HasSuffix(name, "?") {
		return true
	}
	if strings.Contains(name, ":") {
		return true
	}
	return len(strings.Fields(name)) > 6
}

// commanderInline matches a "Commander[s]: Name" line and returns the name
// verbatim (case preserved).
func commanderInline(line string) (string, bool) {
	lower := strings.ToLower(line)
	if !strings.HasPrefix(lower, "commander") {
		return "", false
	}
	rest := lower[len("commander"):]
	if strings.HasPrefix(rest, "s") {
		rest = rest[1:]
	}
	if !strings.HasPrefix(rest, ":") && !strings.HasPrefix(rest, " ") {
		return "", false
	}
	rest = strings.TrimLeft(rest, ": \t")
	if rest == "" {
		return "", false
	}
	name := strings.TrimSpace(line[len(line)-len(rest):])
	if name == "" {
		return "", false
	}
	return name, true
}

// splitCount pulls a leading or trailing count off a line. Leading forms:
// "2x Name", "2 Name", "2x: Name". Trailing forms: "Name x2", "Name x 2".
// A line with no count is one copy.
func splitCount(line string) (int, string) {
	if n, rest, ok := leadingCount(line); ok {
		return n, strings.TrimSpace(rest)
	}
	if n, name, ok := trailingCount(line); ok {
		return n, name
	}
	return 1, strings.TrimSpace(line)
}

// hasLeadingCount reports whether the line opens with an explicit count, which
// makes it a card line no matter how it reads.
func hasLeadingCount(line string) bool {
	_, _, ok := leadingCount(line)
	return ok
}

// leadingCount matches a count at the start of the line.
func leadingCount(line string) (int, string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, "", false
	}
	first := strings.TrimSuffix(fields[0], ":")
	first = strings.TrimSuffix(strings.ToLower(first), "x")
	n, err := strconv.Atoi(first)
	if err != nil || n < 1 || n > maxCopies {
		return 0, "", false
	}
	return n, strings.Join(fields[1:], " "), true
}

// trailingCount matches "Name x2" or "Name x 2" at the end of the line.
func trailingCount(line string) (int, string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return 0, "", false
	}
	last := fields[len(fields)-1]
	// "Name x 2": the x is its own field before a bare number.
	if strings.EqualFold(fields[len(fields)-2], "x") {
		if n, err := strconv.Atoi(last); err == nil && n >= 1 && n <= maxCopies {
			return n, strings.Join(fields[:len(fields)-2], " "), true
		}
	}
	// "Name x2": the count is glued to the x.
	lower := strings.ToLower(last)
	if len(lower) > 1 && lower[0] == 'x' {
		if n, err := strconv.Atoi(lower[1:]); err == nil && n >= 1 && n <= maxCopies {
			return n, strings.Join(fields[:len(fields)-1], " "), true
		}
	}
	return 0, "", false
}

// FormatDecklist renders entries back to a copyable decklist: commander
// first, then maindeck grouped alphabetically, then sideboard.
func FormatDecklist(entries []Entry) string {
	var cmdr, main, side []Entry
	for _, e := range entries {
		switch e.Board {
		case "commander":
			cmdr = append(cmdr, e)
		case "sideboard":
			side = append(side, e)
		default:
			main = append(main, e)
		}
	}
	var b strings.Builder
	for _, e := range cmdr {
		fmt.Fprintf(&b, "Commander: %s\n", e.Name)
	}
	if len(main) > 0 {
		b.WriteString("\n")
		sortEntries(main)
		for _, e := range main {
			fmt.Fprintf(&b, "%d %s\n", e.Count, e.Name)
		}
	}
	if len(side) > 0 {
		b.WriteString("\nSideboard\n")
		sortEntries(side)
		for _, e := range side {
			fmt.Fprintf(&b, "%d %s\n", e.Count, e.Name)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func sortEntries(entries []Entry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Name < entries[j-1].Name; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

// Total returns the total card count across the given board ("main" totals
// the maindeck only). Counts multiply by entry count.
func Total(entries []Entry, board string) int {
	n := 0
	for _, e := range entries {
		if board == "" || board == "main" && e.Board == "" || e.Board == board {
			n += e.Count
		}
	}
	return n
}
