package index

import (
	"context"
	"path/filepath"
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
