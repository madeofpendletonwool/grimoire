// Package journey is travel at a density (MAD-375, stage 3 of MAD-316):
// the road between two places, and what the DM asked to meet on it. The
// route graph, the distances, the calendar and the weather are the
// campaign clock's (MAD-365); this package is the encounter-density layer
// on top — which days carry something, what that something is, and how the
// journey lands back in the campaign.
//
// Which days carry something is a seeded roll against the density band and
// the terrain of each leg: pure, re-runnable, diffable. The same seed, the
// same density and the same route produce a byte-identical day table,
// across calls and across process restarts. The model is never asked what
// happens; it is handed the days the roll already chose and writes one
// line of prose for each (the store's optional pass, the same contract the
// tick's flavour pass carries). A model may not add a day, remove one, or
// move one — a travel montage you cannot reproduce is a travel montage you
// cannot check.
//
// The encounters come from the world, not from nowhere. A day's candidate
// pool is assembled deterministically before any prompt: locations
// reachable from that leg and their place blocks, the factions whose
// territory the leg crosses, the rumours circulating along it, and — for
// combat — encounter.Plan with the party's level band. A travel day that
// invents a brand-new faction is the failure mode; a travel day that
// finally brings the party past the shrine the DM built three sessions ago
// is the feature.
//
// Plan is pure: no database, no wall clock, no model. The rows and the
// review gate live in store.go.
package journey

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/clock"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
)

/* ---------- vocabularies ---------- */

// The density knob. 'none' is "you travel for five days" — a single line
// and a day count, zero model calls, zero day rows. The others are
// days-per-event bands: an eleven-day crossing at standard density
// produces about three incidents, not eleven scenes.
const (
	DensityNone     = "none"
	DensityLight    = "light"
	DensityStandard = "standard"
	DensityDense    = "dense"
)

// Densities is the vocabulary in canonical order.
var Densities = []string{DensityNone, DensityLight, DensityStandard, DensityDense}

// The pace the party rides at. Recorded on the journey and folded into
// the count roll: a forced march and a careful ride are different trips
// even over the same road.
const (
	PaceFast   = "fast"
	PaceNormal = "normal"
	PaceSlow   = "slow"
)

// Paces is the vocabulary in canonical order.
var Paces = []string{PaceFast, PaceNormal, PaceSlow}

// Journey statuses. 'done' is the finalizer's verdict alone; 'abandoned'
// is the DM's.
const (
	StatusPlanned   = "planned"
	StatusUnderway  = "underway"
	StatusDone      = "done"
	StatusAbandoned = "abandoned"
)

// What a day carries. 'uneventful' is a day of weather and ground.
const (
	EventUneventful = "uneventful"
	EventEncounter  = "encounter"
	EventDiscovery  = "discovery"
	EventHazard     = "hazard"
	EventSocial     = "social"
	EventRumor      = "rumour"
	EventLandmark   = "landmark"
)

/* ---------- the density bands ---------- */

// densityBand is the days-per-event band a density names: at least minGap
// days between incidents, at most maxGap. The numbers are the feature's
// published arithmetic — a DM who reads "standard" is owed roughly one
// incident every three to four days, and the count test asserts exactly
// that for every route length.
type densityBand struct{ minGap, maxGap int64 }

var densityBands = map[string]densityBand{
	DensityLight:    {minGap: 5, maxGap: 6},
	DensityStandard: {minGap: 3, maxGap: 4},
	DensityDense:    {minGap: 2, maxGap: 3},
}

// Band returns the honest integer incident-count band for a journey of
// days days at the given density: floor(days/maxGap)..floor(days/minGap).
// Every count in the band corresponds to a plausible packing of incidents
// along the road; the band is never empty and never inverted. Density
// 'none' is [0,0] — the hand-wave.
func Band(density string, days int64) (int64, int64) {
	b, ok := densityBands[density]
	if !ok || days < 1 {
		return 0, 0
	}
	lo, hi := days/b.maxGap, days/b.minGap
	if lo > hi {
		lo = hi
	}
	return lo, hi
}

/* ---------- the shape ---------- */

// Leg is one leg of the stored route: the entity id of where it ends, how
// many days it takes, and what kind of ground it is. It is the same shape
// the location's travel block declares (campaign.Route), stored on the
// journey so it stays reproducible when the route graph later changes.
type Leg struct {
	To      string `json:"to"`
	Days    int64  `json:"days"`
	Terrain string `json:"terrain,omitempty"`
}

// DayPlan is one day of the table: where the road is, what the sky is
// doing, and — when the roll chose this day — what it carries. Rumour days
// put the rumour id in EntityID (a rumour is not an entity; the mill owns
// it) and repeat it in RumorID so callers need not guess which kind
// doubles up. Encounter carries the DMG budget line the day was planned
// against, deterministic in the party's levels.
type DayPlan struct {
	Index     int64          `json:"index"`
	ClockDay  int64          `json:"clock_day"`
	Leg       string         `json:"leg,omitempty"`
	Weather   clock.Forecast `json:"weather"`
	EventKind string         `json:"event_kind"`
	Detail    string         `json:"detail,omitempty"`
	EntityID  string         `json:"entity_id,omitempty"`
	RumorID   string         `json:"rumor_id,omitempty"`
	Encounter string         `json:"encounter_budget,omitempty"`
	Resolved  bool           `json:"resolved"`
}

// Result is the whole planned journey: the window, the route as walked,
// the day table (empty at density none — the Line is that answer), and the
// single line every density produces. JSON-stable: the same inputs and
// seed produce the same bytes, forever.
type Result struct {
	FromDay int64  `json:"from_day"`
	ToDay   int64  `json:"to_day"`
	Days    int64  `json:"days"`
	Density string `json:"density"`
	Pace    string `json:"pace"`
	Seed    int64  `json:"seed"`

	From     string `json:"from"`
	To       string `json:"to"`
	FromName string `json:"from_name"`
	ToName   string `json:"to_name"`

	Route     []Leg    `json:"route"`
	PathNames []string `json:"path_names,omitempty"`
	Line      string   `json:"line"`

	DayTable []DayPlan `json:"day_table,omitempty"`
}

/* ---------- the inputs ---------- */

// Inputs is everything the pure planner consumes: the DM-material
// snapshot (locations, edges, rumours, the party's levels), the campaign
// calendar and weather seed, the location list the route graph is read
// from, the endpoints, an optional day override for roads the map does
// not hold, and the knob with the seed.
type Inputs struct {
	Snapshot     *canon.Snapshot
	Calendar     *clock.Calendar
	WeatherSeed  string
	Locations    []campaign.Entity
	From         string
	To           string
	DaysOverride *int64
	Density      string
	Pace         string
	Seed         int64
}

/* ---------- the plan ---------- */

// Plan works out the journey: the route (the clock's shortest path, or the
// DM's day count when the map holds no road), which days carry something
// (the seeded roll against the density band and each leg's terrain), what
// each of those days carries (a seeded pick from the world's own
// candidates), and the weather for every day. Errors are ErrInvalid-shaped
// refusals that carry their reason: an unknown endpoint, no route and no
// day count, a zero-day road.
func Plan(in Inputs) (*Result, error) {
	if in.Snapshot == nil || in.Calendar == nil {
		return nil, fmt.Errorf("%w: a journey needs the snapshot and the calendar", campaign.ErrInvalid)
	}
	if in.Density != "" && !knownDensity(in.Density) {
		return nil, fmt.Errorf("%w: density %q", campaign.ErrInvalid, in.Density)
	}
	if in.Pace != "" && !knownPace(in.Pace) {
		return nil, fmt.Errorf("%w: pace %q", campaign.ErrInvalid, in.Pace)
	}
	density, pace := canonicalDensity(in.Density), canonicalPace(in.Pace)

	byID := make(map[string]*campaign.Entity, len(in.Locations))
	for i := range in.Locations {
		byID[in.Locations[i].ID] = &in.Locations[i]
	}
	from, ok := byID[in.From]
	if !ok || from.Status == campaign.StatusDeleted {
		return nil, fmt.Errorf("%w: location %s", campaign.ErrNotFound, in.From)
	}
	to, ok := byID[in.To]
	if !ok || to.Status == campaign.StatusDeleted {
		return nil, fmt.Errorf("%w: location %s", campaign.ErrNotFound, in.To)
	}

	// The road: the clock's own shortest path over the declared routes,
	// or the DM's day count when the map holds none.
	var legs []Leg
	var path []string
	var days int64
	if in.DaysOverride != nil {
		if *in.DaysOverride < 1 {
			return nil, fmt.Errorf("%w: a journey takes at least one day", campaign.ErrInvalid)
		}
		legs = []Leg{{To: in.To, Days: *in.DaysOverride}}
		path = []string{in.From, in.To}
		days = *in.DaysOverride
	} else {
		total, walk, ok := campaign.ShortestRoute(in.Locations, in.From, in.To)
		if !ok {
			return nil, fmt.Errorf("%w: no route between %s and %s; pass days to record the journey's length",
				campaign.ErrInvalid, from.Name, to.Name)
		}
		legs = legsOf(in.Locations, walk)
		path = walk
		days = total
	}

	res := &Result{
		FromDay: in.Snapshot.Clock, ToDay: in.Snapshot.Clock + days,
		Days: days, Density: density, Pace: pace, Seed: in.Seed,
		From: in.From, To: in.To, FromName: from.Name, ToName: to.Name,
		Route:     legs,
		PathNames: namesOf(in.Snapshot, path),
		Line:      fmt.Sprintf("You travel from %s to %s: %d days.", from.Name, to.Name, days),
	}

	// Density none is the hand-wave: a single line and a day count. Zero
	// model calls, zero day rows — the caller never reaches a prompt.
	if density == DensityNone {
		return res, nil
	}

	// Which days carry something: the count is a seeded pick inside the
	// band (the route and pace fold into the roll key), the placement a
	// seeded ranking of the day indices.
	lo, hi := Band(density, days)
	count := lo
	if span := hi - lo; span > 0 {
		count += int64(seededIndex(in.Seed, "count:"+density+":"+routeKey(legs, pace), int(span)+1))
	}
	chosen := pickDays(in.Seed, days, count)

	w := readWorld(in.Snapshot, in.Locations)
	for i := int64(0); i < days; i++ {
		leg := legAt(legs, i)
		day := DayPlan{Index: i, ClockDay: res.FromDay + i, Leg: leg.To, EventKind: EventUneventful}
		climate := ""
		if ent, ok := byID[leg.To]; ok {
			climate = campaign.ClimateOf(ent)
		}
		season, _ := in.Calendar.SeasonOf(day.ClockDay)
		day.Weather = clock.Weather(in.WeatherSeed, day.ClockDay, season.Name, climate)
		if chosen[i] {
			w.pick(in.Seed, &day, leg, byID, path, in.Snapshot.Party)
		} else {
			day.Detail = fmt.Sprintf("%s; uneventful travel.", day.Weather.Summary)
		}
		res.DayTable = append(res.DayTable, day)
	}
	return res, nil
}

func knownDensity(d string) bool {
	return d == DensityNone || d == DensityLight || d == DensityStandard || d == DensityDense
}

func knownPace(p string) bool {
	return p == PaceFast || p == PaceNormal || p == PaceSlow
}

// canonicalDensity maps "" onto standard (the workaday default).
func canonicalDensity(d string) string {
	if d == "" {
		return DensityStandard
	}
	return d
}

// canonicalPace maps "" onto normal.
func canonicalPace(p string) string {
	if p == "" {
		return PaceNormal
	}
	return p
}

// legsOf walks the shortest path and reads each consecutive pair's
// declared route, preferring the cheaper edge when a pair is declared both
// ways (they should agree; a hand edit may not).
func legsOf(locations []campaign.Entity, path []string) []Leg {
	byID := make(map[string]*campaign.Entity, len(locations))
	for i := range locations {
		byID[locations[i].ID] = &locations[i]
	}
	legs := make([]Leg, 0, len(path)-1)
	for i := 0; i+1 < len(path); i++ {
		a, b := path[i], path[i+1]
		best := Leg{To: b, Days: 1}
		first := true
		consider := func(r campaign.Route) {
			if first || r.Days < best.Days {
				best = Leg{To: b, Days: r.Days, Terrain: r.Terrain}
				first = false
			}
		}
		if ent, ok := byID[a]; ok {
			for _, r := range campaign.RoutesOf(ent) {
				if r.To == b {
					consider(r)
				}
			}
		}
		if ent, ok := byID[b]; ok {
			for _, r := range campaign.RoutesOf(ent) {
				if r.To == a {
					consider(r)
				}
			}
		}
		legs = append(legs, best)
	}
	return legs
}

// legAt returns the leg a journey day falls on: the leg whose cumulative
// day range contains the index. Days beyond the route's own cost (a DM's
// longer override) hang on the last leg.
func legAt(legs []Leg, day int64) Leg {
	if len(legs) == 0 {
		return Leg{}
	}
	var spent int64
	for _, l := range legs {
		spent += l.Days
		if day < spent {
			return l
		}
	}
	return legs[len(legs)-1]
}

// namesOf joins a path of entity ids to their names.
func namesOf(snap *canon.Snapshot, path []string) []string {
	out := make([]string, 0, len(path))
	for _, id := range path {
		name := id
		if e, ok := entityIn(snap, id); ok {
			name = e.Name
		}
		out = append(out, name)
	}
	return out
}

func entityIn(snap *canon.Snapshot, id string) (*campaign.Entity, bool) {
	for i := range snap.Entities {
		if snap.Entities[i].ID == id {
			return &snap.Entities[i], true
		}
	}
	return nil, false
}

/* ---------- the day's candidate pool ---------- */

// world is the candidate material the whole journey rolls against,
// assembled once per plan from the snapshot: nothing in it is invented,
// everything in it exists. pick weighs what the day's own leg can offer —
// a rumour nobody along the road repeats is not a rumour day, a road with
// no side paths holds no discovery.
type world struct {
	snap      *canon.Snapshot
	locations map[string]*campaign.Entity // live locations by id
	children  map[string][]string         // location -> its live child locations (located_in/contains, either direction)
	standing  map[string][]string         // location -> the live NPCs standing there
	territory map[string][]string         // faction entity -> the locations its territory covers
	rumors    []campaign.Rumor            // circulating, not DM-only
	holders   map[string][]string         // rumor id -> the entities repeating it
}

func readWorld(snap *canon.Snapshot, locations []campaign.Entity) *world {
	w := &world{
		snap:      snap,
		locations: map[string]*campaign.Entity{},
		children:  map[string][]string{},
		standing:  map[string][]string{},
		territory: map[string][]string{},
		holders:   map[string][]string{},
	}
	for i := range locations {
		if locations[i].Status != campaign.StatusDeleted {
			w.locations[locations[i].ID] = &locations[i]
		}
	}
	for _, r := range snap.Relationships {
		switch r.RelType {
		case "located_in", "contains":
			// Children: a live location contained by a live location.
			// Company: a live NPC on the location side of the same edges.
			fromLoc, toLoc := w.locations[r.FromEntity], w.locations[r.ToEntity]
			if fromLoc != nil && toLoc != nil && fromLoc.ID != toLoc.ID {
				w.children[fromLoc.ID] = appendUnique(w.children[fromLoc.ID], toLoc.ID)
				w.children[toLoc.ID] = appendUnique(w.children[toLoc.ID], fromLoc.ID)
			}
			if npc := npcStanding(snap, r.FromEntity); npc != "" && toLoc != nil {
				w.standing[toLoc.ID] = appendUnique(w.standing[toLoc.ID], npc)
			}
			if npc := npcStanding(snap, r.ToEntity); npc != "" && fromLoc != nil {
				w.standing[fromLoc.ID] = appendUnique(w.standing[fromLoc.ID], npc)
			}
		case "owns", "owned_by":
			// Territory: the faction end owns the location end — the
			// dossier's own territory set.
			if f, ok := factionIn(snap, r.FromEntity); ok && f.Status != campaign.StatusDeleted {
				if loc := w.locations[r.ToEntity]; loc != nil {
					w.territory[r.FromEntity] = appendUnique(w.territory[r.FromEntity], loc.ID)
				}
			}
			if f, ok := factionIn(snap, r.ToEntity); ok && f.Status != campaign.StatusDeleted {
				if loc := w.locations[r.FromEntity]; loc != nil {
					w.territory[r.ToEntity] = appendUnique(w.territory[r.ToEntity], loc.ID)
				}
			}
		}
	}
	for _, r := range snap.Rumors {
		if r.DMOnly || r.Status != campaign.RumorStatusCirculating {
			continue
		}
		w.rumors = append(w.rumors, r)
	}
	for _, h := range snap.RumorHolders {
		w.holders[h.RumorID] = appendUnique(w.holders[h.RumorID], h.EntityID)
	}
	return w
}

func npcStanding(snap *canon.Snapshot, id string) string {
	e, ok := entityIn(snap, id)
	if !ok || e.Kind != campaign.KindNPC || e.Status == campaign.StatusDeleted {
		return ""
	}
	return id
}

func factionIn(snap *canon.Snapshot, id string) (*campaign.Entity, bool) {
	e, ok := entityIn(snap, id)
	if !ok || e.Kind != campaign.KindFaction {
		return nil, false
	}
	return e, true
}

func appendUnique(list []string, id string) []string {
	for _, v := range list {
		if v == id {
			return list
		}
	}
	return append(list, id)
}

// option is one thing a day could carry, with the weight the roll reads.
type option struct {
	kind     string
	weight   int
	entity   string
	rumor    string
	detail   string
	encounce string // the encounter budget line, encounter days
}

// pick decides what one rolled day carries: the candidates its own leg
// can offer, weighted, drawn by the seed. A road that offers nothing but
// monsters and weather still offers those — the pool is never empty,
// because encounter and hazard draw on the party and the sky, not the
// graph.
func (w *world) pick(seed int64, day *DayPlan, leg Leg, byID map[string]*campaign.Entity, path []string, party []int) {
	onPath := map[string]bool{}
	for _, id := range path {
		onPath[id] = true
	}
	dayKey := strconv.FormatInt(day.Index, 10)
	var pool []option

	// A wandering monster: always available. The budget is the DMG's own
	// arithmetic over the party's level band — deterministic, published,
	// and the day's recorded plan.
	budget := encounter.Plan(party, encounter.DefaultBand, encounter.Objective{})
	shape := budget.Shapes[seededIndex(seed, "shape:"+dayKey, len(budget.Shapes))]
	pool = append(pool, option{
		kind:   EventEncounter,
		weight: 3,
		encounce: fmt.Sprintf("%s band: target %d XP, ceiling %d XP — %s (about %s each)",
			budget.Band, budget.TargetXP, budget.CeilingXP, shape.Label, shape.EachCR),
		detail: fmt.Sprintf("A wandering encounter: %s, about %s each.", shape.Label, shape.EachCR),
	})

	// A hazard of the ground and the sky.
	pool = append(pool, option{
		kind:   EventHazard,
		weight: 2,
		detail: hazardLine(seed, dayKey, leg.Terrain, day.Weather.Summary),
	})

	// A landmark the road passes: the leg's destination, when it is a
	// real place with an authored interior to notice.
	if legEnt, ok := byID[leg.To]; ok && leg.To != "" {
		p := campaign.PlaceOf(legEnt)
		if p.Kind != "" || len(p.Services) > 0 || len(p.Senses) > 0 {
			pool = append(pool, option{
				kind:   EventLandmark,
				weight: 2,
				entity: legEnt.ID,
				detail: fmt.Sprintf("The road passes %s.", legEnt.Name),
			})
		}
	}

	// A discovery off the road: a live child location of this leg's
	// destination that the route itself does not walk through — the
	// shrine beside the pass, not the pass.
	if kids := w.children[leg.To]; len(kids) > 0 {
		off := make([]string, 0, len(kids))
		for _, k := range kids {
			if !onPath[k] {
				off = append(off, k)
			}
		}
		sort.Strings(off)
		if len(off) > 0 {
			kid := off[seededIndex(seed, "find:"+dayKey, len(off))]
			name := kid
			if e, ok := byID[kid]; ok {
				name = e.Name
			}
			pool = append(pool, option{
				kind:   EventDiscovery,
				weight: 2,
				entity: kid,
				detail: fmt.Sprintf("Something lies off the road: %s.", name),
			})
		}
	}

	// Company: an NPC standing along the leg, or the faction whose
	// territory the leg crosses.
	if npcs := w.standing[leg.To]; len(npcs) > 0 {
		sort.Strings(npcs)
		npc := npcs[seededIndex(seed, "meet:"+dayKey, len(npcs))]
		pool = append(pool, option{
			kind:   EventSocial,
			weight: 2,
			entity: npc,
			detail: fmt.Sprintf("Company on the road: %s.", w.nameOf(npc)),
		})
	}
	var factions []string
	for f, terr := range w.territory {
		for _, loc := range terr {
			if loc == leg.To {
				factions = appendUnique(factions, f)
			}
		}
	}
	sort.Strings(factions)
	if len(factions) > 0 {
		f := factions[seededIndex(seed, "patrol:"+dayKey, len(factions))]
		pool = append(pool, option{
			kind:   EventSocial,
			weight: 1,
			entity: f,
			detail: fmt.Sprintf("A patrol finds them: %s works this road.", w.nameOf(f)),
		})
	}

	// A rumour circulating along the leg: repeated by a holder standing
	// there, or about the place itself. Hearing one on the road is the
	// same event hearing one in a tavern is — the resolve path writes the
	// same stance.
	var along []campaign.Rumor
	for _, r := range w.rumors {
		if w.rumorAlongLeg(r, leg.To) {
			along = append(along, r)
		}
	}
	sort.Slice(along, func(i, j int) bool { return along[i].ID < along[j].ID })
	if len(along) > 0 {
		r := along[seededIndex(seed, "hear:"+dayKey, len(along))]
		pool = append(pool, option{
			kind:   EventRumor,
			weight: 3,
			entity: r.ID,
			rumor:  r.ID,
			detail: fmt.Sprintf("A rumour on the road: %q", r.Statement),
		})
	}

	// The draw: weighted, seeded, once.
	total := 0
	for _, o := range pool {
		total += o.weight
	}
	draw := seededIndex(seed, "kind:"+dayKey, total)
	var chosen option
	for _, o := range pool {
		if draw < o.weight {
			chosen = o
			break
		}
		draw -= o.weight
	}
	day.EventKind = chosen.kind
	day.EntityID = chosen.entity
	day.RumorID = chosen.rumor
	day.Detail = chosen.detail
	day.Encounter = chosen.encounce
}

// nameOf renders an entity id as its name, falling back to the id.
func (w *world) nameOf(id string) string {
	if e, ok := entityIn(w.snap, id); ok {
		return e.Name
	}
	return id
}

// rumorAlongLeg reports whether a rumour circulates along the leg: one of
// its holders stands at the leg's location, or it is about the place
// itself. The party is not its own source — a rumour only the party
// repeats is one they already carry.
func (w *world) rumorAlongLeg(r campaign.Rumor, leg string) bool {
	if leg == "" {
		return false
	}
	if r.AboutEntity == leg {
		return true
	}
	for _, h := range w.holders[r.ID] {
		if h == campaign.PartyKnower {
			continue
		}
		for _, at := range w.standing[leg] {
			if h == at {
				return true
			}
		}
	}
	return false
}

// hazardLines is the hazard vocabulary keyed by the terrain the route
// blocks declare; "" is the ground nobody named.
var hazardLines = map[string][]string{
	"mountain": {"a rockslide blocks the switchbacks", "a crevasse swallows the lead mule", "thin air and a whiteout ridge"},
	"swamp":    {"the causeway drowns under black water", "insects swarm the horses", "a false trail loops into the mire"},
	"forest":   {"deadfall closes the trail", "wolf-sign on every tree", "the canopy swallows the light and the path"},
	"desert":   {"a sandstorm strips the paint from the wagons", "a dry march — the wells lie", "mirage water and real heat"},
	"coast":    {"the tide takes the beach road", "a gale pins the ferry", "salt rot in the water barrels"},
	"road":     {"a washed-out culvert", "a bridge burned last winter", "toll-takers on a king's road"},
	"":         {"hard ground and a wrong turn", "a swollen ford", "a night spent in the rain"},
}

// hazardLine is the day's hazard, drawn from the ground and the sky: the
// terrain vocabulary the route blocks declare, bent by the weather the
// clock already answered for this day.
func hazardLine(seed int64, dayKey, terrain, weather string) string {
	low := strings.ToLower(weather)
	if strings.Contains(low, "snow") || strings.Contains(low, "frost") || strings.Contains(low, "freez") || strings.Contains(low, "blizzard") {
		return fmt.Sprintf("The weather turns: %s, and the road suffers for it.", weather)
	}
	list := hazardLines[strings.ToLower(strings.TrimSpace(terrain))]
	if len(list) == 0 {
		list = hazardLines[""]
	}
	return fmt.Sprintf("A hazard of the road: %s.", list[seededIndex(seed, "hazard:"+dayKey, len(list))])
}

/* ---------- the seed's arithmetic ---------- */

// seededIndex draws one value of n, keyed: the same seed and key always
// draw the same value, and — unlike a bare FNV over seed-then-key, whose
// streams for different keys are affine in the seed and correlate
// horribly — different keys draw independently. The key is hashed on its
// own, the seed folded in multiplicatively, and a splitmix-style
// finalizer avalanches the result before the modulus.
func seededIndex(seed int64, key string, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv64a(key)
	h ^= uint64(seed)
	u := h
	u ^= u >> 30
	u *= 0xbf58476d1ce4e5b9
	u ^= u >> 27
	u *= 0x94d049bb133111eb
	u ^= u >> 31
	return int(u % uint64(n))
}

func fnv64a(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// pickDays chooses count distinct day indices from 0..days-1: every index
// is ranked by its own keyed draw, the count smallest ranks win, and the
// winners come back as a membership map. Same seed, same choice — the
// shuffle is a function, not a shuffle.
func pickDays(seed int64, days, count int64) map[int64]bool {
	out := map[int64]bool{}
	if count <= 0 || days <= 0 {
		return out
	}
	if count > days {
		count = days
	}
	type ranked struct {
		idx  int64
		rank uint64
	}
	all := make([]ranked, 0, days)
	for i := int64(0); i < days; i++ {
		all = append(all, ranked{idx: i, rank: rankOfDay(seed, i)})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].rank != all[j].rank {
			return all[i].rank < all[j].rank
		}
		return all[i].idx < all[j].idx
	})
	for _, r := range all[:count] {
		out[r.idx] = true
	}
	return out
}

// rankOfDay is day i's draw for the shuffle — a full 64-bit splitmix of
// the keyed hash, so two days' ranks are independent streams of the seed.
func rankOfDay(seed int64, i int64) uint64 {
	u := fnv64a("daypick") ^ uint64(seed)*0x9e3779b97f4a7c15 ^ uint64(i+1)*0xbf58476d1ce4e5b9
	u ^= u >> 30
	u *= 0xbf58476d1ce4e5b9
	u ^= u >> 27
	u *= 0x94d049bb133111eb
	u ^= u >> 31
	return u
}

// routeKey folds the route and the pace into one digest for the count
// roll: the same road at the same seed rolls the same count, a different
// pace or a different terrain mix is a different question.
func routeKey(legs []Leg, pace string) string {
	b, err := json.Marshal(struct {
		Legs []Leg  `json:"legs"`
		Pace string `json:"pace"`
	}{legs, pace})
	if err != nil {
		return "unserializable"
	}
	return string(b)
}
