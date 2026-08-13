package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/study"
)

// newStudyServer opens a fresh index + study store pair sharing one SQLite
// file, so a test can index a corpus and exercise the study endpoints against
// it end to end. Auth is left open (zero Auth) so every caller is anonymous.
func newStudyServer(t *testing.T) *Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "study.db")
	store, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	studies, err := study.New(store.DB())
	if err != nil {
		t.Fatalf("new study store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, studies, Auth{})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s
}

func indexKeywordDeck(t *testing.T, s *Server) {
	t.Helper()
	ds := &data.Dataset{Records: []data.Record{
		{Corpus: data.CorpusMTG, Number: "702.2", Title: "Keyword Abilities", Body: "Deathtouch", Source: "MTG Comprehensive Rules"},
		{Corpus: data.CorpusMTG, Number: "702.2a", Title: "Keyword Abilities", Body: "Deathtouch is a static ability.", Source: "MTG Comprehensive Rules"},
		{Corpus: data.CorpusMTG, Number: "702.2b", Title: "Keyword Abilities", Body: "A creature with deathtouch deals lethal damage.", Source: "MTG Comprehensive Rules"},
		{Corpus: data.CorpusMTG, Number: "702.21", Title: "Keyword Abilities", Body: "Ward", Source: "MTG Comprehensive Rules"},
		{Corpus: data.CorpusMTG, Number: "702.21a", Title: "Keyword Abilities", Body: "Ward is a triggered ability.", Source: "MTG Comprehensive Rules"},
	}, Meta: map[data.Corpus]data.CorpusMeta{
		data.CorpusMTG: {Name: "Magic: The Gathering", Version: "2026", SourceURL: "x", RecordCount: 5},
	}}
	if err := s.store.Index(context.Background(), ds); err != nil {
		t.Fatalf("index: %v", err)
	}
}

func TestDeckGenerator_MTGKeywordAbilities(t *testing.T) {
	s := newStudyServer(t)
	indexKeywordDeck(t, s)

	gen := s.deckGen()
	concepts, err := gen.Concepts(context.Background(), "mtg", "keyword-abilities")
	if err != nil {
		t.Fatalf("concepts: %v", err)
	}
	if len(concepts) != 2 {
		t.Fatalf("got %d concepts, want 2 (Deathtouch + Ward)", len(concepts))
	}
	deathtouch := concepts[0]
	if deathtouch.Key != "702.2" || deathtouch.Title != "Deathtouch" {
		t.Errorf("first concept = %+v, want 702.2 / Deathtouch", deathtouch)
	}
	if deathtouch.Front == "" || deathtouch.Back == "" {
		t.Errorf("concept missing content: %+v", deathtouch)
	}
	// The answer face carries both sub-rules so the reader sees the full mechanic.
	if !contains(deathtouch.Back, "static ability") || !contains(deathtouch.Back, "lethal damage") {
		t.Errorf("back should include both sub-rules, got: %q", deathtouch.Back)
	}
	// Rulebook order: 702.2 before 702.21.
	if concepts[1].Key != "702.21" {
		t.Errorf("second concept = %q, want 702.21", concepts[1].Key)
	}
}

func TestDeckGenerator_UnknownTopicIsEmpty(t *testing.T) {
	s := newStudyServer(t)
	indexKeywordDeck(t, s)
	gen := s.deckGen()
	concepts, err := gen.Concepts(context.Background(), "mtg", "no-such-topic")
	if err != nil {
		t.Fatalf("concepts: %v", err)
	}
	if len(concepts) != 0 {
		t.Errorf("unknown topic should yield no concepts, got %d", len(concepts))
	}
}

func TestHandleStudyQueue_ReturnsCardsAndStats(t *testing.T) {
	s := newStudyServer(t)
	indexKeywordDeck(t, s)

	code, body := doGet(t, s, "/api/study/queue?corpus=mtg&limit=10")
	if code != http.StatusOK {
		t.Fatalf("code = %d, code", code)
	}
	cards, _ := body["cards"].([]any)
	if len(cards) != 2 {
		t.Fatalf("got %d cards, want 2", len(cards))
	}
	first, _ := cards[0].(map[string]any)
	if first["key"] != "702.2" {
		t.Errorf("first card key = %v, want 702.2", first["key"])
	}
	if first["front"] == "" || first["back"] == "" {
		t.Errorf("card missing content: %+v", first)
	}
	stats, _ := body["stats"].(map[string]any)
	if got := stats["total"]; got != float64(2) {
		t.Errorf("stats.total = %v, want 2", got)
	}
}

func TestHandleStudyQueue_EmptyDeck(t *testing.T) {
	// No index loaded → the generator has nothing, so the queue returns an
	// empty card list (not a 500).
	s := newStudyServer(t)
	code, body := doGet(t, s, "/api/study/queue?corpus=mtg")
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	cards, _ := body["cards"].([]any)
	if len(cards) != 0 {
		t.Errorf("got %d cards, want empty", len(cards))
	}
}

func TestHandleStudyGrade_ReschedulesCard(t *testing.T) {
	s := newStudyServer(t)
	indexKeywordDeck(t, s)

	// Fetch the queue to confirm the deck exists, then grade the first card.
	_, body := doGet(t, s, "/api/study/queue?corpus=mtg")
	cards, _ := body["cards"].([]any)
	if len(cards) == 0 {
		t.Fatal("expected cards to grade")
	}

	payload, _ := json.Marshal(map[string]string{
		"key": "702.2", "corpus": "mtg", "grade": "good",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/study/grade", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("grade code = %d, want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	card, _ := resp["card"].(map[string]any)
	if got := card["reps"]; got != float64(1) {
		t.Errorf("after Good, reps = %v, want 1", got)
	}
	if got := card["interval_days"]; got != float64(1) {
		t.Errorf("after Good, interval = %v, want 1 day", got)
	}
	if card["new"] == true {
		t.Errorf("graded card should not be new")
	}

	// The schedule persists: a fresh queue call shows the graded card is no
	// longer due today (Good pushes it out a day).
	_, body = doGet(t, s, "/api/study/queue?corpus=mtg")
	for _, c := range body["cards"].([]any) {
		if c.(map[string]any)["key"] == "702.2" {
			t.Errorf("a Good-graded card should not be in today's queue")
		}
	}
}

func TestHandleStudyGrade_AgainKeepsCardDue(t *testing.T) {
	s := newStudyServer(t)
	indexKeywordDeck(t, s)

	payload, _ := json.Marshal(map[string]string{
		"key": "702.2", "corpus": "mtg", "grade": "again",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/study/grade", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("grade code = %d", rec.Code)
	}

	// An Again lapse is due immediately, so it resurfaces in the next queue.
	_, body := doGet(t, s, "/api/study/queue?corpus=mtg")
	cards, _ := body["cards"].([]any)
	if len(cards) == 0 || cards[0].(map[string]any)["key"] != "702.2" {
		t.Errorf("Again-graded card should lead the queue, got %v", cards)
	}
}

func TestHandleStudyGrade_RejectsBadGrade(t *testing.T) {
	s := newStudyServer(t)
	indexKeywordDeck(t, s)
	payload, _ := json.Marshal(map[string]string{
		"key": "702.2", "corpus": "mtg", "grade": "superstar",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/study/grade", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for unknown grade", rec.Code)
	}
}

func TestHandleStudyGrade_RejectsUnknownKey(t *testing.T) {
	s := newStudyServer(t)
	indexKeywordDeck(t, s)
	payload, _ := json.Marshal(map[string]string{
		"key": "999.999", "corpus": "mtg", "grade": "good",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/study/grade", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404 for a key not in the deck", rec.Code)
	}
}

func TestHandleStudyGrade_RejectsEmptyKey(t *testing.T) {
	s := newStudyServer(t)
	payload, _ := json.Marshal(map[string]string{"corpus": "mtg", "grade": "good"})
	req := httptest.NewRequest(http.MethodPost, "/api/study/grade", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for missing key", rec.Code)
	}
}

func TestHandleStudy_WhenStoreNil(t *testing.T) {
	// A server with no study store wired should 503, not panic.
	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	code, _ := doGet(t, s, "/api/study/queue?corpus=mtg")
	if code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503 when study store is nil", code)
	}
}

func TestHandleStudyGrade_PersistsAcrossServerInstances(t *testing.T) {
	// The acceptance criterion: reviews persist per-user across reloads. A
	// grade recorded against one Store must be visible to a fresh Store on the
	// same SQLite file (which is what a restart does).
	dbPath := filepath.Join(t.TempDir(), "study.db")
	store, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	studies, err := study.New(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	s1, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, studies, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	indexKeywordDeck(t, s1)

	payload, _ := json.Marshal(map[string]string{"key": "702.2", "corpus": "mtg", "grade": "good"})
	req := httptest.NewRequest(http.MethodPost, "/api/study/grade", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	s1.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("grade: %d", rec.Code)
	}

	// Reopen a fresh study store + server over the same file.
	studies2, err := study.New(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	s2, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, studies2, Auth{})
	if err != nil {
		t.Fatal(err)
	}

	_, body := doGet(t, s2, "/api/study/queue?corpus=mtg")
	stats, _ := body["stats"].(map[string]any)
	// After grading one of two cards Good, it should no longer be due today.
	if got := stats["due"]; got != float64(0) {
		t.Errorf("due after reload = %v, want 0", got)
	}
}

func contains(s, substr string) bool {
	return bytes.Contains([]byte(s), []byte(substr))
}
