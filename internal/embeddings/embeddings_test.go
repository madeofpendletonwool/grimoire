package embeddings

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// embedHandler replies to /embeddings with a synthetic vector per input: each
// output vector is [float32(len(input)), float32(firstRune)]. That lets a test
// assert both ordering and that the right texts were sent.
func embedHandler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("authorization"); !strings.HasPrefix(got, "Bearer ") {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		var req embedRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
		}
		resp := struct {
			Data []item `json:"data"`
		}{}
		for _, in := range req.Input {
			first := float32(0)
			if len(in) > 0 {
				first = float32(in[0])
			}
			resp.Data = append(resp.Data, item{Embedding: []float32{float32(len(in)), first}})
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	return mux
}

func newTestClient(t *testing.T, baseURL string) *Client {
	return New(Config{BaseURL: baseURL, APIKey: "test-key", Model: "test-embed"})
}

func TestEmbed_OrderedBatch(t *testing.T) {
	srv := httptest.NewServer(embedHandler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	texts := []string{"hello", "world", "hexproof rules"}
	vecs, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}
	for i, want := range []float32{5, 5, 14} {
		if got := vecs[i][0]; got != want {
			t.Errorf("vec[%d][0] = %v, want %v (input length)", i, got, want)
		}
	}
	// First-rune fingerprint proves order lines up with input.
	if vecs[0][1] != 'h' || vecs[1][1] != 'w' || vecs[2][1] != 'h' {
		t.Errorf("vectors out of order: %v", vecs)
	}
}

func TestEmbed_ChunksIntoBatches(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		var req embedRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if len(req.Input) > batchSize {
			t.Errorf("batch too large: %d > %d", len(req.Input), batchSize)
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Data []item `json:"data"`
		}{}
		for range req.Input {
			out.Data = append(out.Data, item{Embedding: []float32{1, 1}})
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	texts := make([]string, batchSize*2+3)
	for i := range texts {
		texts[i] = "x"
	}
	vecs, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Errorf("got %d vectors, want %d", len(vecs), len(texts))
	}
	if calls != 3 {
		t.Errorf("expected 3 batch calls for %d inputs (batch=%d), got %d", len(texts), batchSize, calls)
	}
}

func TestEmbed_NotConfigured(t *testing.T) {
	cases := []Config{
		{APIKey: "", Model: "m"},
		{APIKey: "k", Model: ""},
		{APIKey: "  ", Model: "  "},
	}
	for _, cfg := range cases {
		c := New(cfg)
		if c.Configured() {
			t.Errorf("%+v: Configured() = true, want false", cfg)
		}
		if _, err := c.Embed(context.Background(), []string{"x"}); err != ErrNotConfigured {
			t.Errorf("expected ErrNotConfigured, got %v", err)
		}
	}
}

func TestEmbed_EmptyInput(t *testing.T) {
	c := newTestClient(t, "http://unused.example")
	vecs, err := c.Embed(context.Background(), nil)
	if err != nil || vecs != nil {
		t.Errorf("Embed(nil) = %v, %v; want nil, nil", vecs, err)
	}
}

func TestEmbed_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected error on 4xx, got nil")
	}
}

func TestEmbed_AuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		type item struct {
			Embedding []float32 `json:"embedding"`
		}
		json.NewEncoder(w).Encode(struct {
			Data []item `json:"data"`
		}{Data: []item{{Embedding: []float32{0.1}}}})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization = %q, want %q", gotAuth, "Bearer test-key")
	}
}

func TestEmbed_ModelEchoedInRequest(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req embedRequest
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		type item struct {
			Embedding []float32 `json:"embedding"`
		}
		json.NewEncoder(w).Encode(struct {
			Data []item `json:"data"`
		}{Data: []item{{Embedding: []float32{0.1}}}})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if gotModel != "test-embed" {
		t.Errorf("model = %q, want %q", gotModel, "test-embed")
	}
}
