package entities

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
	dict    *cards.Dictionary
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

// SetDictionary attaches the SRD entity-name dictionary (built at index time
// from Open5e's name listings). It catches the mentions the Title-Case
// heuristics cannot — lowercase, unquoted ("does hunter's mark stack") — the
// same tier the MTG side gets from MTGJSON's card names. Without it, Resolve
// degrades to the heuristics.
func (r *Open5e) SetDictionary(d *cards.Dictionary) {
	if r == nil {
		return
	}
	r.dict = d
}

// Resolve extracts candidate names from the question — using the SRD name
// dictionary when available, plus the same Title-Case heuristics the MTG side
// uses — looks each up against the Open5e SRD, and returns the resolved entity
// text plus the names it could not resolve.
func (r *Open5e) Resolve(ctx context.Context, question string) ([]data.Entity, []string, error) {
	if r == nil {
		return nil, nil, nil
	}
	candidates := cards.ExtractCandidatesWithDict(question, r.dict)
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

// maxSearchVariants bounds how many query forms one name may spend a search
// call on. The verbatim name plus a couple of prefix-stripped forms is enough
// to recover any creator-prefixed SRD spell; the cap keeps a long candidate
// from spending the whole lookup budget on retries.
const maxSearchVariants = 4

// search finds the best SRD match for a name via Open5e's cross-endpoint
// search. The search itself is not fuzzy about extra words: a creator-prefixed
// name ("Tenser's Floating Disk", "Leomund's Tiny Hut", "Melf's Acid Arrow")
// returns zero results even though the SRD renamed the spell to "Floating
// Disk". So the verbatim name is tried first; on a miss the leading word is
// dropped and the search retried — the D&D analogue of MTG's fuzzy card
// lookup. A pure typo with no exact match on any form falls back to the
// strongest credible near-match, gated by cards.NameMatches so a fuzzy search
// can never attach the wrong entity.
func (r *Open5e) search(ctx context.Context, name string, budget *int) (searchHit, error) {
	var bestFuzzy searchHit
	bestFuzzyScore := -1.0
	for _, q := range searchVariants(name) {
		page, err := r.querySearch(ctx, q, budget)
		if err != nil {
			return searchHit{}, err
		}
		// Exact name match on any form is authoritative — return at once.
		if hit, ok := bestExactSRDHit(q, page.Results); ok {
			return hit, nil
		}
		// Otherwise remember the strongest credible fuzzy hit across forms;
		// it is the fallback only if no form yields an exact match.
		if hit, ok := bestFuzzySRDHit(q, page.Results); ok {
			if bestFuzzyScore < 0 || hit.MatchScore > bestFuzzyScore || (hit.MatchScore == bestFuzzyScore && srdRank(hit.Document.Key) < srdRank(bestFuzzy.Document.Key)) {
				bestFuzzy, bestFuzzyScore = hit, hit.MatchScore
			}
		}
	}
	if bestFuzzyScore >= 0 {
		return bestFuzzy, nil
	}
	return searchHit{}, ErrNotFound
}

// searchVariants returns the query forms to try against Open5e search, in order:
// the verbatim name, then the name with leading words dropped one at a time.
// The remainder must stay at least two words: a single-word suffix ("Disk",
// "Spell") is too noisy to search on its own and could fuzzy-attach to an
// unrelated short name. The total is capped at maxSearchVariants.
func searchVariants(name string) []string {
	words := strings.Fields(name)
	if len(words) == 0 {
		return nil
	}
	out := make([]string, 0, len(words))
	out = append(out, strings.Join(words, " "))
	for i := 1; i+2 <= len(words) && len(out) < maxSearchVariants; i++ {
		out = append(out, strings.Join(words[i:], " "))
	}
	return out
}

// querySearch runs one Open5e cross-endpoint search for a single query string.
// A transport error is returned; an empty or missing result is a normal empty
// page so the retry loop can treat "no match for this form" as another miss.
func (r *Open5e) querySearch(ctx context.Context, query string, budget *int) (searchResponse, error) {
	if *budget <= 0 {
		return searchResponse{}, ErrNotFound
	}
	*budget--
	if err := r.throttle(ctx); err != nil {
		return searchResponse{}, err
	}
	params := url.Values{
		"query":  {query},
		"limit":  {fmt.Sprint(searchLimit)},
		"fields": {"document,object_pk,object_name,object_model,route,text,match_type,match_score"},
	}
	var page searchResponse
	if err := r.getJSON(ctx, "/v2/search/", params, &page); err != nil {
		return searchResponse{}, err
	}
	return page, nil
}

// bestExactSRDHit picks the canonical exact SRD match from cross-endpoint
// search results: exact match_type, or a result whose normalized name equals
// the candidate's. Among ties the 2024 SRD is preferred, then older SRD
// editions, then score.
func bestExactSRDHit(query string, results []searchHit) (searchHit, bool) {
	want := nameKey(query)
	var best searchHit
	bestRank := -1
	var bestScore float64 = -1
	for _, h := range results {
		rank := srdRank(h.Document.Key)
		if rank >= len(srdDocumentKeys) {
			continue // not an SRD document
		}
		exactType := strings.EqualFold(h.MatchType, "exact")
		if !exactType && nameKey(h.ObjectName) != want {
			continue
		}
		better := false
		if best.Document.Key == "" {
			better = true
		} else if exactType && !strings.EqualFold(best.MatchType, "exact") {
			better = true
		} else if exactType == strings.EqualFold(best.MatchType, "exact") {
			if rank < bestRank || (rank == bestRank && h.MatchScore > bestScore) {
				better = true
			}
		}
		if better {
			best, bestRank, bestScore = h, rank, h.MatchScore
		}
	}
	return best, best.Document.Key != ""
}

// bestFuzzySRDHit picks the strongest credible near-match from results when no
// exact name match exists. A result qualifies only when cards.NameMatches
// accepts it as a plausible misspelling of the candidate, so a fuzzy search
// cannot attach the wrong entity (e.g. "Firebal" -> "Fireball" yes,
// "Firebal" -> "Delayed Blast Fireball" no). Ranking prefers the newer SRD
// edition, then the search score.
func bestFuzzySRDHit(query string, results []searchHit) (searchHit, bool) {
	want := nameKey(query)
	var best searchHit
	bestRank := -1
	var bestScore float64 = -1
	for _, h := range results {
		rank := srdRank(h.Document.Key)
		if rank >= len(srdDocumentKeys) {
			continue
		}
		if nameKey(h.ObjectName) == want {
			continue // exact matches are handled by bestExactSRDHit
		}
		if !cards.NameMatches(query, h.ObjectName) {
			continue
		}
		better := false
		if best.Document.Key == "" {
			better = true
		} else if rank < bestRank || (rank == bestRank && h.MatchScore > bestScore) {
			better = true
		}
		if better {
			best, bestRank, bestScore = h, rank, h.MatchScore
		}
	}
	return best, best.Document.Key != ""
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
	fields := "name,document,level,school,desc,higher_level,range_text,casting_time,duration,concentration,classes," +
		"type,size,hit_points,armor_class,challenge_rating,speed,actions,rarity,category," +
		"ability_scores,saving_throws,skill_bonuses,passive_perception,blindsight_range,darkvision_range,tremorsense_range,truesight_range," +
		"languages,resistances_and_immunities,traits"
	if err := r.getJSON(ctx, path, url.Values{"fields": {fields}}, &obj); err != nil {
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
	Name            string                `json:"name"`
	Document        documentRef           `json:"document"`
	Level           *float64              `json:"level"`
	School          *namedRef             `json:"school"`
	Desc            string                `json:"desc"`
	HigherLevel     string                `json:"higher_level"`
	RangeText       string                `json:"range_text"`
	CastingTime     string                `json:"casting_time"`
	Duration        string                `json:"duration"`
	Concentration   *bool                 `json:"concentration"`
	Classes         []namedRef            `json:"classes"`
	Type            *namedRef             `json:"type"`
	Size            *namedRef             `json:"size"`
	HitPoints       *float64              `json:"hit_points"`
	ArmorClass      *float64              `json:"armor_class"`
	ChallengeRating *float64              `json:"challenge_rating"`
	Speed           *creatureSpeed        `json:"speed"`
	Actions         []creatureAction      `json:"actions"`
	Rarity          string                `json:"rarity"`
	Category        string                `json:"category"`

	// Full statblock fields (creatures).
	AbilityScores      map[string]int           `json:"ability_scores"`
	SavingThrows       map[string]int           `json:"saving_throws"`
	SkillBonuses       map[string]int           `json:"skill_bonuses"`
	PassivePerception  *float64                 `json:"passive_perception"`
	BlindsightRange    *float64                 `json:"blindsight_range"`
	DarkvisionRange    *float64                 `json:"darkvision_range"`
	TremorsenseRange   *float64                 `json:"tremorsense_range"`
	TruesightRange     *float64                 `json:"truesight_range"`
	Languages          creatureLanguages        `json:"languages"`
	Resistances        resistancesAndImmunities `json:"resistances_and_immunities"`
	Traits             []creatureAction         `json:"traits"`
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

// creatureAction is a named statblock entry. Action-type and legendary cost
// only apply to actions; traits reuse the shape.
type creatureAction struct {
	Name                string `json:"name"`
	Desc                string `json:"desc"`
	ActionType          string `json:"action_type"`
	LegendaryActionCost *int   `json:"legendary_action_cost"`
}

type creatureLanguages struct {
	AsString string `json:"as_string"`
}

// resistancesAndImmunities carries the display strings the API pre-formats.
type resistancesAndImmunities struct {
	DamageVulnerabilities  string `json:"damage_vulnerabilities_display"`
	DamageResistances      string `json:"damage_resistances_display"`
	DamageImmunities       string `json:"damage_immunities_display"`
	ConditionImmunities    string `json:"condition_immunities_display"`
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
	if o.Concentration != nil && *o.Concentration {
		writeField(&b, "Concentration", "yes")
	}
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

// abilityOrder is the display order of the six ability scores.
var abilityOrder = []string{"strength", "dexterity", "constitution", "intelligence", "wisdom", "charisma"}

// abilityShort abbreviates an ability key for statblock lines: "Str", "Dex", ….
func abilityShort(key string) string {
	if len(key) < 3 {
		return key
	}
	return strings.ToUpper(key[:1]) + key[1:3]
}

// signed renders a bonus with an explicit sign, the way statblocks do.
func signed(n int) string {
	if n >= 0 {
		return fmt.Sprintf("+%d", n)
	}
	return fmt.Sprint(n)
}

// formatCreature renders a complete statblock: ability scores, saving throws,
// skills, senses, languages, resistances/immunities, traits, and every action
// category in statblock order (actions, bonus actions, reactions, legendary
// actions). A question about saves, senses, or legendary options grounds in
// the whole block, not just the attack list.
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
	if len(o.AbilityScores) > 0 {
		var parts []string
		for _, k := range abilityOrder {
			if v, ok := o.AbilityScores[k]; ok {
				parts = append(parts, fmt.Sprintf("%s %d (%s)", abilityShort(k), v, signed((v-10)/2)))
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "\nAbilities: %s.", strings.Join(parts, ", "))
		}
	}
	if len(o.SavingThrows) > 0 {
		var parts []string
		for _, k := range abilityOrder {
			if v, ok := o.SavingThrows[k]; ok {
				parts = append(parts, fmt.Sprintf("%s %s", abilityShort(k), signed(v)))
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "\nSaving Throws: %s.", strings.Join(parts, ", "))
		}
	}
	if len(o.SkillBonuses) > 0 {
		var parts []string
		// skills have their own names; sort for stable output
		keys := make([]string, 0, len(o.SkillBonuses))
		for k := range o.SkillBonuses {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %s", skillLabel(k), signed(o.SkillBonuses[k])))
		}
		fmt.Fprintf(&b, "\nSkills: %s.", strings.Join(parts, ", "))
	}
	if senses := sensesLabel(o); senses != "" {
		fmt.Fprintf(&b, "\nSenses: %s.", senses)
	}
	if o.Languages.AsString != "" {
		fmt.Fprintf(&b, "\nLanguages: %s.", o.Languages.AsString)
	}
	if r := o.Resistances; r.DamageVulnerabilities != "" || r.DamageResistances != "" || r.DamageImmunities != "" || r.ConditionImmunities != "" {
		if r.DamageVulnerabilities != "" {
			fmt.Fprintf(&b, "\nDamage Vulnerabilities: %s.", r.DamageVulnerabilities)
		}
		if r.DamageResistances != "" {
			fmt.Fprintf(&b, "\nDamage Resistances: %s.", r.DamageResistances)
		}
		if r.DamageImmunities != "" {
			fmt.Fprintf(&b, "\nDamage Immunities: %s.", r.DamageImmunities)
		}
		if r.ConditionImmunities != "" {
			fmt.Fprintf(&b, "\nCondition Immunities: %s.", r.ConditionImmunities)
		}
	}
	for _, tr := range o.Traits {
		if tr.Name == "" && tr.Desc == "" {
			continue
		}
		fmt.Fprintf(&b, "\n\n%s. %s", strings.TrimSpace(tr.Name), strings.TrimSpace(tr.Desc))
	}
	// Actions grouped by category in statblock order. An action with no
	// action_type (older payloads) is treated as a plain action.
	for _, group := range []struct{ typ, label string }{
		{"ACTION", "Actions"},
		{"BONUS_ACTION", "Bonus Actions"},
		{"REACTION", "Reactions"},
		{"LEGENDARY_ACTION", "Legendary Actions"},
	} {
		var lines []string
		for _, a := range o.Actions {
			typ := a.ActionType
			if typ == "" {
				typ = "ACTION"
			}
			if typ != group.typ || a.Name == "" && a.Desc == "" {
				continue
			}
			entry := fmt.Sprintf("%s. %s", strings.TrimSpace(a.Name), strings.TrimSpace(a.Desc))
			if group.typ == "LEGENDARY_ACTION" && a.LegendaryActionCost != nil && *a.LegendaryActionCost > 1 {
				entry = fmt.Sprintf("%s (costs %d actions). %s", strings.TrimSpace(a.Name), *a.LegendaryActionCost, strings.TrimSpace(a.Desc))
			}
			lines = append(lines, entry)
		}
		if len(lines) > 0 {
			fmt.Fprintf(&b, "\n\n%s:\n%s", group.label, strings.Join(lines, "\n"))
		}
	}
	return strings.TrimSpace(fixFirstLabel(b.String()))
}

// skillLabel prettifies a skill key: "animal_handling" -> "Animal Handling".
func skillLabel(key string) string {
	var b strings.Builder
	for _, part := range strings.Split(key, "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		b.WriteByte(' ')
	}
	return strings.TrimSpace(b.String())
}

// sensesLabel assembles the senses line: passive perception plus any special
// senses with their ranges.
func sensesLabel(o *open5eObject) string {
	var parts []string
	add := func(name string, v *float64) {
		if v == nil || *v == 0 {
			return
		}
		parts = append(parts, fmt.Sprintf("%s %d ft.", name, int(*v)))
	}
	add("blindsight", o.BlindsightRange)
	add("darkvision", o.DarkvisionRange)
	add("tremorsense", o.TremorsenseRange)
	add("truesight", o.TruesightRange)
	if o.PassivePerception != nil {
		parts = append(parts, fmt.Sprintf("passive Perception %d", int(*o.PassivePerception)))
	}
	return strings.Join(parts, ", ")
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
