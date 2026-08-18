package encounter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
)

// Open5e creatures live under /v2/creatures/ on the same public API the D&D
// entity resolver uses. Free, no key.
const (
	open5eMinInterval = 100 * time.Millisecond
	open5eTimeout     = 10 * time.Second
	searchCacheTTL    = 6 * time.Hour
	// searchPageSize bounds one creature query. The SRD has a few hundred
	// creatures; any reasonable name query fits comfortably.
	searchPageSize = 50
	// maxSearchResults caps what the builder's search box shows.
	maxSearchResults = 12
	// prefixFallbackLen is how many leading characters of the squashed query
	// to try when the verbatim query misses — "goblinboss" finds no icontains
	// hit, but "gobli" does, and the client-side squashed-key match then picks
	// "Goblin Boss" out of the page.
	prefixFallbackLen = 5
)

// srdDocKeys are the Open5e document keys that carry SRD creatures, most
// preferred first — the same scoping the entity resolver applies.
var srdDocKeys = []string{"srd-2024", "srd", "srd-2014"}

// MonsterSummary is one search hit: enough to add the monster to an
// encounter. XP is derived from the Monster Manual table, never from the API.
type MonsterSummary struct {
	Name string `json:"name"`
	CR   string `json:"cr"`
	XP   int    `json:"xp"`
	Type string `json:"type,omitempty"`
}

// Bestiary searches Open5e for SRD creatures, with the same cache, throttle,
// and graceful-miss discipline the Scryfall client uses.
type Bestiary struct {
	baseURL string
	http    *http.Client

	mu      sync.Mutex
	lastReq time.Time
	cache   sync.Map // squashed query -> searchCacheEntry
}

type searchCacheEntry struct {
	at    time.Time
	hits  []MonsterSummary
	empty bool
}

// NewBestiary builds a creature search client against the default endpoint.
func NewBestiary() *Bestiary { return NewBestiaryWithBase("") }

// NewBestiaryWithBase builds a client against an alternate endpoint (tests,
// mirrors). An empty base falls back to the default.
func NewBestiaryWithBase(baseURL string) *Bestiary {
	if baseURL == "" {
		baseURL = "https://api.open5e.com"
	}
	return &Bestiary{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: open5eTimeout},
	}
}

// Search finds SRD creatures by name. It tolerates case, spacing, and typo
// noise the way the D&D entity resolver does: the verbatim query first, then
// a prefix fallback whose results are matched client-side on squashed names,
// so "goblin boss", "GoblinBoss", and "gobln boss" all land on the monster.
// An unreachable API returns an error; the caller degrades to a notice.
func (b *Bestiary) Search(ctx context.Context, query string) ([]MonsterSummary, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("empty query")
	}
	key := squash(q)
	if key == "" {
		return nil, fmt.Errorf("empty query")
	}
	if e, ok := b.cache.Load(key); ok {
		if entry := e.(searchCacheEntry); time.Since(entry.at) < searchCacheTTL {
			if entry.empty {
				return nil, nil
			}
			return entry.hits, nil
		}
	}

	hits, err := b.searchOpen5e(ctx, q, key)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		b.cache.Store(key, searchCacheEntry{at: time.Now(), empty: true})
		return nil, nil
	}
	b.cache.Store(key, searchCacheEntry{at: time.Now(), hits: hits})
	return hits, nil
}

// searchOpen5e runs the two query stages: the verbatim name search, then —
// only on a miss — the squashed-prefix fallback.
func (b *Bestiary) searchOpen5e(ctx context.Context, q, key string) ([]MonsterSummary, error) {
	page, err := b.queryCreatures(ctx, q)
	if err != nil {
		return nil, err
	}
	if hits := rankCreatureHits(q, key, page, maxSearchResults); len(hits) > 0 {
		return hits, nil
	}
	// Prefix fallback for squashed-together names ("goblinboss"). Only
	// queries long enough to be distinctive bother.
	if prefix := prefixOf(key, prefixFallbackLen); prefix != "" && prefix != q {
		if page, err = b.queryCreatures(ctx, prefix); err == nil {
			return rankCreatureHits(q, key, page, maxSearchResults), nil
		}
		// A fallback transport failure is not the primary query's problem;
		// report the verbatim result (nothing) rather than an error.
	}
	return nil, nil
}

// queryCreatures fetches one page from /v2/creatures/, scoped to the SRD.
// If the server rejects the document filter, the query retries unfiltered and
// scoping happens client-side — community homebrew never reaches the UI.
func (b *Bestiary) queryCreatures(ctx context.Context, q string) ([]creatureHit, error) {
	fields := "name,challenge_rating,type,document"
	params := url.Values{
		"search":            {q},
		"limit":             {fmt.Sprint(searchPageSize)},
		"ordering":          {"name"},
		"fields":            {fields},
		"document__key__in": {strings.Join(srdDocKeys, ",")},
	}
	page, err := b.getJSON(ctx, "/v2/creatures/", params)
	if err != nil && (errors.Is(err, errOpen5eStatus) || errors.Is(err, errOpen5eDecode)) {
		delete(params, "document__key__in")
		return b.getJSON(ctx, "/v2/creatures/", params)
	}
	return page, err
}

// creatureHit is one row of the creatures listing.
type creatureHit struct {
	Name            string      `json:"name"`
	Document        docRef      `json:"document"`
	ChallengeRating *float64    `json:"challenge_rating"`
	Type            *namedThing `json:"type"`
}

type docRef struct {
	Key string `json:"key"`
}

type namedThing struct {
	Name string `json:"name"`
}

type creaturePage struct {
	Count   int           `json:"count"`
	Results []creatureHit `json:"results"`
}

var (
	errOpen5eStatus = errors.New("open5e status")
	errOpen5eDecode = errors.New("open5e decode")
)

// getJSON issues a GET and decodes the creatures page. Transport and status
// errors are wrapped so a mid-outage search degrades to a notice, not a panic.
func (b *Bestiary) getJSON(ctx context.Context, path string, params url.Values) ([]creatureHit, error) {
	u := b.baseURL + path + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("user-agent", "grimoire/1.0 (+https://github.com/madeofpendletonwool/grimoire)")
	req.Header.Set("accept", "application/json")
	if err := b.throttle(ctx); err != nil {
		return nil, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open5e request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: %s", errOpen5eStatus, resp.Status)
	}
	var page creaturePage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("%w: %v", errOpen5eDecode, err)
	}
	return page.Results, nil
}

// throttle enforces a polite minimum spacing between Open5e requests.
func (b *Bestiary) throttle(ctx context.Context) error {
	b.mu.Lock()
	wait := open5eMinInterval - time.Since(b.lastReq)
	b.mu.Unlock()
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	b.mu.Lock()
	b.lastReq = time.Now()
	b.mu.Unlock()
	return nil
}

// rankCreatureHits scopes hits to SRD documents, dedupes by squashed name
// (keeping the best-ranked document), ranks by how well the name matches the
// query, and caps the list. Tiers: exact squashed-name equality, name starts
// with the query, name contains the query, then a typo-tolerant fuzzy gate —
// the same normalizer discipline the MTG card matcher applies.
func rankCreatureHits(q, key string, hits []creatureHit, limit int) []MonsterSummary {
	type ranked struct {
		sum  MonsterSummary
		tier int
	}
	best := map[string]ranked{}
	for _, h := range hits {
		if srdDocRank(h.Document.Key) >= len(srdDocKeys) {
			continue // community document, not SRD
		}
		nameKey := squash(h.Name)
		if nameKey == "" {
			continue
		}
		cr := ""
		xp := 0
		if h.ChallengeRating != nil {
			cr = CRLabel(*h.ChallengeRating)
			xp, _ = CRXP(cr)
		}
		sum := MonsterSummary{Name: h.Name, CR: cr, XP: xp}
		if h.Type != nil {
			sum.Type = h.Type.Name
		}
		tier := matchTier(q, key, nameKey, h.Name)
		if tier < 0 {
			continue
		}
		if prev, ok := best[nameKey]; !ok || tier < prev.tier {
			best[nameKey] = ranked{sum: sum, tier: tier}
		}
	}
	out := make([]ranked, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].tier != out[j].tier {
			return out[i].tier < out[j].tier
		}
		return out[i].sum.Name < out[j].sum.Name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	sums := make([]MonsterSummary, 0, len(out))
	for _, r := range out {
		sums = append(sums, r.sum)
	}
	return sums
}

// matchTier grades how well a creature name matches the query: 0 exact
// (squashed) equality, 1 name starts with the query, 2 name contains the
// query, 3 typo-tolerant (cards.NameMatches against the original name), -1 no
// credible match.
func matchTier(q, queryKey, nameKey, name string) int {
	switch {
	case nameKey == queryKey:
		return 0
	case strings.HasPrefix(nameKey, queryKey):
		return 1
	case strings.Contains(nameKey, queryKey):
		return 2
	case cards.NameMatches(q, name):
		return 3
	}
	return -1
}

// srdDocRank orders SRD documents by preference; non-SRD ranks last.
func srdDocRank(key string) int {
	for i, k := range srdDocKeys {
		if key == k {
			return i
		}
	}
	return len(srdDocKeys)
}

// squash lowercases and keeps only letters and digits — the same name
// normalizer the cards package and D&D resolver use.
func squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// prefixOf returns the first n characters of a squashed key, or "" when the
// key is too short for a distinctive prefix.
func prefixOf(key string, n int) string {
	if len(key) < n+2 {
		return ""
	}
	runes := []rune(key)
	if len(runes) < n {
		return ""
	}
	return string(runes[:n])
}
