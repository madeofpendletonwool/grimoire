package server

import (
	"context"
	"log"
	"regexp"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// Keyword search finds the rules that share a question's words. It does not
// find the rules that define the question's *terms*, and those are usually the
// ones that decide it: a question about an Equipment granting hexproof is
// decided by 702.6 and 702.11, which a bm25 ranking over a dozen common words
// may never float. The rules index knows those mappings — the glossary states
// them outright ("Equipment ... See rule 301, \"Artifacts,\" and rule 702.6,
// \"Equip\"") — so anchoring resolves every name the question and the resolved
// cards mention and seeds the rules behind them directly.
//
// Anchors join the ranked search hits as seeds rather than replacing them:
// they are precise but narrow, and the cross-reference and section tiers in
// index.Expand grow them out from there.

// anchorPhraseWords is the longest term the phrase sweep will try. The longest
// glossary terms that matter run to four words ("Legend Rule", "In Response
// To", "Phases And Steps").
const anchorPhraseWords = 4

// maxAnchors caps how many anchored rules join the seeds. Anchoring is meant to
// put the defining rules in reach, not to hand the model a glossary.
const maxAnchors = 8

// anchorWordRe splits text into the word tokens phrases are built from. Card
// text is full of punctuation, reminder text and mana symbols; only letters
// carry a term name.
var anchorWordRe = regexp.MustCompile(`[a-z]+`)

// termPhrases returns every 1..anchorPhraseWords word run in the text,
// longest first, so a match on "first strike" is offered ahead of "first".
func termPhrases(text string) []string {
	words := anchorWordRe.FindAllString(strings.ToLower(text), -1)
	if len(words) == 0 {
		return nil
	}
	var out []string
	for n := anchorPhraseWords; n >= 1; n-- {
		for i := 0; i+n <= len(words); i++ {
			out = append(out, strings.Join(words[i:i+n], " "))
		}
	}
	return out
}

// questionRuleRe matches a rule number the asker typed themselves ("what does
// 702.6c actually say"). A number asked about by name is the strongest anchor
// there is.
var questionRuleRe = regexp.MustCompile(`\b\d{1,3}\.\d+[a-z]?\b`)

// anchorNumbers collects the rule numbers worth seeding beyond the ranked
// search: those the question names outright, those the terms in the question
// and in the resolved cards' text resolve to, and those the previous answer
// was already standing on.
func (s *Server) anchorNumbers(ctx context.Context, corpus data.Corpus, question string, cardDocs []llm.CardDoc, history []llm.Turn) []string {
	var out []string
	add := func(nums ...string) {
		for _, n := range nums {
			if n == "" || len(out) >= maxAnchors {
				continue
			}
			for _, have := range out {
				if have == n {
					return
				}
			}
			out = append(out, n)
		}
	}

	add(questionRuleRe.FindAllString(question, -1)...)

	// A card's type line and oracle text name the mechanics the question is
	// really about, whether or not the asker used those words: "Equipment" and
	// "equip" in a type line reach 301 and 702.6 even when the question only
	// says "this thing".
	var text strings.Builder
	text.WriteString(question)
	for _, c := range cardDocs {
		text.WriteString(" " + c.TypeLine + " " + c.OracleText)
	}
	nums, err := s.store.TermRules(ctx, corpus, termPhrases(text.String()))
	if err != nil {
		// Anchoring is an enrichment on top of the ranked search; a failed
		// lookup degrades recall, it never fails the answer.
		log.Printf("rule term anchors: %v", err)
	}
	add(nums...)

	if isFollowUp(question) {
		add(priorAnswerRules(history)...)
	}
	return out
}

// anchorSeeds resolves anchor numbers to real rules, dropping any the corpus
// does not hold — a glossary entry may cite a rule number that no longer
// exists after a CR revision, and a citation to nothing must never reach the
// model as grounding.
func (s *Server) anchorSeeds(ctx context.Context, corpus data.Corpus, numbers []string, have []index.Result) []index.Result {
	if len(numbers) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, r := range have {
		seen[r.Number] = true
	}
	var out []index.Result
	for _, n := range numbers {
		if seen[n] {
			continue
		}
		r, err := s.store.Rule(ctx, corpus, n)
		if err != nil {
			log.Printf("anchor lookup %s: %v", n, err)
			continue
		}
		if r == nil {
			// A bare chapter citation ("rule 115") has no rule of its own;
			// its first rule stands in, and Expand grows the chapter from
			// there.
			if r = s.firstInChapter(ctx, corpus, n); r == nil {
				continue
			}
		}
		seen[r.Number] = true
		r.Anchor = true
		out = append(out, *r)
	}
	return out
}

// firstInChapter returns a chapter's opening rule, so a citation naming only a
// chapter still lands somewhere Expand can grow from. Nil when the chapter has
// no rules or the number is not a bare chapter.
func (s *Server) firstInChapter(ctx context.Context, corpus data.Corpus, chapter string) *index.Result {
	if !bareChapterRe.MatchString(chapter) {
		return nil
	}
	r, err := s.store.Rule(ctx, corpus, chapter+".1")
	if err != nil || r == nil {
		return nil
	}
	r.Anchor = true
	return r
}

// bareChapterRe matches a citation of a whole chapter ("115").
var bareChapterRe = regexp.MustCompile(`^\d{3}$`)
