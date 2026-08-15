package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// provider spins up a fake Messages endpoint that answers with a fixed status
// and body, counting the calls it receives.
type provider struct {
	cfg    Config
	calls  atomic.Int32
	seen   messagesRequest
	status int
	body   string
}

func newProvider(t *testing.T, model string, status int, body string) *provider {
	t.Helper()
	p := &provider{status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &p.seen)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(p.status)
		_, _ = w.Write([]byte(p.body))
	}))
	t.Cleanup(srv.Close)
	p.cfg = Config{BaseURL: srv.URL, APIKey: "key-" + model, Model: model}
	return p
}

func okBody(text string) string {
	return `{"content":[{"type":"text","text":"` + text + `"}]}`
}

func TestFailover_QuotaExhaustedFallsBack(t *testing.T) {
	// Anthropic answers an exhausted balance with HTTP 400, not 429 — the
	// status alone would look like a malformed request.
	primary := newProvider(t, "primary-model", http.StatusBadRequest,
		`{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the API"}}`)
	backup := newProvider(t, "backup-model", http.StatusOK, okBody("from the backup"))

	c := New(primary.cfg, backup.cfg)
	out, err := c.AnswerPrompt(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("AnswerPrompt: %v", err)
	}
	if out != "from the backup" {
		t.Errorf("answer = %q, want the backup's", out)
	}
	if got := primary.calls.Load(); got != 1 {
		t.Errorf("primary called %d times, want 1", got)
	}
	if got := backup.calls.Load(); got != 1 {
		t.Errorf("backup called %d times, want 1", got)
	}
	if backup.seen.Model != "backup-model" {
		t.Errorf("fallback sent model %q, want its own", backup.seen.Model)
	}
}

func TestFailover_RateLimitedFallsBack(t *testing.T) {
	primary := newProvider(t, "primary-model", http.StatusTooManyRequests, `{"error":{"message":"rate_limit_error"}}`)
	backup := newProvider(t, "backup-model", http.StatusOK, okBody("second opinion"))

	out, err := New(primary.cfg, backup.cfg).AnswerPrompt(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("AnswerPrompt: %v", err)
	}
	if out != "second opinion" {
		t.Errorf("answer = %q", out)
	}
}

func TestFailover_ServerErrorWalksWholeChain(t *testing.T) {
	first := newProvider(t, "a", http.StatusInternalServerError, `{"error":"boom"}`)
	second := newProvider(t, "b", http.StatusServiceUnavailable, `{"error":"overloaded_error"}`)
	third := newProvider(t, "c", http.StatusOK, okBody("third time"))

	out, err := New(first.cfg, second.cfg, third.cfg).AnswerPrompt(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("AnswerPrompt: %v", err)
	}
	if out != "third time" {
		t.Errorf("answer = %q", out)
	}
	for name, p := range map[string]*provider{"first": first, "second": second, "third": third} {
		if got := p.calls.Load(); got != 1 {
			t.Errorf("%s called %d times, want 1", name, got)
		}
	}
}

func TestFailover_MalformedRequestDoesNotFallBack(t *testing.T) {
	// A 400 that is genuinely about the request would fail identically
	// everywhere; spending the fallback's quota on it helps nobody.
	primary := newProvider(t, "primary-model", http.StatusBadRequest,
		`{"type":"error","error":{"type":"invalid_request_error","message":"messages: roles must alternate"}}`)
	backup := newProvider(t, "backup-model", http.StatusOK, okBody("unused"))

	_, err := New(primary.cfg, backup.cfg).AnswerPrompt(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("expected the primary's error to surface")
	}
	if !strings.Contains(err.Error(), "roles must alternate") {
		t.Errorf("error = %v, want the primary's message", err)
	}
	if got := backup.calls.Load(); got != 0 {
		t.Errorf("backup called %d times, want 0", got)
	}
}

func TestFailover_LastErrorSurfacesWhenAllFail(t *testing.T) {
	primary := newProvider(t, "a", http.StatusTooManyRequests, `{"error":"quota"}`)
	backup := newProvider(t, "b", http.StatusUnauthorized, `{"error":"invalid api key"}`)

	_, err := New(primary.cfg, backup.cfg).AnswerPrompt(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("expected an error when every provider fails")
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error = %v, want the last provider's", err)
	}
}

func TestFailover_StreamFallsBackBeforeAnyText(t *testing.T) {
	primary := newProvider(t, "a", http.StatusTooManyRequests, `{"error":"quota exceeded"}`)
	backup := newProvider(t, "b", http.StatusOK,
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"streamed \"}}\n\n"+
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n")

	var got strings.Builder
	out, err := New(primary.cfg, backup.cfg).StreamPrompt(context.Background(), "sys", "user", func(s string) error {
		got.WriteString(s)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamPrompt: %v", err)
	}
	if out != "streamed answer" || got.String() != "streamed answer" {
		t.Errorf("out = %q, deltas = %q", out, got.String())
	}
}

func TestFailover_StreamDoesNotRestartAfterFirstDelta(t *testing.T) {
	// The reader has already seen text; a second provider would splice two
	// half-answers together, so the partial answer and its error stand.
	primary := newProvider(t, "a", http.StatusOK,
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"half an \"}}\n\n"+
			"data: {\"type\":\"error\",\"error\":{\"message\":\"upstream died\"}}\n\n")
	backup := newProvider(t, "b", http.StatusOK, okBody("unused"))

	var got strings.Builder
	out, err := New(primary.cfg, backup.cfg).StreamPrompt(context.Background(), "sys", "user", func(s string) error {
		got.WriteString(s)
		return nil
	})
	if err == nil {
		t.Fatal("expected the stream error to surface")
	}
	if out != "half an " {
		t.Errorf("partial answer = %q", out)
	}
	if n := backup.calls.Load(); n != 0 {
		t.Errorf("backup called %d times after text was emitted, want 0", n)
	}
}

func TestFailover_StreamDoesNotRetryWhenReaderGoesAway(t *testing.T) {
	// A browser closing the connection is not a provider fault.
	primary := newProvider(t, "a", http.StatusOK,
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"tokens\"}}\n\n")
	backup := newProvider(t, "b", http.StatusOK, okBody("unused"))

	_, err := New(primary.cfg, backup.cfg).StreamPrompt(context.Background(), "sys", "user", func(string) error {
		return io.ErrClosedPipe
	})
	if err != io.ErrClosedPipe {
		t.Fatalf("err = %v, want the reader's error", err)
	}
	if n := backup.calls.Load(); n != 0 {
		t.Errorf("backup called %d times, want 0", n)
	}
}

func TestFailover_CancelledContextStopsTheWalk(t *testing.T) {
	primary := newProvider(t, "a", http.StatusTooManyRequests, `{"error":"quota"}`)
	backup := newProvider(t, "b", http.StatusOK, okBody("unused"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(primary.cfg, backup.cfg).AnswerPrompt(ctx, "sys", "user"); err == nil {
		t.Fatal("expected an error on a cancelled context")
	}
	if n := backup.calls.Load(); n != 0 {
		t.Errorf("backup called %d times on a cancelled context, want 0", n)
	}
}

func TestFailover_UnconfiguredPrimaryIsSkipped(t *testing.T) {
	backup := newProvider(t, "backup-model", http.StatusOK, okBody("carried by the fallback"))
	c := New(Config{BaseURL: "https://api.anthropic.com", Model: "primary-model"}, backup.cfg)

	if !c.Configured() {
		t.Fatal("a keyed fallback should count as configured")
	}
	if c.Model() != "backup-model" {
		t.Errorf("Model() = %q, want the first keyed provider's", c.Model())
	}
	if fb := c.FallbackModels(); len(fb) != 0 {
		t.Errorf("FallbackModels() = %v, want none behind a lone provider", fb)
	}
	out, err := c.AnswerPrompt(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("AnswerPrompt: %v", err)
	}
	if out != "carried by the fallback" {
		t.Errorf("answer = %q", out)
	}
}

func TestFallbackModels(t *testing.T) {
	c := New(
		Config{APIKey: "k1", Model: "primary"},
		Config{APIKey: "k2", Model: "second"},
		Config{Model: "unkeyed"},
		Config{APIKey: "k3", Model: "third"},
	)
	if c.Model() != "primary" {
		t.Errorf("Model() = %q", c.Model())
	}
	got := strings.Join(c.FallbackModels(), ",")
	if got != "second,third" {
		t.Errorf("FallbackModels() = %q, want the keyed ones in order", got)
	}
}

func TestNoProviderConfigured(t *testing.T) {
	c := New(Config{Model: "m"}, Config{Model: "n"})
	if c.Configured() {
		t.Error("keyless providers must not report configured")
	}
	if _, err := c.AnswerPrompt(context.Background(), "s", "u"); err != ErrNotConfigured {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

func TestShouldFailOver(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"transport", io.ErrUnexpectedEOF, true},
		{"429", &apiError{status: 429, body: "slow down"}, true},
		{"401", &apiError{status: 401, body: "bad key"}, true},
		{"402", &apiError{status: 402, body: "pay up"}, true},
		{"404 model", &apiError{status: 404, body: "model not found"}, true},
		{"500", &apiError{status: 500, body: "internal"}, true},
		{"529 overloaded", &apiError{status: 529, body: "overloaded_error"}, true},
		{"400 quota", &apiError{status: 400, body: "Your credit balance is too low"}, true},
		{"400 malformed", &apiError{status: 400, body: "messages.0.role: unexpected value"}, false},
		{"413 too large", &apiError{status: 413, body: "request entity too large"}, false},
	}
	for _, tc := range cases {
		if got := shouldFailOver(tc.err); got != tc.want {
			t.Errorf("%s: shouldFailOver = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.anthropic.com":       "api.anthropic.com",
		"https://api.z.ai/api/anthropic":  "api.z.ai",
		"http://localhost:8080/v1/":       "localhost:8080",
		"https://gateway.example.com?x=1": "gateway.example.com",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
