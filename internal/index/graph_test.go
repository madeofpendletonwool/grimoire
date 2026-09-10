package index

import (
	"context"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/data"
)

func TestXrefTargets(t *testing.T) {
	cases := []struct {
		name   string
		number string
		body   string
		want   []string
	}{{
		name:   "dotted citation anywhere in the prose",
		number: "608.3b",
		body:   "It checks whether the target is still legal, as described in 608.2b.",
		want:   []string{"608.2b"},
	}, {
		name:   "bare chapter needs the word rule",
		number: "",
		body:   `An artifact subtype. See rule 301, "Artifacts," and rule 702.6, "Equip."`,
		want:   []string{"702.6", "301"},
	}, {
		// Regression: "rule 702.11" also matches the bare-chapter pattern at
		// the word boundary after "702", so without the dotted-suffix guard
		// every keyword citation silently pulled in all ~770 rules of 702.
		name:   "a dotted citation is not also a chapter citation",
		number: "",
		body:   `A keyword ability. See rule 702.11, "Hexproof."`,
		want:   []string{"702.11"},
	}, {
		name:   "self and ancestor citations are dropped",
		number: "608.2b",
		body:   "As described in 608.2 and in 608.2b itself, but see 704.5a.",
		want:   []string{"704.5a"},
	}, {
		name:   "numbers that are not rule numbers",
		number: "302.6",
		body:   "A creature with power 2.5 is impossible; damage of 4 is not.",
		want:   nil,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := xrefTargets(tc.number, tc.body)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("xrefTargets(%q) = %v, want %v", tc.number, got, tc.want)
			}
		})
	}
}

func TestTermName(t *testing.T) {
	cases := []struct {
		number, title, body, want string
	}{
		{"", "Illegal Target", "A target that no longer exists.", "Illegal Target"},
		{"702.6", "Keyword Abilities", "Equip", "Equip"},
		{"701.2", "Keyword Actions", "Activate", "Activate"},
		// A lettered sub-rule is prose, not a name, even when it is short.
		{"702.3a", "Keyword Abilities", "Defender is a static ability.", ""},
		// A rule outside the keyword chapters names nothing.
		{"301.5", "Artifacts", "Equipment", ""},
	}
	for _, tc := range cases {
		if got := termName(tc.number, tc.title, tc.body); got != tc.want {
			t.Errorf("termName(%q, %q, %q) = %q, want %q", tc.number, tc.title, tc.body, got, tc.want)
		}
	}
}

// graphDataset is a miniature of the shape the real corpus has: a keyword rule
// whose text says nothing about targeting, a glossary entry pointing at the
// keyword, and a targeting rule that cites the resolution rule by number.
func graphDataset() *data.Dataset {
	recs := []data.Record{
		{Corpus: data.CorpusMTG, Number: "702.11", Title: "Keyword Abilities", Body: "Hexproof"},
		{Corpus: data.CorpusMTG, Number: "702.11b", Title: "Keyword Abilities", Body: `"Hexproof" on a permanent means "This permanent can't be the target of spells or abilities your opponents control."`},
		{Corpus: data.CorpusMTG, Number: "115.1", Title: "Targets", Body: "Some spells and abilities require their controller to choose one or more targets."},
		{Corpus: data.CorpusMTG, Number: "115.10", Title: "Targets", Body: `Spells and abilities can affect objects they don't target. See rule 608, "Resolving Spells and Abilities."`},
		{Corpus: data.CorpusMTG, Number: "608.1", Title: "Resolving Spells and Abilities", Body: "Each time all players pass in succession, the top object on the stack resolves."},
		{Corpus: data.CorpusMTG, Number: "608.2", Title: "Resolving Spells and Abilities", Body: "If the object that's resolving is an instant spell, its resolution may involve several steps."},
		{Corpus: data.CorpusMTG, Number: "608.2b", Title: "Resolving Spells and Abilities", Body: "If the spell specifies targets, it checks whether the targets are still legal. If all its targets are now illegal, the spell doesn't resolve."},
		{Corpus: data.CorpusMTG, Number: "", Title: "Hexproof", Body: `A keyword ability that precludes a permanent from being targeted by an opponent. See rule 702.11, "Hexproof."`},
	}
	return &data.Dataset{
		Records: recs,
		Meta:    map[data.Corpus]data.CorpusMeta{data.CorpusMTG: {Name: "Magic", Version: "t", SourceURL: "x", RecordCount: len(recs)}},
	}
}

func TestTermRulesResolvesGlossaryAndKeywords(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, graphDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}

	got, err := s.TermRules(ctx, data.CorpusMTG, []string{"hexproof"})
	if err != nil {
		t.Fatalf("term rules: %v", err)
	}
	if len(got) != 1 || got[0] != "702.11" {
		t.Errorf("TermRules(hexproof) = %v, want [702.11]", got)
	}

	if got, _ := s.TermRules(ctx, data.CorpusMTG, []string{"not a term"}); len(got) != 0 {
		t.Errorf("TermRules(unknown) = %v, want none", got)
	}
}

// Expand must follow citations out of the rules it gathered, not just rank
// paragraphs by keyword overlap. Without the cross-reference hops, a question
// about hexproof and targeting never reaches 608.2b — the rule that actually
// decides the interaction — because nothing in the hexproof or targeting text
// contains the words a search would match it on.
func TestExpandFollowsCitationsToTheDecidingRule(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, graphDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}

	seeds := []Result{
		{Number: "702.11b", Title: "Keyword Abilities", Body: "hexproof"},
		{Number: "115.1", Title: "Targets", Body: "targets", Anchor: true},
	}
	docs, err := s.Expand(ctx, data.CorpusMTG, seeds)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	got := map[string]bool{}
	for _, d := range docs {
		got[d.Number] = true
	}
	// 115.10 is two hops from the seeds: pulled in with its section, it cites
	// 608, which brings 608.2b.
	if !got["115.10"] {
		t.Fatalf("expansion missed 115.10; got %v", numbersOf(docs))
	}
	if !got["608.2b"] {
		t.Errorf("expansion never followed the citation to 608.2b; got %v", numbersOf(docs))
	}
}

// An index rebuilt without the graph tables must not leave the previous
// build's citations behind: a cross-reference to a rule that no longer exists
// would send retrieval after a paragraph that is gone.
func TestReindexReplacesGraph(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, graphDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	if got, _ := s.TermRules(ctx, data.CorpusMTG, []string{"hexproof"}); len(got) == 0 {
		t.Fatal("expected a term index after the first build")
	}

	bare := &data.Dataset{
		Records: []data.Record{{Corpus: data.CorpusMTG, Number: "100.1", Title: "General", Body: "These rules apply to any Magic game."}},
		Meta:    map[data.Corpus]data.CorpusMeta{data.CorpusMTG: {Name: "Magic", Version: "t2", SourceURL: "x", RecordCount: 1}},
	}
	if err := s.Index(ctx, bare); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if got, _ := s.TermRules(ctx, data.CorpusMTG, []string{"hexproof"}); len(got) != 0 {
		t.Errorf("stale terms survived a rebuild: %v", got)
	}
	if got, _ := s.Xrefs(ctx, data.CorpusMTG, []string{"115.10"}); len(got) != 0 {
		t.Errorf("stale citations survived a rebuild: %v", got)
	}
}
