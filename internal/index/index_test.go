package index

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/data"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sampleDataset() *data.Dataset {
	return &data.Dataset{
		Records: []data.Record{
			{Corpus: data.CorpusMTG, Number: "702.21", Title: "Ward", Body: "Ward is a triggered ability that counters spells targeting the permanent.", Source: "MTG Comp Rules"},
			{Corpus: data.CorpusMTG, Number: "702.20", Title: "Vigilance", Body: "Vigilance lets a creature attack without tapping.", Source: "MTG Comp Rules"},
			{Corpus: data.CorpusDND, Number: "", Title: "Fireball", Body: "A bright streak flashes to a point then blossoms with a low roar into an explosion of flame.", Source: "D&D SRD"},
			{Corpus: data.CorpusDND, Number: "", Title: "Healing Word", Body: "A creature of your choice regains hit points.", Source: "D&D SRD"},
		},
		Meta: map[data.Corpus]data.CorpusMeta{
			data.CorpusMTG: {Name: "Magic: The Gathering", Version: "2026", SourceURL: "x", RecordCount: 2},
			data.CorpusDND: {Name: "D&D 5e SRD", Version: "master", SourceURL: "y", RecordCount: 2},
		},
	}
}

func TestIndexAndSearch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, sampleDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}

	// full-text search scoped to corpus
	mtg, err := s.Search(ctx, data.CorpusMTG, "ward counters", 10)
	if err != nil {
		t.Fatalf("search mtg: %v", err)
	}
	if len(mtg) == 0 {
		t.Fatal("expected ward result for MTG")
	}
	if mtg[0].Number != "702.21" {
		t.Errorf("top result = %s, want 702.21", mtg[0].Number)
	}

	dnd, err := s.Search(ctx, data.CorpusDND, "fireball", 10)
	if err != nil {
		t.Fatalf("search dnd: %v", err)
	}
	if len(dnd) == 0 || dnd[0].Title != "Fireball" {
		t.Errorf("dnd fireball search = %+v", dnd)
	}

	// corpus isolation: searching D&D shouldn't return MTG ward
	for _, r := range dnd {
		if r.Title == "Ward" {
			t.Error("MTG Ward leaked into D&D corpus results")
		}
	}
}

func TestSearch_DirectRuleNumber(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Index(ctx, sampleDataset())

	hits, err := s.Search(ctx, data.CorpusMTG, "702.20", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].Number != "702.20" {
		t.Errorf("direct number lookup failed: %+v", hits)
	}
}

func sectionDataset() *data.Dataset {
	return &data.Dataset{
		Records: []data.Record{
			// Deathtouch section: parent + sub-rules.
			{Corpus: data.CorpusMTG, Number: "702.2", Title: "Keyword Abilities", Body: "Deathtouch", Source: "MTG Comp Rules"},
			{Corpus: data.CorpusMTG, Number: "702.2a", Title: "Keyword Abilities", Body: "Deathtouch is a static ability.", Source: "MTG Comp Rules"},
			{Corpus: data.CorpusMTG, Number: "702.2b", Title: "Keyword Abilities", Body: "A creature dealt damage by a source with deathtouch is destroyed.", Source: "MTG Comp Rules"},
			{Corpus: data.CorpusMTG, Number: "702.2c", Title: "Keyword Abilities", Body: "Any nonzero amount of combat damage from deathtouch is lethal.", Source: "MTG Comp Rules"},
			// A sibling keyword in the same chapter — must NOT leak in.
			{Corpus: data.CorpusMTG, Number: "702.21", Title: "Keyword Abilities", Body: "Ward is a triggered ability.", Source: "MTG Comp Rules"},
			{Corpus: data.CorpusMTG, Number: "702.21a", Title: "Keyword Abilities", Body: "Ward is a triggered ability.", Source: "MTG Comp Rules"},
		},
		Meta: map[data.Corpus]data.CorpusMeta{
			data.CorpusMTG: {Name: "Magic: The Gathering", Version: "2026", SourceURL: "x", RecordCount: 6},
		},
	}
}

func TestSection_FromParent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, sectionDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}

	got, err := s.Section(ctx, data.CorpusMTG, "702.2")
	if err != nil {
		t.Fatalf("section: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 entries (parent + a,b,c), got %d: %+v", len(got), got)
	}
	if got[0].Number != "702.2" {
		t.Errorf("parent should be first, got %s", got[0].Number)
	}
	rest := []string{got[1].Number, got[2].Number, got[3].Number}
	want := []string{"702.2a", "702.2b", "702.2c"}
	for i := range want {
		if rest[i] != want[i] {
			t.Errorf("child %d = %s, want %s", i, rest[i], want[i])
		}
	}
	// Sibling section 702.21 must not leak in.
	for _, r := range got {
		if r.Number == "702.21" || r.Number == "702.21a" {
			t.Errorf("sibling section leaked into 702.2: %+v", r)
		}
	}
}

func TestSection_FromChildNormalizesToRoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Index(ctx, sectionDataset())

	// Asking for a sub-rule returns the whole section, parent first.
	got, err := s.Section(ctx, data.CorpusMTG, "702.2b")
	if err != nil {
		t.Fatalf("section: %v", err)
	}
	if len(got) != 4 || got[0].Number != "702.2" {
		t.Errorf("child query should resolve to full section parent-first, got %+v", got)
	}
}

func TestSection_RejectsNonRule(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Index(ctx, sectionDataset())

	got, err := s.Section(ctx, data.CorpusMTG, "deathtouch")
	if err != nil {
		t.Fatalf("section: %v", err)
	}
	if got != nil {
		t.Errorf("non-number query should return nil, got %+v", got)
	}
}

func TestSearch_EmptyQuery(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Index(ctx, sampleDataset())

	if _, err := s.Search(ctx, data.CorpusMTG, "   ", 10); err != ErrEmptyQuery {
		t.Errorf("empty query err = %v, want ErrEmptyQuery", err)
	}
}

func TestCorpusMeta(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Index(ctx, sampleDataset())

	meta, err := s.CorpusMeta(ctx)
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if meta[data.CorpusMTG].Name != "Magic: The Gathering" {
		t.Errorf("mtg meta = %+v", meta[data.CorpusMTG])
	}
}

func TestToFTSQueryOR_StripsStopwords(t *testing.T) {
	got, err := toFTSQueryOR("what does ward do and its rule number")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != `"ward"* OR "rule"* OR "number"*` {
		t.Errorf("toFTSQueryOR = %q", got)
	}
}

func TestRetrieve(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Index(ctx, sampleDataset())

	// A natural-language question should retrieve the relevant rule despite
	// stopwords and extra words (OR + bm25 ranking).
	res, err := s.Retrieve(ctx, data.CorpusMTG, "what does ward do", 5)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(res) == 0 || res[0].Number != "702.21" {
		t.Errorf("retrieve did not surface ward rule: %+v", res)
	}

	// All-stopword query returns no error, no docs.
	res, err = s.Retrieve(ctx, data.CorpusMTG, "what does the", 5)
	if err != nil {
		t.Fatalf("retrieve all-stopword err: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("expected no docs for all-stopword query, got %d", len(res))
	}
}

func TestToFTSQuery(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ward counters", `"ward"* "counters"*`},
		{"205.1a", `"205.1a"*`},
		{"\"quoted\"", `"quoted"*`},
		{"   ", ""},
	}
	for _, c := range cases {
		got, err := toFTSQuery(c.in)
		if c.want == "" {
			if err != ErrEmptyQuery {
				t.Errorf("toFTSQuery(%q) err = %v, want ErrEmptyQuery", c.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("toFTSQuery(%q) unexpected err: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("toFTSQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// layerDataset mirrors the shape of the real 613 rules: the layer list lives
// in 613.1a-613.1g, while a question about continuous effects tends to rank
// 613.6 and 613.5 instead. It also carries a large group (702) that must not
// be pulled in whole.
func layerDataset() *data.Dataset {
	recs := []data.Record{
		{Corpus: data.CorpusMTG, Number: "613.1", Title: "Layers", Body: "Characteristic values are determined in a series of layers.", Source: "MTG Comp Rules"},
		{Corpus: data.CorpusMTG, Number: "613.1d", Title: "Layers", Body: "Layer 4: Type-changing effects are applied.", Source: "MTG Comp Rules"},
		{Corpus: data.CorpusMTG, Number: "613.1f", Title: "Layers", Body: "Layer 6: Ability-adding and ability-removing effects are applied.", Source: "MTG Comp Rules"},
		{Corpus: data.CorpusMTG, Number: "613.2", Title: "Layers", Body: "Within layer 1, apply effects in sublayers.", Source: "MTG Comp Rules"},
		{Corpus: data.CorpusMTG, Number: "613.10", Title: "Layers", Body: "Some continuous effects affect players rather than objects.", Source: "MTG Comp Rules"},
		{Corpus: data.CorpusMTG, Number: "613.6", Title: "Layers", Body: "An effect applied in different layers applies in each of them.", Source: "MTG Comp Rules"},
	}
	// 702 is deliberately larger than maxGroupDocs.
	for i := 0; i < maxGroupDocs+5; i++ {
		recs = append(recs, data.Record{
			Corpus: data.CorpusMTG,
			Number: fmt.Sprintf("702.%d", i+1),
			Title:  "Keyword Abilities",
			Body:   fmt.Sprintf("Keyword ability number %d applies to continuous permanents.", i+1),
			Source: "MTG Comp Rules",
		})
	}
	return &data.Dataset{
		Records: recs,
		Meta: map[data.Corpus]data.CorpusMeta{
			data.CorpusMTG: {Name: "Magic: The Gathering", Version: "2026", SourceURL: "x", RecordCount: len(recs)},
		},
	}
}

func TestExpand_PullsWholeRuleGroup(t *testing.T) {
	// Regression for MAD-102: a top-N search returned 613.6 but never 613.1,
	// so the model could only report that the layer rules were missing.
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, layerDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	seeds := []Result{{Number: "613.6"}, {Number: "613.5"}}
	got, err := s.Expand(ctx, data.CorpusMTG, seeds)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	have := map[string]bool{}
	for _, r := range got {
		have[r.Number] = true
	}
	for _, want := range []string{"613.1", "613.1d", "613.1f", "613.2"} {
		if !have[want] {
			t.Errorf("expanded set missing %s: %v", want, numbersOf(got))
		}
	}
	// Seeds stay first so the best hits lead the prompt.
	if got[0].Number != "613.6" {
		t.Errorf("first doc = %s, want the top seed 613.6", got[0].Number)
	}
}

func TestExpand_SkipsOversizedGroup(t *testing.T) {
	// 702 (keyword abilities) is far too large to hand over whole; the seed
	// must still arrive, expanded only to its own section.
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, layerDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	got, err := s.Expand(ctx, data.CorpusMTG, []Result{{Number: "702.3"}})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(got) > maxGroundingDocs {
		t.Errorf("returned %d docs, over the %d cap", len(got), maxGroundingDocs)
	}
	if len(got) != 1 || got[0].Number != "702.3" {
		t.Errorf("want just the seed's own section, got %v", numbersOf(got))
	}
}

func TestExpand_UnnumberedCorpusPassesThrough(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, sampleDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	seeds := []Result{{Title: "Fireball", Body: "A bright streak flashes."}}
	got, err := s.Expand(ctx, data.CorpusDND, seeds)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Fireball" {
		t.Errorf("D&D seeds should pass through unchanged, got %+v", got)
	}
}

// dndPathDataset mirrors path-numbered D&D records: a top-level section
// ("spells/0003", the spells chapter) holding several spell sections, one of
// which is chunked, plus a second top-level section that must not leak in.
func dndPathDataset() *data.Dataset {
	recs := []data.Record{
		{Corpus: data.CorpusDND, Number: "spells/0003.0", Title: "Spells", Body: "This chapter covers the spells of the SRD.", Source: "SRD spells"},
		{Corpus: data.CorpusDND, Number: "spells/0003/0042.0", Title: "Spells — Fireball", Body: "A bright streak flashes and blossoms into fire, 8d6 damage on a failed Dexterity save.", Source: "SRD spells"},
		{Corpus: data.CorpusDND, Number: "spells/0003/0042.1", Title: "Spells — Fireball (part 2)", Body: "The damage increases by 1d6 per slot level above 3rd.", Source: "SRD spells"},
		{Corpus: data.CorpusDND, Number: "spells/0003/0043.0", Title: "Spells — Healing Word", Body: "A creature of your choice regains hit points.", Source: "SRD spells"},
		{Corpus: data.CorpusDND, Number: "spells/0009/0001.0", Title: "Magic Items — Bag of Holding", Body: "This bag has an interior space considerably larger than its outside dimensions.", Source: "SRD items"},
	}
	return &data.Dataset{
		Records: recs,
		Meta: map[data.Corpus]data.CorpusMeta{
			data.CorpusDND: {Name: "D&D 5e SRD", Version: "master", SourceURL: "y", RecordCount: len(recs)},
		},
	}
}

func TestExpand_DNDPullsWholeGroupAndSection(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, dndPathDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	// One seed inside the spells chapter: its whole top-level group fits the
	// budget, so the chapter intro and sibling spell come along.
	got, err := s.Expand(ctx, data.CorpusDND, []Result{{Number: "spells/0003/0042.0", Title: "Spells — Fireball"}})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	have := map[string]bool{}
	for _, r := range got {
		have[r.Number] = true
	}
	for _, want := range []string{"spells/0003.0", "spells/0003/0042.1", "spells/0003/0043.0"} {
		if !have[want] {
			t.Errorf("expanded set missing %s: %v", want, numbersOf(got))
		}
	}
	if have["spells/0009/0001.0"] {
		t.Errorf("unrelated top-level section leaked in: %v", numbersOf(got))
	}
	if got[0].Number != "spells/0003/0042.0" {
		t.Errorf("seed should lead the expansion, got %s", got[0].Number)
	}
}

func TestDNDSectionDocs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, dndPathDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	got, err := s.dndSectionDocs(ctx, data.CorpusDND, "spells/0003/0042")
	if err != nil {
		t.Fatalf("dndSectionDocs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want both chunks of the Fireball section, got %v", numbersOf(got))
	}
	if got[0].Number != "spells/0003/0042.0" || got[1].Number != "spells/0003/0042.1" {
		t.Errorf("chunks out of order: %v", numbersOf(got))
	}
}

func TestExpand_Empty(t *testing.T) {
	got, err := newTestStore(t).Expand(context.Background(), data.CorpusMTG, nil)
	if err != nil || got != nil {
		t.Errorf("Expand(nil) = %v, %v", got, err)
	}
}

func numbersOf(rs []Result) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Number)
	}
	return out
}

func TestLessRuleNumber(t *testing.T) {
	// Rule order is numeric per segment, and a bare rule leads its sub-rules.
	got := []string{"613.10", "613.2a", "702.2", "613.2", "702.2b", "702.2a"}
	sort.SliceStable(got, func(i, j int) bool { return lessRuleNumber(got[i], got[j]) })
	want := []string{"613.2", "613.2a", "613.10", "702.2", "702.2a", "702.2b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted = %v, want %v", got, want)
		}
	}
}

func TestRuleGroup(t *testing.T) {
	cases := map[string]string{"613.6c": "613", "702.21": "702", "613": "", "": "", "Fireball": ""}
	for in, want := range cases {
		if got := ruleGroup(in); got != want {
			t.Errorf("ruleGroup(%q) = %q, want %q", in, got, want)
		}
	}
}
