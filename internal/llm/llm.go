// Package llm is a minimal Anthropic Messages API client used for the Q&A
// feature. The base URL, key, and model are configurable so it can target
// Anthropic directly or an Anthropic-compatible endpoint such as
// z.ai (https://api.z.ai/api/anthropic) running a GLM model.
package llm

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

// Config holds LLM connection settings.
type Config struct {
	BaseURL string // e.g. https://api.anthropic.com or https://api.z.ai/api/anthropic
	APIKey  string // secret; never logged
	Model   string // e.g. glm-4.6, claude-3-5-sonnet-20241022
}

// DefaultBaseURL is the canonical Anthropic endpoint.
const DefaultBaseURL = "https://api.anthropic.com"

// Client is an Anthropic Messages API client.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client. A nil/empty key yields a client that reports
// Configured()==false; calls then return ErrNotConfigured.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// Configured reports whether an API key is set.
func (c *Client) Configured() bool { return strings.TrimSpace(c.cfg.APIKey) != "" }

// Model returns the configured model name.
func (c *Client) Model() string { return c.cfg.Model }

// ErrNotConfigured is returned when no API key is set.
type errNotConfigured struct{}

func (errNotConfigured) Error() string { return "LLM not configured: set ANTHROPIC_API_KEY" }

// ErrNotConfigured signals the client has no API key.
var ErrNotConfigured error = errNotConfigured{}

// ContextDoc is a retrieved chunk fed to the model as grounding context.
type ContextDoc struct {
	Number string
	Title  string
	Body   string
}

// Answer runs a single Messages API turn: the question plus grounding context
// documents are sent as the user message, with a system prompt that constrains
// the answer to the provided rules.
func (c *Client) Answer(ctx context.Context, corpusName string, docs []ContextDoc, question string) (string, error) {
	if !c.Configured() {
		return "", ErrNotConfigured
	}
	system := systemPrompt(corpusName)
	user := buildUserMessage(corpusName, docs, question)

	reqBody := messagesRequest{
		Model:     c.cfg.Model,
		MaxTokens: 1024,
		System:    system,
		Messages:  []message{{Role: "user", Content: user}},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	// Send the key via both headers for max provider compatibility
	// (Anthropic uses x-api-key; some compatible gateways expect Bearer).
	req.Header.Set("x-api-key", c.cfg.APIKey)
	req.Header.Set("authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm request failed: %s: %s", resp.Status, truncate(string(raw), 300))
	}

	var mr messagesResponse
	if err := json.Unmarshal(raw, &mr); err != nil {
		return "", fmt.Errorf("decode llm response: %w", err)
	}
	var b strings.Builder
	for _, block := range mr.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", fmt.Errorf("llm returned no text")
	}
	return out, nil
}

func systemPrompt(corpusName string) string {
	return strings.TrimSpace(fmt.Sprintf(
		`You are the Grimoire, a knowledgeable keeper of %s rules.

Answer the player's question using ONLY the provided rule excerpts. Be precise and cite rule numbers (e.g. 205.1a) or section titles when they support your answer. If the provided excerpts do not contain the answer, say so plainly rather than inventing rules. Keep answers concise and practical for a player or judge at the table.`,
		corpusName))
}

func buildUserMessage(corpusName string, docs []ContextDoc, question string) string {
	var b strings.Builder
	b.WriteString("Relevant " + corpusName + " rules:\n\n")
	if len(docs) == 0 {
		b.WriteString("(no directly matching rules found)\n\n")
	}
	for i, d := range docs {
		header := d.Title
		if d.Number != "" {
			if header != "" {
				header = d.Number + " — " + header
			} else {
				header = d.Number
			}
		}
		fmt.Fprintf(&b, "### %s\n%s\n\n", header, truncate(d.Body, 1500))
		_ = i
	}
	b.WriteString("Question: " + question)
	return b.String()
}

type messagesRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
