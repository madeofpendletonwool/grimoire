package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEndpoint replies to /audio/transcriptions with a verbose_json body
// built from the segments it was handed, and records what it saw.
type fakeEndpoint struct {
	t           *testing.T
	gotAuth     string
	gotModel    string
	gotFormat   string
	gotLang     string
	gotFile     []byte
	gotFilename string
	calls       int
	failWith    int
	segments    string // raw "segments" JSON; empty for a text-only backend
	text        string
}

func (f *fakeEndpoint) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		f.gotAuth = r.Header.Get("authorization")
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		f.gotModel = r.FormValue("model")
		f.gotFormat = r.FormValue("response_format")
		f.gotLang = r.FormValue("language")
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "no file", http.StatusBadRequest)
			return
		}
		defer file.Close()
		f.gotFile, _ = io.ReadAll(file)
		f.gotFilename = header.Filename
		if f.failWith != 0 {
			http.Error(w, "backend says no", f.failWith)
			return
		}
		w.Header().Set("content-type", "application/json")
		resp := map[string]any{"text": f.text}
		if f.segments != "" {
			resp["segments"] = json.RawMessage(f.segments)
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	return mux
}

func TestTranscribe_RequestsTheOpenAIShape(t *testing.T) {
	fake := &fakeEndpoint{text: "hello there"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL + "/v1", Model: "whisper-test", APIKey: "sekrit"})
	audio := strings.Repeat("mp3bytes", 4)
	res, err := c.Transcribe(context.Background(), strings.NewReader(audio), "session.mp3", "en")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if res.Text != "hello there" || res.Segments != nil {
		t.Fatalf("result = %+v", res)
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want 1", fake.calls)
	}
	if fake.gotModel != "whisper-test" {
		t.Errorf("model = %q", fake.gotModel)
	}
	if fake.gotFormat != "verbose_json" {
		t.Errorf("response_format = %q", fake.gotFormat)
	}
	if fake.gotLang != "en" {
		t.Errorf("language = %q", fake.gotLang)
	}
	if fake.gotFilename != "session.mp3" {
		t.Errorf("filename = %q", fake.gotFilename)
	}
	if string(fake.gotFile) != audio {
		t.Errorf("file body = %q", fake.gotFile)
	}
	if fake.gotAuth != "Bearer sekrit" {
		t.Errorf("authorization = %q", fake.gotAuth)
	}
}

func TestTranscribe_NoKeySendsNoAuthorization(t *testing.T) {
	fake := &fakeEndpoint{text: "hi"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL + "/v1", Model: "local-model"})
	if _, err := c.Transcribe(context.Background(), strings.NewReader("x"), "a.ogg", ""); err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if fake.gotAuth != "" {
		t.Errorf("authorization = %q, want empty for a keyless local backend", fake.gotAuth)
	}
}

func TestTranscribe_ParsesSegmentsToMilliseconds(t *testing.T) {
	fake := &fakeEndpoint{
		text: " The door grinds open. I check for traps. ",
		segments: `[
			{"id":0,"start":1.5,"end":4.0,"text":" The door grinds open. "},
			{"id":1,"start":5.5,"end":9.25,"text":" I check for traps. "}
		]`,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	res, err := c.Transcribe(context.Background(), strings.NewReader("x"), "a.wav", "")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if len(res.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(res.Segments))
	}
	want := []Segment{
		{StartMS: 1500, EndMS: 4000, Text: "The door grinds open."},
		{StartMS: 5500, EndMS: 9250, Text: "I check for traps."},
	}
	for i, w := range want {
		if res.Segments[i] != w {
			t.Errorf("segment[%d] = %+v, want %+v", i, res.Segments[i], w)
		}
	}
}

func TestTranscribe_HTTPErrorSurfacesStatus(t *testing.T) {
	fake := &fakeEndpoint{failWith: http.StatusBadGateway}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	_, err := c.Transcribe(context.Background(), strings.NewReader("x"), "a.mp3", "")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want a 502-carrying error", err)
	}
}

func TestTranscribe_EmptyResponseIsAnError(t *testing.T) {
	fake := &fakeEndpoint{text: ""}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	if _, err := c.Transcribe(context.Background(), strings.NewReader("x"), "a.mp3", ""); err == nil {
		t.Fatal("empty text + no segments should error, got nil")
	}
}

func TestTranscribe_NotConfigured(t *testing.T) {
	c := New(Config{})
	if c.Configured() {
		t.Error("no model: Configured() = true")
	}
	_, err := c.Transcribe(context.Background(), strings.NewReader("x"), "a.mp3", "")
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

// A nil *Client is how callers spell "transcription is off"; every method
// must survive it, the same contract the embeddings client keeps.
func TestNilClientIsUnconfiguredRatherThanFatal(t *testing.T) {
	var c *Client
	if c.Configured() {
		t.Error("nil client reported itself configured")
	}
	if got := c.Model(); got != "" {
		t.Errorf("nil client model = %q", got)
	}
	if _, err := c.Transcribe(context.Background(), strings.NewReader("x"), "a.mp3", ""); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("nil client error = %v, want ErrNotConfigured", err)
	}
}
