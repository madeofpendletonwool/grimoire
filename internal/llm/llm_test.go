package llm

import (
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
	got := buildUserMessage("Magic: The Gathering", docs, cards, "What does Lightning Bolt do?")

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
	got := buildUserMessage("D&D 5e SRD", nil, nil, "How does stealth work?")
	if strings.Contains(got, "Card oracle text") {
		t.Errorf("card section should be omitted without cards: %q", got)
	}
	if !strings.Contains(got, "(no directly matching rules found)") {
		t.Errorf("empty rules should be noted: %q", got)
	}
}
