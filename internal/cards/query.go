package cards

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Search (cards.go) is shaped for the command palette: a handful of names,
// ordered alphabetically, with every failure folded into "no match". That is
// the wrong shape for a model answering "which cards have pirate ships on
// them?". The model needs to know how many cards matched beyond the ones it
// was shown, needs the reader to be handed a link to the rest, and above all
// needs Scryfall's own words when a query was malformed — "c:4 is not a
// valid colour" is the correction that lets the next attempt succeed, and
// ErrNotFound throws it away. Query is the search Search cannot be.

// SearchSiteURL is the human-facing search page the API mirrors. A result
// links there so a reader can see every match, not just the page the model
// was shown.
const SearchSiteURL = "https://scryfall.com/search"

// QueryOptions shape one Scryfall search. Zero values mean Scryfall's own
// defaults for unique/order, and a modest page for Limit.
type QueryOptions struct {
	// Unique is Scryfall's rollup mode: "cards" (one per name, the default),
	// "art" (one per illustration — the mode for art questions, because a
	// card printed twenty times has been drawn several ways), or "prints".
	Unique string
	// Order is the sort: "name", "released", "cmc", "color", "edhrec"…
	Order string
	// Limit caps how many cards come back; the total match count is reported
	// regardless.
	Limit int
}

// QueryResult is one page of a Scryfall search plus what the model needs to
// describe the whole.
type QueryResult struct {
	Query    string
	Unique   string
	Order    string
	Cards    []*Card
	Total    int
	HasMore  bool
	Warnings []string
	// URL is the scryfall.com search page for this query, for the reader.
	URL string
}

// QueryError is a query Scryfall rejected outright — malformed syntax, an
// unknown keyword — carrying Scryfall's explanation. It is deliberately not
// ErrNotFound: a bad query and an empty result call for different next moves.
type QueryError struct {
	Details  string
	Warnings []string
}

func (e *QueryError) Error() string {
	msg := "scryfall rejected the query: " + e.Details
	if len(e.Warnings) > 0 {
		msg += " (" + strings.Join(e.Warnings, "; ") + ")"
	}
	return msg
}

// queryDefaultLimit is what a model sees when it does not say otherwise, and
// queryMaxLimit caps what one page can carry: Scryfall pages are 175 cards
// and the tool result is truncated downstream, so more is wasted anyway.
const (
	queryDefaultLimit = 30
	queryMaxLimit     = 100
)

// Query runs a Scryfall search in full syntax and returns the first page
// with its total. A query that matches nothing is a result with Total 0 and
// any warnings Scryfall attached (an unknown tag, an ignored clause), not an
// error; a query Scryfall could not parse is a *QueryError.
func (s *Service) Query(ctx context.Context, query string, opts QueryOptions) (*QueryResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, ErrNotFound
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = queryDefaultLimit
	}
	if limit > queryMaxLimit {
		limit = queryMaxLimit
	}
	unique := strings.ToLower(strings.TrimSpace(opts.Unique))
	switch unique {
	case "cards", "art", "prints":
	default:
		unique = "cards"
	}
	order := strings.ToLower(strings.TrimSpace(opts.Order))
	if order == "" {
		order = "name"
	}

	key := "query:" + strings.ToLower(q) + ":" + unique + ":" + order + ":" + fmt.Sprint(limit)
	if e, ok := s.cache.Load(key); ok {
		if entry := e.(cacheEntry); time.Since(entry.at) < cacheTTL && entry.query != nil {
			return entry.query, nil
		}
	}

	if err := s.throttle(ctx); err != nil {
		return nil, err
	}
	params := url.Values{"q": {q}, "unique": {unique}, "order": {order}}
	status, body, err := s.get(ctx, "/cards/search", params)
	if err != nil {
		return nil, err
	}

	res := &QueryResult{Query: q, Unique: unique, Order: order, URL: searchSiteURL(q, unique, order)}
	switch {
	case status == http.StatusNotFound:
		// "Your query didn't match any cards" — Scryfall's zero, and the
		// warnings on it are usually the reason (a tag that does not exist).
		var e scryfallError
		_ = json.Unmarshal(body, &e)
		res.Warnings = e.Warnings
	case status == http.StatusBadRequest:
		var e scryfallError
		_ = json.Unmarshal(body, &e)
		if e.Details == "" {
			e.Details = http.StatusText(status)
		}
		return nil, &QueryError{Details: e.Details, Warnings: e.Warnings}
	case status >= 300:
		var e scryfallError
		_ = json.Unmarshal(body, &e)
		if e.Details == "" {
			e.Details = http.StatusText(status)
		}
		return nil, fmt.Errorf("scryfall: %s", e.Details)
	default:
		var page scryfallList
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode scryfall response: %w", err)
		}
		res.Total = page.TotalCards
		res.HasMore = page.HasMore
		res.Warnings = page.Warnings
		for i := range page.Data {
			if len(res.Cards) >= limit {
				res.HasMore = true
				break
			}
			res.Cards = append(res.Cards, normalizeCard(&page.Data[i]))
		}
		if res.Total < len(res.Cards) {
			res.Total = len(res.Cards)
		}
	}
	s.cache.Store(key, cacheEntry{at: time.Now(), query: res})
	return res, nil
}

// searchSiteURL is the scryfall.com page for a query, with the same rollup
// and order the model saw so the reader's list matches the model's.
func searchSiteURL(q, unique, order string) string {
	params := url.Values{"q": {q}, "unique": {unique}, "order": {order}}
	return SearchSiteURL + "?" + params.Encode()
}

// scryfallError is the body Scryfall sends with a non-2xx status.
type scryfallError struct {
	Details  string   `json:"details"`
	Warnings []string `json:"warnings"`
}

// get issues a GET and returns the status and body undecoded, for callers that
// need to read Scryfall's error bodies rather than collapse them.
func (s *Service) get(ctx context.Context, path string, params url.Values) (int, []byte, error) {
	u := s.baseURL + path
	if encoded := params.Encode(); encoded != "" {
		u += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("user-agent", "grimoire/1.0 (+https://github.com/madeofpendletonwool/grimoire)")
	req.Header.Set("accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("scryfall request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}
