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
