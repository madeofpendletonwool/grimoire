// Package rulings fetches official MTG rulings for a card from the public
// Scryfall API (https://scryfall.com/docs/api/rulings). It needs no API key.
//
// Rulings are Gatherer/Oracle rulings, Wizards release notes, or Scryfall
// notes — the precedent layer that turns a rules lookup into a rulings oracle.
// Grimoire feeds them to the Q&A model alongside card oracle text and rule
// excerpts so answers can cite official rulings, not just rule text.
//
// Scryfall exposes rulings by card id (GET /cards/{id}/rulings), not by name,
// so a name lookup is resolved to an id first (GET /cards/named). Both calls
// are cached and rate-limited, reusing the same pattern as internal/cards.
package rulings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the canonical Scryfall API endpoint. It mirrors
// cards.DefaultBaseURL so a single SCRYFALL_BASE_URL override retargets both.
const DefaultBaseURL = "https://api.scryfall.com"

// minRequestInterval is Scryfall's requested floor between requests
// (50-100ms). We honor 100ms to stay safely under their rate limit, matching
// internal/cards.
const minRequestInterval = 100 * time.Millisecond

// requestTimeout caps each Scryfall HTTP call.
const requestTimeout = 8 * time.Second

// ErrNotFound is returned when Scryfall has no card or no rulings for a name.
// Callers should treat this as "no rulings available" rather than a hard error.
var ErrNotFound = errors.New("rulings not found")

// Ruling is one official ruling on a card. Source is "wotc" (Gatherer/release
// notes) or "scryfall" (Scryfall-authored notes). PublishedAt is the Scryfall
// date (YYYY-MM-DD).
type Ruling struct {
	Object      string `json:"object"`
	OracleID    string `json:"oracle_id"`
	Source      string `json:"source"`
	PublishedAt string `json:"published_at"`
	Comment     string `json:"comment"`
}

// Service fetches rulings via Scryfall with a small in-memory cache and a
// polite rate-limit so we never hammer the public API.
type Service struct {
	baseURL string
	http    *http.Client

	mu      sync.Mutex
	lastReq time.Time
	cache   sync.Map // string -> cacheEntry
}

type cacheEntry struct {
	at       time.Time
	id       string // resolved Scryfall card id for a name
	rulings  []Ruling
	notFound bool
}

// cacheTTL bounds how long a cached entry is trusted. Rulings rarely change,
// so this matches the cards cache.
const cacheTTL = 6 * time.Hour

// New builds a Scryfall-backed rulings service against the default endpoint.
func New() *Service { return NewWithBase(DefaultBaseURL) }

// NewWithBase builds a service against an alternate endpoint (for tests or a
// Scryfall mirror). An empty base falls back to the default.
func NewWithBase(baseURL string) *Service {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Service{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: requestTimeout},
	}
}

// Fetch returns the official rulings for a card by name. It resolves the name
// to a Scryfall card id, then fetches /cards/{id}/rulings. Both steps are
// cached and rate-limited. An empty name, a missing card, or a card with no
// rulings yields ErrNotFound.
func (s *Service) Fetch(ctx context.Context, cardName string) ([]Ruling, error) {
	key := strings.ToLower(strings.TrimSpace(cardName))
	if key == "" {
		return nil, ErrNotFound
	}
	if e, ok := s.cache.Load(key); ok {
		if entry := e.(cacheEntry); time.Since(entry.at) < cacheTTL {
			if entry.notFound {
				return nil, ErrNotFound
			}
			return entry.rulings, nil
		}
	}

	id, err := s.resolveCardID(ctx, cardName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.cache.Store(key, cacheEntry{at: time.Now(), notFound: true})
		}
		return nil, err
	}
	rulings, err := s.fetchByCardID(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// A card with no rulings is a normal, empty outcome — cache it
			// as an empty (not notFound) list so the next ask reuses it.
			s.cache.Store(key, cacheEntry{at: time.Now(), rulings: nil})
			return nil, nil
		}
		return nil, err
	}
	s.cache.Store(key, cacheEntry{at: time.Now(), id: id, rulings: rulings})
	return rulings, nil
}

// resolveCardID turns a fuzzy card name into a Scryfall card id. The id is the
// key the rulings endpoint needs, since Scryfall has no name-keyed rulings
// route.
func (s *Service) resolveCardID(ctx context.Context, cardName string) (string, error) {
	if err := s.throttle(ctx); err != nil {
		return "", err
	}
	var raw struct {
		ID string `json:"id"`
	}
	if err := s.getJSON(ctx, "/cards/named", url.Values{"fuzzy": {cardName}}, &raw); err != nil {
		return "", err
	}
	if raw.ID == "" {
		return "", ErrNotFound
	}
	return raw.ID, nil
}

// fetchByCardID returns the rulings list for a resolved Scryfall card id.
func (s *Service) fetchByCardID(ctx context.Context, id string) ([]Ruling, error) {
	if err := s.throttle(ctx); err != nil {
		return nil, err
	}
	var page scryfallList
	if err := s.getJSON(ctx, "/cards/"+url.PathEscape(id)+"/rulings", nil, &page); err != nil {
		return nil, err
	}
	out := make([]Ruling, 0, len(page.Data))
	out = append(out, page.Data...)
	return out, nil
}

// throttle enforces Scryfall's minimum spacing between requests.
func (s *Service) throttle(ctx context.Context) error {
	s.mu.Lock()
	wait := minRequestInterval - time.Since(s.lastReq)
	s.mu.Unlock()
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	s.mu.Lock()
	s.lastReq = time.Now()
	s.mu.Unlock()
	return nil
}

// getJSON issues a GET and decodes JSON into dst. A 404 maps to ErrNotFound so
// callers can treat "no card / no rulings" as a normal, non-fatal outcome.
func (s *Service) getJSON(ctx context.Context, path string, params url.Values, dst any) error {
	u := s.baseURL + path
	if encoded := params.Encode(); encoded != "" {
		u += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("user-agent", "grimoire/1.0 (+https://github.com/madeofpendletonwool/grimoire)")
	req.Header.Set("accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("scryfall request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Details string `json:"details"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Details == "" {
			e.Details = resp.Status
		}
		// Scryfall returns 404 for not-found/ambiguous; a 400 with "ambiguous"
		// is also effectively "no single match".
		if resp.StatusCode == http.StatusBadRequest {
			return ErrNotFound
		}
		return fmt.Errorf("scryfall: %s", e.Details)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode scryfall response: %w", err)
	}
	return nil
}

type scryfallList struct {
	Data []Ruling `json:"data"`
}
