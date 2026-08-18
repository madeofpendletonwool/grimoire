package server

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// newReaderServer opens a server over a fresh index and loads the shared
// reader fixture dataset.
func newReaderServer(t *testing.T) *Server {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "reader.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := store.Index(context.Background(), readerFixtureDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	return s
}

func readerFixtureDataset() *data.Dataset {
	return &data.Dataset{
		Records: []data.Record{
			{Corpus: data.CorpusMTG, Number: "205.1", Title: "Type Line", Body: "The type line is...", Source: "MTG Comprehensive Rules"},
		},
		Meta: map[data.Corpus]data.CorpusMeta{
			data.CorpusMTG: {Name: "Magic: The Gathering", Version: "2026", SourceURL: "x", RecordCount: 1},
			data.CorpusDND: {Name: "D&D 5e SRD", Version: "master", SourceURL: "y", RecordCount: 1},
		},
		Reader: []data.ReaderNode{
			{Corpus: data.CorpusMTG, Guide: "rules", GuideTitle: "Comprehensive Rules", GuideKind: "rules", Number: "1", Title: "Game Concepts", Level: 1, Position: 1},
			{Corpus: data.CorpusMTG, Guide: "rules", GuideTitle: "Comprehensive Rules", GuideKind: "rules", Number: "101", Title: "The Magic Golden Rules", Level: 2, Position: 2, Body: "101.1. Cards win.", Source: "MTG Comprehensive Rules"},
			{Corpus: data.CorpusMTG, Guide: "rules", GuideTitle: "Comprehensive Rules", GuideKind: "rules", Number: "glossary", Title: "Glossary", Level: 1, Position: 3},
			{Corpus: data.CorpusMTG, Guide: "rules", GuideTitle: "Comprehensive Rules", GuideKind: "rules", Number: "glossary/0001", Title: "Ability", Level: 2, Position: 4, Body: "Text on an object.", Source: "MTG Comprehensive Rules — Glossary"},
			{Corpus: data.CorpusDND, Guide: "srd:spells", GuideTitle: "Spells", GuideKind: "srd", Number: "spells/0003", Title: "Casting Spells", Level: 1, Position: 1, Body: "casting intro"},
			{Corpus: data.CorpusDND, Guide: "srd:spells", GuideTitle: "Spells", GuideKind: "srd", Number: "spells/0003/0042", Title: "Range", Level: 2, Position: 2, Body: "**Range.** The spell's range."},
		},
	}
}

func TestReaderGuidesEndpoint(t *testing.T) {
	s := newReaderServer(t)

	code, body := doGet(t, s, "/api/reader/guides?corpus=mtg")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	guides, _ := body["guides"].([]any)
	if len(guides) != 1 {
		t.Fatalf("guides = %v", guides)
	}
	g := guides[0].(map[string]any)
	if g["guide"] != "rules" || g["title"] != "Comprehensive Rules" || g["kind"] != "rules" {
		t.Errorf("guide = %v", g)
	}
	if g["nodes"] != float64(4) {
		t.Errorf("nodes = %v", g["nodes"])
	}

	// D&D carries the SRD guide.
	_, body = doGet(t, s, "/api/reader/guides?corpus=dnd")
	guides, _ = body["guides"].([]any)
	if len(guides) != 1 || guides[0].(map[string]any)["guide"] != "srd:spells" {
		t.Fatalf("dnd guides = %v", guides)
	}
}

func TestReaderTOCAndPage(t *testing.T) {
	s := newReaderServer(t)

	code, body := doGet(t, s, "/api/reader/toc?corpus=mtg&guide=rules")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	toc, _ := body["toc"].([]any)
	if len(toc) != 4 {
		t.Fatalf("toc = %v", toc)
	}
	first := toc[0].(map[string]any)
	if first["number"] != "1" || first["has_body"] != false {
		t.Errorf("first stop = %v", first)
	}

	code, body = doGet(t, s, "/api/reader/page?corpus=mtg&guide=rules&number=101")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	page := body
	if page["title"] != "The Magic Golden Rules" || page["body"] != "101.1. Cards win." {
		t.Errorf("page = %v", page)
	}
	crumbs, _ := page["crumbs"].([]any)
	if len(crumbs) != 1 || crumbs[0].(map[string]any)["number"] != "1" {
		t.Errorf("crumbs = %v", crumbs)
	}
	next, _ := page["next"].(map[string]any)
	if next == nil || next["number"] != "glossary/0001" {
		t.Errorf("next = %v (should skip the empty glossary container)", next)
	}

	// Bad guide/number is a 404, not a 500.
	code, _ = doGet(t, s, "/api/reader/page?corpus=mtg&guide=rules&number=nope")
	if code != http.StatusNotFound {
		t.Errorf("missing page code = %d", code)
	}
	// Missing params is a 400.
	code, _ = doGet(t, s, "/api/reader/page?corpus=mtg&guide=rules")
	if code != http.StatusBadRequest {
		t.Errorf("paramless code = %d", code)
	}
}

func TestReaderDeepLinkResolve(t *testing.T) {
	s := newReaderServer(t)

	// A bare MTG rule number resolves through /api/reader/toc.
	code, body := doGet(t, s, "/api/reader/toc?corpus=mtg&number=101.1a")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if body["guide"] != "rules" || body["number"] != "101" {
		t.Errorf("resolve = %v / %v", body["guide"], body["number"])
	}

	// A D&D record number resolves to its heading node's page.
	code, body = doGet(t, s, "/api/reader/page?corpus=dnd&number=spells/0003/0042.1")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if body["guide"] != "srd:spells" || body["title"] != "Range" {
		t.Errorf("resolved page = %v / %v", body["guide"], body["title"])
	}

	// A number nobody knows resolves to an empty guide, not an error.
	code, body = doGet(t, s, "/api/reader/toc?corpus=mtg&number=999.9z")
	if code != http.StatusOK || body["guide"] != nil {
		t.Errorf("unknown resolve code = %d body = %v", code, body["guide"])
	}
}
