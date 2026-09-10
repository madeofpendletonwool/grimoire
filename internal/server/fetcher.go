package server

import (
	"context"
	"fmt"
	"strings"
	"sync"

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

	mu      sync.Mutex
	fetched []index.Result
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

// sources returns the rules fetched mid-answer, as citations.
func (f *ruleFetcher) sources() []searchHit {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return toSources(f.fetched)
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
