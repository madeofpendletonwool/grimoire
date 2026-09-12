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

// CardSearcher runs a Scryfall search the model composed. The model is the
// translator from a question to Scryfall syntax; this is the executor, and its
// result text must carry Scryfall's warnings and rejections verbatim, because
// those are what the model corrects its next query from.
type CardSearcher interface {
	// SearchCards returns a formatted page of matches for a Scryfall query,
	// with the total match count and a link to the full list. A query
	// Scryfall rejects is a normal result carrying its explanation — the
	// model is meant to fix the query and try again, not give up — so the
	// error is reserved for Scryfall being unreachable.
	SearchCards(ctx context.Context, query, unique, order string) (string, error)
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
	toolSearchCards = "search_cards"
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

// cardSearchTool is the Scryfall search, offered only when the request carries
// a CardSearcher (Magic, with Scryfall configured).
var cardSearchTool = tool{
	Name: toolSearchCards,
	Description: "Search every Magic card with Scryfall's query syntax and get back the matches with a total count and a link to the full list. " +
		"Use this for ANY question about which cards exist or fit a description — lists, counts, \"every card that…\", art contents, name patterns, colour combinations. " +
		"Never answer such questions from memory. If Scryfall rejects or warns about the query, fix the syntax and search again.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "A Scryfall search, e.g. \"c=4\", \"art:pirate art:ship\", \"name:sphere\", \"t:artifact -t:creature o:sacrifice\".",
			},
			"unique": map[string]any{
				"type":        "string",
				"enum":        []string{"cards", "art", "prints"},
				"description": "How to roll up printings: cards (default, one per name), art (one per illustration — use for art questions), prints (every printing).",
			},
			"order": map[string]any{
				"type":        "string",
				"description": "Sort order: name (default), released, cmc, color, rarity, edhrec.",
			},
		},
		"required": []string{"query"},
	},
}

// cardSearchGuide is the syntax the model composes queries in. Models know
// most of Scryfall already; the guide exists for the parts they guess at —
// comparison operators on colours, the art tags, the rollup modes — and for
// the two habits that keep answers honest: report the total, hand over the
// link.
const cardSearchGuide = `CARD SEARCH — search_cards speaks Scryfall syntax. Quick reference:
- Name: bare words match names ("sphere" or name:sphere); !"Exact Name" for one card.
- Text and type: o:"draw a card" oracle text; t:pirate type line; kw:flying keyword.
- Colours: c=4 exactly four colours; c>=uw at least blue and white; c:m multicolour; c:c colourless; id<=bg commander identity within Golgari.
- Numbers: mv>=7 (mana value), pow>=5, tou<=1, year<=1995, rarity:mythic, set:mh3.
- Art and function: art:ship, art:skull, art:dragon are crowd-tagged illustration contents (also atag:); otag:removal (also function:) tags what a card does. Tags are single lowercase words — combine them (art:pirate art:ship) rather than inventing compounds (art:pirate-ship). An unknown tag matches nothing WITHOUT a warning, so a zero result on a tag term means the tag does not exist: retry with a simpler or broader tag.
- Card kinds: is:commander, is:reserved, is:token, is:dfc, is:vanilla, is:permanent, is:spell.
- Negate any term with a leading minus (-t:creature); group with parentheses and OR.
- unique: "art" for art questions (one result per illustration), "cards" otherwise.
When answering: state the total match count, name the cards you were shown, give the Scryfall link for the full list, and say plainly when the search returned nothing rather than filling in from memory. If a result carries warnings, read them — an ignored term means the query did not test what you meant.`

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
		offer := toolsFor(r)
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

// toolsFor is the lookups a request can actually answer: rule tools when it
// has an index behind it, the card search when it has Scryfall.
func toolsFor(r Request) []tool {
	var offer []tool
	if r.Fetcher != nil {
		offer = append(offer, ruleTools...)
	}
	if r.CardSearch != nil {
		offer = append(offer, cardSearchTool)
	}
	return offer
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
		text, err := runTool(ctx, r, u)
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
func runTool(ctx context.Context, r Request, u toolUse) (string, error) {
	var args struct {
		Number string `json:"number"`
		Query  string `json:"query"`
		Unique string `json:"unique"`
		Order  string `json:"order"`
	}
	if len(u.Input) > 0 {
		if err := json.Unmarshal(u.Input, &args); err != nil {
			return "", fmt.Errorf("could not read the lookup arguments: %w", err)
		}
	}
	switch u.Name {
	case toolLookupRule:
		if r.Fetcher == nil {
			return "", fmt.Errorf("no rules index available")
		}
		if strings.TrimSpace(args.Number) == "" {
			return "", fmt.Errorf("no rule number given")
		}
		return r.Fetcher.LookupRule(ctx, args.Number)
	case toolSearchRules:
		if r.Fetcher == nil {
			return "", fmt.Errorf("no rules index available")
		}
		if strings.TrimSpace(args.Query) == "" {
			return "", fmt.Errorf("no search query given")
		}
		return r.Fetcher.SearchRules(ctx, args.Query)
	case toolSearchCards:
		if r.CardSearch == nil {
			return "", fmt.Errorf("no card search available")
		}
		if strings.TrimSpace(args.Query) == "" {
			return "", fmt.Errorf("no search query given")
		}
		return r.CardSearch.SearchCards(ctx, args.Query, args.Unique, args.Order)
	}
	return "", fmt.Errorf("unknown lookup %q", u.Name)
}
