// Package embeddings is a minimal OpenAI-compatible embeddings client. The
// base URL, key, and model are configurable so it can target OpenAI directly
// or any OpenAI-compatible endpoint (z.ai/GLM, local gateways, …).
//
// The app is Anthropic-Messages-shaped for the Q&A step, and Anthropic exposes
// no embeddings API, so semantic retrieval targets the OpenAI /embeddings
// contract that compatible gateways also implement. The store is off by
// default; retrieval falls back to FTS5-only when this client is unconfigured.
package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config holds embeddings connection settings.
type Config struct {
	BaseURL string // e.g. https://api.openai.com/v1 or https://api.z.ai/v1
	APIKey  string // secret; never logged
	Model   string // e.g. text-embedding-3-small, embedding-3
}

// DefaultBaseURL is the canonical OpenAI endpoint (with the /v1 prefix, so the
// request path is simply /embeddings appended to it).
const DefaultBaseURL = "https://api.openai.com/v1"

// batchSize caps how many inputs go in one /embeddings request. OpenAI and
// compatible gateways accept batched input; chunking keeps request bodies
// bounded while indexing a corpus of thousands of rules.
const batchSize = 64

// requestTimeout bounds a single embeddings HTTP call. A batch lands well
// inside this; the caller's context bounds the overall index run.
const requestTimeout = 60 * time.Second

// Client is an OpenAI-compatible embeddings client.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client. A nil/empty key or model yields a client that reports
// Configured()==false; calls then return ErrNotConfigured.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: requestTimeout},
	}
}

// Configured reports whether both an API key and a model are set. A model is
// required (unlike the LLM client, which carries a default): embedding model
// names are provider-specific and there is no safe cross-provider default.
//
// A nil *Client counts as unconfigured rather than panicking. Callers use a
// nil client to mean "no embeddings", and that is the same state as a client
// with no key — so every method here treats it that way. Without this, a nil
// client that reaches a call site through an interface (where it stops
// comparing equal to nil) crashes instead of degrading to FTS5-only.
func (c *Client) Configured() bool {
	return c != nil &&
		strings.TrimSpace(c.cfg.APIKey) != "" &&
		strings.TrimSpace(c.cfg.Model) != ""
}

// Model returns the configured model name, or "" when there is none.
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.cfg.Model
}

// ErrNotConfigured signals the client has no key or model.
type errNotConfigured struct{}

func (errNotConfigured) Error() string {
	return "embeddings not configured: set EMBEDDINGS_API_KEY and EMBEDDINGS_MODEL"
}

// ErrNotConfigured is returned when no key or model is set.
var ErrNotConfigured error = errNotConfigured{}

// Embed returns one vector per input text, in input order. Large inputs are
// chunked into batches of batchSize; the returned slice always lines up
// element-for-element with the input. An empty input returns nil.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += batchSize {
		end := i + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := c.embedBatch(ctx, texts[i:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// embedBatch makes one /embeddings call. The OpenAI contract returns data in
// input order; compatible gateways follow the same ordering.
func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.cfg.Model, Input: texts})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("embeddings request failed: %s: %s", resp.Status, truncate(string(raw), 300))
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var er embedResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("decode embeddings response: %w", err)
	}
	if len(er.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings returned %d vectors for %d inputs", len(er.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for i := range er.Data {
		v := er.Data[i].Embedding
		if len(v) == 0 {
			return nil, fmt.Errorf("embeddings response missing vector %d", i)
		}
		out[i] = v
	}
	return out, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
