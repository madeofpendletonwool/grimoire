package index

import (
	"context"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/data"
)

func readerDataset() *data.Dataset {
	return &data.Dataset{
		Records: []data.Record{
			{Corpus: data.CorpusMTG, Number: "205.1", Title: "Type Line", Body: "The type line is...", Source: "MTG Comp Rules"},
			{Corpus: data.CorpusDND, Number: "spells/0003/0042.0", Title: "Casting Spells — Range", Body: "range text", Source: "D&D 5e SRD — spells"},
		},
		Meta: map[data.Corpus]data.CorpusMeta{
			data.CorpusMTG: {Name: "Magic: The Gathering", Version: "2026", SourceURL: "x", RecordCount: 1},
			data.CorpusDND: {Name: "D&D 5e SRD", Version: "master", SourceURL: "y", RecordCount: 1},
		},
		Reader: []data.ReaderNode{
			// MTG: one guide, chapters + sections + glossary terms.
			{Corpus: data.CorpusMTG, Guide: "rules", GuideTitle: "Comprehensive Rules", GuideKind: "rules", Number: "1", Title: "Game Concepts", Level: 1, Position: 1},
			{Corpus: data.CorpusMTG, Guide: "rules", GuideTitle: "Comprehensive Rules", GuideKind: "rules", Number: "101", Title: "The Magic Golden Rules", Level: 2, Position: 2, Body: "101.1. The golden rule text."},
			{Corpus: data.CorpusMTG, Guide: "rules", GuideTitle: "Comprehensive Rules", GuideKind: "rules", Number: "2", Title: "Parts of a Card", Level: 1, Position: 3},
			{Corpus: data.CorpusMTG, Guide: "rules", GuideTitle: "Comprehensive Rules", GuideKind: "rules", Number: "205", Title: "Type Line", Level: 2, Position: 4, Body: "205.1. The type line is..."},
			{Corpus: data.CorpusMTG, Guide: "rules", GuideTitle: "Comprehensive Rules", GuideKind: "rules", Number: "glossary", Title: "Glossary", Level: 1, Position: 5},
			{Corpus: data.CorpusMTG, Guide: "rules", GuideTitle: "Comprehensive Rules", GuideKind: "rules", Number: "glossary/0001", Title: "Ability", Level: 2, Position: 6, Body: "Text on an object..."},
			// D&D: an SRD guide and a local book guide.
			{Corpus: data.CorpusDND, Guide: "srd:spells", GuideTitle: "Spells", GuideKind: "srd", Number: "spells", Title: "Introduction", Level: 1, Position: 1, Body: "spell intro"},
			{Corpus: data.CorpusDND, Guide: "srd:spells", GuideTitle: "Spells", GuideKind: "srd", Number: "spells/0003", Title: "Casting Spells", Level: 1, Position: 2, Body: "casting intro"},
			{Corpus: data.CorpusDND, Guide: "srd:spells", GuideTitle: "Spells", GuideKind: "srd", Number: "spells/0003/0042", Title: "Range", Level: 2, Position: 3, Body: "range text"},
			{Corpus: data.CorpusDND, Guide: "books:xanathars", GuideTitle: "Xanathar's Guide", GuideKind: "book", Number: "xanathars-guide-to-everything", Title: "Introduction", Level: 1, Position: 1, Body: "book intro"},
		},
	}
}

func TestReaderGuidesAndTOC(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, readerDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}

	ok, err := s.ReaderIndexed(ctx)
	if err != nil || !ok {
		t.Fatalf("ReaderIndexed = %v, %v", ok, err)
	}

	mtg, err := s.ReaderGuides(ctx, data.CorpusMTG)
	if err != nil {
		t.Fatalf("guides: %v", err)
	}
	if len(mtg) != 1 || mtg[0].Guide != "rules" || mtg[0].Title != "Comprehensive Rules" || mtg[0].Kind != "rules" {
		t.Fatalf("mtg guides = %+v", mtg)
	}
	if mtg[0].Nodes != 6 {
		t.Errorf("mtg guide nodes = %d, want 6", mtg[0].Nodes)
	}

	dnd, err := s.ReaderGuides(ctx, data.CorpusDND)
	if err != nil {
		t.Fatalf("guides: %v", err)
	}
	if len(dnd) != 2 || dnd[0].Guide != "srd:spells" || dnd[1].Guide != "books:xanathars" || dnd[1].Kind != "book" {
		t.Fatalf("dnd guides = %+v", dnd)
	}

	toc, err := s.ReaderTOC(ctx, data.CorpusMTG, "rules")
	if err != nil {
		t.Fatalf("toc: %v", err)
	}
	if len(toc) != 6 {
		t.Fatalf("toc len = %d, want 6", len(toc))
	}
	// Book order, not lexical order: "2" follows "101".
	if toc[1].Number != "101" || toc[2].Number != "2" {
		t.Errorf("toc order = %s, %s", toc[1].Number, toc[2].Number)
	}
	// Containers report empty bodies; leaves report full ones.
	if toc[0].HasBody || !toc[1].HasBody {
		t.Errorf("HasBody flags wrong: %+v %+v", toc[0], toc[1])
	}
}

func TestReaderPage_CrumbsAndNeighbours(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Index(ctx, readerDataset())

	p, err := s.ReaderPage(ctx, data.CorpusMTG, "rules", "205")
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if p.Title != "Type Line" || !p.HasBodyText("205.1.") {
		t.Errorf("page = %+v", p)
	}
	if len(p.Crumbs) != 1 || p.Crumbs[0].Number != "2" {
		t.Errorf("crumbs = %+v", p.Crumbs)
	}
	if p.Prev == nil || p.Prev.Number != "101" {
		t.Errorf("prev = %+v", p.Prev)
	}
	// Next skips the empty Glossary container and lands on its first term.
	if p.Next == nil || p.Next.Number != "glossary/0001" {
		t.Errorf("next = %+v", p.Next)
	}

	// A glossary term's next walks off the end of the book.
	p2, err := s.ReaderPage(ctx, data.CorpusMTG, "rules", "glossary/0001")
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if p2.Next != nil {
		t.Errorf("last page next = %+v, want nil", p2.Next)
	}

	// A missing node is a plain not-found.
	if _, err := s.ReaderPage(ctx, data.CorpusMTG, "rules", "999"); err == nil {
		t.Error("missing node should error")
	}
}

func TestReaderResolve(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Index(ctx, readerDataset())

	// MTG: any rule in a section resolves to that section's node.
	guide, node, err := s.ReaderResolve(ctx, data.CorpusMTG, "205.1a")
	if err != nil || guide != "rules" || node != "205" {
		t.Errorf("resolve 205.1a = %q/%q, %v", guide, node, err)
	}
	// A number with no reader node resolves empty, not error.
	guide, node, err = s.ReaderResolve(ctx, data.CorpusMTG, "999.9z")
	if err != nil || guide != "" || node != "" {
		t.Errorf("resolve miss = %q/%q, %v", guide, node, err)
	}

	// D&D: a record number (with chunk suffix) resolves to its heading node.
	guide, node, err = s.ReaderResolve(ctx, data.CorpusDND, "spells/0003/0042.1")
	if err != nil || guide != "srd:spells" || node != "spells/0003/0042" {
		t.Errorf("resolve dnd = %q/%q, %v", guide, node, err)
	}
	// An unknown D&D path walks up and still misses quietly.
	guide, node, err = s.ReaderResolve(ctx, data.CorpusDND, "nothing/0001/0002.0")
	if err != nil || guide != "" || node != "" {
		t.Errorf("resolve dnd miss = %q/%q, %v", guide, node, err)
	}
}

// HasBodyText is a test-only peek at the page body.
func (p *ReaderPage) HasBodyText(contains string) bool {
	return len(p.Body) > 0 && contains != "" && stringContains(p.Body, contains)
}

func stringContains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
