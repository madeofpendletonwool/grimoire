package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/cache"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// sessions holds the cookie minted for each authenticated test server, so the
// request helpers can speak to the gated API without every test threading one
// through by hand.
var sessions = map[*Server]*http.Cookie{}

// newChatServer builds a server with chat history and accounts enabled, signs a
// keeper in, and optionally points the model at a stub Anthropic endpoint.
func newChatServer(t *testing.T, anthropic http.HandlerFunc) *Server {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	chats, err := chat.New(store.DB())
	if err != nil {
		t.Fatalf("open chat store: %v", err)
	}
	answers, err := cache.New(store.DB(), cache.DefaultTTL)
	if err != nil {
		t.Fatalf("open answer cache: %v", err)
	}
	users, err := auth.New(store.DB(), 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}

	cfg := llm.Config{}
	if anthropic != nil {
		up := httptest.NewServer(anthropic)
		t.Cleanup(up.Close)
		cfg = llm.Config{BaseURL: up.URL, APIKey: "test-key", Model: "test-model"}
	}

	s, err := New(store, llm.New(cfg), nil, nil, nil, chats, answers, nil, Auth{Users: users})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	signIn(t, s, "keeper", "a-perfectly-fine-passphrase")
	return s
}

// signIn claims a fresh install and remembers the session cookie it returns.
func signIn(t *testing.T, s *Server, username, password string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup",
		strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)))
	req.Header.Set("content-type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: status %d, body %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			sessions[s] = c
			t.Cleanup(func() { delete(sessions, s) })
			return
		}
	}
	t.Fatal("setup did not set a session cookie")
}

// authenticate attaches the server's session cookie, when it has one. Servers
// built without accounts have none, and need none.
func authenticate(s *Server, req *http.Request) *http.Request {
	if c, ok := sessions[s]; ok {
		req.AddCookie(c)
	}
	return req
}

func do(t *testing.T, s *Server, method, target, body string) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := authenticate(s, httptest.NewRequest(method, target, rdr))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func createChat(t *testing.T, s *Server, corpus string) string {
	t.Helper()
	code, body := do(t, s, http.MethodPost, "/api/chats", fmt.Sprintf(`{"corpus":%q}`, corpus))
	if code != http.StatusCreated {
		t.Fatalf("create chat: status %d, body %v", code, body)
	}
	c, ok := body["chat"].(map[string]any)
	if !ok {
		t.Fatalf("create chat: no chat in response: %v", body)
	}
	return c["id"].(string)
}

func TestChatCRUD(t *testing.T) {
	s := newChatServer(t, nil)

	id := createChat(t, s, "dnd")

	code, body := do(t, s, http.MethodGet, "/api/chats", "")
	if code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	chats, _ := body["chats"].([]any)
	if len(chats) != 1 {
		t.Fatalf("expected 1 chat, got %d", len(chats))
	}

	code, body = do(t, s, http.MethodPatch, "/api/chats/"+id, `{"title":"Grappling rules"}`)
	if code != http.StatusOK {
		t.Fatalf("rename: status %d, body %v", code, body)
	}
	if got := body["chat"].(map[string]any)["title"]; got != "Grappling rules" {
		t.Errorf("title = %v, want %q", got, "Grappling rules")
	}

	code, body = do(t, s, http.MethodGet, "/api/chats/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("get: status %d", code)
	}
	if got := body["chat"].(map[string]any)["corpus"]; got != "dnd" {
		t.Errorf("corpus = %v, want dnd — a conversation's corpus is locked at creation", got)
	}

	req := authenticate(s, httptest.NewRequest(http.MethodDelete, "/api/chats/"+id, nil))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d", rec.Code)
	}

	if code, _ = do(t, s, http.MethodGet, "/api/chats/"+id, ""); code != http.StatusNotFound {
		t.Errorf("get after delete: status %d, want 404", code)
	}
}

func TestChatCreateDefaultsToMTG(t *testing.T) {
	s := newChatServer(t, nil)
	code, body := do(t, s, http.MethodPost, "/api/chats", "")
	if code != http.StatusCreated {
		t.Fatalf("create with empty body: status %d, body %v", code, body)
	}
	if got := body["chat"].(map[string]any)["corpus"]; got != "mtg" {
		t.Errorf("corpus = %v, want mtg", got)
	}
}

func TestChatMessageRequiresQuestion(t *testing.T) {
	s := newChatServer(t, nil)
	id := createChat(t, s, "mtg")
	code, _ := do(t, s, http.MethodPost, "/api/chats/"+id+"/messages", `{"question":"   "}`)
	if code != http.StatusBadRequest {
		t.Errorf("blank question: status %d, want 400", code)
	}
}

func TestChatMessageUnknownConversation(t *testing.T) {
	s := newChatServer(t, nil)
	code, _ := do(t, s, http.MethodPost, "/api/chats/nope/messages", `{"question":"hi"}`)
	if code != http.StatusNotFound {
		t.Errorf("unknown conversation: status %d, want 404", code)
	}
}

func TestChatsDisabledWithoutStore(t *testing.T) {
	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if code, _ := do(t, s, http.MethodGet, "/api/chats", ""); code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503 when chat history is unavailable", code)
	}
}

// sseEvents pulls (event, data) pairs out of a server-sent-events body.
func sseEvents(t *testing.T, body string) []struct{ Event, Data string } {
	t.Helper()
	var out []struct{ Event, Data string }
	for _, frame := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var ev, data string
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if ev != "" {
			out = append(out, struct{ Event, Data string }{ev, data})
		}
	}
	return out
}

func TestChatMessageStreamsAnswer(t *testing.T) {
	// Stub Anthropic streaming endpoint: two text deltas.
	s := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] != true {
			t.Errorf("expected stream:true in the upstream request, got %v", req["stream"])
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Deathtouch \"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"is lethal.\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})

	id := createChat(t, s, "mtg")
	req := authenticate(s, httptest.NewRequest(http.MethodPost, "/api/chats/"+id+"/messages",
		strings.NewReader(`{"question":"How does deathtouch work?"}`)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("content-type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	events := sseEvents(t, rec.Body.String())
	var sawMeta, sawDone bool
	var text strings.Builder
	for _, e := range events {
		switch e.Event {
		case "meta":
			sawMeta = true
		case "delta":
			var d struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(e.Data), &d); err != nil {
				t.Fatalf("decode delta: %v", err)
			}
			text.WriteString(d.Text)
		case "done":
			sawDone = true
		case "error":
			t.Fatalf("unexpected error event: %s", e.Data)
		}
	}
	if !sawMeta {
		t.Error("expected a meta event carrying citations before the answer")
	}
	if !sawDone {
		t.Error("expected a terminating done event")
	}
	if got := text.String(); got != "Deathtouch is lethal." {
		t.Errorf("streamed text = %q, want %q", got, "Deathtouch is lethal.")
	}

	// Both turns should now be persisted, and the thread named after the question.
	code, body := do(t, s, http.MethodGet, "/api/chats/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("get after stream: status %d", code)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("got %d saved messages, want 2", len(msgs))
	}
	first := msgs[0].(map[string]any)
	second := msgs[1].(map[string]any)
	if first["role"] != "user" || first["content"] != "How does deathtouch work?" {
		t.Errorf("first saved message = %v", first)
	}
	if second["role"] != "assistant" || second["content"] != "Deathtouch is lethal." {
		t.Errorf("second saved message = %v", second)
	}
	if title := body["chat"].(map[string]any)["title"]; title != "How does deathtouch work?" {
		t.Errorf("title = %v, want the opening question", title)
	}
}

func TestChatMessageReportsUnconfiguredModel(t *testing.T) {
	s := newChatServer(t, nil) // no API key
	id := createChat(t, s, "mtg")

	req := authenticate(s, httptest.NewRequest(http.MethodPost, "/api/chats/"+id+"/messages",
		strings.NewReader(`{"question":"anything"}`)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	events := sseEvents(t, rec.Body.String())
	if len(events) == 0 || events[0].Event != "error" {
		t.Fatalf("expected an error event when no API key is set, got %v", events)
	}
	if !strings.Contains(events[0].Data, "ANTHROPIC_API_KEY") {
		t.Errorf("error should name the missing setting: %s", events[0].Data)
	}

	// The question is still saved, so the thread reflects what the user typed.
	_, body := do(t, s, http.MethodGet, "/api/chats/"+id, "")
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("got %d saved messages, want just the question", len(msgs))
	}
}

func TestChatMessageSendsPriorTurnsAsHistory(t *testing.T) {
	var captured []map[string]any
	s := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		captured = req.Messages
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
	})

	id := createChat(t, s, "mtg")
	for _, q := range []string{"first question", "follow-up question"} {
		req := authenticate(s, httptest.NewRequest(http.MethodPost, "/api/chats/"+id+"/messages",
			strings.NewReader(fmt.Sprintf(`{"question":%q}`, q))))
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}

	if len(captured) < 3 {
		t.Fatalf("second call sent %d messages, want the prior turns plus the new question: %v", len(captured), captured)
	}
	if captured[0]["role"] != "user" {
		t.Errorf("exchange must open on a user turn, got %v", captured[0]["role"])
	}
	if !strings.Contains(captured[0]["content"].(string), "first question") {
		t.Errorf("first history entry should be the earlier question: %v", captured[0]["content"])
	}
	if captured[1]["role"] != "assistant" {
		t.Errorf("second history entry should be the earlier answer, got %v", captured[1]["role"])
	}
	last := captured[len(captured)-1]
	if last["role"] != "user" || !strings.Contains(last["content"].(string), "follow-up question") {
		t.Errorf("final message should carry the new question: %v", last)
	}
}

// postMessage sends a question to a conversation and returns the raw recorder,
// so the caller can read the SSE event stream. An empty query sends a normal
// (cache-eligible) request.
func postMessage(t *testing.T, s *Server, id, question, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/chats/" + id + "/messages"
	if query != "" {
		target += "?" + query
	}
	req := authenticate(s, httptest.NewRequest(http.MethodPost, target,
		strings.NewReader(fmt.Sprintf(`{"question":%q}`, question))))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// streamingStub writes the given text deltas as an Anthropic SSE stream and
// counts how many times the model was actually called.
func streamingStub(t *testing.T, text string, calls *int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("content-type", "text/event-stream")
		// Split the answer so a real stream and a one-shot cached replay are
		// distinguishable by call count, not by event framing.
		half := len(text) / 2
		if half == 0 {
			half = len(text)
		}
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":")
		fmt.Fprintf(w, "%q", text[:half])
		fmt.Fprint(w, "}}\n\n")
		if half < len(text) {
			fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":")
			fmt.Fprintf(w, "%q", text[half:])
			fmt.Fprint(w, "}}\n\n")
		}
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
}

func TestChatMessageCachesAnswer(t *testing.T) {
	var calls int32
	answer := "Deathtouch is lethal."
	s := newChatServer(t, streamingStub(t, answer, &calls))

	// First conversation generates and streams the answer, caching it.
	first := createChat(t, s, "mtg")
	rec := postMessage(t, s, first, "How does deathtouch work?", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("first message: status %d, body %s", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 model call after the first message, got %d", got)
	}

	// A second conversation asking the same question hits the cache: no model
	// call, the cached answer streams back, and the exchange is still saved.
	second := createChat(t, s, "mtg")
	rec = postMessage(t, s, second, "How does deathtouch work?", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second message: status %d, body %s", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("cached answer should skip the model, got %d total calls", got)
	}

	var sawCachedMeta, sawCachedDone bool
	var text strings.Builder
	for _, e := range sseEvents(t, rec.Body.String()) {
		switch e.Event {
		case "meta":
			var d struct {
				Cached bool `json:"cached"`
			}
			_ = json.Unmarshal([]byte(e.Data), &d)
			sawCachedMeta = d.Cached
		case "delta":
			var d struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(e.Data), &d)
			text.WriteString(d.Text)
		case "done":
			sawCachedDone = strings.Contains(e.Data, `"cached":true`)
		}
	}
	if !sawCachedMeta {
		t.Error("meta event on a hit should flag cached:true")
	}
	if !sawCachedDone {
		t.Error("done event on a hit should flag cached:true")
	}
	if got := text.String(); got != answer {
		t.Errorf("cached stream = %q, want %q", got, answer)
	}

	// Both turns are persisted so the saved thread matches what was shown.
	_, body := do(t, s, http.MethodGet, "/api/chats/"+second, "")
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected the cached exchange saved as 2 messages, got %d", len(msgs))
	}
	assistant := msgs[1].(map[string]any)
	if assistant["content"] != answer {
		t.Errorf("saved cached answer = %v, want %q", assistant["content"], answer)
	}
}

func TestChatMessageNoCacheBypass(t *testing.T) {
	var calls int32
	answer := "Fresh from the model."
	s := newChatServer(t, streamingStub(t, answer, &calls))

	warm := createChat(t, s, "mtg")
	postMessage(t, s, warm, "How does deathtouch work?", "") // populate
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("populate: expected 1 model call, got %d", got)
	}

	// ?nocache forces a fresh stream even though the entry is warm.
	fresh := createChat(t, s, "mtg")
	rec := postMessage(t, s, fresh, "How does deathtouch work?", "nocache")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("nocache should call the model again, got %d total calls", got)
	}
	var sawFresh bool
	for _, e := range sseEvents(t, rec.Body.String()) {
		if e.Event == "meta" && strings.Contains(e.Data, `"cached":false`) {
			sawFresh = true
		}
	}
	if !sawFresh {
		t.Error("nocache meta event should flag cached:false")
	}
}
