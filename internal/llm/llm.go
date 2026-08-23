// Package llm is a minimal Anthropic Messages API client used for the Q&A
// feature. The base URL, key, and model are configurable so it can target
// Anthropic directly or an Anthropic-compatible endpoint such as
// z.ai (https://api.z.ai/api/anthropic) running a GLM model.
//
// A client can hold more than one provider: the primary plus an ordered chain
// of fallbacks. When a call fails for a reason another provider might not share
// — exhausted quota, an expired key, an overloaded or unreachable endpoint —
// the next provider in the chain answers instead, so running out of credit on
// one account degrades to a second account rather than to a broken chat.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
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

// Client is an Anthropic Messages API client over one or more providers.
type Client struct {
	providers []Config // primary first, then fallbacks in order
	http      *http.Client
}

// New builds a client from a primary provider and any number of fallbacks,
// tried in order when the one before it fails. A nil/empty key on every
// provider yields a client that reports Configured()==false; calls then return
// ErrNotConfigured. Providers without a key are skipped, so a fallback alone is
// enough to run the chat.
func New(cfg Config, fallbacks ...Config) *Client {
	providers := make([]Config, 0, 1+len(fallbacks))
	for _, p := range append([]Config{cfg}, fallbacks...) {
		if p.BaseURL == "" {
			p.BaseURL = DefaultBaseURL
		}
		providers = append(providers, p)
	}
	return &Client{
		providers: providers,
		// Generous ceiling only: a streamed answer is alive for as long as
		// tokens keep arriving, so the real deadline is the caller's context.
		http: &http.Client{Timeout: 5 * time.Minute},
	}
}

// active returns the providers that carry a key, in preference order.
func (c *Client) active() []Config {
	out := make([]Config, 0, len(c.providers))
	for _, p := range c.providers {
		if strings.TrimSpace(p.APIKey) != "" {
			out = append(out, p)
		}
	}
	return out
}

// Configured reports whether any provider has an API key.
func (c *Client) Configured() bool { return len(c.active()) > 0 }

// Model returns the model name of the provider that answers first.
func (c *Client) Model() string {
	if a := c.active(); len(a) > 0 {
		return a[0].Model
	}
	return ""
}

// FallbackModels returns the model names standing behind the primary, so the
// status endpoint can show what the chat falls back to.
func (c *Client) FallbackModels() []string {
	a := c.active()
	if len(a) < 2 {
		return nil
	}
	models := make([]string, 0, len(a)-1)
	for _, p := range a[1:] {
		models = append(models, p.Model)
	}
	return models
}

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
	// Source names the book or corpus the excerpt came from ("D&D books —
	// Player's Handbook", "D&D 5e SRD — classes"). The prompt asks the model to
	// name the book behind a ruling and to flag 2014/2024 edition conflicts
	// between excerpts; neither is possible unless the excerpt says where it
	// came from.
	Source string
}

// CardDoc is a real card's oracle text fed to the model so it answers card
// questions from authoritative text rather than fabricating effects.
type CardDoc struct {
	Name       string
	ManaCost   string
	TypeLine   string
	OracleText string
}

// EntityDoc is a resolved reference entry (a D&D spell, creature, item, feat)
// fed to the model so it answers questions about named entities from the
// authoritative SRD text rather than from memory. It is the corpus-neutral
// counterpart to CardDoc, used by corpora without card-shaped entities.
type EntityDoc struct {
	Name string
	Kind string // "spell", "creature", "magic item", "feat", ...
	Body string // the formatted reference / stat block text
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
	Entities   []EntityDoc
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

// Usage is the token accounting of one completed exchange, as reported by the
// provider that answered. Consumers that bill or budget against tokens (the
// canon engine) read it instead of estimating.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AnswerPrompt runs a single-turn exchange with a caller-supplied system prompt
// and user message. It is the entry point for features that need their own
// prompt shape (the interaction resolver) while reusing the same Messages API
// plumbing, streaming, and not-configured guard as the Q&A chat. onDelta nil
// reads one JSON body; non-nil asks for SSE.
func (c *Client) AnswerPrompt(ctx context.Context, system, user string) (string, error) {
	out, _, err := c.callMessages(ctx, system, []message{{Role: "user", Content: user}}, false, nil)
	return out, err
}

// AnswerPromptUsage is AnswerPrompt that also reports the exchange's token
// usage. Non-streaming by design: usage comes back in the JSON body, and the
// features that need it (canon-engine batch passes) never stream.
func (c *Client) AnswerPromptUsage(ctx context.Context, system, user string) (string, Usage, error) {
	return c.callMessages(ctx, system, []message{{Role: "user", Content: user}}, false, nil)
}

// StreamPrompt is the streaming form of AnswerPrompt.
func (c *Client) StreamPrompt(ctx context.Context, system, user string, onDelta func(string) error) (string, error) {
	if onDelta == nil {
		return c.AnswerPrompt(ctx, system, user)
	}
	out, _, err := c.callMessages(ctx, system, []message{{Role: "user", Content: user}}, true, onDelta)
	return out, err
}

// StreamChat runs a multi-turn exchange with a caller-supplied system prompt.
// It is what a feature reaches for when the conversation itself is the point —
// the deck builder's "talk about this deck" — rather than the single grounded
// question AnswerPrompt covers. History is replayed as-is; the final turn is
// the one being answered. onDelta nil reads one JSON body; non-nil asks for
// SSE.
func (c *Client) StreamChat(ctx context.Context, system string, turns []Turn, onDelta func(string) error) (string, error) {
	msgs := chatMessages(turns)
	if len(msgs) == 0 {
		return "", fmt.Errorf("no message to answer")
	}
	out, _, err := c.callMessages(ctx, system, msgs, onDelta != nil, onDelta)
	return out, err
}

// chatMessages normalizes turns into API messages: unknown roles become user
// turns, empty content is dropped, and consecutive same-role turns are folded
// because the API rejects them.
func chatMessages(turns []Turn) []message {
	msgs := make([]message, 0, len(turns))
	for _, t := range turns {
		role := t.Role
		if role != "assistant" {
			role = "user"
		}
		if strings.TrimSpace(t.Content) == "" {
			continue
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content += "\n\n" + t.Content
			continue
		}
		msgs = append(msgs, message{Role: role, Content: t.Content})
	}
	// The exchange must begin with a user turn.
	for len(msgs) > 0 && msgs[0].Role != "user" {
		msgs = msgs[1:]
	}
	return msgs
}

// run builds the Q&A exchange from a Request and sends it.
func (c *Client) run(ctx context.Context, r Request, onDelta func(string) error) (string, error) {
	streaming := onDelta != nil
	out, _, err := c.callMessages(ctx,
		systemPrompt(r.CorpusName, len(r.Cards) > 0, len(r.Entities) > 0, len(r.Rulings) > 0),
		buildMessages(r), streaming, onDelta)
	return out, err
}

// callMessages performs one exchange, walking the provider chain until one
// answers. A provider that fails for a reason the next one might not share
// (exhausted quota, bad key, overload, unreachable host) hands off; a failure
// that would repeat everywhere — a malformed request, a cancelled caller —
// stops the walk and surfaces as-is. Usage is whatever the answering provider
// reported (zero on the streaming path, which does not read it).
//
// Streaming complicates handoff: once the reader has seen text, restarting on
// another provider would splice two half-answers together. So a stream that
// already emitted a delta never fails over, and neither does one whose reader
// went away (a browser closing the connection is not a provider fault).
func (c *Client) callMessages(ctx context.Context, system string, msgs []message, streaming bool, onDelta func(string) error) (string, Usage, error) {
	providers := c.active()
	if len(providers) == 0 {
		return "", Usage{}, ErrNotConfigured
	}

	var lastOut string
	var lastUsage Usage
	var lastErr error
	for i, p := range providers {
		emitted, readerGone := false, false
		delta := onDelta
		if onDelta != nil {
			delta = func(s string) error {
				emitted = true
				if err := onDelta(s); err != nil {
					readerGone = true
					return err
				}
				return nil
			}
		}

		out, usage, err := c.callProvider(ctx, p, system, msgs, streaming, delta)
		if err == nil {
			return out, usage, nil
		}
		lastOut, lastUsage, lastErr = out, usage, err

		last := i == len(providers)-1
		if last || emitted || readerGone || ctx.Err() != nil || !shouldFailOver(err) {
			break
		}
		next := providers[i+1]
		log.Printf("llm: provider %s (%s) failed, falling back to %s (%s): %v",
			hostOf(p.BaseURL), p.Model, hostOf(next.BaseURL), next.Model, err)
	}
	return lastOut, lastUsage, lastErr
}

// callProvider runs one exchange against a single provider. With onDelta nil it
// reads a single JSON body; otherwise it asks for SSE and decodes the event
// stream. Usage is filled on the JSON path; the SSE path does not read the
// usage events and reports zero.
func (c *Client) callProvider(ctx context.Context, cfg Config, system string, msgs []message, streaming bool, onDelta func(string) error) (string, Usage, error) {
	reqBody := messagesRequest{
		Model:     cfg.Model,
		MaxTokens: maxAnswerTokens,
		System:    system,
		Messages:  msgs,
		Stream:    streaming,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", Usage{}, err
	}

	url := strings.TrimRight(cfg.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	// Send the key via both headers for max provider compatibility
	// (Anthropic uses x-api-key; some compatible gateways expect Bearer).
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("authorization", "Bearer "+cfg.APIKey)
	if streaming {
		req.Header.Set("accept", "text/event-stream")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", Usage{}, &apiError{status: resp.StatusCode, statusText: resp.Status, body: string(raw)}
	}
	if streaming {
		out, err := readStream(resp.Body, onDelta)
		return out, Usage{}, err
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", Usage{}, err
	}
	var mr messagesResponse
	if err := json.Unmarshal(raw, &mr); err != nil {
		return "", Usage{}, fmt.Errorf("decode llm response: %w", err)
	}
	var b strings.Builder
	for _, block := range mr.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", Usage{}, fmt.Errorf("llm returned no text")
	}
	return out, mr.Usage, nil
}

// apiError is a non-2xx response from a provider, kept structured so the
// failover decision can read the status and the body.
type apiError struct {
	status     int
	statusText string
	body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("llm request failed: %s: %s", e.statusText, truncate(e.body, 300))
}

// quotaHints are the phrases providers use when the account, not the request,
// is the problem. Anthropic answers an exhausted balance with HTTP 400, which
// would otherwise read as "your request is malformed" and stop the walk — the
// exact case a fallback provider exists for.
var quotaHints = []string{
	"credit balance", "insufficient", "quota", "billing", "payment",
	"exceeded", "rate limit", "rate_limit", "overloaded", "capacity",
	"too many requests", "out of credit", "run out", "no credit",
	"subscription", "suspended",
}

// shouldFailOver reports whether another provider is worth trying. Transport
// and stream failures always are; HTTP statuses are worth it when they describe
// the account or the endpoint (quota, auth, missing model, overload, server
// error) rather than the request itself.
func shouldFailOver(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return true // transport error, truncated stream, empty answer
	}
	switch ae.status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests,
		http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusNotFound:
		return true
	}
	if ae.status >= 500 {
		return true
	}
	lower := strings.ToLower(ae.body)
	for _, hint := range quotaHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// hostOf reduces a base URL to its host for logging, so a failover line names
// the provider without ever printing a key or a path.
func hostOf(base string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return base
	}
	return s
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

	user := buildUserMessage(r.CorpusName, r.Docs, r.Cards, r.Entities, r.Rulings, r.Unresolved, r.Question)
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

func systemPrompt(corpusName string, hasCards bool, hasEntities bool, hasRulings bool) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		`You are the Grimoire, a knowledgeable keeper of %s rules. Answer like a careful judge: precise, grounded, and unmoved by pressure.

GROUNDING RULES — follow these strictly:
1. Answer using ONLY the provided rule excerpts and (if present) the provided card oracle text. Do NOT rely on your own memory of cards or rules for any specific fact.
2. Cite rule numbers (e.g. 205.1a) or section titles that support your answer.`, corpusName)

	// Grounding rules are numbered dynamically: the base pair above, then the
	// layers the question's grounding actually carries.
	rule := 3
	if hasCards {
		fmt.Fprintf(&b, "\n%d. For any card, use ONLY the provided oracle text for its name, mana cost, type, and effects — quote it faithfully. Do not invent or guess mana costs, types, or abilities.", rule)
		rule++
		fmt.Fprintf(&b, "\n%d. If a card is named in the question but its oracle text was NOT provided, say plainly that you could not look it up — do NOT describe what you think it does.", rule)
		rule++
	} else if strings.Contains(corpusName, "Magic") {
		fmt.Fprintf(&b, "\n%d. If a card is named in the question but no card oracle text was provided, say plainly that you could not look the card up — do NOT guess what the card does.", rule)
		rule++
	}
	if hasEntities {
		fmt.Fprintf(&b, "\n%d. For any named entity (spell, creature, magic item, feat, condition, or weapon), use ONLY the provided reference text for its mechanics and stats — quote damage, ranges, durations, and effects faithfully. Do not invent or guess stats or effects from memory.", rule)
		rule++
		fmt.Fprintf(&b, "\n%d. If a named entity is in the question but no reference text was provided for it, say plainly that you could not look it up — do NOT describe what you think it does.", rule)
		rule++
	} else if strings.Contains(corpusName, "D&D") {
		fmt.Fprintf(&b, "\n%d. If a spell, creature, magic item, or feat is named in the question but was not looked up, say plainly that you could not look it up — do NOT describe what you think it does.", rule)
		rule++
	}
	if hasRulings {
		fmt.Fprintf(&b, "\n%d. Official rulings were provided. Cite them when they decide the interaction — name the card, the ruling's source (wotc or scryfall), and its date, and quote the decisive phrase. Treat wotc rulings as authoritative precedent; treat scryfall rulings as official guidance.", rule)
		rule++
		fmt.Fprintf(&b, "\n%d. If a ruling is relevant to a named card but no ruling for that card was provided, say plainly that no official ruling was available — do NOT invent one from memory.", rule)
		rule++
	}
	fmt.Fprintf(&b, "\n%d. If the provided excerpts do not contain the answer, say so plainly rather than inventing anything.", rule)

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

	if strings.Contains(corpusName, "D&D") {
		b.WriteString("\n\nD&D INTERACTIONS — check these common traps before answering:")
		b.WriteString(`
- Specific beats general: a feature or spell does exactly what its text says — nothing more. Two features with the same name don't stack; multiple sources of the same spell on one target don't double up. Apply the most specific rule that speaks to the situation.
- Action economy: on your turn you get one action and at most one bonus action; one reaction per round, taken on someone else's turn. Check whether the text actually grants the action type the question assumes.
- Bonus-action spellcasting: if you cast any spell with a bonus action, the only other spell you can cast that turn is a cantrip with a casting time of one action.
- Concentration: a creature can concentrate on only one spell at a time — casting another concentration spell ends the first. Taking damage while concentrating forces a Constitution saving throw. Check concentration before stacking buffs.
- Attack rolls vs. saving throws: an attack roll hits or misses (AC); a save is made by the target (DC). Read which one the text calls for before answering "does it hit" questions.
- Edition drift: the provided excerpts may mix the 2024 revision with 2014-era books. Each excerpt's header ends with its book in brackets — e.g. "[D&D 5e SRD — classes]" (2024 revision) or "[D&D books — Player's Handbook]" (2014). When two excerpts disagree, ground in the one at hand, name its book, and say plainly that the editions differ rather than blending them.
- Official rulings: excerpts sourced from the Sage Advice Compendium are official Q&A rulings, and their titles are the question asked. Treat them as authoritative interpretation that clarifies the rule text — cite them as precedent when they decide the question, alongside (not instead of) the rules they interpret.`)
	}

	b.WriteString("\n\nKeep answers concise and practical for a player or judge at the table.")
	return strings.TrimSpace(b.String())
}

func buildUserMessage(corpusName string, docs []ContextDoc, cards []CardDoc, entities []EntityDoc, rulings []RulingDoc, unresolved []string, question string) string {
	var b strings.Builder
	b.WriteString("Relevant " + corpusName + " rules:\n\n")
	if len(docs) == 0 {
		b.WriteString("(no directly matching rules found)\n\n")
	}
	for _, d := range docs {
		header := d.Title
		// Each excerpt's header ends with the book it came from, in brackets:
		// the D&D corpus mixes the 2024 SRD with 2014-era books, and the
		// prompt's edition-drift and Sage Advice rules both depend on the model
		// being able to tell one excerpt's provenance from another's.
		//
		// Only MTG-style rule numbers lead the header; D&D path-style record
		// ids are internal anchors, not reader-facing citations.
		if ruleNumRe.MatchString(d.Number) {
			if header != "" {
				header = d.Number + " — " + header
			} else {
				header = d.Number
			}
		}
		if d.Source != "" {
			if header != "" {
				header += " [" + d.Source + "]"
			} else {
				header = "[" + d.Source + "]"
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

	if len(entities) > 0 {
		b.WriteString("Reference entries (authoritative — from Open5e):\n\n")
		for _, e := range entities {
			header := e.Name
			if e.Kind != "" {
				header = e.Name + " (" + e.Kind + ")"
			}
			body := strings.TrimSpace(e.Body)
			if body == "" {
				body = "(no reference text)"
			}
			fmt.Fprintf(&b, "### %s\n%s\n\n", header, truncate(body, 1500))
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
	Usage Usage `json:"usage"`
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

// ruleNumRe matches an MTG-style rule number ("205.1a") — the only number
// form shown to the model as a citation anchor. Path-style D&D record ids
// ("spells/0003/0042.1") are internal and stay out of the prompt.
var ruleNumRe = regexp.MustCompile(`^\d{1,3}(?:\.\d+)+[a-z]?$`)

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
