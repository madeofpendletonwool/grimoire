package data

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// mtgDefaultURL is the canonical MTG Comprehensive Rules text file.
const mtgDefaultURL = "https://media.wizards.com/2026/downloads/MagicCompRules%2020260807.txt"

// sectionTitleRe matches top-level section headers like "205. Type Line".
// A section header is "<number>. <Title>" — period immediately followed by whitespace.
var sectionTitleRe = regexp.MustCompile(`^(\d{2,3})\.\s+(.+)$`)

// ruleRe matches a numbered rule or subrule like "205.1. The type line" or
// "205.1a Some text". A real rule always has at least one ".digit" sub-number
// (bare "100." / "1." are section/chapter headers, handled separately). An
// optional trailing period is allowed (top-level rules like "205.1." have one).
var ruleRe = regexp.MustCompile(`^(\d{1,3}(?:\.\d+)+[a-z]?)\.?\s+(.+)$`)

// effectiveRe finds the "effective as of" date line.
var effectiveRe = regexp.MustCompile(`(?i)effective as of\s+([A-Za-z]+ \d{1,2}, \d{4})`)

// ParseMTG parses the MTG Comprehensive Rules text into a Dataset.
func ParseMTG(r io.Reader) (*Dataset, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read mtg rules: %w", err)
	}
	text := normalizeMTG(string(raw))

	// Split off the glossary and credits.
	rulesText, glossaryText := splitGlossary(text)

	version := ""
	if m := effectiveRe.FindStringSubmatch(text); m != nil {
		version = m[1]
	}

	ds := &Dataset{Meta: map[Corpus]CorpusMeta{}}
	var records []Record

	// --- Rules ---
	scanner := bufio.NewScanner(strings.NewReader(rulesText))
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	currentSection := ""
	var cur *Record
	flush := func() {
		if cur != nil && strings.TrimSpace(cur.Body) != "" {
			cur.Body = strings.TrimSpace(cur.Body)
			records = append(records, *cur)
		}
		cur = nil
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := sectionTitleRe.FindStringSubmatch(line); m != nil {
			flush()
			currentSection = strings.TrimSpace(m[2])
			continue
		}
		if m := ruleRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &Record{
				Corpus: CorpusMTG,
				Number: m[1],
				Title:  currentSection,
				Body:   strings.TrimSpace(m[2]),
				Source: "MTG Comprehensive Rules",
			}
			continue
		}
		// continuation line for the current rule
		if cur != nil {
			cur.Body += " " + strings.TrimSpace(line)
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan mtg rules: %w", err)
	}

	// --- Glossary ---
	gloss := parseGlossary(glossaryText)
	for _, g := range gloss {
		records = append(records, Record{
			Corpus: CorpusMTG,
			Number: "",
			Title:  g.Term,
			Body:   g.Definition,
			Source: "MTG Comprehensive Rules — Glossary",
		})
	}

	ds.Records = records
	ds.Meta[CorpusMTG] = CorpusMeta{
		Name:        "Magic: The Gathering",
		Version:     version,
		SourceURL:   mtgDefaultURL,
		RecordCount: countCorpus(records, CorpusMTG),
	}
	return ds, nil
}

// glossEntry is a parsed glossary term/definition pair.
type glossEntry struct {
	Term       string
	Definition string
}

// parseGlossary groups lines into blank-separated blocks: first line is the
// term, remaining lines joined are the definition. Separators are
// whitespace-only lines (the source uses non-breaking spaces for blanks).
func parseGlossary(text string) []glossEntry {
	var out []glossEntry
	var block []string
	flushBlock := func() {
		var ne []string
		for _, l := range block {
			if strings.TrimSpace(l) != "" {
				ne = append(ne, strings.TrimSpace(l))
			}
		}
		block = nil
		if len(ne) < 2 {
			return
		}
		out = append(out, glossEntry{Term: ne[0], Definition: strings.Join(ne[1:], " ")})
	}
	for _, raw := range strings.Split(text, "\n") {
		if strings.TrimSpace(raw) == "" {
			flushBlock()
			continue
		}
		block = append(block, raw)
	}
	flushBlock()
	return out
}

// splitGlossary separates the main rules text from the glossary.
// The file lists "Glossary" and "Credits" in the table of contents AND as real
// sections, so we take the LAST "Glossary" line and the first "Credits" after it.
func splitGlossary(text string) (rules, glossary string) {
	lines := strings.Split(text, "\n")
	glossStart, glossEnd := -1, -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "Glossary" {
			glossStart = i + 1
		}
	}
	if glossStart < 0 {
		return text, ""
	}
	for i := glossStart; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "Credits" {
			glossEnd = i
			break
		}
	}
	if glossEnd < 0 {
		glossEnd = len(lines)
	}
	rules = strings.Join(lines[:glossStart-1], "\n")
	glossary = strings.Join(lines[glossStart:glossEnd], "\n")
	return rules, glossary
}

// normalizeMTG replaces common typographic quirks: non-breaking spaces,
// smart quotes/dashes, and Windows line endings.
func normalizeMTG(s string) string {
	r := strings.NewReplacer(
		"\r\n", "\n",
		"\r", "\n",
		"\u00a0", " ", // non-breaking space
		"\u2013", "-", // en dash
		"\u2014", "-", // em dash
		"\u2018", "'", // left single quote
		"\u2019", "'", // right single quote
		"\u201c", `"`, // left double quote
		"\u201d", `"`, // right double quote
		"\u2026", "...", // ellipsis
	)
	return r.Replace(s)
}

func countCorpus(records []Record, c Corpus) int {
	n := 0
	for _, r := range records {
		if r.Corpus == c {
			n++
		}
	}
	return n
}
