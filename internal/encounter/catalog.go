package encounter

// The local bestiary. Searching Open5e one query at a time is enough to look
// a monster up by name, but it cannot answer the question the builder is
// actually for — "what would fit here?" — because that needs the whole shelf
// at once: every SRD creature, its challenge rating, what it does, and how it
// fights. The SRD is only a few hundred creatures, so the catalog mirrors it
// into the shared database once, derives a vocabulary of tags from each
// statblock, and serves filtering from memory thereafter.
//
// Everything derived here is deterministic and explainable: a tag is present
// because a speed is non-zero or a piece of statblock text says so, never
// because a model guessed. The model's job downstream is choosing among
// candidates the catalog vouched for, the same discipline the deck builder
// keeps with real card data.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
)

const (
	// catalogPageSize is how many creatures one listing request pulls. The
	// SRD is a few hundred rows, so the whole mirror is a handful of calls.
	catalogPageSize = 100
	// catalogMaxPages bounds a sync against a mirror that paginates forever.
	catalogMaxPages = 40
	// catalogTTL is how long a mirrored bestiary is trusted before a refresh
	// is attempted. The SRD does not change; this only catches Open5e
	// corrections and a sync that stored a partial shelf.
	catalogTTL = 30 * 24 * time.Hour
	// catalogSyncTimeout caps a whole mirror pass.
	catalogSyncTimeout = 3 * time.Minute
)

// NamedText is a statblock trait or action: what it is called and what it
// does, verbatim from the SRD.
type NamedText struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
	Kind string `json:"kind,omitempty"` // ACTION, BONUS_ACTION, REACTION, LEGENDARY_ACTION
}

// Creature is one SRD statblock, flattened to what an encounter builder needs
// and tagged with how it fights.
type Creature struct {
	Slug       string         `json:"slug"`
	Name       string         `json:"name"`
	Doc        string         `json:"doc"`
	CR         string         `json:"cr"`
	CRNum      float64        `json:"cr_num"`
	XP         int            `json:"xp"`
	Type       string         `json:"type,omitempty"`
	Size       string         `json:"size,omitempty"`
	Alignment  string         `json:"alignment,omitempty"`
	AC         int            `json:"ac,omitempty"`
	HP         int            `json:"hp,omitempty"`
	HitDice    string         `json:"hit_dice,omitempty"`
	Speeds     map[string]int `json:"speeds,omitempty"`
	Senses     string         `json:"senses,omitempty"`
	Languages  string         `json:"languages,omitempty"`
	Resist     string         `json:"resist,omitempty"`
	Immune     string         `json:"immune,omitempty"`
	Vulnerable string         `json:"vulnerable,omitempty"`
	CondImmune string         `json:"cond_immune,omitempty"`
	Traits     []NamedText    `json:"traits,omitempty"`
	Actions    []NamedText    `json:"actions,omitempty"`
	Tags       []string       `json:"tags,omitempty"`
}

// Line renders the creature as one compact prompt/UI line: everything a
// choice needs, nothing that would bury forty of them.
func (c Creature) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s | CR %s (%d XP)", c.Name, c.CR, c.XP)
	if c.Size != "" || c.Type != "" {
		fmt.Fprintf(&b, " | %s %s", c.Size, c.Type)
	}
	if c.AC > 0 {
		fmt.Fprintf(&b, " | AC %d", c.AC)
	}
	if c.HP > 0 {
		fmt.Fprintf(&b, " HP %d", c.HP)
	}
	if len(c.Tags) > 0 {
		fmt.Fprintf(&b, " | %s", strings.Join(c.Tags, ", "))
	}
	return b.String()
}

// Catalog is the mirrored SRD bestiary: persisted in the shared database,
// held in memory for filtering, refreshed from Open5e when stale.
type Catalog struct {
	db      *sql.DB
	baseURL string
	http    *http.Client

	mu       sync.RWMutex
	list     []Creature
	byKey    map[string]int // squashed name -> index into list
	syncedAt time.Time

	syncing sync.Mutex
}

// NewCatalog builds a catalog over the shared database and ensures its table
// exists. It does not touch the network: call Load, then Sync (or EnsureFresh)
// when a refresh is wanted.
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
		return nil, fmt.Errorf("bestiary migrate: %w", err)
	}
	return c, nil
}

const catalogSchema = `
CREATE TABLE IF NOT EXISTS bestiary (
	key         TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	cr          TEXT NOT NULL DEFAULT '',
	cr_num      REAL NOT NULL DEFAULT 0,
	xp          INTEGER NOT NULL DEFAULT 0,
	type        TEXT NOT NULL DEFAULT '',
	data        TEXT NOT NULL,
	synced_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS bestiary_cr ON bestiary(cr_num);
`

// Count reports how many creatures the catalog currently holds.
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

// Load reads the mirrored bestiary out of the database into memory. An empty
// table is not an error — the catalog simply reports zero creatures until a
// sync fills it.
func (c *Catalog) Load() error {
	rows, err := c.db.Query(`SELECT data, synced_at FROM bestiary ORDER BY name`)
	if err != nil {
		return fmt.Errorf("load bestiary: %w", err)
	}
	defer rows.Close()

	var list []Creature
	var newest int64
	for rows.Next() {
		var blob string
		var syncedAt int64
		if err := rows.Scan(&blob, &syncedAt); err != nil {
			return err
		}
		var cr Creature
		if err := json.Unmarshal([]byte(blob), &cr); err != nil {
			continue // a corrupt row is dropped, not fatal: the next sync rewrites it
		}
		if syncedAt > newest {
			newest = syncedAt
		}
		list = append(list, cr)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.replace(list, time.UnixMilli(newest).UTC())
	return nil
}

// replace swaps in a freshly loaded shelf under the write lock.
func (c *Catalog) replace(list []Creature, syncedAt time.Time) {
	index := make(map[string]int, len(list))
	for i, cr := range list {
		index[squash(cr.Name)] = i
	}
	c.mu.Lock()
	c.list, c.byKey, c.syncedAt = list, index, syncedAt
	c.mu.Unlock()
}

// Stale reports whether the mirror is missing or old enough to re-fetch.
func (c *Catalog) Stale() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.list) == 0 || time.Since(c.syncedAt) > catalogTTL
}

// EnsureFresh syncs when the mirror is missing or stale, and is a no-op
// otherwise. A failed sync leaves whatever is already mirrored in place —
// a bestiary from last month beats no bestiary at all.
func (c *Catalog) EnsureFresh(ctx context.Context) error {
	if c == nil || !c.Stale() {
		return nil
	}
	return c.Sync(ctx)
}

// Sync mirrors every SRD creature from Open5e into the database and reloads
// memory. Concurrent calls serialize; a partial fetch is discarded rather
// than written, so the table never holds half a bestiary.
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
		return fmt.Errorf("bestiary sync: open5e returned no SRD creatures")
	}

	now := time.Now().UTC()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("bestiary sync: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM bestiary`); err != nil {
		return fmt.Errorf("bestiary sync: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO bestiary (key, name, cr, cr_num, xp, type, data, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("bestiary sync: %w", err)
	}
	defer stmt.Close()
	for _, cr := range fetched {
		blob, err := json.Marshal(cr)
		if err != nil {
			continue
		}
		if _, err := stmt.ExecContext(ctx, squash(cr.Name), cr.Name, cr.CR, cr.CRNum, cr.XP, cr.Type, string(blob), now.UnixMilli()); err != nil {
			return fmt.Errorf("bestiary sync: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("bestiary sync: %w", err)
	}

	sort.Slice(fetched, func(i, j int) bool { return fetched[i].Name < fetched[j].Name })
	c.replace(fetched, now)
	return nil
}

// fetchAll pages through /v2/creatures/ scoped to the SRD documents, keeping
// the best-ranked document's version of each name.
func (c *Catalog) fetchAll(ctx context.Context) ([]Creature, error) {
	best := map[string]creatureRecord{}
	next := ""
	for page := 0; page < catalogMaxPages; page++ {
		var raw creatureRecordPage
		var err error
		if next == "" {
			params := url.Values{
				"limit":             {fmt.Sprint(catalogPageSize)},
				"ordering":          {"name"},
				"document__key__in": {strings.Join(srdDocKeys, ",")},
			}
			raw, err = c.getPage(ctx, c.baseURL+"/v2/creatures/?"+params.Encode())
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

	out := make([]Creature, 0, len(best))
	for _, rec := range best {
		if cr, ok := rec.toCreature(); ok {
			out = append(out, cr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Catalog) getPage(ctx context.Context, u string) (creatureRecordPage, error) {
	var page creatureRecordPage
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return page, err
	}
	req.Header.Set("user-agent", "grimoire/1.0 (+https://github.com/madeofpendletonwool/grimoire)")
	req.Header.Set("accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return page, fmt.Errorf("bestiary fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return page, err
	}
	if resp.StatusCode >= 300 {
		return page, fmt.Errorf("bestiary fetch: %s", resp.Status)
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return page, fmt.Errorf("bestiary decode: %w", err)
	}
	return page, nil
}

/* ---------- Open5e wire shapes ---------- */

type creatureRecordPage struct {
	Next    string           `json:"next"`
	Results []creatureRecord `json:"results"`
}

type creatureRecord struct {
	Key             string         `json:"key"`
	Name            string         `json:"name"`
	Document        docRef         `json:"document"`
	Type            *namedThing    `json:"type"`
	Size            *namedThing    `json:"size"`
	ChallengeRating *float64       `json:"challenge_rating"`
	Alignment       string         `json:"alignment"`
	ArmorClass      int            `json:"armor_class"`
	HitPoints       int            `json:"hit_points"`
	HitDice         string         `json:"hit_dice"`
	SpeedAll        map[string]any `json:"speed_all"`
	Languages       struct {
		AsString string `json:"as_string"`
	} `json:"languages"`
	PassivePerception int      `json:"passive_perception"`
	DarkvisionRange   *float64 `json:"darkvision_range"`
	BlindsightRange   *float64 `json:"blindsight_range"`
	TremorsenseRange  *float64 `json:"tremorsense_range"`
	TruesightRange    *float64 `json:"truesight_range"`
	Resistances       struct {
		DamageImmunities      string `json:"damage_immunities_display"`
		DamageResistances     string `json:"damage_resistances_display"`
		DamageVulnerabilities string `json:"damage_vulnerabilities_display"`
		ConditionImmunities   string `json:"condition_immunities_display"`
	} `json:"resistances_and_immunities"`
	Actions []struct {
		Name       string `json:"name"`
		Desc       string `json:"desc"`
		ActionType string `json:"action_type"`
	} `json:"actions"`
	Traits []struct {
		Name string `json:"name"`
		Desc string `json:"desc"`
	} `json:"traits"`
}

// toCreature flattens one API record. A creature with no challenge rating in
// the XP table cannot be budgeted for and is skipped.
func (rec creatureRecord) toCreature() (Creature, bool) {
	if rec.ChallengeRating == nil {
		return Creature{}, false
	}
	label := CRLabel(*rec.ChallengeRating)
	xp, ok := CRXP(label)
	if !ok {
		return Creature{}, false
	}
	c := Creature{
		Slug: rec.Key, Name: strings.TrimSpace(rec.Name), Doc: rec.Document.Key,
		CR: label, CRNum: *rec.ChallengeRating, XP: xp,
		Alignment: strings.TrimSpace(rec.Alignment),
		AC:        rec.ArmorClass, HP: rec.HitPoints, HitDice: strings.TrimSpace(rec.HitDice),
		Languages:  strings.TrimSpace(rec.Languages.AsString),
		Resist:     strings.TrimSpace(rec.Resistances.DamageResistances),
		Immune:     strings.TrimSpace(rec.Resistances.DamageImmunities),
		Vulnerable: strings.TrimSpace(rec.Resistances.DamageVulnerabilities),
		CondImmune: strings.TrimSpace(rec.Resistances.ConditionImmunities),
	}
	if rec.Type != nil {
		c.Type = rec.Type.Name
	}
	if rec.Size != nil {
		c.Size = rec.Size.Name
	}
	c.Speeds = flattenSpeeds(rec.SpeedAll)
	c.Senses = describeSenses(rec)
	for _, t := range rec.Traits {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		c.Traits = append(c.Traits, NamedText{Name: t.Name, Desc: strings.TrimSpace(t.Desc)})
	}
	for _, a := range rec.Actions {
		if strings.TrimSpace(a.Name) == "" {
			continue
		}
		c.Actions = append(c.Actions, NamedText{Name: a.Name, Desc: strings.TrimSpace(a.Desc), Kind: a.ActionType})
	}
	c.Tags = deriveTags(c)
	return c, true
}

// flattenSpeeds keeps the numeric movement modes, dropping the unit key, the
// hover flag, and the crawl speed — the API derives crawl from walk, and a
// statblock never prints it.
func flattenSpeeds(raw map[string]any) map[string]int {
	if len(raw) == 0 {
		return nil
	}
	out := map[string]int{}
	for k, v := range raw {
		if k == "crawl" {
			continue
		}
		f, ok := v.(float64)
		if !ok || f <= 0 {
			continue
		}
		out[k] = int(f)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// describeSenses renders the senses line the way a statblock prints it.
func describeSenses(rec creatureRecord) string {
	var parts []string
	add := func(label string, r *float64) {
		if r != nil && *r > 0 {
			parts = append(parts, fmt.Sprintf("%s %d ft.", label, int(*r)))
		}
	}
	add("blindsight", rec.BlindsightRange)
	add("darkvision", rec.DarkvisionRange)
	add("tremorsense", rec.TremorsenseRange)
	add("truesight", rec.TruesightRange)
	if rec.PassivePerception > 0 {
		parts = append(parts, fmt.Sprintf("passive Perception %d", rec.PassivePerception))
	}
	return strings.Join(parts, ", ")
}

/* ---------- Derived tags ---------- */

// tagRule is one keyword rule over a creature's statblock text.
type tagRule struct {
	tag   string
	words []string
}

// textTagRules are matched against the creature's trait and action text. They
// name the things a DM actually picks monsters for — how it moves, what it
// does to you, whether it fights alone.
var textTagRules = []tagRule{
	{"spellcaster", []string{"spellcasting", "innate spellcasting", "casts the following", "spellcasting ability"}},
	{"ranged", []string{"ranged weapon attack", "ranged attack roll", "longbow", "shortbow", "crossbow", "javelin", "sling"}},
	{"grappler", []string{"grappled", "restrained (escape", "swallow", "engulf"}},
	{"frightening", []string{"frightened"}},
	{"charmer", []string{"charmed"}},
	{"paralyzing", []string{"paralyzed", "petrified", "stunned"}},
	{"poisoner", []string{"poison damage", "poisoned condition", "the poisoned condition"}},
	{"fire", []string{"fire damage"}},
	{"cold", []string{"cold damage"}},
	{"lightning", []string{"lightning damage"}},
	{"acid", []string{"acid damage"}},
	{"necrotic", []string{"necrotic damage"}},
	{"psychic", []string{"psychic damage"}},
	{"radiant", []string{"radiant damage"}},
	{"thunder", []string{"thunder damage"}},
	{"pack-fighter", []string{"pack tactics"}},
	{"ambusher", []string{"surprise", "invisible", "stealth", "hide"}},
	{"regenerating", []string{"regeneration"}},
	{"summoner", []string{"summon", "conjure"}},
	{"breath-weapon", []string{"breath weapon"}},
	{"aoe", []string{"cone", "sphere centered", "line that is", "each creature in a"}},
	{"multiattack", []string{"multiattack"}},
	{"teleporter", []string{"teleport"}},
	{"undead-caller", []string{"raise", "animate dead"}},
}

// deriveTags reads a statblock and reports how the creature fights, so the
// filter and the prompt can talk about roles rather than raw numbers.
func deriveTags(c Creature) []string {
	seen := map[string]bool{}
	var tags []string
	add := func(t string) {
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		tags = append(tags, t)
	}

	// Movement: how it reaches the party.
	if c.Speeds["fly"] > 0 {
		add("flying")
	}
	if c.Speeds["swim"] > 0 {
		add("aquatic")
	}
	if c.Speeds["burrow"] > 0 {
		add("burrowing")
	}
	if c.Speeds["climb"] > 0 {
		add("climbing")
	}
	if c.Speeds["walk"] >= 40 {
		add("fast")
	}

	// Senses: whether darkness or invisibility helps the party.
	low := strings.ToLower(c.Senses)
	if strings.Contains(low, "darkvision") {
		add("darkvision")
	}
	if strings.Contains(low, "truesight") {
		add("truesight")
	}
	if strings.Contains(low, "blindsight") {
		add("blindsight")
	}

	// Statblock text: the interesting half.
	var text strings.Builder
	for _, t := range c.Traits {
		text.WriteString(strings.ToLower(t.Name + " " + t.Desc + "\n"))
	}
	for _, a := range c.Actions {
		text.WriteString(strings.ToLower(a.Name + " " + a.Desc + "\n"))
		if a.Kind == "LEGENDARY_ACTION" {
			add("legendary")
		}
		if a.Kind == "REACTION" {
			add("reactive")
		}
	}
	body := text.String()
	for _, rule := range textTagRules {
		for _, w := range rule.words {
			if strings.Contains(body, w) {
				add(rule.tag)
				break
			}
		}
	}

	// Damage profile: what the party's own damage types will bounce off.
	if c.Immune != "" {
		add("damage-immune")
	}
	if c.Resist != "" {
		add("damage-resistant")
	}
	if c.Vulnerable != "" {
		add("vulnerable")
	}

	// Encounter role by challenge rating: what slot it can fill in a build.
	switch {
	case c.CRNum <= 0.5:
		add("minion")
	case c.CRNum <= 4:
		add("standard")
	case c.CRNum <= 10:
		add("elite")
	default:
		add("boss")
	}
	if seen["legendary"] {
		add("solo")
	}
	if strings.Contains(strings.ToLower(c.Name), "swarm") {
		add("swarm")
	}

	sort.Strings(tags)
	return tags
}

/* ---------- Filtering ---------- */

// Filter narrows the shelf to the creatures a build could use.
type Filter struct {
	MinCR   float64
	MaxCR   float64
	Types   []string        // creature types, e.g. "undead", "dragon"
	Tags    []string        // derived tags, e.g. "flying", "spellcaster"
	Terms   []string        // free-text terms from the DM's idea
	Exclude map[string]bool // squashed names already in the roster
	Limit   int
}

// scored pairs a creature with why it surfaced.
type scored struct {
	c     Creature
	score float64
}

// Filter ranks the catalog against a brief. Scoring is additive and
// explainable: a type match, a tag match, and a term appearing in the name,
// type or statblock text each contribute, with the name weighted hardest so
// "goblin" surfaces goblins before things that merely mention them.
func (c *Catalog) Filter(f Filter) []Creature {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	list := c.list
	c.mu.RUnlock()
	if len(list) == 0 {
		return nil
	}
	if f.Limit <= 0 {
		f.Limit = 40
	}
	if f.MaxCR <= 0 {
		f.MaxCR = 30
	}

	types := lowerAll(f.Types)
	tags := lowerAll(f.Tags)
	terms := lowerAll(f.Terms)

	var out []scored
	for _, cr := range list {
		if cr.CRNum < f.MinCR || cr.CRNum > f.MaxCR {
			continue
		}
		if f.Exclude != nil && f.Exclude[squash(cr.Name)] {
			continue
		}
		score := 1.0
		lowType := strings.ToLower(cr.Type)
		if len(types) > 0 {
			matched := false
			for _, t := range types {
				if strings.Contains(lowType, t) {
					matched = true
					break
				}
			}
			if !matched {
				continue // an explicit type filter is a gate, not a nudge
			}
			score += 4
		}
		for _, want := range tags {
			if hasTag(cr.Tags, want) {
				score += 2
			}
		}
		if len(terms) > 0 {
			score += termScore(cr, terms)
		}
		out = append(out, scored{c: cr, score: score})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		// Ties break toward the tougher creature so a pool is not all rats.
		if out[i].c.CRNum != out[j].c.CRNum {
			return out[i].c.CRNum > out[j].c.CRNum
		}
		return out[i].c.Name < out[j].c.Name
	})
	if len(out) > f.Limit {
		out = out[:f.Limit]
	}
	res := make([]Creature, 0, len(out))
	for _, s := range out {
		res = append(res, s.c)
	}
	return res
}

// termScore weighs a creature against the DM's own words.
func termScore(cr Creature, terms []string) float64 {
	name := strings.ToLower(cr.Name)
	typeSize := strings.ToLower(cr.Type + " " + cr.Size + " " + cr.Alignment)
	var body strings.Builder
	for _, t := range cr.Traits {
		body.WriteString(strings.ToLower(t.Name))
		body.WriteByte(' ')
	}
	for _, a := range cr.Actions {
		body.WriteString(strings.ToLower(a.Name))
		body.WriteByte(' ')
	}
	body.WriteString(strings.ToLower(strings.Join(cr.Tags, " ")))
	text := body.String()

	score := 0.0
	for _, term := range terms {
		switch {
		case strings.Contains(name, term):
			score += 6
		case strings.Contains(typeSize, term):
			score += 3
		case strings.Contains(text, term):
			score += 1
		}
	}
	return score
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

// Lookup resolves a creature by name: squashed equality first, then the same
// typo-tolerant gate the card matcher uses, so a model that writes "Goblin
// Boss." or "goblin-boss" still lands on the real statblock.
func (c *Catalog) Lookup(name string) (Creature, bool) {
	if c == nil {
		return Creature{}, false
	}
	key := squash(name)
	if key == "" {
		return Creature{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if i, ok := c.byKey[key]; ok {
		return c.list[i], true
	}
	// Plural and possessive noise the model adds ("6 Goblins").
	for _, suffix := range []string{"s", "es"} {
		if trimmed := strings.TrimSuffix(key, suffix); trimmed != key && trimmed != "" {
			if i, ok := c.byKey[trimmed]; ok {
				return c.list[i], true
			}
		}
	}
	for i, cr := range c.list {
		if cards.NameMatches(name, cr.Name) {
			return c.list[i], true
		}
	}
	return Creature{}, false
}

// Search finds creatures by name for the manual picker, ranked the way the
// remote bestiary ranks: exact, prefix, contains, then typo-tolerant.
func (c *Catalog) Search(query string, limit int) []MonsterSummary {
	if c == nil {
		return nil
	}
	key := squash(query)
	if key == "" {
		return nil
	}
	if limit <= 0 {
		limit = maxSearchResults
	}
	c.mu.RLock()
	list := c.list
	c.mu.RUnlock()

	type hit struct {
		sum  MonsterSummary
		tier int
	}
	var hits []hit
	for _, cr := range list {
		tier := matchTier(query, key, squash(cr.Name), cr.Name)
		if tier < 0 {
			continue
		}
		hits = append(hits, hit{
			sum:  MonsterSummary{Name: cr.Name, CR: cr.CR, XP: cr.XP, Type: cr.Type},
			tier: tier,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].tier != hits[j].tier {
			return hits[i].tier < hits[j].tier
		}
		return hits[i].sum.Name < hits[j].sum.Name
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]MonsterSummary, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.sum)
	}
	return out
}

// Types lists the creature types present in the catalog, with how many
// creatures each holds — the vocabulary the UI offers as filters.
func (c *Catalog) Types() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	list := c.list
	c.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, cr := range list {
		t := strings.TrimSpace(cr.Type)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// nearestCR returns the highest table CR whose XP fits the given budget, and
// its XP. Used to tell the model how big a single monster may get.
func nearestCR(maxXP int) (string, int) {
	bestLabel, bestXP := "0", 10
	for label := range xpByCR {
		xp := xpByCR[label]
		if xp <= maxXP && xp >= bestXP {
			if xp > bestXP || crValue(label) > crValue(bestLabel) {
				bestLabel, bestXP = label, xp
			}
		}
	}
	return bestLabel, bestXP
}

// crValue parses a CR table label back to a number for ordering.
func crValue(label string) float64 {
	switch label {
	case "1/8":
		return 0.125
	case "1/4":
		return 0.25
	case "1/2":
		return 0.5
	}
	var f float64
	if _, err := fmt.Sscanf(label, "%g", &f); err != nil {
		return math.Inf(-1)
	}
	return f
}
