// Package edhrec is a small client for EDHREC's Next.js data routes: the
// JSON behind edhrec.com's pages, fetched the same way a browser does. There
// is no official API — the routes verified live on 2026-08-18 are
//
//	GET /_next/data/<buildId>/commanders/<slug>.json?commanderName=<slug>
//	GET /_next/data/<buildId>/combos/<slug>.json?colors=<slug>
//	GET /_next/data/<buildId>/average-decks/<slug>.json?commanderName=<slug>
//
// where <buildId> comes from the homepage's __NEXT_DATA__ blob and rotates on
// every EDHREC deploy. Because the surface is unofficial, every call is
// best-effort: responses are cached aggressively on disk, requests are spaced
// ~1s apart as a courtesy, and callers (the deck builder) degrade to the
// local engine on any error rather than failing the request.
package edhrec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the live EDHREC site.
const DefaultBaseURL = "https://edhrec.com"

// DefaultCacheTTL is how long a cached response is trusted before refresh.
// Deck data drifts slowly, so a day is plenty.
const DefaultCacheTTL = 24 * time.Hour

// minRequestInterval spaces requests to the live site — the politeness budget
// for an unofficial surface.
const minRequestInterval = time.Second

// ErrDisabled reports that the client was constructed disabled (feature off).
var ErrDisabled = errors.New("edhrec client disabled")

// ErrNotFound reports a card or commander EDHREC does not know (HTTP 404).
var ErrNotFound = errors.New("edhrec: not found")

// Options configures a Client. Zero values take the defaults; Enabled=false
// produces a client whose every call returns ErrDisabled.
type Options struct {
	BaseURL     string        // default https://edhrec.com
	CacheDir    string        // empty = no disk cache (tests, disabled)
	CacheTTL    time.Duration // default 24h
	MinInterval time.Duration // default 1s; tests shrink it
	Enabled     bool          // feature flag
	HTTPClient  *http.Client  // default shared client
}

// Client fetches EDHREC data through a disk cache and a rate limiter. It is
// safe for concurrent use.
type Client struct {
	base     string
	cacheDir string
	ttl      time.Duration
	hc       *http.Client
	enabled  bool

	mu       sync.Mutex // guards the rate limiter and build-id cache
	interval time.Duration
	lastReq  time.Time

	bidMu sync.Mutex
	bid   string
	bidAt time.Time
}

// New builds a client from options.
func New(opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	if opts.MinInterval <= 0 {
		opts.MinInterval = minRequestInterval
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		base:     strings.TrimRight(opts.BaseURL, "/"),
		cacheDir: opts.CacheDir,
		ttl:      opts.CacheTTL,
		hc:       hc,
		enabled:  opts.Enabled,
		interval: opts.MinInterval,
	}
}

// Enabled reports whether the client will make network calls.
func (c *Client) Enabled() bool { return c != nil && c.enabled }

// CardStat is one card's popularity numbers as EDHREC reports them per
// commander: how many decks contain it and how overrepresented it is there
// versus the format at large (the synergy float, roughly -1..+1).
type CardStat struct {
	Name     string  `json:"name"`
	NumDecks int     `json:"num_decks,omitempty"`
	Synergy  float64 `json:"synergy,omitempty"`
}

// CommanderData is the parsed payload of a commander's page: its themed card
// lists keyed by EDHREC's list tag.
type CommanderData struct {
	Commander string                `json:"commander"`
	Lists     map[string][]CardStat `json:"lists"`
}

// HighSynergy returns the commander's high-synergy list, the strongest local
// signal EDHREC offers — cards overrepresented in this commander's decks.
func (d *CommanderData) HighSynergy() []CardStat { return d.Lists["highsynergycards"] }

// TopCards returns the commander's overall top cards.
func (d *CommanderData) TopCards() []CardStat { return d.Lists["topcards"] }

// Combo is one Commander Spellbook combo as EDHREC lists it: the cards that
// combine and the page title ("Hawkeye's Bow + Seeker of Skybreak").
type Combo struct {
	Title string   `json:"title"`
	Cards []string `json:"cards"`
}

// DeckEntry is one card in an average decklist.
type DeckEntry struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Board string `json:"board"`
}

// Slugify formats a card name as an EDHREC URL slug: lowercase, spaces to
// hyphens, apostrophes and commas removed ("Hawkeye's Bow" →
// "hawkeyes-bow", "Miirym, Sentinel Wyrm" → "miirym-sentinel-wyrm").
func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, "'", "")
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// buildIDRe finds the current Next.js build id in the homepage HTML.
var buildIDRe = regexp.MustCompile(`"buildId":"([^"]+)"`)

// buildID fetches (and caches for an hour) the site's current build id. A
// cached id that stops working is retried once after the cache is cleared, so
// an EDHREC deploy mid-flight cannot wedge the client for an hour.
func (c *Client) buildID(ctx context.Context) (string, error) {
	c.bidMu.Lock()
	if c.bid != "" && time.Since(c.bidAt) < time.Hour {
		id := c.bid
		c.bidMu.Unlock()
		return id, nil
	}
	c.bidMu.Unlock()

	var html string
	err := c.fetch(ctx, "", map[string]string{}, &html)
	if err != nil {
		return "", err
	}
	m := buildIDRe.FindStringSubmatch(html)
	if m == nil {
		return "", fmt.Errorf("edhrec: no buildId in homepage")
	}
	c.bidMu.Lock()
	c.bid, c.bidAt = m[1], time.Now()
	c.bidMu.Unlock()
	return m[1], nil
}

// CommanderData fetches a commander's themed card lists. 404 → ErrNotFound;
// any other failure returns the error for the caller to degrade with.
func (c *Client) CommanderData(ctx context.Context, commander string) (*CommanderData, error) {
	slug := Slugify(commander)
	var payload struct {
		PageProps struct {
			Data struct {
				Container struct {
					JSONDict struct {
						CardLists []struct {
							Tag    string `json:"tag"`
							Header string `json:"header"`
							Cards  []struct {
								Name     string  `json:"name"`
								NumDecks int     `json:"num_decks"`
								Synergy  float64 `json:"synergy"`
							} `json:"cardviews"`
						} `json:"cardlists"`
					} `json:"json_dict"`
				} `json:"container"`
			} `json:"data"`
		} `json:"pageProps"`
	}
	if err := c.fetchRoute(ctx, "commanders", slug, map[string]string{"commanderName": slug}, &payload); err != nil {
		return nil, err
	}
	lists := payload.PageProps.Data.Container.JSONDict.CardLists
	out := &CommanderData{Commander: commander, Lists: map[string][]CardStat{}}
	for _, l := range lists {
		stats := make([]CardStat, 0, len(l.Cards))
		for _, cv := range l.Cards {
			stats = append(stats, CardStat{Name: cv.Name, NumDecks: cv.NumDecks, Synergy: cv.Synergy})
		}
		out.Lists[l.Tag] = stats
	}
	return out, nil
}

// Combos fetches the Commander Spellbook combos featuring a card.
func (c *Client) Combos(ctx context.Context, card string) ([]Combo, error) {
	slug := Slugify(card)
	var payload struct {
		PageProps struct {
			Data struct {
				Container struct {
					JSONDict struct {
						CardLists []struct {
							Header string `json:"header"`
							Cards  []struct {
								Name string `json:"name"`
							} `json:"cardviews"`
						} `json:"cardlists"`
					} `json:"json_dict"`
				} `json:"container"`
			} `json:"data"`
		} `json:"pageProps"`
	}
	if err := c.fetchRoute(ctx, "combos", slug, map[string]string{"colors": slug}, &payload); err != nil {
		return nil, err
	}
	lists := payload.PageProps.Data.Container.JSONDict.CardLists
	out := make([]Combo, 0, len(lists))
	for _, l := range lists {
		combo := Combo{Title: l.Header}
		for _, cv := range l.Cards {
			combo.Cards = append(combo.Cards, cv.Name)
		}
		out = append(out, combo)
	}
	return out, nil
}

// AverageDeck fetches the average decklist for a commander.
func (c *Client) AverageDeck(ctx context.Context, commander string) ([]DeckEntry, error) {
	slug := Slugify(commander)
	var payload struct {
		PageProps struct {
			Data struct {
				Deck struct {
					Cards map[string][][2]any `json:"cards"`
				} `json:"deck"`
			} `json:"data"`
		} `json:"pageProps"`
	}
	if err := c.fetchRoute(ctx, "average-decks", slug, map[string]string{"commanderName": slug}, &payload); err != nil {
		return nil, err
	}
	var out []DeckEntry
	for board, rows := range payload.PageProps.Data.Deck.Cards {
		for _, row := range rows {
			if len(row) != 2 {
				continue
			}
			name, _ := row[0].(string)
			count := 1
			switch v := row[1].(type) {
			case float64:
				count = int(v)
			case string:
				fmt.Sscanf(v, "%d", &count)
			}
			if name == "" || count < 1 {
				continue
			}
			out = append(out, DeckEntry{Name: name, Count: count, Board: board})
		}
	}
	return out, nil
}

// fetchRoute composes a /_next/data URL and loads it through the cache. A
// stale build id (404 with a cached id) clears the cache and retries once.
func (c *Client) fetchRoute(ctx context.Context, section, slug string, params map[string]string, into any) error {
	id, err := c.buildID(ctx)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/_next/data/%s/%s/%s.json", id, section, slug)
	err = c.fetch(ctx, path, params, into)
	if errors.Is(err, ErrNotFound) {
		// The cached build id probably rotated; forget it and try once more.
		c.bidMu.Lock()
		c.bid = ""
		c.bidMu.Unlock()
		id, err = c.buildID(ctx)
		if err != nil {
			return err
		}
		path = fmt.Sprintf("/_next/data/%s/%s/%s.json", id, section, slug)
		err = c.fetch(ctx, path, params, into)
	}
	return err
}

// cacheKey builds the on-disk cache filename for a route. The build id is
// deliberately excluded: deck data outlives deploys, and a new deploy should
// not invalidate yesterday's fetch.
func cacheKey(path string, params map[string]string) string {
	h := fnv32(path + "?" + encodeParams(params))
	return sanitizeFile(path) + fmt.Sprintf("-%08x", h) + ".json"
}

func fnv32(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func encodeParams(params map[string]string) string {
	var b strings.Builder
	for k, v := range params {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(v)
		b.WriteString("&")
	}
	return b.String()
}

func sanitizeFile(path string) string {
	var b strings.Builder
	for _, r := range path {
		switch {
		case 'a' <= r && r <= 'z', '0' <= r && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// fetch loads a URL through the disk cache: fresh cache hit → decode from
// disk; miss → rate-limited network GET, 404 → ErrNotFound, other network
// failure → fall back to a stale cached copy when one exists.
func (c *Client) fetch(ctx context.Context, path string, params map[string]string, into any) error {
	if !c.enabled {
		return ErrDisabled
	}

	url := c.base + path
	if len(params) > 0 {
		url += "?" + strings.TrimSuffix(encodeParams(params), "&")
	}

	var cachePath string
	if c.cacheDir != "" {
		cachePath = filepath.Join(c.cacheDir, cacheKey(path, params))
		if c.readCache(cachePath, c.ttl, into) {
			return nil
		}
	}

	if err := c.throttle(ctx); err != nil {
		return err
	}
	body, err := c.get(ctx, url)
	if err != nil {
		// Network failed; a stale cached copy beats no data.
		if cachePath != "" && errors.Is(err, ErrNotFound) == false {
			if c.readCache(cachePath, 0, into) {
				return nil
			}
		}
		return err
	}
	if cachePath != "" {
		if err := c.writeCache(cachePath, body); err != nil {
			log.Printf("edhrec cache write: %v", err)
		}
	}
	if s, ok := into.(*string); ok {
		*s = string(body)
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("edhrec decode: %w", err)
	}
	return nil
}

// get performs the rate-limited HTTP GET and returns the raw body.
func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "grimoire/1.0 (+https://github.com/madeofpendletonwool/grimoire)")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("edhrec: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// throttle sleeps just long enough to keep requests at least c.interval
// apart. The lock is held across the sleep deliberately — concurrent callers
// queue behind the politeness budget rather than bursting past it.
func (c *Client) throttle(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	wait := c.interval - time.Since(c.lastReq)
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.lastReq = time.Now()
	return nil
}

// readCache decodes a cached file into into when it exists and is younger
// than ttl (ttl 0 accepts any age). Returns false on miss or decode error.
func (c *Client) readCache(path string, ttl time.Duration, into any) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if ttl > 0 && time.Since(info.ModTime()) > ttl {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	// A cached copy was written as the raw body; decode it the same way.
	if s, ok := into.(*string); ok {
		*s = string(raw)
		return true
	}
	return json.Unmarshal(raw, into) == nil
}

// writeCache stores the raw body atomically (tmp + rename).
func (c *Client) writeCache(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
