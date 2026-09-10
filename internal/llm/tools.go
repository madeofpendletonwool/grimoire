package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// A grounded answer is only as good as what retrieval happened to fetch, and
// retrieval runs before the model has read the question. When the model then
// works out that the interaction turns on a rule nobody searched for — the
// rule a cited paragraph points at, the rule the question implies but never
// names — it has, until now, been able only to say so: "the rule that decides
// this isn't in the excerpts I was given." That sentence is a retrieval
// failure written as an apology.
//
// These tools turn it into a lookup. The model can pull an exact rule by
// number or run a fresh search mid-answer, as many as maxToolRounds times, and
// then answer from what it found. Everything it fetches is real rule text from
// the same index, so the grounding guarantee is unchanged: the model gains
// reach, not licence.

// RuleFetcher resolves the lookups a model asks for while answering. The
// implementation binds the corpus and formats the result; this package stays
// ignorant of how rules are stored.
type RuleFetcher interface {
	// LookupRule returns the text of a rule and its sub-rules, or a plain
	// statement that no such rule exists.
	LookupRule(ctx context.Context, number string) (string, error)
	// SearchRules returns the best matching rules for a phrase.
	SearchRules(ctx context.Context, query string) (string, error)
}

// maxToolRounds bounds how many times one answer may go back to the index.
// Three lookups is enough for the deepest real chain — a rule, the rule it
// cites, and a search to confirm — and the last round is asked without tools
// so the model must finish rather than loop.
const maxToolRounds = 4

// maxToolResult truncates one lookup's text. A whole chapter returned verbatim
// would crowd out the grounding the answer already has.
const maxToolResult = 6000

const (
	toolLookupRule  = "lookup_rule"
	toolSearchRules = "search_rules"
)

// ruleTools are the lookups offered to the model.
var ruleTools = []tool{{
	Name: toolLookupRule,
	Description: "Fetch the exact text of a numbered rule and its sub-rules from the rules index. " +
		"Use this whenever an excerpt cites a rule you have not been shown (\"see rule 608.2b\"), " +
		"or when you need to quote a rule you know by number. Prefer it over answering from memory.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"number": map[string]any{
				"type":        "string",
				"description": "The rule number, e.g. \"608.2b\" or \"115\".",
			},
		},
		"required": []string{"number"},
	},
}, {
	Name: toolSearchRules,
	Description: "Search the rules index for a phrase and get back the best matching rules. " +
		"Use this when the rule you need is not in the excerpts and you do not know its number — " +
		"search for the mechanic in the rulebook's own words, e.g. \"target illegal when it resolves\".",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "What to search for, in the rulebook's vocabulary.",
			},
		},
		"required": []string{"query"},
	},
}}

// runWithTools answers a question over several rounds, executing whatever rule
// lookups the model asks for between them.
//
// Text streams to the reader as it arrives in every round, so a model that
// thinks aloud before looking something up is not held back waiting for the
// lookup. The final round is offered no tools at all: without that, a model
// that keeps finding one more rule to check never reaches an answer.
func (c *Client) runWithTools(ctx context.Context, r Request, system string, msgs []message, onDelta func(string) error) (string, error) {
	var answer strings.Builder
	for round := 0; round < maxToolRounds; round++ {
		offer := ruleTools
		if round == maxToolRounds-1 {
			offer = nil
		}
		ex, err := c.exchangeMessages(ctx, system, msgs, offer, onDelta != nil, onDelta)
		if ex.Text != "" {
			if answer.Len() > 0 {
				answer.WriteString("\n\n")
			}
			answer.WriteString(ex.Text)
		}
		if err != nil {
			return answer.String(), err
		}
		if len(ex.Tools) == 0 {
			return answer.String(), nil
		}
		msgs = append(msgs, assistantTurn(ex), c.toolResults(ctx, r, ex.Tools))
	}
	return answer.String(), nil
}

// assistantTurn rebuilds the model's turn as content blocks so the tool
// results that follow can name the requests they answer.
func assistantTurn(ex exchange) message {
	blocks := make([]contentBlock, 0, len(ex.Tools)+1)
	if strings.TrimSpace(ex.Text) != "" {
		blocks = append(blocks, contentBlock{Type: "text", Text: ex.Text})
	}
	for _, t := range ex.Tools {
		blocks = append(blocks, contentBlock{Type: "tool_use", ID: t.ID, Name: t.Name, Input: t.Input})
	}
	return message{Role: "assistant", Content: blocks}
}

// toolResults executes every lookup in one turn and packages the answers. A
// lookup that fails comes back as an error result rather than aborting the
// answer: the model can say what it could not find, which is the honest
// outcome, where a dropped exchange would be no answer at all.
func (c *Client) toolResults(ctx context.Context, r Request, uses []toolUse) message {
	blocks := make([]contentBlock, 0, len(uses))
	for _, u := range uses {
		if r.OnLookup != nil {
			r.OnLookup(u.Name, lookupArg(u))
		}
		text, err := runTool(ctx, r.Fetcher, u)
		if err != nil {
			log.Printf("llm tool %s: %v", u.Name, err)
			blocks = append(blocks, contentBlock{
				Type: "tool_result", ToolUseID: u.ID, IsError: true,
				Content: fmt.Sprintf("The lookup failed: %v. Say so rather than answering from memory.", err),
			})
			continue
		}
		blocks = append(blocks, contentBlock{Type: "tool_result", ToolUseID: u.ID, Content: truncate(text, maxToolResult)})
	}
	return message{Role: "user", Content: blocks}
}

// lookupArg is the human-readable subject of a lookup, for announcing it.
func lookupArg(u toolUse) string {
	var args struct {
		Number string `json:"number"`
		Query  string `json:"query"`
	}
	if len(u.Input) > 0 {
		_ = json.Unmarshal(u.Input, &args)
	}
	if args.Number != "" {
		return args.Number
	}
	return args.Query
}

// runTool dispatches one lookup.
func runTool(ctx context.Context, f RuleFetcher, u toolUse) (string, error) {
	if f == nil {
		return "", fmt.Errorf("no rules index available")
	}
	var args struct {
		Number string `json:"number"`
		Query  string `json:"query"`
	}
	if len(u.Input) > 0 {
		if err := json.Unmarshal(u.Input, &args); err != nil {
			return "", fmt.Errorf("could not read the lookup arguments: %w", err)
		}
	}
	switch u.Name {
	case toolLookupRule:
		if strings.TrimSpace(args.Number) == "" {
			return "", fmt.Errorf("no rule number given")
		}
		return f.LookupRule(ctx, args.Number)
	case toolSearchRules:
		if strings.TrimSpace(args.Query) == "" {
			return "", fmt.Errorf("no search query given")
		}
		return f.SearchRules(ctx, args.Query)
	}
	return "", fmt.Errorf("unknown lookup %q", u.Name)
}
