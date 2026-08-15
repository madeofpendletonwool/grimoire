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
	recs, err := ParseDNDDocs(dir)
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
}

func keys(m map[string]string) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	return k
}
