// Package llm is a minimal Anthropic Messages API client used for the Q&A
// feature. The base URL, key, and model are configurable so it can target
// Anthropic directly or an Anthropic-compatible endpoint such as
// z.ai (https://api.z.ai/api/anthropic) running a GLM model.
package llm

import (
	"bufio"
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

// maxAnswerTokens caps the answer length. A layer-by-layer rules walkthrough
// runs long, and a truncated answer is worse than a verbose one.
const maxAnswerTokens = 4096

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
		cfg: cfg,
		// Generous ceiling only: a streamed answer is alive for as long as
		// tokens keep arriving, so the real deadline is the caller's context.
		http: &http.Client{Timeout: 5 * time.Minute},
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

// RulingDoc is one official ruling on a card, fed to the model as precedent so
// it can cite Gatherer/Oracle rulings alongside the rule text — turning a
// rules lookup into a rulings oracle. Source is "wotc" or "scryfall".
type RulingDoc struct {
	CardName    string
	Source      string
	PublishedAt string
	Comment     string
}

// Turn is one prior exchange in a saved conversation, replayed so follow-up
// questions ("what if it were tapped instead?") resolve against what was
// already said.
type Turn struct {
	Role    string // "user" or "assistant"
	Content string
}

// Request is one grounded question: the retrieved rules and cards, the earlier
// turns of the conversation, and the question itself.
type Request struct {
	CorpusName string
	Docs       []ContextDoc
	Cards      []CardDoc
	Rulings    []RulingDoc
	Unresolved []string
	History    []Turn
	Question   string
}

// Answer runs a Messages API call and returns the complete answer.
func (c *Client) Answer(ctx context.Context, req Request) (string, error) {
	return c.run(ctx, req, nil)
}

// Stream runs the same call with server-sent events enabled, invoking onDelta
// for each chunk of text as it arrives. It returns the full concatenated
// answer. A non-nil error from onDelta aborts the stream — that is how a
// disconnected browser stops work that no longer has a reader.
func (c *Client) Stream(ctx context.Context, req Request, onDelta func(string) error) (string, error) {
	if onDelta == nil {
		return c.Answer(ctx, req)
	}
	return c.run(ctx, req, onDelta)
}

// run performs the HTTP call. With onDelta nil it reads a single JSON body;
// otherwise it asks for SSE and decodes the event stream.
func (c *Client) run(ctx context.Context, r Request, onDelta func(string) error) (string, error) {
	if !c.Configured() {
		return "", ErrNotConfigured
	}
	streaming := onDelta != nil
	reqBody := messagesRequest{
		Model:     c.cfg.Model,
		MaxTokens: maxAnswerTokens,
		System:    systemPrompt(r.CorpusName, len(r.Cards) > 0, len(r.Rulings) > 0),
		Messages:  buildMessages(r),
		Stream:    streaming,
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
	if streaming {
		req.Header.Set("accept", "text/event-stream")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("llm request failed: %s: %s", resp.Status, truncate(string(raw), 300))
	}
	if streaming {
		return readStream(resp.Body, onDelta)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
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

// buildMessages lays out the Messages API exchange: prior turns verbatim, then
// the current question with its freshly retrieved grounding attached. Only the
// latest turn carries rule and card text — re-sending grounding for every
// historical turn would blow the context window on stale excerpts.
func buildMessages(r Request) []message {
	msgs := make([]message, 0, len(r.History)+1)
	for _, t := range r.History {
		role := t.Role
		if role != "assistant" {
			role = "user"
		}
		if strings.TrimSpace(t.Content) == "" {
			continue
		}
		msgs = append(msgs, message{Role: role, Content: t.Content})
	}
	// The API rejects two consecutive messages with the same role, which a
	// truncated or interrupted history can produce; fold those together.
	merged := msgs[:0]
	for _, m := range msgs {
		if n := len(merged); n > 0 && merged[n-1].Role == m.Role {
			merged[n-1].Content += "\n\n" + m.Content
			continue
		}
		merged = append(merged, m)
	}
	msgs = merged

	user := buildUserMessage(r.CorpusName, r.Docs, r.Cards, r.Rulings, r.Unresolved, r.Question)
	if n := len(msgs); n > 0 && msgs[n-1].Role == "user" {
		msgs[n-1].Content += "\n\n" + user
		return msgs
	}
	return append(msgs, message{Role: "user", Content: user})
}

// readStream decodes an Anthropic SSE body, forwarding text deltas as they
// arrive. Event framing is "data: {json}" lines separated by blank lines; we
// only care about content_block_delta payloads and inline error events.
func readStream(body io.Reader, onDelta func(string) error) (string, error) {
	sc := bufio.NewScanner(body)
	// Long single-line JSON payloads are normal here; the default 64KB scanner
	// limit would truncate one and desync the decode.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var full strings.Builder
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue // ignore keep-alives and any framing we don't model
		}
		switch ev.Type {
		case "content_block_delta":
			if ev.Delta.Type != "" && ev.Delta.Type != "text_delta" {
				continue // thinking / tool deltas are not answer text
			}
			if ev.Delta.Text == "" {
				continue
			}
			full.WriteString(ev.Delta.Text)
			if err := onDelta(ev.Delta.Text); err != nil {
				return full.String(), err
			}
		case "error":
			msg := ev.Error.Message
			if msg == "" {
				msg = "stream error"
			}
			return full.String(), fmt.Errorf("llm stream: %s", msg)
		}
	}
	if err := sc.Err(); err != nil {
		return full.String(), fmt.Errorf("read llm stream: %w", err)
	}
	out := strings.TrimSpace(full.String())
	if out == "" {
		return "", fmt.Errorf("llm returned no text")
	}
	return out, nil
}

func systemPrompt(corpusName string, hasCards bool, hasRulings bool) string {
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
	if hasRulings {
		b.WriteString("\n6. Official rulings were provided. Cite them when they decide the interaction — name the card, the ruling's source (wotc or scryfall), and its date, and quote the decisive phrase. Treat wotc rulings as authoritative precedent; treat scryfall rulings as official guidance.")
		b.WriteString("\n7. If a ruling is relevant to a named card but no ruling for that card was provided, say plainly that no official ruling was available — do NOT invent one from memory.")
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

func buildUserMessage(corpusName string, docs []ContextDoc, cards []CardDoc, rulings []RulingDoc, unresolved []string, question string) string {
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

	if len(rulings) > 0 {
		b.WriteString("Official rulings (authoritative precedent — from Scryfall/Gatherer):\n\n")
		byCard := groupRulingsByCard(rulings)
		for _, g := range byCard {
			fmt.Fprintf(&b, "### %s\n", g.card)
			for _, r := range g.rulings {
				src := r.Source
				if src == "" {
					src = "official"
				}
				fmt.Fprintf(&b, "- [%s, %s] %s\n", src, r.PublishedAt, truncate(strings.TrimSpace(r.Comment), 600))
			}
			b.WriteString("\n")
		}
	}

	if len(unresolved) > 0 {
		fmt.Fprintf(&b, "Names in the question that could not be looked up: %s\n"+
			"Do not describe these from memory — say the lookup failed.\n\n",
			strings.Join(unresolved, ", "))
	}

	b.WriteString("Question: " + question)
	return b.String()
}

// rulingGroup is the set of rulings for one card, in the order they arrived.
type rulingGroup struct {
	card    string
	rulings []RulingDoc
}

// groupRulingsByCard preserves first-seen card order while keeping each card's
// rulings contiguous, so the model sees "card -> its rulings" rather than a
// flat interleaved list.
func groupRulingsByCard(rulings []RulingDoc) []rulingGroup {
	var groups []rulingGroup
	index := map[string]int{}
	for _, r := range rulings {
		name := r.CardName
		if i, ok := index[name]; ok {
			groups[i].rulings = append(groups[i].rulings, r)
			continue
		}
		index[name] = len(groups)
		groups = append(groups, rulingGroup{card: name, rulings: []RulingDoc{r}})
	}
	return groups
}

type messagesRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Stream    bool      `json:"stream,omitempty"`
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

// streamEvent is the subset of the Anthropic SSE event shape we consume.
type streamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
