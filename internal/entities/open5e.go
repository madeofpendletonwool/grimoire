package entities

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
	"unicode"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/data"
)

// DefaultBaseURL is the canonical Open5e API root. The D&D 5e SRD is exposed
// under /v2/ as DRF-style REST endpoints. Free, no key.
const DefaultBaseURL = "https://api.open5e.com"

// minRequestInterval is a polite floor between Open5e requests, matching the
// rate-limit discipline used for Scryfall.
const minRequestInterval = 100 * time.Millisecond

// requestTimeout caps each Open5e HTTP call.
const requestTimeout = 10 * time.Second

// cacheTTL bounds how long a resolved entity is trusted. SRD entries change
// rarely, so this matches the cards/rulings cache.
const cacheTTL = 6 * time.Hour

// maxEntityLookups bounds how many Open5e calls one question may spend. Each
// candidate costs at most two calls (a search plus an object fetch), and
// extraction caps candidates, so this is a ceiling on distinct names.
const maxEntityLookups = 16

// searchLimit is how many search hits to request per candidate. The
// cross-endpoint search spans community documents as well as the SRD, so a
// handful of hits is enough to find the SRD entry after filtering.
const searchLimit = 12

// srdDocumentKeys are the Open5e document keys that carry D&D 5e SRD content,
// in preference order. The cross-endpoint search returns matches from many
// documents (community homebrew among them); resolved entities are filtered to
// the SRD so the answer grounds in the canonical rules text, the way the MTG
// side grounds in real Scryfall cards.
var srdDocumentKeys = []string{"srd-2024", "srd", "srd-2014"}

// srdRank ranks an SRD document key by preference: 0 for the 2024 SRD, then
// older editions. Lower is better; a non-SRD document ranks last so an SRD hit
// always wins when both are present.
func srdRank(key string) int {
	for i, k := range srdDocumentKeys {
		if key == k {
			return i
		}
	}
	return len(srdDocumentKeys)
}

// ErrNotFound is returned when an entity name has no SRD match. Callers treat
// it as a normal "no entity available" outcome and surface the name as
// unresolved rather than a hard failure.
var ErrNotFound = errors.New("entity not found")

// Open5e resolves D&D entities (spells, creatures, magic items, feats,
// conditions, weapons) mentioned in a question via the public Open5e API. It
// implements data.EntityResolver.
type Open5e struct {
	baseURL string
	http    *http.Client

	mu      sync.Mutex
	lastReq time.Time
	cache   sync.Map // string (lowercased name) -> cacheEntry
}

type cacheEntry struct {
	at       time.Time
	entity   *data.Entity
	notFound bool
}

// New builds an Open5e resolver against the default endpoint.
func New() *Open5e { return NewWithBase(DefaultBaseURL) }

// NewWithBase builds a resolver against an alternate endpoint (for tests or a
// mirror). An empty base falls back to the default.
func NewWithBase(baseURL string) *Open5e {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Open5e{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: requestTimeout},
	}
}

// Resolve extracts candidate names from the question using the same Title-Case
// heuristics the MTG side uses (D&D spells, monsters, and feats are Title-Case
// too), looks each up against the Open5e SRD, and returns the resolved entity
// text plus the names it could not resolve.
func (r *Open5e) Resolve(ctx context.Context, question string) ([]data.Entity, []string, error) {
	if r == nil {
		return nil, nil, nil
	}
	candidates := cards.ExtractCandidates(question)
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	budget := maxEntityLookups
	var entities []data.Entity
	var unresolved []string
	for _, name := range candidates {
		ent, err := r.resolveName(ctx, name, &budget)
		if err != nil || ent == nil {
			unresolved = append(unresolved, name)
			continue
		}
		entities = append(entities, *ent)
	}
	return entities, unresolved, nil
}

// resolveName looks up a single candidate, caching the outcome. A miss is
// cached as notFound so a repeat mention in the same conversation is not
// re-queried.
func (r *Open5e) resolveName(ctx context.Context, name string, budget *int) (*data.Entity, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return nil, ErrNotFound
	}
	if e, ok := r.cache.Load(key); ok {
		if entry := e.(cacheEntry); time.Since(entry.at) < cacheTTL {
			if entry.notFound {
				return nil, ErrNotFound
			}
			return entry.entity, nil
		}
	}

	hit, err := r.search(ctx, name, budget)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			r.cache.Store(key, cacheEntry{at: time.Now(), notFound: true})
		}
		return nil, err
	}
	ent, err := r.buildEntity(ctx, hit, budget)
	if err != nil || ent == nil {
		// A fetch failure still degrades to the search text rather than a
		// hard miss — the search hit is already authoritative SRD content.
		if hit.Text == "" {
			r.cache.Store(key, cacheEntry{at: time.Now(), notFound: true})
			return nil, ErrNotFound
		}
		ent = searchFallbackEntity(hit)
	}
	r.cache.Store(key, cacheEntry{at: time.Now(), entity: ent})
	return ent, nil
}

// search finds the best SRD match for a name via Open5e's cross-endpoint search.
// The search spans every document, so results are filtered to the SRD and the
// preferred edition is chosen when several carry the same name.
func (r *Open5e) search(ctx context.Context, name string, budget *int) (searchHit, error) {
	if *budget <= 0 {
		return searchHit{}, ErrNotFound
	}
	*budget--
	if err := r.throttle(ctx); err != nil {
		return searchHit{}, err
	}
	params := url.Values{
		"query":  {name},
		"limit":  {fmt.Sprint(searchLimit)},
		"fields": {"document,object_pk,object_name,object_model,route,text,match_type,match_score"},
	}
	var page searchResponse
	if err := r.getJSON(ctx, "/v2/search/", params, &page); err != nil {
		return searchHit{}, err
	}
	return bestSRDHit(name, page.Results)
}

// bestSRDHit picks the canonical SRD match from cross-endpoint search results.
// Exact name matches win; among ties the 2024 SRD is preferred, then older SRD
// editions. A non-exact hit is accepted only when its normalized name equals
// the candidate's, so a fuzzy search cannot attach the wrong entity.
func bestSRDHit(query string, results []searchHit) (searchHit, error) {
	want := nameKey(query)
	var best searchHit
	bestRank := -1
	var bestScore float64 = -1
	for _, h := range results {
		rank := srdRank(h.Document.Key)
		if rank >= len(srdDocumentKeys) {
			continue // not an SRD document
		}
		exact := strings.EqualFold(h.MatchType, "exact") || nameKey(h.ObjectName) == want
		if !exact {
			continue
		}
		// Prefer exact match_type, then document edition, then score.
		exactType := strings.EqualFold(h.MatchType, "exact")
		better := false
		if best.Document.Key == "" {
			better = true
		} else if exactType && !strings.EqualFold(best.MatchType, "exact") {
			better = true
		} else if exactType == strings.EqualFold(best.MatchType, "exact") {
			if rank < bestRank {
				better = true
			} else if rank == bestRank && h.MatchScore > bestScore {
				better = true
			}
		}
		if better {
			best, bestRank, bestScore = h, rank, h.MatchScore
		}
	}
	if best.Document.Key == "" {
		return searchHit{}, ErrNotFound
	}
	return best, nil
}

// buildEntity fetches the full SRD object for a search hit and formats it into
// grounding text. The object endpoint carries the complete stat block (a spell's
// level/school/range, a creature's HP/AC/actions) that the search payload only
// summarizes.
func (r *Open5e) buildEntity(ctx context.Context, hit searchHit, budget *int) (*data.Entity, error) {
	if hit.Route == "" || hit.ObjectPK == "" {
		return nil, ErrNotFound
	}
	if *budget <= 0 {
		return nil, ErrNotFound
	}
	*budget--
	if err := r.throttle(ctx); err != nil {
		return nil, err
	}
	var obj open5eObject
	path := "/" + strings.Trim(hit.Route, "/") + "/" + url.PathEscape(hit.ObjectPK) + "/"
	if err := r.getJSON(ctx, path, url.Values{"fields": {"name,document,level,school,desc,higher_level,range_text,casting_time,duration,classes,type,size,hit_points,armor_class,challenge_rating,speed,actions,rarity,category"}}, &obj); err != nil {
		return nil, err
	}
	name := obj.Name
	if name == "" {
		name = hit.ObjectName
	}
	body := formatObject(&obj, hit)
	if body == "" {
		return nil, ErrNotFound
	}
	return &data.Entity{Name: name, Kind: kindFromModel(hit.ObjectModel), Body: body}, nil
}

// searchFallbackEntity builds an entity from a search hit's text alone, used
// when the richer object fetch is unavailable. The search text is still
// authoritative SRD content.
func searchFallbackEntity(hit searchHit) *data.Entity {
	name := hit.ObjectName
	body := strings.TrimSpace(hit.Text)
	// The search text opens with the entity name on its own line; drop that
	// redundant header so the body starts at the description.
	if lines := strings.SplitN(body, "\n", 2); len(lines) == 2 && nameKey(lines[0]) == nameKey(name) {
		body = strings.TrimSpace(lines[1])
	}
	if body == "" {
		body = strings.TrimSpace(hit.Text)
	}
	return &data.Entity{Name: name, Kind: kindFromModel(hit.ObjectModel), Body: body}
}

// kindFromModel maps an Open5e object model to a short, human-readable kind
// label used in citations and the model prompt.
func kindFromModel(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "spell":
		return "spell"
	case "creature":
		return "creature"
	case "magicitem":
		return "magic item"
	case "feat":
		return "feat"
	case "condition":
		return "condition"
	case "weapon":
		return "weapon"
	default:
		if model == "" {
			return "entity"
		}
		return strings.ToLower(model)
	}
}

// open5eObject models the union of fields the formatted kinds need. Fields the
// fetched object does not carry stay zero-valued and are omitted from the body.
type open5eObject struct {
	Name            string           `json:"name"`
	Document        documentRef      `json:"document"`
	Level           *float64         `json:"level"`
	School          *namedRef        `json:"school"`
	Desc            string           `json:"desc"`
	HigherLevel     string           `json:"higher_level"`
	RangeText       string           `json:"range_text"`
	CastingTime     string           `json:"casting_time"`
	Duration        string           `json:"duration"`
	Classes         []namedRef       `json:"classes"`
	Type            *namedRef        `json:"type"`
	Size            *namedRef        `json:"size"`
	HitPoints       *float64         `json:"hit_points"`
	ArmorClass      *float64         `json:"armor_class"`
	ChallengeRating *float64         `json:"challenge_rating"`
	Speed           *creatureSpeed   `json:"speed"`
	Actions         []creatureAction `json:"actions"`
	Rarity          string           `json:"rarity"`
	Category        string           `json:"category"`
}

type documentRef struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type namedRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type creatureSpeed struct {
	Walk   *float64 `json:"walk"`
	Fly    *float64 `json:"fly"`
	Climb  *float64 `json:"climb"`
	Swim   *float64 `json:"swim"`
	Burrow *float64 `json:"burrow"`
	Unit   string   `json:"unit"`
}

type creatureAction struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

// formatObject renders an SRD object as grounding prose, per kind. Only
// populated fields are included so a sparse object never emits empty labels.
func formatObject(o *open5eObject, hit searchHit) string {
	switch strings.ToLower(hit.ObjectModel) {
	case "spell":
		return formatSpell(o)
	case "creature":
		return formatCreature(o)
	default:
		return formatGeneric(o, hit)
	}
}

func formatSpell(o *open5eObject) string {
	var b strings.Builder
	if o.Level != nil {
		fmt.Fprintf(&b, "Level %d %s.", int(*o.Level), refName(o.School))
	} else if o.School != nil && o.School.Name != "" {
		fmt.Fprintf(&b, "%s spell.", o.School.Name)
	}
	writeField(&b, "Casting Time", o.CastingTime)
	writeField(&b, "Range", o.RangeText)
	writeField(&b, "Duration", o.Duration)
	if len(o.Classes) > 0 {
		writeField(&b, "Classes", joinNames(o.Classes))
	}
	if desc := strings.TrimSpace(o.Desc); desc != "" {
		b.WriteString("\n\n")
		b.WriteString(desc)
	}
	if hl := strings.TrimSpace(o.HigherLevel); hl != "" {
		fmt.Fprintf(&b, "\n\nAt Higher Levels: %s", hl)
	}
	return strings.TrimSpace(fixFirstLabel(b.String()))
}

func formatCreature(o *open5eObject) string {
	var b strings.Builder
	if o.Size != nil && o.Size.Name != "" || o.Type != nil && o.Type.Name != "" {
		fmt.Fprintf(&b, "%s.", strings.TrimSpace(refName(o.Size)+" "+refName(o.Type)))
	}
	if o.ChallengeRating != nil {
		fmt.Fprintf(&b, " CR %s.", ratingLabel(*o.ChallengeRating))
	}
	if o.HitPoints != nil {
		fmt.Fprintf(&b, " HP %d.", int(*o.HitPoints))
	}
	if o.ArmorClass != nil {
		fmt.Fprintf(&b, " AC %d.", int(*o.ArmorClass))
	}
	if o.Speed != nil {
		if s := speedLabel(o.Speed); s != "" {
			fmt.Fprintf(&b, " Speed %s.", s)
		}
	}
	for _, a := range o.Actions {
		if a.Name == "" && a.Desc == "" {
			continue
		}
		fmt.Fprintf(&b, "\n\n%s. %s", strings.TrimSpace(a.Name), strings.TrimSpace(a.Desc))
	}
	return strings.TrimSpace(fixFirstLabel(b.String()))
}

// formatGeneric covers magic items, feats, conditions, weapons, and anything
// else: lead with the description (or the search text when the object has no
// desc), prefixed by rarity/category when present.
func formatGeneric(o *open5eObject, hit searchHit) string {
	var b strings.Builder
	if o.Rarity != "" {
		writeField(&b, "Rarity", o.Rarity)
	}
	if o.Category != "" {
		writeField(&b, "Category", o.Category)
	}
	desc := strings.TrimSpace(o.Desc)
	if desc == "" {
		desc = strings.TrimSpace(hit.Text)
		if lines := strings.SplitN(desc, "\n", 2); len(lines) == 2 && nameKey(lines[0]) == nameKey(o.Name) {
			desc = strings.TrimSpace(lines[1])
		}
	}
	if desc != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(desc)
	}
	return strings.TrimSpace(fixFirstLabel(b.String()))
}

// writeField appends a "Label: value" line, skipping empties.
func writeField(b *strings.Builder, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	fmt.Fprintf(b, "%s: %s.", label, value)
}

// fixFirstLabel tidies the spacing that writeField leaves before the first
// appended field (a leading space) so the body opens cleanly.
func fixFirstLabel(s string) string {
	return strings.TrimLeft(s, " ")
}

func refName(r *namedRef) string {
	if r == nil {
		return ""
	}
	return r.Name
}

func joinNames(refs []namedRef) string {
	var names []string
	for _, r := range refs {
		if r.Name != "" {
			names = append(names, r.Name)
		}
	}
	return strings.Join(names, ", ")
}

func speedLabel(s *creatureSpeed) string {
	if s == nil {
		return ""
	}
	var parts []string
	add := func(label string, v *float64) {
		if v == nil {
			return
		}
		unit := s.Unit
		if unit == "" {
			unit = "ft"
		}
		parts = append(parts, fmt.Sprintf("%s %d %s", label, int(*v), unit))
	}
	add("walk", s.Walk)
	add("fly", s.Fly)
	add("climb", s.Climb)
	add("swim", s.Swim)
	add("burrow", s.Burrow)
	return strings.Join(parts, ", ")
}

// ratingLabel renders a challenge rating readably: 0.25 -> "1/4", 0.5 -> "1/2",
// whole numbers without a trailing decimal.
func ratingLabel(r float64) string {
	switch r {
	case 0.125:
		return "1/8"
	case 0.25:
		return "1/4"
	case 0.5:
		return "1/2"
	}
	if r == float64(int(r)) {
		return fmt.Sprint(int(r))
	}
	return fmt.Sprint(r)
}

// nameKey lowercases a name and keeps only letters and digits, so "Prize Fight"
// and "Prizefight" compare equal. Mirrors the cards package's normalizer.
func nameKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// throttle enforces a polite minimum spacing between requests.
func (r *Open5e) throttle(ctx context.Context) error {
	r.mu.Lock()
	wait := minRequestInterval - time.Since(r.lastReq)
	r.mu.Unlock()
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	r.mu.Lock()
	r.lastReq = time.Now()
	r.mu.Unlock()
	return nil
}

// getJSON issues a GET and decodes JSON into dst. A 404 or empty result maps to
// ErrNotFound so callers can treat "no match" as a normal, non-fatal outcome.
func (r *Open5e) getJSON(ctx context.Context, path string, params url.Values, dst any) error {
	u := r.baseURL + path
	if encoded := params.Encode(); encoded != "" {
		u += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("user-agent", "grimoire/1.0 (+https://github.com/madeofpendletonwool/grimoire)")
	req.Header.Set("accept", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("open5e request: %w", err)
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
		return fmt.Errorf("open5e: %s", resp.Status)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode open5e response: %w", err)
	}
	return nil
}

type searchResponse struct {
	Count   int         `json:"count"`
	Results []searchHit `json:"results"`
}

type searchHit struct {
	Document    documentRef `json:"document"`
	ObjectPK    string      `json:"object_pk"`
	ObjectName  string      `json:"object_name"`
	ObjectModel string      `json:"object_model"`
	Route       string      `json:"route"`
	Text        string      `json:"text"`
	MatchType   string      `json:"match_type"`
	MatchScore  float64     `json:"match_score"`
}
