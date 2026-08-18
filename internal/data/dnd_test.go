package data

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChunkMarkdown(t *testing.T) {
	in := mdFile{
		Path: "spells.md",
		Content: strings.Join([]string{
			"# Spells",
			"",
			"Intro about spells.",
			"",
			"## Fireball",
			"",
			"A burst of fire dealing **8d6** damage.",
			"",
			"### Scaling",
			"",
			"Damage increases per slot level.",
		}, "\n"),
	}
	recs := chunkMarkdown(in, "master")

	// Titles carry their full ancestor chain for context.
	have := map[string]string{}
	numbers := map[string]string{}
	for _, r := range recs {
		have[r.Title] = r.Body
		numbers[r.Title] = r.Number
	}
	if _, ok := have["Spells"]; !ok {
		t.Errorf("missing file-level section 'Spells'; titles: %v", keys(have))
	}
	fireball, ok := have["Spells — Fireball"]
	if !ok {
		t.Fatalf("missing nested section 'Spells — Fireball'; titles: %v", keys(have))
	}
	if !strings.Contains(fireball, "8d6") {
		t.Errorf("Fireball body = %q", fireball)
	}
	scaling, ok := have["Spells — Fireball — Scaling"]
	if !ok {
		t.Fatalf("missing nested section 'Spells — Fireball — Scaling'; titles: %v", keys(have))
	}
	if !strings.Contains(scaling, "per slot level") {
		t.Errorf("scaling body = %q", scaling)
	}
	// markdown bold should be stripped
	for _, r := range recs {
		if strings.Contains(r.Body, "**") {
			t.Errorf("body still has markdown bold: %q", r.Body)
		}
	}
	// Records carry stable, file-scoped numbers: slug/ordinal.chunk.
	for title, num := range numbers {
		if !strings.HasPrefix(num, "spells/") {
			t.Errorf("record %q number = %q, want spells/ prefix", title, num)
		}
		if !strings.Contains(num, ".") {
			t.Errorf("record %q number = %q, want a chunk suffix", title, num)
		}
	}
}

func TestChunkMarkdownNumbersSections(t *testing.T) {
	in := mdFile{
		Path:    "rules.md",
		Content: "# Rules\n\nA.\n\n## One\n\nB.\n\n## Two\n\nC.\n\n### Two-Bit\n\nD.\n",
	}
	recs := chunkMarkdown(in, "")
	byTitle := map[string]Record{}
	for _, r := range recs {
		byTitle[r.Title] = r
	}
	// Section ordinals are document order of the heading walk, so sibling
	// chunks share a section prefix and sections are distinguishable.
	one, two := byTitle["Rules — One"].Number, byTitle["Rules — Two"].Number
	if one == two {
		t.Errorf("sections One and Two share number %q", one)
	}
	// A nested heading lands in its own section, but its ordinal path keeps
	// the parent, so group expansion can still pull the whole subsection.
	twoBit := byTitle["Rules — Two — Two-Bit"].Number
	if DNDSectionKey(twoBit) == DNDSectionKey(two) {
		t.Errorf("child section %q and parent %q share a section key", twoBit, two)
	}
	if DNDGroupKey(twoBit) != DNDGroupKey(two) {
		t.Errorf("child section %q (group %q) left its parent's group %q", twoBit, DNDGroupKey(twoBit), DNDGroupKey(two))
	}
}

func TestSplitBodyChunksLongSections(t *testing.T) {
	para := strings.Repeat("word ", 60) // ~300 chars
	var body []string
	for i := 0; i < 12; i++ { // ~3600 chars total, must split
		body = append(body, para)
	}
	in := mdFile{Path: "long.md", Content: "# Long\n\n" + strings.Join(body, "\n\n")}
	recs := chunkMarkdown(in, "")
	if len(recs) < 3 {
		t.Fatalf("long section produced %d records, want >= 3 chunks", len(recs))
	}
	for i, r := range recs {
		if len(r.Body) > dndMaxChunkChars+2 {
			t.Errorf("chunk %d too long: %d chars", i, len(r.Body))
		}
		if want := fmt.Sprintf("(part %d)", i+1); !strings.Contains(r.Title, want) {
			t.Errorf("chunk %d title %q missing %q", i, r.Title, want)
		}
	}
	// The section prefix is shared so expansion can pull siblings.
	keys := map[string]bool{}
	for _, r := range recs {
		keys[DNDSectionKey(r.Number)] = true
	}
	if len(keys) != 1 {
		t.Errorf("chunks map to %d section keys, want 1: %v", len(keys), keys)
	}
}

func TestSplitSentencesRunaway(t *testing.T) {
	long := strings.Repeat("The rule applies here and there. ", 100) // no newline breaks
	for _, chunk := range splitSentences(long, 200) {
		if len(chunk) > 220 { // hard-split slack
			t.Errorf("sentence chunk too long: %d", len(chunk))
		}
	}
}

func TestCleanMarkdown_KeepsTablesAsText(t *testing.T) {
	in := "line one\n| Name | Cost |\n|---|---|\n| Longsword | 15 gp |\n| Battleaxe | 10 gp |\nline two"
	out := cleanMarkdown(in)
	if strings.Contains(out, "|") {
		t.Errorf("table pipes survived: %q", out)
	}
	if !strings.Contains(out, "Longsword — 15 gp") {
		t.Errorf("table row not flattened: %q", out)
	}
	if !strings.Contains(out, "Battleaxe — 10 gp") {
		t.Errorf("second table row lost: %q", out)
	}
	if !strings.Contains(out, "line one") || !strings.Contains(out, "line two") {
		t.Errorf("non-table content lost: %q", out)
	}
}

// The SRD carries its most valuable tables — class progression, monster stats,
// equipment — as raw HTML rather than markdown pipes. Left alone they reach FTS
// and the model as a wall of <tr>/<td> that matches no query.
func TestCleanMarkdown_FlattensHTMLTables(t *testing.T) {
	in := `Core Barbarian Traits

<table>
  <tbody>
    <tr>
      <td>Primary Ability</td>
      <td>Strength</td>
    </tr>
    <tr>
      <th>Hit Point Die</th>
      <td>D12 per <em>Barbarian</em> level</td>
    </tr>
  </tbody>
</table>`
	out := cleanMarkdown(in)
	for _, tag := range []string{"<table", "<tr", "<td", "<th", "<tbody", "<em"} {
		if strings.Contains(out, tag) {
			t.Errorf("markup %q survived: %q", tag, out)
		}
	}
	if !strings.Contains(out, "Primary Ability — Strength") {
		t.Errorf("row not flattened: %q", out)
	}
	// A header cell is a cell: the row carries its own context either way.
	if !strings.Contains(out, "Hit Point Die — D12 per Barbarian level") {
		t.Errorf("header row or nested markup mishandled: %q", out)
	}
	if !strings.Contains(out, "Core Barbarian Traits") {
		t.Errorf("surrounding prose lost: %q", out)
	}
}

// Tag stripping must not eat prose that merely contains angle brackets.
func TestCleanMarkdown_KeepsComparisonsInProse(t *testing.T) {
	out := cleanMarkdown("A result of < 10 > the DC fails.")
	if !strings.Contains(out, "< 10 >") {
		t.Errorf("comparison eaten as markup: %q", out)
	}
}

func TestParseDNDDocs(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		// The extractor writes the book's real title as the document's H1, and
		// that is what a citation should name — not the slugified file name.
		"phb.md":          "# Player's Handbook\n\nSome rule text about ability checks.\n",
		"sage-advice.txt": "Sage Advice — Equipment\n\nQ. Does cover stack?\nA. No.\n",
		"ignored.pdf":     "not a text doc",
		".hidden.md":      "skipped",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recs, reader, err := ParseDNDDocs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (md + txt only): %+v", len(recs), recs)
	}
	var sources []string
	for _, r := range recs {
		if r.Corpus != CorpusDND {
			t.Errorf("record corpus = %q", r.Corpus)
		}
		sources = append(sources, r.Source)
	}
	joined := strings.Join(sources, "\n")
	// The H1 names the document...
	if !strings.Contains(joined, "D&D books — Player's Handbook") {
		t.Errorf("sources = %v", sources)
	}
	// ...and a file without one falls back to its name.
	if !strings.Contains(joined, "D&D books — Sage-advice") {
		t.Errorf("sources = %v", sources)
	}

	// Each book also becomes its own reader guide, titled by the same label.
	if len(reader) == 0 {
		t.Fatal("no reader nodes for local books")
	}
	guides := map[string]bool{}
	for _, n := range reader {
		if n.Corpus != CorpusDND || n.GuideKind != "book" {
			t.Errorf("reader node corpus/kind = %s/%s", n.Corpus, n.GuideKind)
		}
		guides[n.Guide] = true
	}
	if !guides["books:phb"] || !guides["books:sage-advice"] {
		t.Errorf("reader guides = %v", keys2guides(reader))
	}
}

func keys2guides(nodes []ReaderNode) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.Guide)
	}
	return out
}

func keys(m map[string]string) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	return k
}

func TestBuildReaderNodes(t *testing.T) {
	f := mdFile{Path: "volo.md", Content: `# Volo's Guide

Some front matter about the book.

## Chapter One: Monsters

An intro to the chapter.

### Goblins

They are small.

**Alignment.** Typically chaotic.

## Chapter Two: Tables

<table>
  <thead>
    <tr><th>Name</th><th>CR</th></tr>
  </thead>
  <tbody>
    <tr><td>Goblin</td><td>1/4</td></tr>
  </tbody>
</table>
`}
	nodes := buildReaderNodes(f, CorpusDND, "books:volo-s-guide", "Volo's Guide", "book")

	// Expect: root (front matter), ch1, goblins, ch2 — plus the ch1 heading
	// intro living on the chapter node itself.
	var titles []string
	for _, n := range nodes {
		titles = append(titles, n.Title)
	}
	want := []string{"Introduction", "Chapter One: Monsters", "Goblins", "Chapter Two: Tables"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Fatalf("titles = %v, want %v", titles, want)
	}

	byTitle := map[string]ReaderNode{}
	for _, n := range nodes {
		byTitle[n.Title] = n
	}

	// The H1 names the guide and its preamble becomes the intro node.
	if nodes[0].Level != 1 || !strings.Contains(nodes[0].Body, "front matter") {
		t.Errorf("intro node = %+v", nodes[0])
	}

	// H2s are level 1 (the guide root's children); H3s nest at level 2.
	if byTitle["Chapter One: Monsters"].Level != 1 {
		t.Errorf("chapter level = %d", byTitle["Chapter One: Monsters"].Level)
	}
	if byTitle["Goblins"].Level != 2 {
		t.Errorf("section level = %d", byTitle["Goblins"].Level)
	}

	// Bodies are RAW markdown: the HTML table survives for reading.
	if !strings.Contains(byTitle["Chapter Two: Tables"].Body, "<table>") {
		t.Errorf("raw table lost from reading body: %q", byTitle["Chapter Two: Tables"].Body)
	}
	if !strings.Contains(byTitle["Chapter Two: Tables"].Body, "<td>Goblin</td>") {
		t.Errorf("table rows lost from reading body")
	}
	// A chapter with an intro keeps it as its direct body.
	if !strings.Contains(byTitle["Chapter One: Monsters"].Body, "intro to the chapter") {
		t.Errorf("chapter intro lost: %q", byTitle["Chapter One: Monsters"].Body)
	}
	// A leaf section's body stops at the next heading.
	if !strings.Contains(byTitle["Goblins"].Body, "Alignment.") {
		t.Errorf("leaf body lost bold run: %q", byTitle["Goblins"].Body)
	}

	// Node numbers use the record path scheme, so citations can deep-link.
	num := byTitle["Goblins"].Number
	if !strings.HasPrefix(num, "volo/") {
		t.Errorf("goblins number = %q", num)
	}
}

func TestBuildReaderNodes_BOMTitle(t *testing.T) {
	// The SRD's markdown ships a UTF-8 BOM; the H1 behind it must still name
	// the guide (regression: the BOM hid the heading and the guide fell back
	// to its file path).
	f := mdFile{Path: "spells.md", Content: "\ufeff# Spells\n\n## Casting Spells\n\nText.\n"}
	nodes := buildReaderNodes(f, CorpusDND, "srd:spells", docTitle(f), "srd")
	for _, n := range nodes {
		if n.GuideTitle != "Spells" {
			t.Errorf("guide title = %q, want Spells (BOM should not hide the H1)", n.GuideTitle)
		}
	}
	if nodes[0].Title != "Casting Spells" || !strings.Contains(nodes[0].Body, "Text.") {
		t.Errorf("first stop = %+v (no preamble means no Introduction stop)", nodes[0])
	}
}
