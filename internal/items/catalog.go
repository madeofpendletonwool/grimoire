package items

// The local magic-item catalog (MAD-383). The encounter builder works
// because the bestiary mirrors the whole SRD shelf into SQLite — "choosing
// what would fit here needs the whole shelf at once". Items had no
// equivalent: the app held only a name list for the FTS dictionary and the
// item text as prose reading pages. This mirrors them, with the pattern
// the bestiary proved: a `magic_items` table in the shared database,
// replaced wholesale on sync, held in memory for filtering, refreshed when
// missing or stale — with the same background-refresh-on-cold-start
// behaviour, so first use never blocks.
//
// Everything derived here is deterministic and explainable: a tag is
// present because the item's type or text says so, a charge count because
// the text states it. Nothing downstream invents rarity, power or price
// from a guess.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
)

// Errors the HTTP surface maps onto statuses, the encounter package's own
// rule: not-found stays 404 (a foreign id is indistinguishable from a
// missing one), invalid input is a 400.
var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
)

const (
	// catalogPageSize is how many items one listing request pulls. The SRD
	// carries a few hundred magic items, so the whole mirror is a handful
	// of calls.
	catalogPageSize = 200
	// catalogMaxPages bounds a sync against a mirror that paginates forever.
	catalogMaxPages = 20
	// catalogTTL is how long a mirrored catalog is trusted before a refresh
	// is attempted. The SRD does not change; this only catches Open5e
	// corrections and a sync that stored a partial shelf.
	catalogTTL = 30 * 24 * time.Hour
	// catalogSyncTimeout caps a whole mirror pass.
	catalogSyncTimeout = 3 * time.Minute
	// catalogSchemaVersion is the version of the mirrored item JSON shape.
	// It rises whenever Item grows fields an older mirror would be missing;
	// a mirror written under a lower version reports stale, so the next
	// EnsureFresh re-syncs once and fills the new fields rather than
	// serving items with empty halves.
	catalogSchemaVersion = 1
)

// Item is one SRD magic item, flattened to what a designer and a loot
// table need. Charges and Recharge are parsed from the item's own text —
// the API carries no structured charge field — so a count of charges here
// is a claim the printed item actually makes.
type Item struct {
	Slug                string `json:"slug"`
	Name                string `json:"name"`
	Doc                 string `json:"doc"`
	Type                string `json:"type,omitempty"`   // the SRD category: "Weapon", "Potion", "Wondrous Item"...
	Rarity              string `json:"rarity,omitempty"` // "Common".."Legendary", as the corpus carries it
	RarityRank          int    `json:"rarity_rank,omitempty"`
	RequiresAttunement  bool   `json:"requires_attunement"`
	AttunementCondition string `json:"attunement_condition,omitempty"`
	// Base is the base weapon or armor key the item is built on, when the
	// corpus carries one — the "+1 weapon" half of "a +1 weapon".
	Base     string   `json:"base,omitempty"`
	Charges  int      `json:"charges,omitempty"`  // parsed from the text
	Recharge string   `json:"recharge,omitempty"` // parsed from the text, e.g. "1d6+1 daily at dawn"
	Text     string   `json:"text,omitempty"`
	Tags     []string `json:"tags,omitempty"`

	// Homebrew marks an item that came from a DM's or a campaign's own
	// designs (homebrew_items) rather than the SRD mirror. It rides the
	// JSON so every surface that renders or lists the item can keep the
	// label visible — a homebrew item is never presented as SRD.
	Homebrew bool `json:"homebrew,omitempty"`
}

// Line renders the item as one compact picker line: everything a choice
// needs, nothing that would bury forty of them.
func (i Item) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", i.Name)
	parts := []string{}
	if i.Type != "" {
		parts = append(parts, i.Type)
	}
	if i.Rarity != "" {
		parts = append(parts, i.Rarity)
	}
	if i.RequiresAttunement {
		att := "attunement"
		if i.AttunementCondition != "" {
			att += " (" + i.AttunementCondition + ")"
		}
		parts = append(parts, att)
	}
	if len(parts) > 0 {
		fmt.Fprintf(&b, " | %s", strings.Join(parts, ", "))
	}
	if len(i.Tags) > 0 {
		fmt.Fprintf(&b, " | %s", strings.Join(i.Tags, ", "))
	}
	return b.String()
}

// Catalog is the mirrored SRD item shelf: persisted in the shared
// database, held in memory for filtering, refreshed from Open5e when
// stale.
type Catalog struct {
	db      *sql.DB
	baseURL string
	http    *http.Client

	mu            sync.RWMutex
	list          []Item
	byKey         map[string]int // squashed name -> index into list
	syncedAt      time.Time
	schemaVersion int

	syncing sync.Mutex
}

// NewCatalog builds a catalog over the shared database and ensures its
// table exists. It does not touch the network: call Load, then Sync (or
// EnsureFresh) when a refresh is wanted.
func NewCatalog(db *sql.DB, baseURL string) (*Catalog, error) {
	if baseURL == "" {
		baseURL = "https://api.open5e.com"
	}
	c := &Catalog{
		db:      db,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: catalogSyncTimeout},
		byKey:   map[string]int{},
	}
	if _, err := db.Exec(catalogSchema); err != nil {
		return nil, fmt.Errorf("magic item catalog migrate: %w", err)
	}
	return c, nil
}

// catalogSchema is the mirror's cache DDL — the exact shape migration 0028
// declares, kept in step by the schema-compat test. The mirror is a cache,
// not owned data: Sync replaces its contents wholesale.
const catalogSchema = `
CREATE TABLE IF NOT EXISTS magic_items (
	key         TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	rarity      TEXT NOT NULL DEFAULT '',
	rarity_rank INTEGER NOT NULL DEFAULT 0,
	category    TEXT NOT NULL DEFAULT '',
	data        TEXT NOT NULL,
	synced_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS magic_items_rarity ON magic_items(rarity_rank);
CREATE TABLE IF NOT EXISTS magic_items_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// Count reports how many items the catalog currently holds.
func (c *Catalog) Count() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.list)
}

// SyncedAt reports when the mirror was last written.
func (c *Catalog) SyncedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.syncedAt
}

// Load reads the mirrored catalog out of the database into memory. An
// empty table is not an error — the catalog simply reports zero items
// until a sync fills it. A mirror written before the current schema
// version loads too, but reports stale, so EnsureFresh re-syncs it once.
func (c *Catalog) Load() error {
	version, err := c.readSchemaVersion()
	if err != nil {
		return err
	}
	rows, err := c.db.Query(`SELECT data, synced_at FROM magic_items ORDER BY name`)
	if err != nil {
		return fmt.Errorf("load magic items: %w", err)
	}
	defer rows.Close()

	var list []Item
	var newest int64
	for rows.Next() {
		var blob string
		var syncedAt int64
		if err := rows.Scan(&blob, &syncedAt); err != nil {
			return err
		}
		var it Item
		if err := json.Unmarshal([]byte(blob), &it); err != nil {
			continue // a corrupt row is dropped, not fatal: the next sync rewrites it
		}
		if syncedAt > newest {
			newest = syncedAt
		}
		list = append(list, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.schemaVersion = version
	c.mu.Unlock()
	c.replace(list, time.UnixMilli(newest).UTC())
	return nil
}

// readSchemaVersion reports the schema version the stored mirror was
// written under, 0 when nothing is stored (an empty database needs a sync
// regardless).
func (c *Catalog) readSchemaVersion() (int, error) {
	var value string
	err := c.db.QueryRow(`SELECT value FROM magic_items_meta WHERE key = 'schema_version'`).Scan(&value)
	switch {
	case err == sql.ErrNoRows:
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("magic item schema version: %w", err)
	}
	v, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || v < 0 {
		return 0, nil // an unreadable version is the oldest version
	}
	return v, nil
}

// replace swaps in a freshly loaded shelf under the write lock.
func (c *Catalog) replace(list []Item, syncedAt time.Time) {
	index := make(map[string]int, len(list))
	for i, it := range list {
		index[squash(it.Name)] = i
	}
	c.mu.Lock()
	c.list, c.byKey, c.syncedAt = list, index, syncedAt
	c.mu.Unlock()
}

// Stale reports whether the mirror is missing, old enough to re-fetch, or
// written under an older schema.
func (c *Catalog) Stale() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.list) == 0 || c.schemaVersion < catalogSchemaVersion || time.Since(c.syncedAt) > catalogTTL
}

// EnsureFresh syncs when the mirror is missing or stale, and is a no-op
// otherwise. A failed sync leaves whatever is already mirrored in place —
// a shelf from last month beats no shelf at all.
func (c *Catalog) EnsureFresh(ctx context.Context) error {
	if c == nil || !c.Stale() {
		return nil
	}
	return c.Sync(ctx)
}

// Sync mirrors every SRD magic item from Open5e into the database and
// reloads memory. Concurrent calls serialize; a partial fetch is discarded
// rather than written, so the table never holds half a shelf.
func (c *Catalog) Sync(ctx context.Context) error {
	c.syncing.Lock()
	defer c.syncing.Unlock()

	ctx, cancel := context.WithTimeout(ctx, catalogSyncTimeout)
	defer cancel()

	fetched, err := c.fetchAll(ctx)
	if err != nil {
		return err
	}
	if len(fetched) == 0 {
		return fmt.Errorf("magic item sync: open5e returned no SRD items")
	}

	now := time.Now().UTC()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("magic item sync: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM magic_items`); err != nil {
		return fmt.Errorf("magic item sync: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO magic_items (key, name, rarity, rarity_rank, category, data, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("magic item sync: %w", err)
	}
	defer stmt.Close()
	for _, it := range fetched {
		blob, err := json.Marshal(it)
		if err != nil {
			continue
		}
		if _, err := stmt.ExecContext(ctx, it.Slug, it.Name, it.Rarity, it.RarityRank, it.Type, string(blob), now.UnixMilli()); err != nil {
			return fmt.Errorf("magic item sync: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("magic item sync: %w", err)
	}
	// The schema version lands right after the mirror commit. A crash
	// between the two leaves rows stamped one version back — which only
	// costs one extra re-sync, the same safety valve an unknown version
	// rides.
	if _, err := c.db.Exec(`
		INSERT INTO magic_items_meta (key, value) VALUES ('schema_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(catalogSchemaVersion)); err != nil {
		return fmt.Errorf("magic item sync: %w", err)
	}

	c.replace(fetched, now)
	c.mu.Lock()
	c.schemaVersion = catalogSchemaVersion
	c.mu.Unlock()
	return nil
}

// fetchAll pages through /v2/magicitems/ scoped to the SRD documents,
// keeping the best-ranked document's version of each name.
func (c *Catalog) fetchAll(ctx context.Context) ([]Item, error) {
	best := map[string]itemRecord{}
	next := ""
	for page := 0; page < catalogMaxPages; page++ {
		var raw itemRecordPage
		var err error
		if next == "" {
			params := url.Values{
				"limit":             {fmt.Sprint(catalogPageSize)},
				"ordering":          {"name"},
				"document__key__in": {strings.Join(srdDocKeys, ",")},
			}
			raw, err = c.getPage(ctx, c.baseURL+"/v2/magicitems/?"+params.Encode())
		} else {
			raw, err = c.getPage(ctx, next)
		}
		if err != nil {
			return nil, err
		}
		for _, rec := range raw.Results {
			key := squash(rec.Name)
			if key == "" {
				continue
			}
			rank := srdDocRank(rec.Document.Key)
			if rank >= len(srdDocKeys) {
				continue // community document, not SRD
			}
			if prev, ok := best[key]; ok && srdDocRank(prev.Document.Key) <= rank {
				continue
			}
			best[key] = rec
		}
		if raw.Next == "" {
			break
		}
		next = raw.Next
	}

	out := make([]Item, 0, len(best))
	for _, rec := range best {
		if it, ok := rec.toItem(); ok {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Catalog) getPage(ctx context.Context, u string) (itemRecordPage, error) {
	var page itemRecordPage
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return page, err
	}
	req.Header.Set("user-agent", "grimoire/1.0 (+https://github.com/madeofpendletonwool/grimoire)")
	req.Header.Set("accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return page, fmt.Errorf("magic item fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return page, err
	}
	if resp.StatusCode >= 300 {
		return page, fmt.Errorf("magic item fetch: %s", resp.Status)
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return page, fmt.Errorf("magic item decode: %w", err)
	}
	return page, nil
}

/* ---------- Open5e wire shapes ---------- */

type itemRecordPage struct {
	Next    string       `json:"next"`
	Results []itemRecord `json:"results"`
}

// itemRecord is one /v2/magicitems/ row. Charges are not a field — they
// live in the printed text and are parsed, not trusted.
type itemRecord struct {
	Key      string  `json:"key"`
	Name     string  `json:"name"`
	Desc     string  `json:"desc"`
	Document docRef  `json:"document"`
	Category *ref    `json:"category"`
	Rarity   *rarity `json:"rarity"`

	RequiresAttunement bool    `json:"requires_attunement"`
	AttunementDetail   *string `json:"attunement_detail"`
	Weapon             *ref    `json:"weapon"`
	Armor              *ref    `json:"armor"`
}

type docRef struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type ref struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type rarity struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Rank int    `json:"rank"`
}

// srdDocKeys are the Open5e document keys that carry SRD items, most
// preferred first — the same scoping the bestiary mirror applies.
var srdDocKeys = []string{"srd-2024", "srd", "srd-2014"}

// srdDocRank reports where a document key sits in the preference order,
// len(srdDocKeys) when it is not SRD at all.
func srdDocRank(key string) int {
	for i, k := range srdDocKeys {
		if k == key {
			return i
		}
	}
	return len(srdDocKeys)
}

// toItem flattens one API record. An item with no name cannot be looked
// up, keyed or compared, and is skipped; an item without rarity is kept —
// the designer's bands simply don't count it.
func (rec itemRecord) toItem() (Item, bool) {
	name := strings.TrimSpace(rec.Name)
	if name == "" {
		return Item{}, false
	}
	it := Item{
		Slug: rec.Key, Name: name, Doc: rec.Document.Key,
		Text: strings.TrimSpace(rec.Desc),
	}
	if rec.Category != nil {
		it.Type = strings.TrimSpace(rec.Category.Name)
	}
	if rec.Rarity != nil {
		it.Rarity = strings.TrimSpace(rec.Rarity.Name)
		it.RarityRank = rec.Rarity.Rank
	}
	it.RequiresAttunement = rec.RequiresAttunement
	if rec.AttunementDetail != nil {
		it.AttunementCondition = strings.TrimSpace(*rec.AttunementDetail)
	}
	// Where a name collides with its base item ("Weapon +1" built on the
	// generic weapon), the base is still worth carrying: the designer's
	// nearest-neighbour read and the rarity bands both use it.
	if rec.Weapon != nil {
		it.Base = rec.Weapon.Key
	} else if rec.Armor != nil {
		it.Base = rec.Armor.Key
	}
	it.Charges, it.Recharge = parseCharges(it.Text)
	it.Tags = deriveTags(it)
	return it, true
}

var (
	reCharges     = regexp.MustCompile(`(\d+) charges`)
	reRecharge    = regexp.MustCompile(`regains (\d+d\d+(?:\s*[+-]\s*\d+)?) expended charges(?: daily)? at (dawn|dusk|noon|midnight)`)
	reRechargeAll = regexp.MustCompile(`regains all expended charges(?: daily)? at (dawn|dusk|noon|midnight)`)
)

// parseCharges reads the charge grammar out of an item's printed text:
// how many it holds and how it regains them. Both come from the text or
// not at all — an item whose text states no charges reports none.
func parseCharges(text string) (int, string) {
	low := strings.ToLower(text)
	n := 0
	for _, m := range reCharges.FindAllStringSubmatch(low, -1) {
		if v, err := strconv.Atoi(m[1]); err == nil && v > n {
			n = v
		}
	}
	if n == 0 {
		return 0, ""
	}
	if m := reRecharge.FindStringSubmatch(low); m != nil {
		dice := strings.ReplaceAll(m[1], " ", "")
		return n, dice + " daily at " + m[2]
	}
	if m := reRechargeAll.FindStringSubmatch(low); m != nil {
		return n, "all daily at " + m[1]
	}
	return n, ""
}

// squash lowercases and keeps only letters and digits — the same name
// normalizer the cards package, the D&D resolver and the bestiary use.
func squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

/* ---------- Filtering ---------- */

// Filter narrows the shelf to the items a design or a loot table could
// use.
type Filter struct {
	Types    []string        // SRD categories, e.g. "ring", "weapon"
	Rarities []string        // rarity names, e.g. "Rare", "Very Rare"
	Tags     []string        // derived tags, e.g. "damage-rider", "save-boost"
	Terms    []string        // free-text terms from the DM's idea
	Exclude  map[string]bool // squashed names already spoken for
	Limit    int
}

// scored pairs an item with why it surfaced.
type scored struct {
	it    Item
	score float64
}

// Overlay is an explicit homebrew layer for the catalog's reads: the items
// a DM (or one of their campaigns) designed, in the catalog's own Item
// shape. It is a value the caller loads and hands to Filter, Lookup and
// Search — never rows inside the mirror — so Catalog.Sync can never
// destroy it, and nothing that trusts the mirror mistakes it for SRD. A
// nil *Overlay is the plain catalog: every existing call site keeps its
// behaviour.
//
// Where a homebrew name collides with an SRD name, the homebrew item wins
// in the owner's own scope: the DM's design is the more specific statement
// about their table. The SRD entry is untouched and still serves every
// other owner.
type Overlay struct {
	list  []Item
	byKey map[string]int // squashed name -> index into list
}

// NewOverlay builds the layer from loaded homebrew items. Names are keyed
// squashed, the way the mirror's own index is.
func NewOverlay(list []Item) *Overlay {
	if len(list) == 0 {
		return nil
	}
	o := &Overlay{list: list, byKey: make(map[string]int, len(list))}
	for i, it := range list {
		if k := squash(it.Name); k != "" {
			if _, dup := o.byKey[k]; !dup {
				o.byKey[k] = i
			}
		}
	}
	return o
}

// Len reports how many homebrew items the layer carries.
func (o *Overlay) Len() int {
	if o == nil {
		return 0
	}
	return len(o.list)
}

// Items exposes the layer's items in load order.
func (o *Overlay) Items() []Item {
	if o == nil {
		return nil
	}
	return o.list
}

// Lookup resolves a homebrew item by name with the same tolerance the
// catalog applies to the mirror: squashed equality, plural and possessive
// noise, then the typo-tolerant gate the card matcher uses.
func (o *Overlay) Lookup(name string) (Item, bool) {
	if o == nil {
		return Item{}, false
	}
	key := squash(name)
	if key == "" {
		return Item{}, false
	}
	if i, ok := o.byKey[key]; ok {
		return o.list[i], true
	}
	for _, suffix := range []string{"s", "es"} {
		if trimmed := strings.TrimSuffix(key, suffix); trimmed != key && trimmed != "" {
			if i, ok := o.byKey[trimmed]; ok {
				return o.list[i], true
			}
		}
	}
	for _, it := range o.list {
		if cards.NameMatches(name, it.Name) {
			return it, true
		}
	}
	return Item{}, false
}

// Filter ranks the shelf against a brief. Scoring is additive and
// explainable: a type or rarity gate, a tag match, and a term appearing in
// the name, type or text each contribute, with the name weighted hardest.
//
// The homebrew overlay, when one is handed, is ranked in the same pass and
// under the same gates — a designed item competes with the SRD on what its
// own text says, and carries its homebrew tag through.
func (c *Catalog) Filter(f Filter, hb *Overlay) []Item {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	list := c.list
	c.mu.RUnlock()
	if len(list) == 0 && hb.Len() == 0 {
		return nil
	}
	if f.Limit <= 0 {
		f.Limit = 40
	}

	types := lowerAll(f.Types)
	rarities := lowerAll(f.Rarities)
	tags := lowerAll(f.Tags)
	terms := lowerAll(f.Terms)

	var out []scored
	appendScored := func(it Item) {
		if f.Exclude != nil && f.Exclude[squash(it.Name)] {
			return
		}
		score := 1.0
		if len(types) > 0 {
			if !matchesAny(it.Type, types) {
				return // an explicit type filter is a gate, not a nudge
			}
			score += 4
		}
		if len(rarities) > 0 {
			if !matchesAny(it.Rarity, rarities) {
				return // so is a rarity filter
			}
			score += 3
		}
		for _, want := range tags {
			if hasTag(it.Tags, want) {
				score += 2
			}
		}
		if len(terms) > 0 {
			score += termScore(it, terms)
		}
		out = append(out, scored{it: it, score: score})
	}
	for _, it := range list {
		appendScored(it)
	}
	for _, it := range hb.Items() {
		appendScored(it)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		// Ties break toward the rarer item, then the name — the same
		// stable, explainable order the bestiary keeps.
		if out[i].it.RarityRank != out[j].it.RarityRank {
			return out[i].it.RarityRank > out[j].it.RarityRank
		}
		return out[i].it.Name < out[j].it.Name
	})
	if len(out) > f.Limit {
		out = out[:f.Limit]
	}
	res := make([]Item, 0, len(out))
	for _, s := range out {
		res = append(res, s.it)
	}
	return res
}

// termScore weighs an item against the DM's own words.
func termScore(it Item, terms []string) float64 {
	name := strings.ToLower(it.Name)
	typeRarity := strings.ToLower(it.Type + " " + it.Rarity)
	text := strings.ToLower(it.Text + " " + strings.Join(it.Tags, " "))

	score := 0.0
	for _, term := range terms {
		switch {
		case strings.Contains(name, term):
			score += 6
		case strings.Contains(typeRarity, term):
			score += 3
		case strings.Contains(text, term):
			score += 1
		}
	}
	return score
}

func matchesAny(value string, want []string) bool {
	low := strings.ToLower(strings.TrimSpace(value))
	for _, w := range want {
		if low == w || (w != "" && strings.Contains(low, w)) {
			return true
		}
	}
	return false
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func lowerAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Lookup resolves an item by name: squashed equality first, then the same
// typo-tolerant gate the card matcher uses, so a design that writes
// "Flame Tongue." or "flame-tongue" still lands on the real item. When a
// homebrew overlay is handed, it is asked first — the DM's own design is
// the more specific answer about their table, and an SRD name it reuses
// stays untouched for everyone else.
func (c *Catalog) Lookup(name string, hb *Overlay) (Item, bool) {
	if h, ok := hb.Lookup(name); ok {
		return h, true
	}
	if c == nil {
		return Item{}, false
	}
	key := squash(name)
	if key == "" {
		return Item{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if i, ok := c.byKey[key]; ok {
		return c.list[i], true
	}
	// Plural and possessive noise a brief adds ("two Potions of Healing").
	for _, suffix := range []string{"s", "es"} {
		if trimmed := strings.TrimSuffix(key, suffix); trimmed != key && trimmed != "" {
			if i, ok := c.byKey[trimmed]; ok {
				return c.list[i], true
			}
		}
	}
	for _, it := range c.list {
		if cards.NameMatches(name, it.Name) {
			return it, true
		}
	}
	return Item{}, false
}

// maxSearchResults is the picker's default search cap.
const maxSearchResults = 20

// Search finds items by name for the manual picker, ranked the way the
// remote bestiary ranks: exact, prefix, contains, then typo-tolerant. The
// homebrew overlay, when one is handed, is searched in the same pass and
// its hits lead the list — the DM's own items are the ones they are most
// likely reaching for, and every homebrew hit carries its flag.
func (c *Catalog) Search(query string, limit int, hb *Overlay) []Item {
	key := squash(query)
	if key == "" {
		return nil
	}
	if limit <= 0 {
		limit = maxSearchResults
	}
	var list []Item
	if c != nil {
		c.mu.RLock()
		list = c.list
		c.mu.RUnlock()
	}
	if len(list) == 0 && hb.Len() == 0 {
		return nil
	}
	var out []Item
	if hb.Len() > 0 {
		out = append(out, searchList(query, key, hb.list, limit)...)
	}
	if len(out) >= limit {
		return out[:limit]
	}
	if len(list) > 0 {
		out = append(out, searchList(query, key, list, limit-len(out))...)
	}
	return out
}

// searchList ranks one item list against a query — the shared body of the
// picker's search, used for both the mirror and a homebrew overlay.
func searchList(query, key string, list []Item, limit int) []Item {
	if key == "" || limit <= 0 {
		return nil
	}

	type hit struct {
		it   Item
		tier int
	}
	var hits []hit
	for _, it := range list {
		tier := matchTier(query, key, squash(it.Name))
		if tier < 0 {
			continue
		}
		hits = append(hits, hit{it: it, tier: tier})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].tier != hits[j].tier {
			return hits[i].tier < hits[j].tier
		}
		return hits[i].it.Name < hits[j].it.Name
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Item, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.it)
	}
	return out
}

// matchTier grades a query against a squashed name: 0 exact, 1 prefix,
// 2 contains, 3 typo-tolerant, -1 no match.
func matchTier(query, key, candidate string) int {
	switch {
	case key == candidate:
		return 0
	case strings.HasPrefix(candidate, key):
		return 1
	case strings.Contains(candidate, key):
		return 2
	case cards.NameMatches(query, candidate):
		return 3
	}
	return -1
}

// Types lists the item categories present in the catalog, with how many
// items each holds — the vocabulary the UI offers as filters.
func (c *Catalog) Types() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	list := c.list
	c.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, it := range list {
		t := strings.TrimSpace(it.Type)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// All exposes the whole shelf in load order — the list the designer's
// rarity bands and nearest-neighbour reads rank against.
func (c *Catalog) All() []Item {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.list
}
