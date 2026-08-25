// Package transcribe is a minimal OpenAI-compatible audio transcription
// client. The base URL, key, and model are configurable so it can target
// OpenAI directly or any endpoint that speaks the same POST
// /audio/transcriptions multipart shape — whisper.cpp's server,
// faster-whisper-server, LocalAI, and the like. One wire format reaches every
// realistic backend.
//
// The hook is deliberately optional (ADR 5): Grimoire ships no speech model,
// and a client with no model configured reports Configured()==false, which
// callers treat as "the affordance is not there" — exactly how the embeddings
// client behaves. The API key is optional too, unlike embeddings: the local
// backends this targets do not authenticate.
package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Config holds transcription connection settings.
type Config struct {
	BaseURL string // e.g. https://api.openai.com/v1 or http://transcribe:8000/v1
	APIKey  string // secret; never logged — optional, local backends need none
	Model   string // e.g. whisper-1 — required; a model name is provider-specific
	// Timeout bounds one transcription HTTP call. One request can carry tens
	// of minutes of audio and CPU whisper is slower than realtime, so this
	// needs to be far longer than a usual API timeout. Zero means
	// DefaultTimeout.
	Timeout time.Duration
}

// DefaultBaseURL is the canonical OpenAI endpoint (with the /v1 prefix, so
// the request path is simply /audio/transcriptions appended to it).
const DefaultBaseURL = "https://api.openai.com/v1"

// DefaultTimeout bounds a single transcription request. Local CPU backends
// chew through a chunk far slower than realtime; thirty minutes keeps the
// request bounded without failing a healthy box.
const DefaultTimeout = 30 * time.Minute

// Client is an OpenAI-compatible transcription client.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client. An empty model yields a client that reports
// Configured()==false; calls then return ErrNotConfigured.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout},
	}
}

// Configured reports whether a model is set — the model is the switch, since
// the base URL has a default and the key is optional (local backends
// authenticate nobody). Unset model means the audio affordance is simply not
// there, no degraded path.
//
// A nil *Client counts as unconfigured rather than panicking, the same
// contract the embeddings client keeps.
func (c *Client) Configured() bool {
	return c != nil && strings.TrimSpace(c.cfg.Model) != ""
}

// Model returns the configured model name, or "" when there is none.
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.cfg.Model
}

// ErrNotConfigured signals the client has no model set.
type errNotConfigured struct{}

func (errNotConfigured) Error() string {
	return "transcription not configured: set TRANSCRIBE_BASE_URL and TRANSCRIBE_MODEL"
}

// ErrNotConfigured is returned when no model is set.
var ErrNotConfigured error = errNotConfigured{}

// Segment is one timed stretch of the returned transcript, in milliseconds
// relative to the start of the audio that was sent.
type Segment struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
}

// Result is one transcription. Segments is nil when the backend returned no
// timings — the transcript is then plain text, like a pasted one.
type Result struct {
	Text     string
	Segments []Segment
}

// Transcribe sends one audio blob to the endpoint. The filename's extension
// is how backends detect the container format, so pass the recording's real
// name (or the same extension for a chunk). The request asks for
// verbose_json so segment timings come back; a backend that ignores the
// format and returns plain json still works — Text lands, Segments stays
// empty. The language may be empty (auto-detect).
func (c *Client) Transcribe(ctx context.Context, audio io.Reader, filename, language string) (Result, error) {
	if !c.Configured() {
		return Result{}, ErrNotConfigured
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreatePart(fileHeader(filename, MIMEByFilename(filename)))
	if err != nil {
		return Result{}, err
	}
	if _, err := io.Copy(part, audio); err != nil {
		return Result{}, fmt.Errorf("read audio: %w", err)
	}
	_ = w.WriteField("model", c.cfg.Model)
	_ = w.WriteField("response_format", "verbose_json")
	if strings.TrimSpace(language) != "" {
		_ = w.WriteField("language", strings.TrimSpace(language))
	}
	if err := w.Close(); err != nil {
		return Result{}, err
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body.Bytes()))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("content-type", w.FormDataContentType())
	if c.cfg.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("transcription request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Result{}, fmt.Errorf("transcription request failed: %s: %s", resp.Status, truncate(string(raw), 300))
	}

	var tr struct {
		Text     string `json:"text"`
		Segments []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&tr); err != nil {
		return Result{}, fmt.Errorf("decode transcription response: %w", err)
	}
	out := Result{Text: strings.TrimSpace(tr.Text)}
	for _, seg := range tr.Segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		out.Segments = append(out.Segments, Segment{
			StartMS: secondsToMS(seg.Start), EndMS: secondsToMS(seg.End), Text: text,
		})
	}
	if out.Text == "" && len(out.Segments) == 0 {
		return Result{}, fmt.Errorf("transcription response carried no text")
	}
	return out, nil
}

// secondsToMS rounds float seconds (the OpenAI wire format) to milliseconds.
func secondsToMS(s float64) int64 {
	if s < 0 {
		return 0
	}
	return int64(s*1000 + 0.5)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
