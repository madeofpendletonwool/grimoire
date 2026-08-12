package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

const singleFaceJSON = `{
	"name": "Lightning Bolt",
	"mana_cost": "{R}",
	"type_line": "Instant",
	"oracle_text": "Lightning Bolt deals 3 damage to any target.",
	"set": "lea",
	"scryfall_uri": "https://scryfall.com/card/lea/161/lightning-bolt",
	"image_uris": {"normal": "https://img.scryfall.com/bolt.jpg"}
}`

func newTestServer(t *testing.T, scryfallHandler http.HandlerFunc) (*Server, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(scryfallHandler))
	t.Cleanup(srv.Close)

	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	s, err := New(store, llm.New(llm.Config{}), cards.NewWithBase(srv.URL), nil, nil, Auth{})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s, srv
}

func doGet(t *testing.T, s *Server, target string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestHandleCard_DirectMatch(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cards/named" && r.URL.Query().Get("fuzzy") == "Lightning Bolt" {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(singleFaceJSON))
			return
		}
		http.NotFound(w, r)
	})

	code, body := doGet(t, s, "/api/card?q=Lightning+Bolt")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	card, ok := body["card"].(map[string]any)
	if !ok || card == nil {
		t.Fatalf("no card in response: %v", body)
	}
	if card["name"] != "Lightning Bolt" {
		t.Errorf("name = %v", card["name"])
	}
	if card["oracle_text"] != "Lightning Bolt deals 3 damage to any target." {
		t.Errorf("oracle = %v", card["oracle_text"])
	}
}

func TestHandleCard_FallsBackToSearch(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// named lookup misses
		if r.URL.Path == "/cards/named" {
			http.NotFound(w, r)
			return
		}
		// search returns a couple of candidates
		if r.URL.Path == "/cards/search" {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"data":[` + singleFaceJSON + `]}`))
			return
		}
		http.NotFound(w, r)
	})

	code, body := doGet(t, s, "/api/card?q=bolt")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if body["card"] != nil {
		t.Errorf("card should be nil on no direct match, got %v", body["card"])
	}
	matches, ok := body["matches"].([]any)
	if !ok || len(matches) != 1 {
		t.Fatalf("matches = %v", body["matches"])
	}
}

func TestHandleCard_RequiresQuery(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be hit on empty query")
	})
	code, body := doGet(t, s, "/api/card?q=")
	if code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", code)
	}
	if !strings.Contains(body["error"].(string), "required") {
		t.Errorf("error = %v", body["error"])
	}
}

func TestHandleCardSearch_Success(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Autocomplete must hit /cards/search directly — not waste a
		// /cards/named round-trip first like /api/card does.
		if r.URL.Path == "/cards/named" {
			t.Errorf("autocomplete must not call /cards/named")
		}
		if r.URL.Path == "/cards/search" {
			if got := r.URL.Query().Get("q"); got != "bolt" {
				t.Errorf("q param = %q, want %q", got, "bolt")
			}
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"data":[` + singleFaceJSON + `]}`))
			return
		}
		http.NotFound(w, r)
	})

	code, body := doGet(t, s, "/api/card/search?q=bolt")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	matches, ok := body["matches"].([]any)
	if !ok || len(matches) != 1 {
		t.Fatalf("matches = %v", body["matches"])
	}
	card, _ := matches[0].(map[string]any)
	if card["name"] != "Lightning Bolt" {
		t.Errorf("first match name = %v", card["name"])
	}
	// Autocomplete must never return a single "card" field — callers
	// expect {matches: [...]} only.
	if body["card"] != nil {
		t.Errorf("autocomplete must not return card, got %v", body["card"])
	}
}

func TestHandleCardSearch_NoMatches(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // Scryfall signals "no matches" with a 404
	})

	code, body := doGet(t, s, "/api/card/search?q=zzzznotacard")
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (empty matches, not 404)", code)
	}
	matches, _ := body["matches"].([]any)
	if len(matches) != 0 {
		t.Errorf("matches = %v, want empty slice so dropdown collapses cleanly", matches)
	}
}

func TestHandleCardSearch_RequiresQuery(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be hit on empty query")
	})
	code, body := doGet(t, s, "/api/card/search?q=")
	if code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", code)
	}
	if !strings.Contains(body["error"].(string), "required") {
		t.Errorf("error = %v", body["error"])
	}
}

func TestHandleCardSearch_NilService(t *testing.T) {
	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	code, _ := doGet(t, s, "/api/card/search?q=bolt")
	if code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503 when card service is nil", code)
	}
}

func TestLookupQuestionCards_MTGOnly(t *testing.T) {
	// D&D corpus must never trigger Scryfall lookups.
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("scryfall should not be called for D&D")
	})
	docs, hits, unresolved := s.lookupQuestionCards(context.Background(), "dnd", `what does "Fireball" do`)
	if docs != nil || hits != nil || unresolved != nil {
		t.Errorf("expected nil for D&D, got docs=%v hits=%v unresolved=%v", docs, hits, unresolved)
	}
}

func TestLookupQuestionCards_NilService(t *testing.T) {
	// A nil card service is graceful (no panic, no lookups).
	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	docs, hits, unresolved := s.lookupQuestionCards(context.Background(), "mtg", `what does "Lightning Bolt" do`)
	if docs != nil || hits != nil || unresolved != nil {
		t.Errorf("expected nil with no card service, got %v / %v / %v", docs, hits, unresolved)
	}
}

func TestHandleSection(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // no Scryfall traffic expected
	})
	ds := &data.Dataset{Records: []data.Record{
		{Corpus: data.CorpusMTG, Number: "702.2", Title: "Keyword Abilities", Body: "Deathtouch", Source: "MTG Comp Rules"},
		{Corpus: data.CorpusMTG, Number: "702.2a", Title: "Keyword Abilities", Body: "Deathtouch is a static ability.", Source: "MTG Comp Rules"},
		{Corpus: data.CorpusMTG, Number: "702.2b", Title: "Keyword Abilities", Body: "Lethal damage rule.", Source: "MTG Comp Rules"},
		{Corpus: data.CorpusMTG, Number: "702.21", Title: "Keyword Abilities", Body: "Ward is a triggered ability.", Source: "MTG Comp Rules"},
	}, Meta: map[data.Corpus]data.CorpusMeta{
		data.CorpusMTG: {Name: "Magic: The Gathering", Version: "2026", SourceURL: "x", RecordCount: 4},
	}}
	if err := s.store.Index(context.Background(), ds); err != nil {
		t.Fatalf("index: %v", err)
	}

	code, body := doGet(t, s, "/api/section?corpus=mtg&number=702.2")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	parent, _ := body["parent"].(map[string]any)
	if parent == nil {
		t.Fatalf("expected parent, got %v", body)
	}
	if parent["number"] != "702.2" {
		t.Errorf("parent number = %v", parent["number"])
	}
	children, _ := body["children"].([]any)
	if len(children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(children))
	}
	first, _ := children[0].(map[string]any)
	if first["number"] != "702.2a" {
		t.Errorf("first child = %v", first["number"])
	}
}

func TestHandleSection_RequiresNumber(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	code, body := doGet(t, s, "/api/section?corpus=mtg&number=")
	if code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", code)
	}
	if !strings.Contains(body["error"].(string), "required") {
		t.Errorf("error = %v", body["error"])
	}
}

func TestLookupQuestionCards_CommaSeparatedNames(t *testing.T) {
	// Regression for MAD-102: "Humility, Opalescence, and Enchanted Evening"
	// used to reach Scryfall as one bogus name, so the two cards the question
	// hinged on were never looked up.
	oracle := map[string]string{
		"Humility":          "All creatures lose all abilities and have base power and toughness 1/1.",
		"Opalescence":       "Each other non-Aura enchantment is a creature in addition to its other types.",
		"Enchanted Evening": "All permanents are enchantments in addition to their other types.",
		"Blood Moon":        "Nonbasic lands are Mountains.",
	}
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("fuzzy")
		text, ok := oracle[name]
		if r.URL.Path != "/cards/named" || !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"name":` + jsonString(name) + `,"type_line":"Enchantment","oracle_text":` + jsonString(text) + `}`))
	})

	q := "You control Humility, Opalescence, and Enchanted Evening. Your opponent controls a Blood Moon."
	docs, hits, unresolved := s.lookupQuestionCards(context.Background(), data.CorpusMTG, q)
	if len(docs) != 4 || len(hits) != 4 {
		t.Fatalf("resolved %d cards, want 4: %+v", len(docs), docs)
	}
	got := map[string]bool{}
	for _, d := range docs {
		got[d.Name] = true
		if d.OracleText == "" {
			t.Errorf("%s carries no oracle text", d.Name)
		}
	}
	for name := range oracle {
		if !got[name] {
			t.Errorf("missing %s from grounding set %v", name, got)
		}
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}

func TestLookupQuestionCards_ReportsMisses(t *testing.T) {
	// Every lookup misses: the phrase must come back as unresolved so the
	// model is told the lookup failed instead of silently seeing no cards.
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	docs, hits, unresolved := s.lookupQuestionCards(context.Background(), data.CorpusMTG, "What does Fake Cardname Here do?")
	if len(docs) != 0 || len(hits) != 0 {
		t.Errorf("expected no cards, got %v", docs)
	}
	if len(unresolved) == 0 {
		t.Error("a multi-word miss must be reported, not dropped")
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
