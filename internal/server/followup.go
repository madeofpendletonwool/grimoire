package server

import (
	"regexp"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// A follow-up question is retrieved for badly on its own. "The equipment gives
// it hexproof" is six words about hexproof; the rules that decide the
// interaction — targeting, and when a spell's targets are rechecked — are named
// only in the turn before it. Retrieval that sees just the latest message
// therefore hands the model grounding for the wrong half of the question, and
// the model can only report that the rule it needs is missing.
//
// So the retrieval query for a follow-up carries the earlier turns' subject
// matter with it. The current question always leads (bm25 still ranks its terms
// first); prior turns only widen recall.

// followUpMarkers open a turn that continues the previous one rather than
// starting a new subject.
var followUpMarkers = []string{
	"what if", "what about", "and if", "and what", "but what", "but if",
	"so ", "then ", "also ", "does it", "can it", "would it", "does that",
	"why", "how about", "what happens", "it ", "its ", "it's ", "that ", "the ",
	"ok ", "okay ", "wait", "actually",
}

// followUpWords is the token count under which a question is treated as a
// continuation regardless of how it opens. A genuinely self-contained rules
// question names its actors; a short one is leaning on what was already said.
const followUpWords = 14

// isFollowUp reports whether a question should be retrieved with the earlier
// turns' vocabulary attached.
func isFollowUp(question string) bool {
	q := strings.ToLower(strings.TrimSpace(question))
	if q == "" {
		return false
	}
	if len(strings.Fields(q)) < followUpWords {
		return true
	}
	for _, m := range followUpMarkers {
		if strings.HasPrefix(q, m) {
			return true
		}
	}
	return false
}

// priorUserTurns is how many earlier questions a follow-up's retrieval query
// reaches back through. Two covers the common "question → clarification →
// refinement" shape without dragging in an unrelated earlier subject.
const priorUserTurns = 2

// retrievalQuery builds the text retrieval actually searches for. For a
// self-contained question that is the question itself; for a follow-up it is
// the question followed by the recent user turns, so the mechanic under
// discussion stays in the query even when the latest message never names it.
func retrievalQuery(question string, history []llm.Turn) string {
	if len(history) == 0 || !isFollowUp(question) {
		return question
	}
	parts := []string{question}
	used := 0
	for i := len(history) - 1; i >= 0 && used < priorUserTurns; i-- {
		if history[i].Role != "user" {
			continue
		}
		t := strings.TrimSpace(history[i].Content)
		if t == "" || strings.EqualFold(t, strings.TrimSpace(question)) {
			continue
		}
		parts = append(parts, t)
		used++
	}
	return strings.Join(parts, " ")
}

// answerRuleRe matches the MTG rule numbers an earlier answer cited.
var answerRuleRe = regexp.MustCompile(`\b\d{1,3}\.\d+[a-z]?\b`)

// anchorRules is the cap on how many rules from the previous answer are pulled
// back in as seeds. The point is continuity, not re-sending the whole answer.
const anchorRules = 4

// priorAnswerRules returns the rule numbers the most recent assistant turn
// cited. They are re-seeded for a follow-up because the conversation is
// already standing on them: the interaction being refined is anchored to those
// rules, and — once cross-references are followed — they are the shortest path
// to the rule that decides the refinement.
func priorAnswerRules(history []llm.Turn) []string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != "assistant" {
			continue
		}
		var out []string
		seen := map[string]bool{}
		for _, m := range answerRuleRe.FindAllString(history[i].Content, -1) {
			if seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
			if len(out) >= anchorRules {
				break
			}
		}
		return out
	}
	return nil
}
