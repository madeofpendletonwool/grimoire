package data

import (
	"context"
	"sync"
)

// A corpus is added to Grimoire by registering a Definition, not by editing a
// switch. The registry is the single source of truth for which rule systems the
// app knows about: /api/meta lists whatever is registered, an index build
// fetches whatever is registered, and parsing an unknown corpus value falls
// back to the default. MTG and D&D register themselves here at package init; a
// third rule system registers the same way and is picked up with no further
// edits.

// Definition is the declarative description of a registered corpus: the slug it
// is known by, how it is shown to readers, its theme accent, and the fetcher
// that builds its Dataset. EntityResolver and RulingsSource are optional and
// wired by sibling features; they stay nil until then.
type Definition struct {
	Corpus  Corpus  // slug identifier, e.g. "mtg"
	Name    string  // display name, e.g. "Magic: The Gathering"
	Accent  string  // theme accent key, e.g. "mana-blue"
	Fetcher Fetcher // builds this corpus's Dataset during an index build

	// Optional capabilities, wired by sibling issues. A nil resolver/source
	// means the corpus contributes no entity grounding or external rulings.
	EntityResolver EntityResolver // resolves named entities (e.g. cards) to grounding text
	RulingsSource  RulingsSource  // contributes external rulings/FAQ entries
}

// Fetcher builds a corpus's Dataset during an index build. It receives the
// active FetchOptions so a corpus can honor its own source overrides.
type Fetcher func(ctx context.Context, opts FetchOptions) (*Dataset, error)

// EntityResolver resolves named entities (cards, spells, monsters) mentioned in
// a question into grounding text, and reports the names it could not resolve.
// MTG wires Scryfall card lookup here via the entity-resolver sibling issue;
// corpora without a resolver simply contribute nothing.
type EntityResolver interface {
	Resolve(ctx context.Context, question string) (entities []Entity, unresolved []string, err error)
}

// Entity is a single resolved grounding entity (e.g. a card's oracle text). The
// server layer projects these into its response shape; the data layer stays
// neutral about presentation.
type Entity struct {
	Name string // canonical name
	Kind string // "card", "spell", ...
	Body string // oracle / reference text
}

// RulingsSource contributes external rulings or FAQ entries for a corpus beyond
// the indexed rule text. Wired by the rulings-layer sibling issue.
type RulingsSource interface {
	Rulings(ctx context.Context, query string) ([]Ruling, error)
}

// Ruling is a single external ruling entry contributed by a RulingsSource.
type Ruling struct {
	Source string
	Title  string
	Body   string
	URL    string
}

var (
	registryMu sync.RWMutex
	registry   []Definition
)

// Register adds a corpus Definition to the registry. Registrations are kept in
// declaration order so /api/meta and index builds present corpora
// deterministically; the first registered corpus is the default (see Default).
// Register is safe to call concurrently — in practice it runs from package init
// for the built-in corpora. Re-registering a slug replaces its Definition.
func Register(d Definition) {
	registryMu.Lock()
	defer registryMu.Unlock()
	for i, existing := range registry {
		if existing.Corpus == d.Corpus {
			registry[i] = d
			return
		}
	}
	registry = append(registry, d)
}

// SetResolver attaches an EntityResolver to an already-registered corpus in
// place. It lets the server wire runtime-constructed resolvers — which need an
// HTTP client, a card service, or a name dictionary — after package init has
// registered the definitions. A corpus with no resolver is left as-is.
func SetResolver(c Corpus, r EntityResolver) {
	registryMu.Lock()
	defer registryMu.Unlock()
	for i, existing := range registry {
		if existing.Corpus == c {
			registry[i].EntityResolver = r
			return
		}
	}
}

// deregister removes a corpus Definition from the registry. It is intended for
// tests that register a hypothetical corpus and must not leak it to other
// tests; production code never removes a built-in corpus.
func deregister(c Corpus) {
	registryMu.Lock()
	defer registryMu.Unlock()
	for i, existing := range registry {
		if existing.Corpus == c {
			registry = append(registry[:i], registry[i+1:]...)
			return
		}
	}
}

// Registered returns every registered corpus Definition in declaration order.
// The returned slice is a copy, so callers may iterate it freely.
func Registered() []Definition {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Definition, len(registry))
	copy(out, registry)
	return out
}

// Lookup returns the Definition registered under a slug, and ok=false if no
// corpus matches.
func Lookup(c Corpus) (Definition, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	for _, d := range registry {
		if d.Corpus == c {
			return d, true
		}
	}
	return Definition{}, false
}

// Default returns the default corpus — the first registered one — used when a
// request omits or sends an unknown corpus value. Registering MTG first keeps
// the historical default. It returns a zero Definition if nothing is
// registered.
func Default() Definition {
	registered := Registered()
	if len(registered) == 0 {
		return Definition{}
	}
	return registered[0]
}

// init registers the two built-in corpora. MTG is registered first so it
// remains the default corpus for unknown/empty corpus values, matching the
// prior hardcoded behavior.
func init() {
	Register(Definition{
		Corpus:  CorpusMTG,
		Name:    "Magic: The Gathering",
		Accent:  "mana-blue",
		Fetcher: fetchMTGDataset,
	})
	Register(Definition{
		Corpus:  CorpusDND,
		Name:    "D&D 5e SRD",
		Accent:  "dragon-red",
		Fetcher: fetchDNDDataset,
	})
}
