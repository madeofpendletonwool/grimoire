package data

import (
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
	for _, r := range recs {
		have[r.Title] = r.Body
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
}

func TestCleanMarkdown_StripsTables(t *testing.T) {
	in := "line one\n| col1 | col2 |\n|---|---|\n| a | b |\nline two"
	out := cleanMarkdown(in)
	if strings.Contains(out, "|") {
		t.Errorf("table not stripped: %q", out)
	}
	if !strings.Contains(out, "line one") || !strings.Contains(out, "line two") {
		t.Errorf("non-table content lost: %q", out)
	}
}

func keys(m map[string]string) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	return k
}
