package server

import (
	"context"
	"encoding/json"
	"io"
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

// capturedRequest is the slice of the Messages API request the test inspects,
// decoded locally so the test does not depend on llm's unexported types.
type capturedRequest struct {
	System   string `json:"system"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// newResolveServer wires a Server against a fake Scryfall endpoint and a fake
// Messages endpoint, with the LLM actually configured so the resolve flow runs.
// The LLM handler receives the parsed request so a test can assert on the
// prompt, and streams back the given trace.
func newResolveServer(t *testing.T, oracle map[string]string, trace string, captured *capturedRequest) *Server {
	t.Helper()
	scryfall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cards/named" {
			http.NotFound(w, r)
			return
		}
		name := r.URL.Query().Get("fuzzy")
		text, ok := oracle[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"name":` + jsonString(name) + `,"type_line":"Creature","oracle_text":` + jsonString(text) + `}`))
	}))
	t.Cleanup(scryfall.Close)

	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if captured != nil {
			_ = json.Unmarshal(raw, captured)
		}
		w.Header().Set("content-type", "text/event-stream")
		// Two deltas so the test exercises the stream concatenation.
		half := len(trace) / 2
		_, _ = w.Write([]byte("data: " + sseDelta(trace[:half]) + "\n\n"))
		_, _ = w.Write([]byte("data: " + sseDelta(trace[half:]) + "\n\n"))
	}))
	t.Cleanup(llmSrv.Close)

	store, err := index.Open(filepath.Join(t.TempDir(), "resolve.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store, llm.New(llm.Config{BaseURL: llmSrv.URL, APIKey: "k", Model: "test"}),
		cards.NewWithBase(scryfall.URL), nil, nil, Auth{})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s
}

func sseDelta(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	return string(b)
}

func indexResolveRules(t *testing.T, s *Server) {
	t.Helper()
	ds := &data.Dataset{Records: []data.Record{
		{Corpus: data.CorpusMTG, Number: "117.1", Title: "Timing", Body: "Priority."},
		{Corpus: data.CorpusMTG, Number: "117.4", Title: "Timing", Body: "Stack resolves."},
		{Corpus: data.CorpusMTG, Number: "603.2", Title: "Triggers", Body: "Trigger event."},
		{Corpus: data.CorpusMTG, Number: "603.3b", Title: "Triggers", Body: "APNAP: active player puts triggers on the stack first."},
		{Corpus: data.CorpusMTG, Number: "613.1", Title: "Layers", Body: "Apply layers."},
		{Corpus: data.CorpusMTG, Number: "616.1", Title: "Replacement", Body: "Replacement effects."},
	}, Meta: map[data.Corpus]data.CorpusMeta{
		data.CorpusMTG: {Name: "Magic: The Gathering", Version: "t", SourceURL: "x", RecordCount: 6},
	}}
	if err := s.store.Index(context.Background(), ds); err != nil {
		t.Fatalf("index: %v", err)
	}
}

func doResolve(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/resolve", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// sseFrames splits an SSE body into (event, data-json) pairs.
func sseFrames(body string) []map[string]any {
	var out []map[string]any
	for _, frame := range strings.Split(body, "\n\n") {
		event, data := "message", []string{}
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(data) == 0 {
			continue
		}
		var payload map[string]any
		_ = json.Unmarshal([]byte(strings.Join(data, "\n")), &payload)
		payload["__event"] = event
		out = append(out, payload)
	}
	return out
}

func TestHandleResolve_StreamsCitedTrace(t *testing.T) {
	oracle := map[string]string{
		"Blood Artist": "Whenever Blood Artist or another creature dies, you may have target player lose 1 life.",
		"Wrath of God": "Destroy all creatures.",
	}
	var captured capturedRequest
	trace := "1. Wrath resolves. 2. Blood Artist triggers (603.3b). Result: opponent loses life."
	s := newResolveServer(t, oracle, trace, &captured)
	indexResolveRules(t, s)

	body := `{"board":"You: Blood Artist","sequence":"1. Opp casts Wrath of God","note":"opponent's turn"}`
	rec := doResolve(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}

	frames := sseFrames(rec.Body.String())
	want := []string{"meta", "delta", "delta", "done"}
	var got []string
	for _, f := range frames {
		got = append(got, f["__event"].(string))
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i, ev := range want {
		if got[i] != ev {
			t.Errorf("frame %d = %s, want %s", i, got[i], ev)
		}
	}

	// meta carries the resolved card.
	meta := frames[0]
	cardsField, _ := meta["cards"].([]any)
	if len(cardsField) == 0 {
		t.Errorf("meta should list resolved cards, got %v", meta)
	}

	// The two deltas concatenate into the trace; it is cited (mentions 603.3b).
	var traceText strings.Builder
	for _, f := range frames {
		if f["__event"] == "delta" {
			traceText.WriteString(f["text"].(string))
		}
	}
	if traceText.String() != trace {
		t.Errorf("concatenated trace = %q, want %q", traceText.String(), trace)
	}
	if !strings.Contains(traceText.String(), "603.3b") {
		t.Errorf("trace should cite a rule number, got %q", traceText.String())
	}

	// The prompt the model received carries the board card, the sequence, and
	// the APNAP rule pulled from the wholesale chapter.
	user := captured.Messages[0].Content
	for _, want := range []string{"Blood Artist", "Wrath of God", "603.3b", "opponent's turn"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt missing %q:\n%s", want, user)
		}
	}
	if !strings.Contains(captured.System, "603.3b") {
		t.Errorf("resolver system prompt not used; system = %q", captured.System)
	}
}

func TestHandleResolve_RequiresInput(t *testing.T) {
	s := newResolveServer(t, nil, "", nil)
	rec := doResolve(t, s, `{"board":"","sequence":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for empty board+sequence", rec.Code)
	}
}

func TestHandleResolve_NotConfigured(t *testing.T) {
	store, err := index.Open(filepath.Join(t.TempDir(), "resolve.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// No API key → not configured.
	s, err := New(store, llm.New(llm.Config{}), cards.New(), nil, nil, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	rec := doResolve(t, s, `{"board":"You: Blood Artist","sequence":"1. Opp casts Wrath of God"}`)
	frames := sseFrames(rec.Body.String())
	var sawError bool
	for _, f := range frames {
		if f["__event"] == "error" {
			sawError = true
			if msg, _ := f["error"].(string); !strings.Contains(msg, "not configured") {
				t.Errorf("error = %q, want not-configured notice", msg)
			}
		}
	}
	if !sawError {
		t.Errorf("expected an SSE error event for an unconfigured resolver")
	}
}
