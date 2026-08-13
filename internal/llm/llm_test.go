package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSystemPrompt_NoCards(t *testing.T) {
	got := systemPrompt("Magic: The Gathering", false)
	if !strings.Contains(got, "using ONLY the provided rule excerpts") {
		t.Errorf("system prompt missing ONLY constraint: %q", got)
	}
	if !strings.Contains(got, "could not look the card up") {
		t.Errorf("system prompt should tell the model not to guess cards: %q", got)
	}
}

func TestSystemPrompt_WithCards(t *testing.T) {
	got := systemPrompt("Magic: The Gathering", true)
	if !strings.Contains(got, "use ONLY the provided oracle text") {
		t.Errorf("system prompt missing oracle-text constraint: %q", got)
	}
	if !strings.Contains(got, "could not look it up") {
		t.Errorf("system prompt missing no-oracle fallback: %q", got)
	}
}

func TestSystemPrompt_ReasoningDiscipline(t *testing.T) {
	for _, corpus := range []string{"Magic: The Gathering", "D&D 5e SRD"} {
		got := systemPrompt(corpus, false)
		if !strings.Contains(got, "Do NOT adopt a conclusion asserted by the question") {
			t.Errorf("%s: prompt missing anti-sycophancy directive: %q", corpus, got)
		}
		if !strings.Contains(got, "Do not change a correct answer because the questioner pushes back") {
			t.Errorf("%s: prompt missing hold-firm directive: %q", corpus, got)
		}
	}
}

func TestSystemPrompt_MTGInteractionTraps(t *testing.T) {
	got := systemPrompt("Magic: The Gathering", true)
	if !strings.Contains(got, "a card's own name in its text means") {
		t.Errorf("MTG prompt missing self-reference trap: %q", got)
	}
	if !strings.Contains(got, "refer to that card's controller") {
		t.Errorf("MTG prompt missing controller-resolution trap: %q", got)
	}
	if !strings.Contains(got, "Comprehensive Rule 201.4") {
		t.Errorf("MTG prompt missing self-reference rule citation: %q", got)
	}
}

func TestSystemPrompt_NoMTGTrapsForDND(t *testing.T) {
	got := systemPrompt("D&D 5e SRD", false)
	if strings.Contains(got, "MTG INTERACTIONS") {
		t.Errorf("D&D prompt should not include MTG interaction block: %q", got)
	}
	if strings.Contains(got, "self-reference") || strings.Contains(got, "Controller resolution") {
		t.Errorf("D&D prompt should not include MTG-specific traps: %q", got)
	}
}

func TestBuildUserMessage_RulesAndCards(t *testing.T) {
	docs := []ContextDoc{
		{Number: "702.21", Title: "Ward", Body: "Ward is a triggered ability."},
	}
	cards := []CardDoc{
		{Name: "Lightning Bolt", ManaCost: "{R}", TypeLine: "Instant", OracleText: "Lightning Bolt deals 3 damage to any target."},
	}
	got := buildUserMessage("Magic: The Gathering", docs, cards, nil, "What does Lightning Bolt do?")

	if !strings.Contains(got, "702.21 — Ward") {
		t.Errorf("missing rule header: %q", got)
	}
	if !strings.Contains(got, "Ward is a triggered ability.") {
		t.Errorf("missing rule body: %q", got)
	}
	if !strings.Contains(got, "Card oracle text (authoritative — from Scryfall)") {
		t.Errorf("missing card section header: %q", got)
	}
	if !strings.Contains(got, "Lightning Bolt") || !strings.Contains(got, "deals 3 damage") {
		t.Errorf("missing card oracle text: %q", got)
	}
	if !strings.Contains(got, "Mana cost: {R}") || !strings.Contains(got, "Type: Instant") {
		t.Errorf("missing card mana/type: %q", got)
	}
	if !strings.HasSuffix(got, "Question: What does Lightning Bolt do?") {
		t.Errorf("question not appended: ...%q", got[len(got)-60:])
	}
}

func TestBuildUserMessage_NoCards(t *testing.T) {
	got := buildUserMessage("D&D 5e SRD", nil, nil, nil, "How does stealth work?")
	if strings.Contains(got, "Card oracle text") {
		t.Errorf("card section should be omitted without cards: %q", got)
	}
	if !strings.Contains(got, "(no directly matching rules found)") {
		t.Errorf("empty rules should be noted: %q", got)
	}
}

// newPromptClient points a configured client at a fake Messages endpoint. The
// handler receives the parsed request so a test can assert on the prompt shape.
func newPromptClient(t *testing.T, handler func(req messagesRequest) (status int, body string)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req messagesRequest
		_ = json.Unmarshal(raw, &req)
		status, body := handler(req)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"})
}

func TestAnswerPrompt_SendsCustomSystemAndUser(t *testing.T) {
	var seen messagesRequest
	c := newPromptClient(t, func(req messagesRequest) (int, string) {
		seen = req
		return http.StatusOK, `{"content":[{"type":"text","text":"resolved trace"}]}`
	})

	out, err := c.AnswerPrompt(context.Background(), "RESOLVER SYSTEM", "the board and sequence")
	if err != nil {
		t.Fatalf("AnswerPrompt: %v", err)
	}
	if out != "resolved trace" {
		t.Errorf("answer = %q", out)
	}
	if seen.System != "RESOLVER SYSTEM" {
		t.Errorf("system prompt not forwarded, got %q", seen.System)
	}
	if len(seen.Messages) != 1 || seen.Messages[0].Role != "user" || seen.Messages[0].Content != "the board and sequence" {
		t.Errorf("user message not forwarded, got %+v", seen.Messages)
	}
	if seen.Stream {
		t.Errorf("non-streaming call must not set stream=true")
	}
	if seen.Model != "test-model" {
		t.Errorf("model not forwarded, got %q", seen.Model)
	}
}

func TestStreamPrompt_DecodesDeltas(t *testing.T) {
	// One content_block_delta event carrying the trace, then stream end.
	c := newPromptClient(t, func(req messagesRequest) (int, string) {
		if !req.Stream {
			t.Errorf("streaming call must set stream=true")
		}
		return http.StatusOK, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"step 1 \"}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"cited\"}}\n\n"
	})

	var got strings.Builder
	out, err := c.StreamPrompt(context.Background(), "sys", "user", func(text string) error {
		got.WriteString(text)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamPrompt: %v", err)
	}
	if out != "step 1 cited" {
		t.Errorf("concatenated answer = %q", out)
	}
	if got.String() != "step 1 cited" {
		t.Errorf("deltas = %q", got.String())
	}
}

func TestAnswerPrompt_NotConfigured(t *testing.T) {
	c := New(Config{})
	if _, err := c.AnswerPrompt(context.Background(), "s", "u"); err != ErrNotConfigured {
		t.Errorf("expected ErrNotConfigured, got %v", err)
	}
}
