package data

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// mtgDefaultURL is the canonical MTG Comprehensive Rules text file.
const mtgDefaultURL = "https://media.wizards.com/2026/downloads/MagicCompRules%2020260807.txt"

// mtgReaderGuide identifies the Comprehensive Rules reader guide.
const mtgReaderGuide = "rules"

// sectionTitleRe matches top-level section headers like "205. Type Line".
// A section header is "<number>. <Title>" — period immediately followed by whitespace.
var sectionTitleRe = regexp.MustCompile(`^(\d{2,3})\.\s+(.+)$`)

// chapterTitleRe matches a chapter header like "2. Parts of a Card". Chapters
// are single-digit and appear in the table of contents and again as the real
// header ahead of their first section; either occurrence carries the title.
var chapterTitleRe = regexp.MustCompile(`^(\d)\.\s+(.+)$`)

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
	rulesText, glossaryText, creditsText := splitGlossary(text)

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
	// Reader-tree inputs, gathered during the same scan: chapter titles
	// ("2. Parts of a Card") and section headers in first-seen (book) order.
	// Each section's rule lines are assembled afterwards from the records.
	chapterTitles := map[int]string{}
	sectionTitles := map[string]string{}
	var sectionOrder []string
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := chapterTitleRe.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				if _, ok := chapterTitles[n]; !ok {
					chapterTitles[n] = strings.TrimSpace(m[2])
				}
			}
			continue // a chapter header is never a rule or a continuation
		}
		if m := sectionTitleRe.FindStringSubmatch(line); m != nil {
			flush()
			currentSection = strings.TrimSpace(m[2])
			if _, ok := sectionTitles[m[1]]; !ok {
				sectionTitles[m[1]] = currentSection
				sectionOrder = append(sectionOrder, m[1])
			}
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

	// Assemble each section's reading body from its rules. A rule belongs to
	// the section named by the leading segment of its number ("205.1a" → 205).
	secBodies := map[string][]string{}
	for _, rec := range records {
		sec := strings.SplitN(rec.Number, ".", 2)[0]
		secBodies[sec] = append(secBodies[sec], rec.Number+". "+rec.Body)
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
	ds.Reader = mtgReaderNodes(text, chapterTitles, sectionOrder, sectionTitles, secBodies, gloss, creditsText)
	ds.Meta[CorpusMTG] = CorpusMeta{
		Name:        "Magic: The Gathering",
		Version:     version,
		SourceURL:   mtgDefaultURL,
		RecordCount: countCorpus(records, CorpusMTG),
	}
	return ds, nil
}

// mtgReaderNodes shapes the Comprehensive Rules into the reader tree: one
// guide, with a chapter per top-level part ("1. Game Concepts" …), a section
// per 3-digit header ("205. Type Line"), the glossary as a chapter of term
// entries, and the credits closing the book.
func mtgReaderNodes(text string, chapterTitles map[int]string, sectionOrder []string, sectionTitles map[string]string, secBodies map[string][]string, gloss []glossEntry, credits string) []ReaderNode {
	const source = "MTG Comprehensive Rules"
	pos := 0
	node := func(number, title string, level int, body string) ReaderNode {
		pos++
		return ReaderNode{
			Corpus: CorpusMTG, Guide: mtgReaderGuide, GuideTitle: "Comprehensive Rules", GuideKind: "rules",
			Number: number, Title: title, Level: level, Position: pos, Body: body, Source: source,
		}
	}
	var nodes []ReaderNode
	add := func(number, title string, level int, body string) {
		nodes = append(nodes, node(number, title, level, body))
	}

	if intro := mtgIntro(text); intro != "" {
		add("intro", "Introduction", 1, intro)
	}
	sectionsByChapter := map[int][]string{}
	for _, sec := range sectionOrder {
		n, err := strconv.Atoi(sec)
		if err != nil {
			continue
		}
		sectionsByChapter[n/100] = append(sectionsByChapter[n/100], sec)
	}
	for ch := 1; ch <= 9; ch++ {
		title, ok := chapterTitles[ch]
		if !ok && len(sectionsByChapter[ch]) == 0 {
			continue // a chapter the file never names and never fills
		}
		if !ok {
			title = fmt.Sprintf("Chapter %d", ch)
		}
		add(strconv.Itoa(ch), title, 1, "")
		for _, sec := range sectionsByChapter[ch] {
			secTitle, ok := sectionTitles[sec]
			if !ok || secTitle == "" {
				secTitle = "Section " + sec
			}
			add(sec, secTitle, 2, strings.Join(secBodies[sec], "\n\n"))
		}
	}
	add("glossary", "Glossary", 1, "")
	for i, g := range gloss {
		add(fmt.Sprintf("glossary/%04d", i+1), g.Term, 2, g.Definition)
	}
	if body := strings.TrimSpace(credits); body != "" {
		add("credits", "Credits", 1, body)
	}
	return nodes
}

// mtgIntro pulls the prose between the "Introduction" and "Contents" headers —
// the front matter a reader expects before chapter 1.
func mtgIntro(text string) string {
	lines := strings.Split(text, "\n")
	start, end := -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if start < 0 && t == "Introduction" {
			start = i + 1
			continue
		}
		if start >= 0 && t == "Contents" {
			end = i
			break
		}
	}
	if start < 0 || end < 0 || end <= start {
		return ""
	}
	var kept []string
	for _, l := range lines[start:end] {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, strings.TrimSpace(l))
		}
	}
	return strings.Join(kept, "\n\n")
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

// splitGlossary separates the main rules text from the glossary and the
// credits. The file lists "Glossary" and "Credits" in the table of contents
// AND as real sections, so we take the LAST "Glossary" line and the first
// "Credits" after it. The credits text runs from there to the end of file.
func splitGlossary(text string) (rules, glossary, credits string) {
	lines := strings.Split(text, "\n")
	glossStart, glossEnd := -1, -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "Glossary" {
			glossStart = i + 1
		}
	}
	if glossStart < 0 {
		return text, "", ""
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
	credits = strings.Join(lines[min(glossEnd+1, len(lines)):], "\n")
	return rules, glossary, credits
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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
