package deck

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseDecklist parses a pasted decklist: one card per line, with counts as
// either "2x Name", "2 Name", "Name x2", or "Name x 2". Commander-zone lines
// marked "Commander" (or a lone "1 Name" after a "Commander:" header) are
// tagged to the commander board. Comments (#, //), sideboard headers, and
// blank lines are skipped. Every line that names a card is kept verbatim —
// resolution against the card database happens separately, so a typo'd name
// survives to be fuzzy-matched or flagged, never silently dropped.
func ParseDecklist(text string) []Entry {
	var entries []Entry
	board := "main"
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)

		// "Commander: Kaalia" carries the name inline (one-shot); a bare
		// "Commander" / "Commanders" header switches the following lines.
		if lower == "commander" || lower == "commanders" || lower == "command zone" {
			board = "commander"
			continue
		}
		if name, ok := commanderInline(line); ok {
			entries = append(entries, Entry{Name: name, Count: 1, Board: "commander"})
			continue
		}
		if lower == "sideboard" || lower == "side" || strings.HasPrefix(lower, "sideboard") {
			board = "sideboard"
			continue
		}
		if lower == "mainboard" || lower == "main" || lower == "deck" {
			board = "main"
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}

		count, name := splitCount(line)
		if name == "" {
			continue
		}
		// Prose tolerance: model-written drafts wrap the list in sentences.
		// A bare line (no leading count) that reads like prose — sentence
		// punctuation, a colon, or too many words — is skipped. Card names
		// never end in ".", "!", or "?" and never contain ":".
		if count == 1 && name == line && looksLikeProse(name) {
			continue
		}
		entries = append(entries, Entry{Name: name, Count: count, Board: board})
	}
	return entries
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
