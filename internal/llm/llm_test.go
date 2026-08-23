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
	got := systemPrompt("Magic: The Gathering", false, false, false)
	if !strings.Contains(got, "using ONLY the provided rule excerpts") {
		t.Errorf("system prompt missing ONLY constraint: %q", got)
	}
	if !strings.Contains(got, "could not look the card up") {
		t.Errorf("system prompt should tell the model not to guess cards: %q", got)
	}
}

func TestSystemPrompt_WithCards(t *testing.T) {
	got := systemPrompt("Magic: The Gathering", true, false, false)
	if !strings.Contains(got, "use ONLY the provided oracle text") {
		t.Errorf("system prompt missing oracle-text constraint: %q", got)
	}
	if !strings.Contains(got, "could not look it up") {
		t.Errorf("system prompt missing no-oracle fallback: %q", got)
	}
}

func TestSystemPrompt_WithRulings(t *testing.T) {
	got := systemPrompt("Magic: The Gathering", true, false, true)
	if !strings.Contains(got, "Official rulings were provided") {
		t.Errorf("rulings prompt missing cite directive: %q", got)
	}
	if !strings.Contains(got, "no official ruling was available") {
		t.Errorf("rulings prompt missing absent-ruling directive: %q", got)
	}
	if !strings.Contains(got, "wotc rulings as authoritative") {
		t.Errorf("rulings prompt should rank wotc as authoritative: %q", got)
	}
}

func TestSystemPrompt_WithoutRulingsMentionsNothing(t *testing.T) {
	got := systemPrompt("Magic: The Gathering", true, false, false)
	if strings.Contains(got, "Official rulings were provided") {
		t.Errorf("non-rulings prompt must not mention rulings: %q", got)
	}
}

func TestSystemPrompt_ReasoningDiscipline(t *testing.T) {
	for _, corpus := range []string{"Magic: The Gathering", "D&D 5e SRD"} {
		got := systemPrompt(corpus, false, false, false)
		if !strings.Contains(got, "Do NOT adopt a conclusion asserted by the question") {
			t.Errorf("%s: prompt missing anti-sycophancy directive: %q", corpus, got)
		}
		if !strings.Contains(got, "Do not change a correct answer because the questioner pushes back") {
			t.Errorf("%s: prompt missing hold-firm directive: %q", corpus, got)
		}
	}
}

func TestSystemPrompt_MTGInteractionTraps(t *testing.T) {
	got := systemPrompt("Magic: The Gathering", true, false, true)
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
	got := systemPrompt("D&D 5e SRD", false, false, false)
	if strings.Contains(got, "MTG INTERACTIONS") {
		t.Errorf("D&D prompt should not include MTG interaction block: %q", got)
	}
	if strings.Contains(got, "self-reference") || strings.Contains(got, "Controller resolution") {
		t.Errorf("D&D prompt should not include MTG-specific traps: %q", got)
	}
}

func TestSystemPrompt_DNDInteractionTraps(t *testing.T) {
	got := systemPrompt("D&D 5e SRD", false, true, false)
	for _, want := range []string{
		"D&D INTERACTIONS",
		"Specific beats general",
		"one action and at most one bonus action",
		"the only other spell you can cast that turn is a cantrip",
		"concentrate on only one spell",
		"Attack rolls vs. saving throws",
		"Sage Advice",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("D&D prompt missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "MTG INTERACTIONS") {
		t.Errorf("D&D prompt must not carry MTG traps: %q", got)
	}
}

func TestSystemPrompt_GroundingRulesNumberedSequentially(t *testing.T) {
	// Entities + rulings together once produced colliding rule numbers (both
	// layers claimed 5-6/6-7). The dynamic numbering must stay sequential.
	got := systemPrompt("D&D 5e SRD", false, true, false)
	if strings.Contains(got, "\n6. ") && strings.Count(got, "\n6. ") > 1 {
		t.Errorf("grounding rule numbers collide: %q", got)
	}
}

func TestSystemPrompt_WithEntities(t *testing.T) {
	got := systemPrompt("D&D 5e SRD", false, true, false)
	if !strings.Contains(got, "use ONLY the provided reference text for its mechanics and stats") {
		t.Errorf("entity prompt missing reference-text constraint: %q", got)
	}
	if !strings.Contains(got, "could not look it up") {
		t.Errorf("entity prompt missing no-reference fallback: %q", got)
	}
}

func TestSystemPrompt_WithoutEntitiesMentionsNothing(t *testing.T) {
	// A D&D turn with no resolved entities must not promise reference text that
	// was not provided, and must not carry the MTG card-grounding clauses.
	got := systemPrompt("D&D 5e SRD", false, false, false)
	if strings.Contains(got, "provided reference text") {
		t.Errorf("non-entity prompt must not mention reference entries: %q", got)
	}
	if strings.Contains(got, "use ONLY the provided oracle text") {
		t.Errorf("D&D prompt must not carry the MTG card-grounding clause: %q", got)
	}
}

func TestBuildUserMessage_RulesAndCards(t *testing.T) {
	docs := []ContextDoc{
		{Number: "702.21", Title: "Ward", Body: "Ward is a triggered ability."},
	}
	cards := []CardDoc{
		{Name: "Lightning Bolt", ManaCost: "{R}", TypeLine: "Instant", OracleText: "Lightning Bolt deals 3 damage to any target."},
	}
	got := buildUserMessage("Magic: The Gathering", docs, cards, nil, nil, nil, "What does Lightning Bolt do?")

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
	got := buildUserMessage("D&D 5e SRD", nil, nil, nil, nil, nil, "How does stealth work?")
	if strings.Contains(got, "Card oracle text") {
		t.Errorf("card section should be omitted without cards: %q", got)
	}
	if !strings.Contains(got, "(no directly matching rules found)") {
		t.Errorf("empty rules should be noted: %q", got)
	}
}

func TestBuildUserMessage_PathNumbersStayOutOfHeaders(t *testing.T) {
	// D&D records carry internal path-style ids; the model sees the title
	// alone, the way unnumbered records always did.
	docs := []ContextDoc{
		{Number: "spells/0003/0042.0", Title: "Spells — Fireball", Body: "A bright streak flashes."},
	}
	got := buildUserMessage("D&D 5e SRD", docs, nil, nil, nil, nil, "What does fireball do?")
	if strings.Contains(got, "spells/0003/0042.0") {
		t.Errorf("internal path id leaked into the prompt: %q", got)
	}
	if !strings.Contains(got, "### Spells — Fireball") {
		t.Errorf("title-only header missing: %q", got)
	}
}

// The D&D corpus mixes the 2024 SRD with 2014-era books, and the prompt asks
// the model to name an excerpt's book and to flag edition conflicts. Neither is
// possible unless every excerpt says where it came from.
func TestBuildUserMessage_HeadersCarryTheSource(t *testing.T) {
	docs := []ContextDoc{
		{Number: "classes/0001/0002.0", Title: "Classes — Barbarian", Body: "Rage.", Source: "D&D 5e SRD — classes"},
		{Number: "player-s-handbook/0007.0", Title: "Player's Handbook — COMBAT", Body: "Attacks.", Source: "D&D books — Player's Handbook"},
	}
	got := buildUserMessage("D&D 5e SRD", docs, nil, nil, nil, nil, "How does rage work?")
	if !strings.Contains(got, "### Classes — Barbarian [D&D 5e SRD — classes]") {
		t.Errorf("SRD source missing from header: %q", got)
	}
	if !strings.Contains(got, "### Player's Handbook — COMBAT [D&D books — Player's Handbook]") {
		t.Errorf("book source missing from header: %q", got)
	}
}

// A corpus whose records carry no source still gets a clean header.
func TestBuildUserMessage_SourcelessHeaderUnchanged(t *testing.T) {
	docs := []ContextDoc{{Number: "205.1a", Title: "Types", Body: "Text."}}
	got := buildUserMessage("Magic: The Gathering", docs, nil, nil, nil, nil, "q")
	if !strings.Contains(got, "### 205.1a — Types\n") {
		t.Errorf("header changed for a sourceless record: %q", got)
	}
}

func TestBuildUserMessage_Entities(t *testing.T) {
	entities := []EntityDoc{{
		Name: "Fireball", Kind: "spell",
		Body: "Level 3 Evocation. Range: 150 feet. A bright streak flashes from you.",
	}}
	got := buildUserMessage("D&D 5e SRD", nil, nil, entities, nil, nil, "What does Fireball do?")
	if !strings.Contains(got, "Reference entries (authoritative — from Open5e)") {
		t.Errorf("missing entity section header: %q", got)
	}
	if !strings.Contains(got, "### Fireball (spell)") {
		t.Errorf("entity header should carry name and kind: %q", got)
	}
	if !strings.Contains(got, "bright streak flashes from you") {
		t.Errorf("missing entity body text: %q", got)
	}
	if strings.Contains(got, "Card oracle text") {
		t.Errorf("card section should be omitted for D&D entities: %q", got)
	}
	if !strings.HasSuffix(got, "Question: What does Fireball do?") {
		t.Errorf("question not appended: ...%q", got[len(got)-60:])
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

func TestAnswerPromptUsage_ReportsTokenUsage(t *testing.T) {
	c := newPromptClient(t, func(req messagesRequest) (int, string) {
		return http.StatusOK, `{"content":[{"type":"text","text":"extraction payload"}],"usage":{"input_tokens":123,"output_tokens":456}}`
	})

	out, usage, err := c.AnswerPromptUsage(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("AnswerPromptUsage: %v", err)
	}
	if out != "extraction payload" {
		t.Errorf("answer = %q", out)
	}
	if usage.InputTokens != 123 || usage.OutputTokens != 456 {
		t.Errorf("usage = %+v, want 123/456", usage)
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

func TestBuildUserMessage_Rulings(t *testing.T) {
	cards := []CardDoc{
		{Name: "Derevi, Empyrial Tactician", TypeLine: "Legendary Creature — Bird Wizard", OracleText: "Flying"},
	}
	rulings := []RulingDoc{
		{CardName: "Derevi, Empyrial Tactician", Source: "wotc", PublishedAt: "2020-11-10", Comment: "You can activate Derevi's last ability only when it is in the command zone."},
		{CardName: "Derevi, Empyrial Tactician", Source: "scryfall", PublishedAt: "2015-01-19", Comment: "Derevi is banned as a commander in Duel Commander."},
	}
	got := buildUserMessage("Magic: The Gathering", nil, cards, nil, rulings, nil, "How does Derevi's tap ability interact with commander tax?")

	if !strings.Contains(got, "Official rulings (authoritative precedent — from Scryfall/Gatherer)") {
		t.Errorf("missing rulings section header: %q", got)
	}
	if !strings.Contains(got, "[wotc, 2020-11-10]") {
		t.Errorf("missing wotc ruling attribution: %q", got)
	}
	if !strings.Contains(got, "command zone") {
		t.Errorf("missing ruling comment text: %q", got)
	}
	if !strings.Contains(got, "[scryfall, 2015-01-19]") {
		t.Errorf("missing scryfall ruling attribution: %q", got)
	}
	// The card appears once in the oracle-text section and once as the grouped
	// rulings header — NOT once per ruling. Two rulings sharing one header is
	// what grouping guarantees; ungrouped would be three occurrences.
	if got, want := strings.Count(got, "### Derevi, Empyrial Tactician"), 2; got != want {
		t.Errorf("rulings should be grouped under one card header, got %d occurrences want %d", got, want)
	}
}

func TestBuildUserMessage_RulingsOmittedWhenAbsent(t *testing.T) {
	got := buildUserMessage("Magic: The Gathering", nil, []CardDoc{{Name: "Bolt"}}, nil, nil, nil, "What does Bolt do?")
	if strings.Contains(got, "Official rulings") {
		t.Errorf("rulings section should be omitted without rulings: %q", got)
	}
}
