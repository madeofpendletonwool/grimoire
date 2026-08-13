package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/cache"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// fakeEntityResolver is a data.EntityResolver stub for D&D grounding tests,
// standing in for Open5e so the server path is exercised with no network.
type fakeEntityResolver struct {
	entities   []data.Entity
	unresolved []string
	err        error
}

func (f *fakeEntityResolver) Resolve(context.Context, string) ([]data.Entity, []string, error) {
	return f.entities, f.unresolved, f.err
}

// indexDNDDataset indexes a single D&D record so retrieval returns a source,
// which keeps the answer-cache key stable for the test.
func indexDNDDataset(t *testing.T, store *index.Store, title, body string) {
	t.Helper()
	ds := &data.Dataset{Records: []data.Record{
		{Corpus: data.CorpusDND, Number: "", Title: title, Body: body, Source: "D&D 5e SRD"},
	}, Meta: map[data.Corpus]data.CorpusMeta{
		data.CorpusDND: {Name: "D&D 5e SRD", Version: "2024", SourceURL: "x", RecordCount: 1},
	}}
	if err := store.Index(context.Background(), ds); err != nil {
		t.Fatalf("index: %v", err)
	}
}

// TestAskDNDEntityGrounding is the acceptance test for the D&D side: a question
// about a named spell resolves the real entity block via the corpus's
// registered EntityResolver and grounds the answer in it — fed both to the
// model prompt and to the response as a citation.
func TestAskDNDEntityGrounding(t *testing.T) {
	// Register a fake Open5e-style resolver on D&D for the duration of the test.
	fireball := data.Entity{Name: "Fireball", Kind: "spell", Body: "Level 3 Evocation. 8d6 Fire damage on a failed save."}
	data.SetResolver(data.CorpusDND, &fakeEntityResolver{entities: []data.Entity{fireball}})
	t.Cleanup(func() { data.SetResolver(data.CorpusDND, nil) })

	// Capture the Messages API request so we can assert the entity reached the
	// prompt, while still returning a normal answer.
	var capturedRequest string
	anthropic := func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		capturedRequest = string(buf)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"Fireball is a 3rd-level evocation spell dealing 8d6 fire damage."}]}`))
	}

	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	indexDNDDataset(t, store, "Spells", "Casting a spell requires the listed components.")

	answers, err := cache.New(store.DB(), cache.DefaultTTL)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	up := httptest.NewServer(http.HandlerFunc(anthropic))
	t.Cleanup(up.Close)
	s, err := New(store, llm.New(llm.Config{BaseURL: up.URL, APIKey: "test-key", Model: "test-model"}), nil, nil, nil, nil, answers, nil, Auth{})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"corpus":"dnd","question":"What does Fireball do?"}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	// The resolved entity reaches the model prompt...
	if !strings.Contains(capturedRequest, "Reference entries") {
		t.Errorf("prompt missing the reference-entries section: %q", capturedRequest)
	}
	if !strings.Contains(capturedRequest, "Fireball") || !strings.Contains(capturedRequest, "8d6 Fire damage") {
		t.Errorf("prompt not grounded in the Fireball entity block: %q", capturedRequest)
	}
	// ...and the response, as a citation the UI can render.
	entities, _ := body["entities"].([]any)
	if len(entities) != 1 {
		t.Fatalf("response entities = %v, want 1", entities)
	}
	e, _ := entities[0].(map[string]any)
	if e["name"] != "Fireball" || e["kind"] != "spell" {
		t.Errorf("response entity = %+v, want Fireball / spell", e)
	}
	// MTG's card citations stay absent on a D&D turn.
	if body["cards"] != nil {
		cards, _ := body["cards"].([]any)
		if len(cards) != 0 {
			t.Errorf("D&D response should carry no card citations, got %v", cards)
		}
	}
}

// TestAskDNDCachesEntityCitations confirms a cached D&D answer replays its
// entity citations, the way cached MTG answers replay card citations — so a
// repeat question is not missing the entities the reader saw the first time.
func TestAskDNDCachesEntityCitations(t *testing.T) {
	fireball := data.Entity{Name: "Fireball", Kind: "spell", Body: "Level 3 Evocation. 8d6 Fire damage."}
	data.SetResolver(data.CorpusDND, &fakeEntityResolver{entities: []data.Entity{fireball}})
	t.Cleanup(func() { data.SetResolver(data.CorpusDND, nil) })

	s, _ := newAskServer(t, stubAsk("Fireball deals 8d6 fire damage.", new(int32)))
	indexDNDDataset(t, s.store, "Spells", "Spells have a level and a school.")

	target := `/api/ask`
	body := `{"corpus":"dnd","question":"What does Fireball do?"}`

	// First call misses the cache and stores the answer with its entity citation.
	_, resp := postAskBody(t, s, target, body)
	if resp["cached"] != false {
		t.Errorf("first call: cached = %v, want false", resp["cached"])
	}
	if entities, _ := resp["entities"].([]any); len(entities) != 1 {
		t.Errorf("first call: entities = %v, want 1", entities)
	}

	// Second call hits the cache and must replay the entity citation.
	_, resp = postAskBody(t, s, target, body)
	if resp["cached"] != true {
		t.Errorf("second call: cached = %v, want true", resp["cached"])
	}
	entities, _ := resp["entities"].([]any)
	if len(entities) != 1 {
		t.Fatalf("cached response should replay the entity citation, got %v", entities)
	}
	if e, _ := entities[0].(map[string]any); e["name"] != "Fireball" {
		t.Errorf("cached entity = %+v, want Fireball", e)
	}
}

// postAskBody is like postAsk but lets a test supply the JSON body, so a D&D
// turn can carry its own corpus and question.
func postAskBody(t *testing.T, s *Server, target, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}
