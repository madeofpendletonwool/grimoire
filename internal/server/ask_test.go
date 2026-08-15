package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/cache"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// newAskServer builds a server with a configured (stubbed) LLM, an answer
// cache, and a small MTG index so grounding returns real sources. It returns
// the store so a test can reindex and observe cache invalidation.
func newAskServer(t *testing.T, anthropic http.HandlerFunc) (*Server, *index.Store) {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	indexDataset(t, store, "702.2", "Deathtouch", "Deathtouch is a static ability.")

	answers, err := cache.New(store.DB(), cache.DefaultTTL)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}

	up := httptest.NewServer(http.HandlerFunc(anthropic))
	t.Cleanup(up.Close)
	cfg := llm.Config{BaseURL: up.URL, APIKey: "test-key", Model: "test-model"}

	s, err := New(store, llm.New(cfg), nil, nil, nil, nil, answers, nil, Auth{}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s, store
}

func indexDataset(t *testing.T, store *index.Store, number, title, body string) {
	t.Helper()
	ds := &data.Dataset{Records: []data.Record{
		{Corpus: data.CorpusMTG, Number: number, Title: title, Body: body, Source: "MTG Comp Rules"},
	}, Meta: map[data.Corpus]data.CorpusMeta{
		data.CorpusMTG: {Name: "Magic: The Gathering", Version: "2026", SourceURL: "x", RecordCount: 1},
	}}
	if err := store.Index(context.Background(), ds); err != nil {
		t.Fatalf("index: %v", err)
	}
}

// stubAsk returns a non-streaming Messages API response carrying answer, and
// counts how many times the model was actually called.
func stubAsk(answer string, calls *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": answer}},
		})
	}
}

func postAsk(t *testing.T, s *Server, target string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"corpus":"mtg","question":"How does deathtouch work?"}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestAskCachesRepeatQuestion(t *testing.T) {
	var calls int32
	s, _ := newAskServer(t, stubAsk("Deathtouch is lethal.", &calls))

	// First request misses the cache and calls the model.
	_, body := postAsk(t, s, "/api/ask")
	if body["cached"] != false {
		t.Errorf("first call: cached = %v, want false", body["cached"])
	}
	if body["answer"] != "Deathtouch is lethal." {
		t.Errorf("first call: answer = %v", body["answer"])
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 model call after miss, got %d", got)
	}

	// Second request is a grounding-equivalent repeat: instant hit, no model call.
	_, body = postAsk(t, s, "/api/ask")
	if body["cached"] != true {
		t.Errorf("second call: cached = %v, want true", body["cached"])
	}
	if body["answer"] != "Deathtouch is lethal." {
		t.Errorf("second call: answer = %v, want the cached value", body["answer"])
	}
	if sources, ok := body["sources"].([]any); !ok || len(sources) == 0 {
		t.Errorf("cached response should still carry sources, got %v", body["sources"])
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected the model to be skipped on hit, got %d total calls", got)
	}
}

func TestAskNoCacheBypassesLookup(t *testing.T) {
	var calls int32
	s, _ := newAskServer(t, stubAsk("fresh answer", &calls))

	postAsk(t, s, "/api/ask") // populate
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("populate call: expected 1 model call, got %d", got)
	}

	// ?nocache forces a fresh answer even though the entry is warm.
	_, body := postAsk(t, s, "/api/ask?nocache")
	if body["cached"] != false {
		t.Errorf("nocache call: cached = %v, want false", body["cached"])
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("nocache should hit the model again, got %d total calls", got)
	}

	// A subsequent normal request hits the refreshed entry.
	_, body = postAsk(t, s, "/api/ask")
	if body["cached"] != true {
		t.Errorf("post-nocache call: cached = %v, want true (entry was refreshed)", body["cached"])
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("refreshed entry should be reused, got %d total calls", got)
	}
}

func TestAskDoesNotCacheModelErrors(t *testing.T) {
	var calls int32
	s, _ := newAskServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"type":"error","message":"upstream down"}`))
	})

	code, body := postAsk(t, s, "/api/ask")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error surfaces in the body)", code)
	}
	if body["cached"] != false {
		t.Errorf("errored call: cached = %v, want false", body["cached"])
	}
	if !strings.Contains(body["answer"].(string), "couldn't reach the model") {
		t.Errorf("error placeholder should surface, got %v", body["answer"])
	}

	// Second request must NOT be a hit — the error placeholder was never stored.
	postAsk(t, s, "/api/ask")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("errored answers must not be cached: got %d model calls, want 2", got)
	}
}

// TestAskReindexInvalidatesEntries proves the key acceptance property: after a
// rules reindex that moves the grounding, the same question misses the cache.
func TestAskReindexInvalidatesEntries(t *testing.T) {
	var calls int32
	s, store := newAskServer(t, stubAsk("answer for 702.2", &calls))

	postAsk(t, s, "/api/ask") // cached against grounding {702.2}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("populate: expected 1 model call, got %d", got)
	}

	// Reindex with the same rule body under a different number. Retrieval for
	// the same question now lands on a different source set, so the prior key
	// cannot hit — the stale answer cannot survive the reindex.
	if err := store.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	indexDataset(t, store, "999.1", "Deathtouch", "Deathtouch is a static ability.")

	postAsk(t, s, "/api/ask")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("after reindex the entry should miss (different grounding), got %d model calls, want 2", got)
	}
}
