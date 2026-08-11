// Package cards provides Magic: The Gathering card lookups against the public
// Scryfall API (https://scryfall.com/docs/api). It needs no API key.
//
// Grimoire uses it two ways: a direct lookup endpoint for the UI, and as a
// grounding source for the Q&A chat so the model answers card questions from
// real oracle text instead of fabricating effects.
package cards

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

// DefaultBaseURL is the canonical Scryfall API endpoint.
const DefaultBaseURL = "https://api.scryfall.com"

// minRequestInterval is Scryfall's requested floor between requests
// (50-100ms). We honor 100ms to stay safely under their rate limit.
const minRequestInterval = 100 * time.Millisecond

// requestTimeout caps each Scryfall HTTP call.
const requestTimeout = 8 * time.Second

// Card is a normalized MTG card, sufficient to answer rules questions and to
// render a card in the UI. Multi-faced cards (transform, modal DFCs, etc.)
// expose their faces via Faces; for single-faced cards OracleText is set
// directly.
type Card struct {
	Name        string     `json:"name"`
	ManaCost    string     `json:"mana_cost,omitempty"`
	TypeLine    string     `json:"type_line,omitempty"`
	OracleText  string     `json:"oracle_text,omitempty"`
	Power       string     `json:"power,omitempty"`
	Toughness   string     `json:"toughness,omitempty"`
	Loyalty     string     `json:"loyalty,omitempty"`
	Set         string     `json:"set,omitempty"`
	SetName     string     `json:"set_name,omitempty"`
	ImageURL    string     `json:"image_url,omitempty"`
	ScryfallURI string     `json:"scryfall_uri,omitempty"`
	Faces       []CardFace `json:"faces,omitempty"`
}

// CardFace is one face of a multi-faced card.
type CardFace struct {
	Name       string `json:"name"`
	ManaCost   string `json:"mana_cost,omitempty"`
	TypeLine   string `json:"type_line,omitempty"`
	OracleText string `json:"oracle_text,omitempty"`
	Power      string `json:"power,omitempty"`
	Toughness  string `json:"toughness,omitempty"`
	Loyalty    string `json:"loyalty,omitempty"`
	ImageURL   string `json:"image_url,omitempty"`
}

// ErrNotFound is returned when Scryfall has no unambiguous match for a name.
// Callers should treat this as "no card available" rather than a hard error.
var ErrNotFound = errors.New("card not found")

// Service looks up cards via Scryfall with a small in-memory cache and a
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
	card     *Card
	list     []*Card
	notFound bool
}

// cacheTTL bounds how long a cached entry is trusted.
const cacheTTL = 6 * time.Hour

// New builds a Scryfall-backed card service against the default endpoint.
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

// Lookup finds a single card by fuzzy name (e.g. "Lightning Bolt", "lbolt",
// "lightning bolt"). It returns the best Scryfall match or ErrNotFound when
// the name is missing or ambiguous.
func (s *Service) Lookup(ctx context.Context, name string) (*Card, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return nil, ErrNotFound
	}
	if e, ok := s.cache.Load(key); ok {
		if entry := e.(cacheEntry); time.Since(entry.at) < cacheTTL {
			if entry.notFound {
				return nil, ErrNotFound
			}
			return entry.card, nil
		}
	}

	if err := s.throttle(ctx); err != nil {
		return nil, err
	}
	var raw scryfallCard
	if err := s.getJSON(ctx, "/cards/named", url.Values{"fuzzy": {name}}, &raw); err != nil {
		if errors.Is(err, ErrNotFound) {
			s.cache.Store(key, cacheEntry{at: time.Now(), notFound: true})
		}
		return nil, err
	}
	c := normalizeCard(&raw)
	s.cache.Store(key, cacheEntry{at: time.Now(), card: c})
	return c, nil
}

// Search returns cards matching a Scryfall search query (plain words work;
// Scryfall's query syntax is also accepted). The number of results is capped
// at limit (clamped to 1..20).
func (s *Service) Search(ctx context.Context, query string, limit int) ([]*Card, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, ErrNotFound
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	key := "search:" + strings.ToLower(q) + ":" + fmt.Sprint(limit)
	if e, ok := s.cache.Load(key); ok {
		if entry := e.(cacheEntry); time.Since(entry.at) < cacheTTL {
			if entry.notFound {
				return nil, ErrNotFound
			}
			return entry.list, nil
		}
	}

	if err := s.throttle(ctx); err != nil {
		return nil, err
	}
	var page scryfallList
	params := url.Values{"q": {q}, "order": {"name"}, "unique": {"cards"}}
	if err := s.getJSON(ctx, "/cards/search", params, &page); err != nil {
		if errors.Is(err, ErrNotFound) {
			s.cache.Store(key, cacheEntry{at: time.Now(), notFound: true})
		}
		return nil, err
	}
	out := make([]*Card, 0, limit)
	for i := range page.Data {
		if len(out) >= limit {
			break
		}
		out = append(out, normalizeCard(&page.Data[i]))
	}
	s.cache.Store(key, cacheEntry{at: time.Now(), list: out})
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
// callers can treat "no match" as a normal, non-fatal outcome.
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

// scryfallCard models the subset of Scryfall's card object we use.
type scryfallCard struct {
	Name        string             `json:"name"`
	ManaCost    string             `json:"mana_cost"`
	TypeLine    string             `json:"type_line"`
	OracleText  string             `json:"oracle_text"`
	Power       string             `json:"power"`
	Toughness   string             `json:"toughness"`
	Loyalty     string             `json:"loyalty"`
	Set         string             `json:"set"`
	SetName     string             `json:"set_name"`
	ScryfallURI string             `json:"scryfall_uri"`
	ImageURIs   map[string]string  `json:"image_uris"`
	CardFaces   []scryfallCardFace `json:"card_faces"`
}

type scryfallCardFace struct {
	Name       string            `json:"name"`
	ManaCost   string            `json:"mana_cost"`
	TypeLine   string            `json:"type_line"`
	OracleText string            `json:"oracle_text"`
	Power      string            `json:"power"`
	Toughness  string            `json:"toughness"`
	Loyalty    string            `json:"loyalty"`
	ImageURIs  map[string]string `json:"image_uris"`
}

type scryfallList struct {
	Data []scryfallCard `json:"data"`
}

// normalizeCard converts a Scryfall card object into our Card type,
// flattening multi-faced cards so callers always see real oracle text.
func normalizeCard(c *scryfallCard) *Card {
	out := &Card{
		Name:        c.Name,
		ManaCost:    c.ManaCost,
		TypeLine:    c.TypeLine,
		OracleText:  c.OracleText,
		Power:       c.Power,
		Toughness:   c.Toughness,
		Loyalty:     c.Loyalty,
		Set:         c.Set,
		SetName:     c.SetName,
		ScryfallURI: c.ScryfallURI,
		ImageURL:    imageURL(c.ImageURIs),
	}
	// Double-faced cards carry no top-level oracle text or image; the per-face
	// data is authoritative. Flatten so downstream code (and the LLM) sees the
	// real text for every face.
	if len(c.CardFaces) > 0 && strings.TrimSpace(c.OracleText) == "" {
		var faces []CardFace
		var joined strings.Builder
		for i := range c.CardFaces {
			f := &c.CardFaces[i]
			faces = append(faces, CardFace{
				Name: f.Name, ManaCost: f.ManaCost, TypeLine: f.TypeLine,
				OracleText: f.OracleText, Power: f.Power, Toughness: f.Toughness,
				Loyalty: f.Loyalty, ImageURL: imageURL(f.ImageURIs),
			})
			if strings.TrimSpace(f.OracleText) != "" {
				if joined.Len() > 0 {
					joined.WriteString("\n\n")
				}
				fmt.Fprintf(&joined, "%s\n%s", f.Name, f.OracleText)
			}
		}
		out.Faces = faces
		if out.OracleText == "" {
			out.OracleText = joined.String()
		}
		if out.ImageURL == "" && len(faces) > 0 {
			out.ImageURL = faces[0].ImageURL
		}
	}
	return out
}

// imageURL picks the largest raster image Scryfall offers for a card.
func imageURL(uris map[string]string) string {
	if len(uris) == 0 {
		return ""
	}
	for _, k := range []string{"normal", "large", "small", "png"} {
		if v := uris[k]; v != "" {
			return v
		}
	}
	for _, v := range uris {
		return v
	}
	return ""
}
