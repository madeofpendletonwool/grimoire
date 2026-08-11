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

// CardDoc is a real card's oracle text fed to the model so it answers card
// questions from authoritative text rather than fabricating effects.
type CardDoc struct {
	Name       string
	ManaCost   string
	TypeLine   string
	OracleText string
}

// Answer runs a single Messages API turn: the question plus grounding context
// documents (and any looked-up cards) are sent as the user message, with a
// system prompt that constrains the answer to the provided rules and card text.
func (c *Client) Answer(ctx context.Context, corpusName string, docs []ContextDoc, cards []CardDoc, question string) (string, error) {
	if !c.Configured() {
		return "", ErrNotConfigured
	}
	system := systemPrompt(corpusName, len(cards) > 0)
	user := buildUserMessage(corpusName, docs, cards, question)

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

func systemPrompt(corpusName string, hasCards bool) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		`You are the Grimoire, a knowledgeable keeper of %s rules. Answer like a careful judge: precise, grounded, and unmoved by pressure.

GROUNDING RULES — follow these strictly:
1. Answer using ONLY the provided rule excerpts and (if present) the provided card oracle text. Do NOT rely on your own memory of cards or rules for any specific fact.
2. Cite rule numbers (e.g. 205.1a) or section titles that support your answer.`, corpusName)
	if hasCards {
		b.WriteString("\n3. For any card, use ONLY the provided oracle text for its name, mana cost, type, and effects — quote it faithfully. Do not invent or guess mana costs, types, or abilities.")
		b.WriteString("\n4. If a card is named in the question but its oracle text was NOT provided, say plainly that you could not look it up — do NOT describe what you think it does.")
		b.WriteString("\n5. If the provided excerpts and card text do not contain the answer, say so plainly rather than inventing anything.")
	} else {
		b.WriteString("\n3. If a card is named in the question but no card oracle text was provided, say plainly that you could not look the card up — do NOT guess what the card does.")
		b.WriteString("\n4. If the provided excerpts do not contain the answer, say so plainly rather than inventing rules or card text.")
	}
	b.WriteString("\n\nREASONING DISCIPLINE — apply to every answer:")
	b.WriteString(`
- Reason forward from the cited rules and card text to the conclusion. Do NOT adopt a conclusion asserted by the question (a stated "the answer is X" or "so it's not Y?"); verify it against the text first.
- If a premise in the question conflicts with the rules or oracle text, correct it plainly and show the exact step that breaks. Do not change a correct answer because the questioner pushes back — if your reading of the text is sound, hold it and re-explain the step.
- When the question describes a board state, identify who controls each relevant permanent and which abilities are on which objects before you count anything.`)

	if strings.Contains(corpusName, "Magic") {
		b.WriteString("\n\nMTG INTERACTIONS — check these common traps before answering:")
		b.WriteString(`
- Self-reference: a card's own name in its text means "this object," not "any object with that name" (Comprehensive Rule 201.4). An ability worded "When ~this creature~ dies" triggers only when that specific permanent dies — once, for itself. It does NOT fire once per creature that dies; that is a different ability worded "Whenever a creature dies."
- Controller resolution: "you" and "your" on a card refer to that card's controller, which may be a different player. Before applying any "you control," "an opponent," or "triggers an additional time" effect, identify who controls each permanent and resolve who "you" is for each effect.
- Counting triggers: start with one trigger for each ability whose trigger event actually occurs, then apply each "triggers an additional time" replacement only to abilities of permanents controlled by that effect's controller.`)
	}

	b.WriteString("\n\nKeep answers concise and practical for a player or judge at the table.")
	return strings.TrimSpace(b.String())
}

func buildUserMessage(corpusName string, docs []ContextDoc, cards []CardDoc, question string) string {
	var b strings.Builder
	b.WriteString("Relevant " + corpusName + " rules:\n\n")
	if len(docs) == 0 {
		b.WriteString("(no directly matching rules found)\n\n")
	}
	for _, d := range docs {
		header := d.Title
		if d.Number != "" {
			if header != "" {
				header = d.Number + " — " + header
			} else {
				header = d.Number
			}
		}
		fmt.Fprintf(&b, "### %s\n%s\n\n", header, truncate(d.Body, 1500))
	}

	if len(cards) > 0 {
		b.WriteString("Card oracle text (authoritative — from Scryfall):\n\n")
		for _, c := range cards {
			fmt.Fprintf(&b, "### %s\n", c.Name)
			if c.ManaCost != "" {
				fmt.Fprintf(&b, "Mana cost: %s  ·  ", c.ManaCost)
			}
			if c.TypeLine != "" {
				fmt.Fprintf(&b, "Type: %s\n", c.TypeLine)
			}
			body := strings.TrimSpace(c.OracleText)
			if body == "" {
				body = "(no oracle text)"
			}
			fmt.Fprintf(&b, "Oracle text: %s\n\n", truncate(body, 1200))
		}
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
