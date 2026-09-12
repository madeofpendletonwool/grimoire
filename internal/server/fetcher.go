package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
)

// ruleFetcher serves the lookups the model asks for while it is answering,
// out of the same index that produced its grounding. Nothing it returns comes
// from anywhere else, so a mid-answer lookup widens what the model can reach
// without weakening what "grounded" means.
//
// It also remembers what it handed over. A rule the model went and fetched is
// exactly as much a source as one retrieval happened to rank, and the reader
// is owed the same clickable citation for it.
type ruleFetcher struct {
	store  *index.Store
	corpus data.Corpus
	// cards, when set, is Scryfall — the model may also compose card searches
	// mid-answer. Nil for D&D and for an install without card lookup.
	cards *cards.Service

	mu      sync.Mutex
	fetched []index.Result
	// searches are the Scryfall queries the model ran, kept as citations that
	// link out to the full result list.
	searches []searchHit
}

// lookupResults caps how many rules one search hands back — enough to choose
// from, few enough that a search cannot displace the grounding.
const lookupResults = 8

func (f *ruleFetcher) LookupRule(ctx context.Context, number string) (string, error) {
	num := strings.TrimSpace(number)
	if num == "" {
		return "", fmt.Errorf("no rule number given")
	}

	// A sub-rule read without its parent is routinely unusable, so a numbered
	// lookup returns the whole section it belongs to.
	docs, err := f.store.Section(ctx, f.corpus, num)
	if err != nil {
		return "", err
	}
	if len(docs) == 0 {
		hits, err := f.store.Search(ctx, f.corpus, num, lookupResults)
		if err != nil {
			return "", err
		}
		docs = hits
	}
	if len(docs) == 0 {
		return fmt.Sprintf("No rule numbered %q exists in the index.", num), nil
	}
	f.record(docs)
	return formatRules(docs), nil
}

func (f *ruleFetcher) SearchRules(ctx context.Context, query string) (string, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", fmt.Errorf("no search query given")
	}
	// Retrieve, not Search: this is a natural-language phrase from the model,
	// the same shape as a question, and it deserves the same lenient matching
	// (and the same semantic recall when embeddings are configured).
	docs, err := f.store.Retrieve(ctx, f.corpus, q, lookupResults)
	if err != nil {
		return "", err
	}
	if len(docs) == 0 {
		return fmt.Sprintf("Nothing in the rules index matches %q.", q), nil
	}
	f.record(docs)
	return formatRules(docs), nil
}

// record keeps a fetched rule for the citation strip, skipping repeats so a
// model that looks the same rule up twice does not double the chips.
func (f *ruleFetcher) record(docs []index.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range docs {
		dup := false
		for _, have := range f.fetched {
			if have.Number == d.Number && have.Title == d.Title {
				dup = true
				break
			}
		}
		if !dup {
			f.fetched = append(f.fetched, d)
		}
	}
}

// sources returns the rules fetched and the searches run mid-answer, as
// citations.
func (f *ruleFetcher) sources() []searchHit {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(toSources(f.fetched), f.searches...)
}

// searchCardsShown is how many matches one search hands the model. Enough to
// name the notable ones and see the shape of the list; the total and the link
// carry the rest.
const searchCardsShown = 30

// SearchCards runs the model's Scryfall query and reports the page, the total
// and the link. A rejected query comes back as text, not an error: Scryfall's
// explanation of what was wrong is the most useful thing the model can be
// told, and it is meant to try again, not to apologise.
func (f *ruleFetcher) SearchCards(ctx context.Context, query, unique, order string) (string, error) {
	if f.cards == nil {
		return "", fmt.Errorf("card search is not configured")
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return "", fmt.Errorf("no search query given")
	}
	res, err := f.cards.Query(ctx, q, cards.QueryOptions{Unique: unique, Order: order, Limit: searchCardsShown})
	if err != nil {
		var qe *cards.QueryError
		if !errors.As(err, &qe) {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Scryfall rejected the query `%s`: %s", q, qe.Details)
		for _, w := range qe.Warnings {
			fmt.Fprintf(&b, "\n- %s", w)
		}
		b.WriteString("\nFix the syntax and search again; do not answer from memory.")
		return b.String(), nil
	}
	f.recordSearch(res)
	return formatCardSearch(res), nil
}

// recordSearch keeps one chip per distinct query.
func (f *ruleFetcher) recordSearch(res *cards.QueryResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, have := range f.searches {
		if have.URL == res.URL {
			return
		}
	}
	f.searches = append(f.searches, searchHit{
		Title:  "Scryfall: " + res.Query,
		Body:   fmt.Sprintf("%d cards match", res.Total),
		Source: "scryfall",
		URL:    res.URL,
	})
}

// formatCardSearch renders a search page for the model: the count first, so
// "showing 30 of 412" is understood before the list is read; the link, so it
// is handed on; the warnings, because an ignored term means the list is not
// what the query meant; then one compact line per card.
func formatCardSearch(res *cards.QueryResult) string {
	var b strings.Builder
	switch {
	case res.Total == 0:
		fmt.Fprintf(&b, "No cards match `%s`.", res.Query)
		// Scryfall does not warn about a tag nobody has created; it just
		// matches nothing. Without this the model reads "no such cards" where
		// the truth is "no such tag".
		if strings.Contains(res.Query, "art:") || strings.Contains(res.Query, "atag:") || strings.Contains(res.Query, "otag:") || strings.Contains(res.Query, "function:") {
			b.WriteString(" A tag that matches nothing usually does not exist — retry with a simpler single-word tag, or combine two broader ones.")
		}
	case res.HasMore || len(res.Cards) < res.Total:
		fmt.Fprintf(&b, "%d cards match `%s` (showing the first %d, ordered by %s).", res.Total, res.Query, len(res.Cards), res.Order)
	default:
		fmt.Fprintf(&b, "%d cards match `%s` (all of them, ordered by %s).", res.Total, res.Query, res.Order)
	}
	if res.Total > 0 {
		fmt.Fprintf(&b, "\nFull list: %s", res.URL)
	}
	if len(res.Warnings) > 0 {
		b.WriteString("\nScryfall warnings:")
		for _, w := range res.Warnings {
			fmt.Fprintf(&b, "\n- %s", w)
		}
	}
	for _, c := range res.Cards {
		fmt.Fprintf(&b, "\n- %s", c.Name)
		if c.ManaCost != "" {
			fmt.Fprintf(&b, " — %s", c.ManaCost)
		}
		if c.TypeLine != "" {
			fmt.Fprintf(&b, " — %s", c.TypeLine)
		}
		if c.Set != "" {
			fmt.Fprintf(&b, " (%s)", strings.ToUpper(c.Set))
		}
	}
	return b.String()
}

// formatRules renders rules for the model in the same shape as the grounding
// excerpts, so a fetched rule reads no differently from a retrieved one.
func formatRules(docs []index.Result) string {
	var b strings.Builder
	for _, d := range docs {
		header := d.Number
		if d.Title != "" {
			if header != "" {
				header += " — " + d.Title
			} else {
				header = d.Title
			}
		}
		if d.Source != "" {
			header += " [" + d.Source + "]"
		}
		fmt.Fprintf(&b, "### %s\n%s\n\n", header, d.Body)
	}
	return strings.TrimSpace(b.String())
}
