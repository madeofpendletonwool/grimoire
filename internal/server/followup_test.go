package server

import (
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// The bug this guards: a follow-up used to be retrieved for on its own words.
// "The equipment gives it hexproof" is six words about hexproof, so retrieval
// returned the hexproof rules and nothing about targeting or timing — the half
// of the interaction the earlier turn established — and the model could only
// report that the rule it needed had not been provided.
func TestRetrievalQueryCarriesTheEarlierTurn(t *testing.T) {
	history := []llm.Turn{
		{Role: "user", Content: "Can I equip as a response to an instant that would target the creature?"},
		{Role: "assistant", Content: "Equip is an activated ability, so yes — see 702.6 and 115.1."},
	}
	got := retrievalQuery("The equipment gives it hexproof. That's why I ask", history)

	for _, want := range []string{"hexproof", "equip", "target", "instant"} {
		if !strings.Contains(strings.ToLower(got), want) {
			t.Errorf("retrieval query %q lost %q", got, want)
		}
	}
	// The question still leads, so bm25 ranks its terms first.
	if !strings.HasPrefix(got, "The equipment gives it hexproof") {
		t.Errorf("retrieval query does not lead with the question: %q", got)
	}
}

func TestRetrievalQueryLeavesSelfContainedQuestionsAlone(t *testing.T) {
	history := []llm.Turn{{Role: "user", Content: "How does deathtouch interact with trample?"}}
	q := "When a creature with first strike and deathtouch is blocked by two creatures, how is combat damage assigned?"
	if got := retrievalQuery(q, history); got != q {
		t.Errorf("a self-contained question was widened: %q", got)
	}
}

func TestIsFollowUp(t *testing.T) {
	followUps := []string{
		"The equipment gives it hexproof",
		"What if it were tapped instead?",
		"so it doesn't resolve?",
		"why",
	}
	for _, q := range followUps {
		if !isFollowUp(q) {
			t.Errorf("isFollowUp(%q) = false, want true", q)
		}
	}
	standalone := "When two players each control a permanent with the same triggered ability and one of them has doubled it, how many triggers go on the stack in total?"
	if isFollowUp(standalone) {
		t.Errorf("isFollowUp(%q) = true, want false", standalone)
	}
}

func TestPriorAnswerRules(t *testing.T) {
	history := []llm.Turn{
		{Role: "user", Content: "does equip use the stack?"},
		{Role: "assistant", Content: "Yes — rule 702.6a says so, and 117.7 covers timing. See also 702.6a again."},
		{Role: "user", Content: "and if it has hexproof?"},
	}
	got := priorAnswerRules(history)
	if len(got) != 2 || got[0] != "702.6a" || got[1] != "117.7" {
		t.Errorf("priorAnswerRules = %v, want [702.6a 117.7]", got)
	}
}

func TestTermPhrasesOffersLongestFirst(t *testing.T) {
	got := termPhrases("In response to")
	if len(got) == 0 {
		t.Fatal("no phrases")
	}
	if got[0] != "in response to" {
		t.Errorf("longest phrase should lead, got %q", got[0])
	}
	found := false
	for _, p := range got {
		if p == "response" {
			found = true
		}
	}
	if !found {
		t.Error("single words should still be offered")
	}
}
