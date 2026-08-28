// Package downtime is downtime resolution (MAD-368, stage 5.3 of MAD-315):
// the simulation tick pointed at one character. "I spend three weeks
// researching the cult" is answered in three deterministic stages before any
// prose exists, and the same window is also one sim.Tick — so the answer
// includes the world having moved.
//
// Downtime is the first place in MAD-315 where scope is load-bearing. What
// the character can find out is not what the DM knows: candidates are
// filtered by reachability (stage 1), by what the character does not already
// hold and what a reachable source could plausibly carry (stage 2), and by a
// seeded score against the fact's own difficulty (stage 3). A resolver that
// asked a model "what do they find out?" would leak the campaign on its
// first use — perspective is authorization, not instruction (ADR 2).
//
// Resolve is pure: no database, no wall clock, no model. The same inputs and
// seed produce a byte-identical Result, forever — reproducible exactly like
// a tick, and staged through the same review gate.
package downtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/clock"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/sim"
)

/* ---------- the activity vocabulary ---------- */

// The closed vocabulary a downtime activity maps onto. Free text is read the
// way encounter.ReadIdea reads a free-text encounter idea: words map onto
// the vocabulary, and text that maps to none — or to several — is a
// clarifying question, never a guess.
const (
	ActivityResearch   = "research"
	ActivityCraft      = "craft"
	ActivityTrain      = "train"
	ActivityCarouse    = "carouse"
	ActivityWork       = "work"
	ActivityTravel     = "travel"
	ActivityRecuperate = "recuperate"
	ActivityScheme     = "scheme"
)

// Activities is the vocabulary in canonical order.
var Activities = []string{
	ActivityResearch, ActivityCraft, ActivityTrain, ActivityCarouse,
	ActivityWork, ActivityTravel, ActivityRecuperate, ActivityScheme,
}

// activityWords maps the words a player actually types onto vocabulary
// entries — same mechanism, different vocabulary, as encounter's ideaTags.
var activityWords = map[string]string{
	"research": ActivityResearch, "study": ActivityResearch, "investigate": ActivityResearch,
	"dig": ActivityResearch, "library": ActivityResearch, "libraries": ActivityResearch,
	"archive": ActivityResearch, "archives": ActivityResearch, "read": ActivityResearch,
	"reading": ActivityResearch, "lore": ActivityResearch, "ask": ActivityResearch,
	"asking": ActivityResearch, "questions": ActivityResearch, "rumors": ActivityResearch,
	"rumours": ActivityResearch,

	"craft": ActivityCraft, "make": ActivityCraft, "forge": ActivityCraft,
	"build": ActivityCraft, "smith": ActivityCraft, "brew": ActivityCraft,
	"sew": ActivityCraft, "carve": ActivityCraft, "cobble": ActivityCraft,

	"train": ActivityTrain, "practice": ActivityTrain, "practise": ActivityTrain,
	"drill": ActivityTrain, "exercise": ActivityTrain, "learn": ActivityTrain,
	"study-with": ActivityTrain, "spar": ActivityTrain,

	"carouse": ActivityCarouse, "drink": ActivityCarouse, "drinking": ActivityCarouse,
	"party": ActivityCarouse, "celebrate": ActivityCarouse, "tavern": ActivityCarouse,
	"taverns": ActivityCarouse, "feast": ActivityCarouse, "festival": ActivityCarouse,
	"socialize": ActivityCarouse, "socialise": ActivityCarouse, "gamble": ActivityCarouse,
	"gambling": ActivityCarouse,

	"work": ActivityWork, "job": ActivityWork, "labor": ActivityWork, "labour": ActivityWork,
	"earn": ActivityWork, "wages": ActivityWork, "apprentice": ActivityWork,
	"hire": ActivityWork, "employment": ActivityWork,

	"travel": ActivityTravel, "journey": ActivityTravel, "trip": ActivityTravel,
	"ride": ActivityTravel, "walk": ActivityTravel, "march": ActivityTravel,
	"sail": ActivityTravel, "visit": ActivityTravel, "voyage": ActivityTravel,
	"explore": ActivityTravel,

	"recuperate": ActivityRecuperate, "rest": ActivityRecuperate, "recover": ActivityRecuperate,
	"heal": ActivityRecuperate, "recuperating": ActivityRecuperate, "recoup": ActivityRecuperate,
	"convalesce": ActivityRecuperate,

	"scheme": ActivityScheme, "plot": ActivityScheme, "conspire": ActivityScheme,
	"intrigue": ActivityScheme, "maneuver": ActivityScheme, "manoeuvre": ActivityScheme,
	"network": ActivityScheme, "finagle": ActivityScheme,
}

// activityStopwords carry no activity signal — without them "I spend" and
// "three weeks" would outrank the verb in every mapping.
var activityStopwords = map[string]bool{
	"i": true, "my": true, "me": true, "spend": true, "spending": true,
	"want": true, "to": true, "the": true, "a": true, "an": true, "of": true,
	"in": true, "on": true, "at": true, "for": true, "with": true, "about": true,
	"three": true, "two": true, "one": true, "week": true, "weeks": true,
	"day": true, "days": true, "month": true, "months": true, "downtime": true,
	"time": true, "some": true, "downtimes": true, "character": true,
	"character's": true, "their": true, "them": true, "and": true, "then": true,
}

var activityWordRE = regexp.MustCompile(`[a-z][a-z'-]+`)

// ClarifyError is the clarifying question an unmappable activity comes back
// as: the text as given, and either the several vocabulary entries its words
// hit (ambiguous) or none at all (unknown). Nothing is resolved, nothing is
// staged — the caller answers the question and asks again.
type ClarifyError struct {
	Text       string
	Candidates []string // the vocabulary entries the words hit; empty when none
}

func (e *ClarifyError) Error() string {
	joined := strings.Join(Activities, " | ")
	if len(e.Candidates) > 0 {
		joined = strings.Join(e.Candidates, " | ")
	}
	return fmt.Sprintf("could not read one activity in %q — it points at %s; the downtime vocabulary is %s",
		e.Text, joined, strings.Join(Activities, " | "))
}

// ReadActivity pulls the activity out of free text: the text that IS a
// vocabulary entry resolves directly; otherwise its words map onto the
// vocabulary (plain, plural, and gerund forms included) and exactly one hit
// resolves. Zero hits or several are a *ClarifyError — a question, not a
// guess.
func ReadActivity(text string) (string, error) {
	low := strings.ToLower(strings.TrimSpace(text))
	if low == "" {
		return "", &ClarifyError{Text: text}
	}
	vocabulary := map[string]string{}
	for _, a := range Activities {
		vocabulary[a] = a
	}
	for w, a := range activityWords {
		vocabulary[w] = a
	}
	seen := map[string]bool{}
	for _, w := range activityWordRE.FindAllString(low, -1) {
		if activityStopwords[w] {
			continue
		}
		for _, form := range wordForms(w) {
			if a, ok := vocabulary[form]; ok {
				seen[a] = true
			}
		}
	}
	hit := make([]string, 0, len(seen))
	for a := range seen {
		hit = append(hit, a)
	}
	sort.Slice(hit, func(i, j int) bool {
		return indexOfDay(hit[i]) < indexOfDay(hit[j])
	})
	if len(hit) != 1 {
		return "", &ClarifyError{Text: text, Candidates: hit}
	}
	return hit[0], nil
}

// wordForms renders the word shapes one token maps through: the word, its
// plain plural ("libraries" -> "library" is listed directly; "rumors" ->
// "rumor"), and its gerund stem ("carousing" -> "carouse", "training" ->
// "train", "recuperating" -> "recuperate").
func wordForms(w string) []string {
	forms := []string{w}
	if strings.HasSuffix(w, "s") && len(w) > 3 {
		forms = append(forms, strings.TrimSuffix(w, "s"))
	}
	if strings.HasSuffix(w, "ing") && len(w) > 5 {
		stem := strings.TrimSuffix(w, "ing")
		forms = append(forms, stem, stem+"e")
	}
	return forms
}

func indexOfDay(a string) int {
	for i, v := range Activities {
		if v == a {
			return i
		}
	}
	return len(Activities)
}

/* ---------- relevant proficiencies ---------- */

// relevantProficiencies is the proficiency list a pc payload may declare
// ("proficiencies": ["investigation", ...]) that bears on each activity's
// discovery check. Exact membership match, lowercased.
var relevantProficiencies = map[string][]string{
	ActivityResearch:   {"investigation", "history", "arcana", "religion"},
	ActivityScheme:     {"deception", "insight", "stealth", "persuasion"},
	ActivityCarouse:    {"persuasion", "performance", "insight"},
	ActivityCraft:      {"smithing", "alchemy", "carpentry", "weaving", "tinkering"},
	ActivityTrain:      {"athletics", "acrobatics"},
	ActivityWork:       {"athletics"},
	ActivityTravel:     {"survival", "nature"},
	ActivityRecuperate: {"medicine"},
}

// proficienciesOf reads a pc payload's "proficiencies" list, tolerating the
// shapes JSON round-trips produce.
func proficienciesOf(e *campaign.Entity) map[string]bool {
	out := map[string]bool{}
	if e == nil {
		return out
	}
	list, ok := e.Payload["proficiencies"].([]any)
	if !ok {
		return out
	}
	for _, v := range list {
		if s, ok := v.(string); ok {
			if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
				out[s] = true
			}
		}
	}
	return out
}

/* ---------- the inputs and the result ---------- */

// Inputs is everything Resolve consumes: the DM-material snapshot (the
// caller holds the DM scope; a player-facing surface must never see the
// Result whole), the calendar, plans and schedule the tick reads, the
// character, the activity and its optional subject, the window and the seed.
type Inputs struct {
	Snapshot  *canon.Snapshot
	Calendar  *clock.Calendar
	Plans     []faction.Plan
	Entries   []campaign.ScheduledEvent
	Character string // pc entity id
	Activity  string // free text; read through ReadActivity
	Subject   string // entity id the activity is about; "" when none
	Days      int
	Seed      int64
}

// ReachableLocation is one location inside the window's reach.
type ReachableLocation struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Days   int64  `json:"days"`   // one-way travel cost from the character's position
	Source bool   `json:"source"` // an NPC the character can talk to stands here
}

// Finding is one discovery that landed: the fact, the stance it leaves the
// character holding, the reachable sources that carried it, and the check's
// arithmetic — the whole audit line, deterministic.
type Finding struct {
	FactID     string   `json:"fact_id"`
	Statement  string   `json:"statement"`
	Visibility string   `json:"visibility"`
	Stance     string   `json:"stance"` // knows | suspects
	Confidence float64  `json:"confidence"`
	Method     string   `json:"method"`
	Sources    []string `json:"sources,omitempty"` // npc entity ids that held it
	Score      float64  `json:"score"`
	Difficulty int      `json:"difficulty"`
}

// Result is the whole deterministic answer: the window's identity, the three
// stages' working (reachability, findings), and the world's own movement —
// the same window as one sim.Tick. JSON-stable: same inputs and seed produce
// the same bytes, forever.
type Result struct {
	FromDay int64  `json:"from_day"`
	ToDay   int64  `json:"to_day"`
	Days    int    `json:"days"`
	Seed    int64  `json:"seed"`
	Digest  string `json:"digest"`

	Character     string `json:"character"`
	CharacterName string `json:"character_name"`
	Activity      string `json:"activity"`
	Subject       string `json:"subject,omitempty"`
	SubjectName   string `json:"subject_name,omitempty"`

	// Location is the character's position (a located_in edge); empty when
	// the campaign has not recorded where they stand.
	Location     string `json:"location,omitempty"`
	LocationName string `json:"location_name,omitempty"`

	Reachable []ReachableLocation `json:"reachable,omitempty"`
	Findings  []Finding           `json:"findings,omitempty"`

	// TravelDays is the route's cost when the activity is travel.
	TravelDays int64 `json:"travel_days,omitempty"`

	// Tick is the same window's world outcomes — plans, dues, actions,
	// consequences. The character's three weeks and the cult's three weeks
	// are the same three weeks.
	Tick *sim.Result `json:"tick,omitempty"`
}

/* ---------- the score's constants ---------- */

// The check (stage 3) is an arithmetic, published here so a DM can read a
// result and know why:
//
//	score = dayScore + sourceScore + proficiencyScore + roll
//	  dayScore        min(3, days / 7)      — a week of work per point
//	  sourceScore     min(3, Σ stance weight over distinct reachable
//	                   granting sources: knows 1, suspects 0.7,
//	                   believes_false 0.5)
//	  proficiencyScore min(2, relevant proficiencies declared in the pc
//	                   payload's "proficiencies" list)
//	  roll            seeded 0..2           — the one honest die
//	difficulty = 7 for a secret fact, 4 for a public one, +1 when the fact's
//	  confidence is contested (disagreeing sources are harder to learn
//	  straight).
//	  score >= difficulty + 1 lands as knows; score >= difficulty - 1 lands
//	  as suspects; below that, nothing lands.
const (
	dayScoreCap          = 3.0
	sourceScoreCap       = 3.0
	profScoreCap         = 2.0
	difficultyBase       = 4
	difficultySecret     = 3 // added to the base
	difficultyContested  = 1
	rollSides            = 3
	stanceKnowsW         = 1.0
	stanceSuspectsW      = 0.7
	stanceBelievesFalseW = 0.5
)

// stanceWeight is one granting source's quality contribution.
func stanceWeight(stance string) float64 {
	switch stance {
	case "knows":
		return stanceKnowsW
	case "suspects":
		return stanceSuspectsW
	case "believes_false":
		return stanceBelievesFalseW
	default:
		return 0
	}
}

/* ---------- the resolution ---------- */

// Resolve runs the three stages and the tick. Errors are ErrInvalid-shaped
// refusals that carry their reason: an unmappable activity (a *ClarifyError),
// a character that is not a live pc, a travel request with no route. A
// refusal is the answer, not a fallback to guessing.
func Resolve(in Inputs) (*Result, error) {
	if in.Snapshot == nil || in.Calendar == nil {
		return nil, fmt.Errorf("%w: downtime needs the snapshot and the calendar", campaign.ErrInvalid)
	}
	activity, err := ReadActivity(in.Activity)
	if err != nil {
		return nil, err
	}
	names := entityNames(in.Snapshot)

	char, ok := entityByID(in.Snapshot, in.Character)
	if !ok || char.Kind != campaign.KindPC || char.Status == campaign.StatusDeleted {
		return nil, fmt.Errorf("%w: %s is not a live player character of this campaign",
			campaign.ErrInvalid, in.Character)
	}

	res := &Result{
		FromDay: in.Snapshot.Clock, ToDay: in.Snapshot.Clock + int64(max(in.Days, 0)),
		Days: in.Days, Seed: in.Seed,
		Character: char.ID, CharacterName: char.Name,
		Activity: activity,
		Digest:   Digest(&in, activity),
	}

	var subject *campaign.Entity
	if in.Subject != "" {
		subject, ok = entityByID(in.Snapshot, in.Subject)
		if !ok || subject.Status == campaign.StatusDeleted || subject.Status == campaign.StatusDestroyed {
			return nil, fmt.Errorf("%w: subject %s", campaign.ErrNotFound, in.Subject)
		}
		res.Subject, res.SubjectName = subject.ID, subject.Name
	}

	// Stage 1 — what is reachable. The character's position is a located_in
	// edge; from it, the route graph bounds the locations the window can
	// touch, and the NPCs standing at those locations are the sources.
	pos := positionOf(in.Snapshot, char.ID)
	if pos != "" {
		if e, ok := entityByID(in.Snapshot, pos); ok {
			res.Location, res.LocationName = pos, e.Name
		}
	}
	locations := locationsOf(in.Snapshot)
	reach := reachableLocations(locations, pos, int64(in.Days))
	sourceLocs, sourceNPCs := sourcesStanding(in.Snapshot, reach)
	for id, days := range reach {
		res.Reachable = append(res.Reachable, ReachableLocation{
			ID: id, Name: names[id], Days: days, Source: sourceLocs[id],
		})
	}
	sort.Slice(res.Reachable, func(i, j int) bool {
		if res.Reachable[i].Days != res.Reachable[j].Days {
			return res.Reachable[i].Days < res.Reachable[j].Days
		}
		return res.Reachable[i].ID < res.Reachable[j].ID
	})

	// Travel is the activity whose subject is a destination: it is refused
	// with the reason when no route exists or the window cannot cover the
	// journey — never silently resolved.
	if activity == ActivityTravel {
		if subject == nil || subject.Kind != campaign.KindLocation {
			return nil, fmt.Errorf("%w: travel names no location; give the destination as the subject",
				campaign.ErrInvalid)
		}
		if pos == "" {
			return nil, fmt.Errorf("%w: %s has no position on record; record where they stand before traveling",
				campaign.ErrInvalid, char.Name)
		}
		days, _, ok := campaign.ShortestRoute(locations, pos, subject.ID)
		if !ok {
			fromName, toName := pos, subject.Name
			if e, ok := entityByID(in.Snapshot, pos); ok {
				fromName = e.Name
			}
			return nil, fmt.Errorf("%w: no route between %s and %s; record the journey's legs as routes first",
				campaign.ErrInvalid, fromName, toName)
		}
		if days > int64(in.Days) {
			return nil, fmt.Errorf("%w: the road from %s to %s takes %d days and the window is %d",
				campaign.ErrInvalid, res.LocationName, subject.Name, days, in.Days)
		}
		res.TravelDays = days
	}

	// Stage 2 + 3 — what is discoverable, and whether it lands. Candidates
	// are the live facts the subject touches that this character does not
	// already hold (character scope: their own rows and the party's); a
	// secret is a candidate only when a reachable source holds a granting
	// stance itself. Each candidate is then scored, seeded, against its own
	// difficulty.
	if subject != nil {
		held := heldGrants(in.Snapshot, char.ID)
		for i := range in.Snapshot.Facts {
			f := &in.Snapshot.Facts[i]
			if !touches(f, subject.ID) || !live(f) || held[f.ID] {
				continue
			}
			sources, sourceScore := grantingSources(in.Snapshot, f.ID, sourceNPCs)
			if f.Visibility == campaign.VisibilitySecret && len(sources) == 0 {
				continue // no reachable source could plausibly hold it
			}
			difficulty := factDifficulty(f)
			dayScore := float64(in.Days) / 7.0
			if dayScore > dayScoreCap {
				dayScore = dayScoreCap
			}
			prof := proficienciesOf(char)
			var profScore float64
			for _, name := range relevantProficiencies[activity] {
				if prof[name] {
					profScore++
				}
			}
			if profScore > profScoreCap {
				profScore = profScoreCap
			}
			roll := float64(seededIndex(in.Seed, "downtime:"+char.ID+":"+f.ID, rollSides))
			score := dayScore + sourceScore + profScore + roll
			var stance string
			var confidence float64
			switch {
			case score >= float64(difficulty+1):
				stance, confidence = "knows", 0.9
			case score >= float64(difficulty-1):
				stance, confidence = "suspects", 0.6
			default:
				continue // the check failed; nothing lands, nothing leaks
			}
			where := res.LocationName
			if where == "" {
				where = "the campaign"
			}
			sort.Strings(sources)
			method := fmt.Sprintf("%s spends %d days %s in %s", char.Name, in.Days, activityPlain[activity], where)
			if subject != nil {
				method = fmt.Sprintf("%s spends %d days "+activityWithSubject[activity]+" in %s",
					char.Name, in.Days, subject.Name, where)
			}
			res.Findings = append(res.Findings, Finding{
				FactID: f.ID, Statement: f.Statement, Visibility: f.Visibility,
				Stance: stance, Confidence: confidence,
				Method:  method,
				Sources: sources, Score: score, Difficulty: difficulty,
			})
		}
		sort.Slice(res.Findings, func(i, j int) bool { return res.Findings[i].FactID < res.Findings[j].FactID })
	}

	// And what the world did: the same window, one tick, one seed.
	tick := sim.Tick(in.Snapshot, in.Calendar, in.Plans, in.Entries, in.Days, in.Seed)
	res.Tick = &tick
	return res, nil
}

// activityPlain is the activity's gerund for a method or summary line with
// no subject named: "recuperating", "researching".
var activityPlain = map[string]string{
	ActivityResearch: "researching", ActivityCraft: "crafting", ActivityTrain: "training",
	ActivityCarouse: "carousing", ActivityWork: "working", ActivityTravel: "traveling",
	ActivityRecuperate: "recuperating", ActivityScheme: "scheming",
}

// activityWithSubject is the gerund as a format over the subject's name:
// "researching %s", "scheming against %s".
var activityWithSubject = map[string]string{
	ActivityResearch: "researching %s", ActivityCraft: "crafting for %s",
	ActivityTrain: "training with %s", ActivityCarouse: "carousing with %s",
	ActivityWork: "working for %s", ActivityTravel: "traveling to %s",
	ActivityRecuperate: "recuperating at %s", ActivityScheme: "scheming against %s",
}

// Digest hashes everything a downtime result is a function of: the snapshot,
// the plans, the schedule, the character, the mapped activity, the subject,
// the day count and the seed. Two requests compare digests; a mismatch is
// "the campaign has changed since this request", detectable rather than
// silently stale. The free text is deliberately absent: two phrasings that
// read as the same activity over the same subject are the same question.
func Digest(in *Inputs, activity string) string {
	payload, err := json.Marshal(struct {
		Snapshot  *canon.Snapshot           `json:"snapshot"`
		Plans     []faction.Plan            `json:"plans"`
		Entries   []campaign.ScheduledEvent `json:"entries"`
		Character string                    `json:"character"`
		Activity  string                    `json:"activity"`
		Subject   string                    `json:"subject"`
		Days      int                       `json:"days"`
		Seed      int64                     `json:"seed"`
	}{in.Snapshot, in.Plans, in.Entries, in.Character, activity, in.Subject, in.Days, in.Seed})
	if err != nil {
		payload = []byte(fmt.Sprintf("marshal error: %v", err))
	}
	sum := sha256.Sum256(payload)
	return "downtime:" + hex.EncodeToString(sum[:])
}

/* ---------- stage 1: position and reach ---------- */

// positionOf reads the character's location: a located_in edge from the
// character to a live location. Empty when unrecorded.
func positionOf(snap *canon.Snapshot, characterID string) string {
	for _, r := range snap.Relationships {
		if r.FromEntity == characterID && r.RelType == "located_in" {
			if e, ok := entityByID(snap, r.ToEntity); ok &&
				e.Kind == campaign.KindLocation && e.Status != campaign.StatusDeleted {
				return r.ToEntity
			}
		}
	}
	return ""
}

// locationsOf collects the campaign's live locations.
func locationsOf(snap *canon.Snapshot) []campaign.Entity {
	var out []campaign.Entity
	for i := range snap.Entities {
		e := &snap.Entities[i]
		if e.Kind == campaign.KindLocation && e.Status != campaign.StatusDeleted {
			out = append(out, *e)
		}
	}
	return out
}

// reachableLocations answers which locations the window touches: Dijkstra
// over the same route graph campaign.ShortestRoute walks (routes are
// undirected, every cost positive), bounded by the days available. The
// character's own position is reachable at cost 0; an unrecorded position
// (or a bound of zero days) reaches nothing.
func reachableLocations(locations []campaign.Entity, from string, days int64) map[string]int64 {
	out := map[string]int64{}
	if from == "" || days < 0 {
		return out
	}
	known := map[string]bool{}
	for i := range locations {
		known[locations[i].ID] = true
	}
	if !known[from] {
		return out
	}
	adj := map[string][]campaign.Route{}
	for i := range locations {
		loc := &locations[i]
		for _, r := range campaign.RoutesOf(loc) {
			if !known[r.To] || r.Days < 0 {
				continue
			}
			adj[loc.ID] = append(adj[loc.ID], r)
			adj[r.To] = append(adj[r.To], campaign.Route{To: loc.ID, Days: r.Days, Terrain: r.Terrain})
		}
	}
	dist := map[string]int64{from: 0}
	visited := map[string]bool{}
	for {
		best := ""
		var bestDist int64
		for id, d := range dist {
			if visited[id] || d > days {
				continue
			}
			if best == "" || d < bestDist {
				best, bestDist = id, d
			}
		}
		if best == "" {
			break
		}
		visited[best] = true
		if _, seen := out[best]; !seen {
			out[best] = bestDist
		}
		for _, r := range adj[best] {
			if visited[r.To] {
				continue
			}
			if d, seen := dist[r.To]; !seen || bestDist+r.Days < d {
				dist[r.To] = bestDist + r.Days
			}
		}
	}
	return out
}

// sourceEdges are the relationship types that put an NPC at a location,
// either direction.
var sourceEdges = map[string]bool{
	"located_in": true, "contains": true,
}

// sourcesStanding walks the reachability set for its people: which reachable
// locations carry at least one live NPC (the "someone to ask" flag), and the
// set of those NPCs — the sources stage 2 and stage 3 consult. Nothing
// outside the reachable set can be a source.
func sourcesStanding(snap *canon.Snapshot, reach map[string]int64) (map[string]bool, map[string]bool) {
	liveNPC := map[string]bool{}
	for i := range snap.Entities {
		e := &snap.Entities[i]
		if e.Kind == campaign.KindNPC && e.Status != campaign.StatusDeleted {
			liveNPC[e.ID] = true
		}
	}
	locs := map[string]bool{}
	npcs := map[string]bool{}
	at := func(id string) bool { _, ok := reach[id]; return ok }
	for _, r := range snap.Relationships {
		if !sourceEdges[r.RelType] {
			continue
		}
		switch {
		case at(r.ToEntity) && liveNPC[r.FromEntity]:
			locs[r.ToEntity] = true
			npcs[r.FromEntity] = true
		case at(r.FromEntity) && liveNPC[r.ToEntity]:
			locs[r.FromEntity] = true
			npcs[r.ToEntity] = true
		}
	}
	return locs, npcs
}

/* ---------- stage 2: candidacy ---------- */

// touches reports whether a fact is about the subject at either end.
func touches(f *campaign.Fact, subject string) bool {
	return f.SubjectEntity == subject || f.ObjectEntity == subject
}

// live is the retrieval liveness rule: a proposed fact is invisible to every
// perspective, and superseded history is not current truth.
func live(f *campaign.Fact) bool {
	return f.Confidence != campaign.ConfidenceProposed && f.SupersededBy == ""
}

// heldGrants is what the character already holds a granting stance over,
// read at the character scope's knowers: their own rows and the party's. A
// fact the party knows is not a discovery for them.
func heldGrants(snap *canon.Snapshot, characterID string) map[string]bool {
	out := map[string]bool{}
	for _, a := range snap.Awareness {
		if a.Knower != characterID && a.Knower != campaign.PartyKnower {
			continue
		}
		if a.Stance == "knows" || a.Stance == "suspects" || a.Stance == "believes_false" {
			out[a.FactID] = true
		}
	}
	return out
}

/* ---------- stage 3: the check ---------- */

// factDifficulty is the fact's own difficulty, derived from its attributes:
// secrets are hard, public facts are the base, contested facts are murkier.
func factDifficulty(f *campaign.Fact) int {
	d := difficultyBase
	if f.Visibility == campaign.VisibilitySecret {
		d += difficultySecret
	}
	if f.Confidence == campaign.ConfidenceContested {
		d += difficultyContested
	}
	return d
}

// grantingSources lists the reachable NPCs holding a granting stance on the
// fact, and the quality score their stances add: a secret somebody at the
// table could actually carry, not a die roll against nothing.
func grantingSources(snap *canon.Snapshot, factID string, sourceNPCs map[string]bool) ([]string, float64) {
	var sources []string
	var score float64
	for _, a := range snap.Awareness {
		if a.FactID != factID || !sourceNPCs[a.Knower] {
			continue
		}
		if w := stanceWeight(a.Stance); w > 0 {
			sources = append(sources, a.Knower)
			score += w
		}
	}
	if score > sourceScoreCap {
		score = sourceScoreCap
	}
	return sources, score
}

/* ---------- shared graph reads (same shape as sim's) ---------- */

func entityByID(snap *canon.Snapshot, id string) (*campaign.Entity, bool) {
	for i := range snap.Entities {
		if snap.Entities[i].ID == id {
			return &snap.Entities[i], true
		}
	}
	return nil, false
}

func entityNames(snap *canon.Snapshot) map[string]string {
	out := make(map[string]string, len(snap.Entities))
	for i := range snap.Entities {
		out[snap.Entities[i].ID] = snap.Entities[i].Name
	}
	return out
}

/* ---------- the seed's arithmetic (same draws as sim's) ---------- */

func seededHash(seed int64, key string) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d\x00%s", seed, key)
	return h.Sum64()
}

func seededIndex(seed int64, key string, n int) int {
	if n <= 1 {
		return 0
	}
	return int(seededHash(seed, key) % uint64(n))
}
