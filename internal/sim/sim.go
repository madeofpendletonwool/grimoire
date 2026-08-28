// Package sim is the simulation tick (MAD-367, stage 5.2 of MAD-315):
// advance the world by N days as a pure, deterministic function.
//
// Tick takes the campaign snapshot, the calendar, the faction plans and the
// schedule and produces the window's outcomes: plan advances, the scheduled
// entries that fell due, one goal action per NPC whose faction's plan moved,
// and the reactions of enemy factions to rival plans that reached a publicly
// visible step. No database, no wall clock, no model: the same inputs — and
// the seed — produce a byte-identical Result, forever, which is what makes a
// tick re-runnable (staging re-derives the outcomes rather than caching
// them) and two ticks diffable.
//
// The preconditions a plan's step declares are evaluated against the
// snapshot, the same rules internal/faction's store evaluates against the
// database: entity liveness, edge existence, credible facts, an enemy plan's
// position. Deriving them here — from the same in-memory inputs the digest
// covers — is what keeps the outcome a function of the inputs alone; a
// second database pass would be a second truth the digest cannot see.
//
// The model has no role in deciding anything here. Flavour is a second,
// optional pass over the already-decided outcomes (see the store): prose
// about what this looked like, never a change to what happened.
package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/clock"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
)

/* ---------- the result ---------- */

// PlanAdvance is one plan's window: the arithmetic faction.Advance produced,
// whether it moved, and the plan as it stands at the window's end (state,
// progress, reached states, status) — what a caller feeds back in to chain
// windows.
type PlanAdvance struct {
	PlanID        string              `json:"plan_id"`
	FactionEntity string              `json:"faction_entity"`
	FactionName   string              `json:"faction_name"`
	Name          string              `json:"name"`
	Moved         bool                `json:"moved"`
	Progression   faction.Progression `json:"progression"`
	Advanced      faction.Plan        `json:"advanced"`
}

// DueOutcome is one schedule occurrence inside the window.
type DueOutcome struct {
	EntryID string `json:"entry_id"`
	Name    string `json:"name"`
	Day     int64  `json:"day"`
	// Entity is the entry's owner (whose routine this is), Location where it
	// happens — both entity ids, empty when unset.
	Entity   string `json:"entity,omitempty"`
	Location string `json:"location,omitempty"`
	// Secret marks a secret-visibility entry so the staged event's summary
	// can say so; events carry no visibility of their own.
	Secret bool `json:"secret,omitempty"`
}

// MissedEntry is a pending schedule entry the clock has already passed —
// reported, never quietly fired late.
type MissedEntry struct {
	EntryID string `json:"entry_id"`
	Name    string `json:"name"`
	Day     int64  `json:"day"`
}

// NPCAction is one NPC pursuing one goal over the window. Goal order is the
// priority (MAD-313): the first goal is the one pursued; whether a goal is
// blocked is knowledge-layer state a later stage owns, not something this
// function guesses at.
type NPCAction struct {
	NPC           string `json:"npc"`
	NPCName       string `json:"npc_name"`
	Goal          string `json:"goal"`
	FactionEntity string `json:"faction_entity"`
	TriggerPlanID string `json:"trigger_plan_id"`
	Day           int64  `json:"day"`
	Summary       string `json:"summary"`
}

// Consequence is an enemy faction reacting to a rival plan that reached a
// publicly visible step over the window. The reaction mode is the seed's one
// honest job: a fixed vocabulary, picked deterministically.
type Consequence struct {
	Reactor     string `json:"reactor"`
	ReactorName string `json:"reactor_name"`
	RivalEntity string `json:"rival_entity"`
	RivalName   string `json:"rival_name"`
	PlanID      string `json:"plan_id"`
	PlanName    string `json:"plan_name"`
	ToState     string `json:"to_state"`
	Mode        string `json:"mode"`
	Day         int64  `json:"day"`
	Summary     string `json:"summary"`
}

// Result is the tick's whole output, deterministic and JSON-stable: same
// snapshot, plans, schedule, day count and seed produce the same bytes.
type Result struct {
	FromDay int64  `json:"from_day"`
	ToDay   int64  `json:"to_day"`
	Days    int    `json:"days"`
	Seed    int64  `json:"seed"`
	Digest  string `json:"digest"`

	Plans        []PlanAdvance `json:"plans,omitempty"`
	Due          []DueOutcome  `json:"due,omitempty"`
	Missed       []MissedEntry `json:"missed,omitempty"`
	Actions      []NPCAction   `json:"actions,omitempty"`
	Consequences []Consequence `json:"consequences,omitempty"`
}

// reactionModes is the fixed vocabulary a reacting enemy faction draws from.
var reactionModes = []string{
	"registers the shift and protests it openly",
	"quietly mobilizes its own people",
	"sends agents to watch the outcome",
	"moves to undercut the effort",
}

// membershipTypes are the relationship types that make an NPC a faction's
// own people — the seeded vocabulary's belonging edges, either direction.
var membershipTypes = map[string]bool{
	"member_of": true, "has_member": true,
	"leads": true, "led_by": true,
}

/* ---------- the tick ---------- */

// Tick advances the world by days days. fromDay is the snapshot's clock; the
// window is [fromDay, fromDay+days). The calendar expands recurrence through
// clock.Due — the one expansion in the codebase. entries are the campaign's
// scheduled_events; terminal-status entries (cancelled, missed) never come
// due, matching campaign.ScheduleDue.
func Tick(snap *canon.Snapshot, cal *clock.Calendar, plans []faction.Plan, entries []campaign.ScheduledEvent, days int, seed int64) Result {
	res := Result{Days: days, Seed: seed}
	if snap == nil || cal == nil {
		return res
	}
	res.FromDay = snap.Clock
	res.ToDay = snap.Clock + int64(max(days, 0))
	res.Digest = Digest(snap, plans, entries, days, seed)
	res.Plans = make([]PlanAdvance, 0, len(plans))

	names := entityNames(snap)

	// 1. Plan advances, in the order given (the caller orders by faction,
	// name, id — deterministic). The advanced plan is carried forward so a
	// caller can chain windows; only active plans move, and a plan's own
	// preconditions are read against the pre-advance world: the window is
	// one question about one state of the campaign, not a simulation of
	// intra-window feedback.
	for i := range plans {
		p := plans[i]
		mods := snapshotModifiers(snap, plans, &p)
		pr := faction.Advance(p, days, mods)
		next := advancedPlan(p, pr)
		res.Plans = append(res.Plans, PlanAdvance{
			PlanID: p.ID, FactionEntity: p.FactionEntity, FactionName: names[p.FactionEntity],
			Name:        p.Name,
			Moved:       pr.ToState != p.CurrentState || pr.ToProgress != p.Progress,
			Progression: pr, Advanced: next,
		})
	}

	// 2. Scheduled entries that fell due, in day order.
	var pure []clock.Entry
	byID := map[string]*campaign.ScheduledEvent{}
	for i := range entries {
		e := &entries[i]
		if e.Status != campaign.SchedulePending && e.Status != campaign.ScheduleFired {
			continue
		}
		pure = append(pure, clock.Entry{ID: e.ID, Day: e.Day, Recurrence: e.Recurrence, EveryN: e.EveryNDays})
		byID[e.ID] = e
	}
	for _, occ := range clock.Due(cal, pure, res.FromDay, res.ToDay) {
		e := byID[occ.EntryID]
		res.Due = append(res.Due, DueOutcome{
			EntryID: e.ID, Name: e.Name, Day: occ.Day,
			Entity: e.EntityID, Location: e.LocationEntity,
			Secret: e.Visibility == campaign.VisibilitySecret,
		})
	}

	// A pending entry behind the clock is a decision the world is waiting
	// on: report it, never fire it late.
	for i := range entries {
		e := &entries[i]
		if e.Status == campaign.SchedulePending && e.Day < res.FromDay {
			res.Missed = append(res.Missed, MissedEntry{EntryID: e.ID, Name: e.Name, Day: e.Day})
		}
	}
	sort.Slice(res.Missed, func(i, j int) bool {
		if res.Missed[i].Day != res.Missed[j].Day {
			return res.Missed[i].Day < res.Missed[j].Day
		}
		return res.Missed[i].EntryID < res.Missed[j].EntryID
	})

	// 3. NPC goal actions: one per NPC per window, for NPCs whose faction's
	// plan moved. The first goal is the priority; the action's day is the
	// seed's, inside the window.
	acted := map[string]bool{}
	for i := range res.Plans {
		pa := &res.Plans[i]
		if !pa.Moved {
			continue
		}
		for _, npc := range factionMembers(snap, pa.FactionEntity) {
			if acted[npc] {
				continue // one action per NPC per window, whichever faction moved first
			}
			e, _ := entityByID(snap, npc)
			agent := campaign.NPCAgentOf(e)
			if len(agent.Goals) == 0 {
				continue
			}
			goal := agent.Goals[0]
			day := seededDay(seed, "npc:"+npc, res.FromDay, days)
			res.Actions = append(res.Actions, NPCAction{
				NPC: npc, NPCName: names[npc], Goal: goal,
				FactionEntity: pa.FactionEntity, TriggerPlanID: pa.PlanID,
				Day:     day,
				Summary: fmt.Sprintf("%s acts on the goal %q.", names[npc], goal),
			})
			acted[npc] = true
		}
	}
	sort.Slice(res.Actions, func(i, j int) bool { return res.Actions[i].NPC < res.Actions[j].NPC })

	// 4. Derived consequences: an enemy_of faction reacting to a rival plan
	// that reached a publicly visible step. Publicly visible is the plan's
	// own visibility: a public plan that moved is news in the world.
	var conseq []Consequence
	for i := range res.Plans {
		pa := &res.Plans[i]
		if !pa.Moved {
			continue
		}
		p := &plans[i]
		if p.Visibility != campaign.VisibilityPublic {
			continue
		}
		for _, enemy := range enemyFactions(snap, pa.FactionEntity) {
			mode := reactionModes[seededIndex(seed, "react:"+enemy+":"+pa.PlanID, len(reactionModes))]
			day := seededDay(seed, "react:"+enemy+":"+pa.PlanID, res.FromDay, days)
			conseq = append(conseq, Consequence{
				Reactor: enemy, ReactorName: names[enemy],
				RivalEntity: pa.FactionEntity, RivalName: pa.FactionName,
				PlanID: pa.PlanID, PlanName: pa.Name, ToState: pa.Progression.ToState,
				Mode: mode, Day: day,
				Summary: fmt.Sprintf("%s %s as %s's plan %q reaches %s.",
					names[enemy], mode, pa.FactionName, pa.Name, pa.Progression.ToState),
			})
		}
	}
	sort.Slice(conseq, func(i, j int) bool {
		if conseq[i].Reactor != conseq[j].Reactor {
			return conseq[i].Reactor < conseq[j].Reactor
		}
		if conseq[i].RivalEntity != conseq[j].RivalEntity {
			return conseq[i].RivalEntity < conseq[j].RivalEntity
		}
		return conseq[i].PlanID < conseq[j].PlanID
	})
	res.Consequences = conseq
	return res
}

/* ---------- the digest ---------- */

// Digest hashes everything a tick is a function of: the snapshot, the plans,
// the schedule, the day count and the seed, serialized as canonical JSON
// (encoding/json orders map keys, so the bytes are stable). Two previews of
// the same campaign compare digests; a mismatch is "the campaign has since
// changed", detectable rather than silently stale.
func Digest(snap *canon.Snapshot, plans []faction.Plan, entries []campaign.ScheduledEvent, days int, seed int64) string {
	payload, err := json.Marshal(struct {
		Snapshot *canon.Snapshot           `json:"snapshot"`
		Plans    []faction.Plan            `json:"plans"`
		Entries  []campaign.ScheduledEvent `json:"entries"`
		Days     int                       `json:"days"`
		Seed     int64                     `json:"seed"`
	}{snap, plans, entries, days, seed})
	if err != nil {
		// A snapshot that cannot be serialized still gets a stable,
		// distinct digest rather than a silent empty one.
		payload = []byte(fmt.Sprintf("marshal error: %v", err))
	}
	sum := sha256.Sum256(payload)
	return "sim:" + hex.EncodeToString(sum[:])
}

/* ---------- preconditions over the snapshot ---------- */

// snapshotModifiers derives a plan's active step's modifier set from the
// snapshot: one modifier per broken requirement whose author declared a
// reaction — the same rule faction.Store.Modifiers applies to live rows.
func snapshotModifiers(snap *canon.Snapshot, plans []faction.Plan, p *faction.Plan) []faction.Modifier {
	step := p.ActiveStep()
	if step == nil {
		return nil
	}
	var mods []faction.Modifier
	for _, req := range step.Requires {
		if met := requirementHolds(snap, plans, p, req); met {
			continue
		}
		if req.IfBroken == nil {
			continue
		}
		label := req.Label
		if label == "" {
			label = "requirement"
		}
		mods = append(mods, faction.Modifier{
			Label: label, Factor: req.IfBroken.Factor, Reason: req.IfBroken.Reason,
		})
	}
	return mods
}

// requirementHolds is one precondition against the snapshot: entity
// liveness, edge existence, a credible (non-superseded canon/derived) fact,
// or no enemy_of plan sitting at the named state.
func requirementHolds(snap *canon.Snapshot, plans []faction.Plan, p *faction.Plan, req faction.Requirement) bool {
	switch {
	case req.Entity != "":
		e, ok := entityByID(snap, req.Entity)
		return ok && e.Status != campaign.StatusDestroyed && e.Status != campaign.StatusDeleted

	case req.Edge != nil:
		for _, r := range snap.Relationships {
			if r.FromEntity == req.Edge.From && r.RelType == req.Edge.Type && r.ToEntity == req.Edge.To {
				return true
			}
		}
		return false

	case req.Fact != nil:
		for _, f := range snap.Facts {
			if f.SubjectEntity != req.Fact.Subject || f.Predicate != req.Fact.Predicate || f.SupersededBy != "" {
				continue
			}
			if f.Confidence != campaign.ConfidenceCanon && f.Confidence != campaign.ConfidenceDerived {
				continue
			}
			if req.Fact.Object == "" || f.ObjectEntity == req.Fact.Object || f.ObjectLiteral == req.Fact.Object {
				return true
			}
		}
		return false

	case req.EnemyPlan != nil:
		for i := range plans {
			other := &plans[i]
			if other.ID == p.ID || other.Status == faction.PlanAbandoned {
				continue
			}
			if other.CurrentState != req.EnemyPlan.State {
				continue
			}
			if joinedByEnemyOf(snap, p.FactionEntity, other.FactionEntity) {
				return false
			}
		}
		return true

	default:
		return true
	}
}

/* ---------- graph reads over the snapshot ---------- */

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

// factionMembers lists an entity's own people — live npc entities joined by
// a membership edge, either direction — in stable id order.
func factionMembers(snap *canon.Snapshot, factionEntity string) []string {
	known := map[string]bool{}
	for i := range snap.Entities {
		if snap.Entities[i].Kind == campaign.KindNPC && snap.Entities[i].Status != campaign.StatusDeleted {
			known[snap.Entities[i].ID] = true
		}
	}
	var out []string
	for _, r := range snap.Relationships {
		if !membershipTypes[r.RelType] {
			continue
		}
		member := ""
		switch {
		case r.FromEntity == factionEntity && known[r.ToEntity]:
			member = r.ToEntity
		case r.ToEntity == factionEntity && known[r.FromEntity]:
			member = r.FromEntity
		}
		if member != "" {
			out = append(out, member)
		}
	}
	sort.Strings(out)
	return out
}

// enemyFactions lists the factions joined to this one by enemy_of, either
// direction, in stable id order.
func enemyFactions(snap *canon.Snapshot, factionEntity string) []string {
	var out []string
	for _, r := range snap.Relationships {
		if r.RelType != "enemy_of" {
			continue
		}
		switch {
		case r.FromEntity == factionEntity:
			out = append(out, r.ToEntity)
		case r.ToEntity == factionEntity:
			out = append(out, r.FromEntity)
		}
	}
	sort.Strings(out)
	return out
}

func joinedByEnemyOf(snap *canon.Snapshot, a, b string) bool {
	for _, r := range snap.Relationships {
		if r.RelType != "enemy_of" {
			continue
		}
		if (r.FromEntity == a && r.ToEntity == b) || (r.FromEntity == b && r.ToEntity == a) {
			return true
		}
	}
	return false
}

/* ---------- the seed's arithmetic ---------- */

// seededHash is a stable 64-bit draw of a key under a seed: the same seed
// and key give the same number in every process, forever.
func seededHash(seed int64, key string) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d\x00%s", seed, key)
	return h.Sum64()
}

// seededIndex picks a vocabulary slot.
func seededIndex(seed int64, key string, n int) int {
	if n <= 1 {
		return 0
	}
	return int(seededHash(seed, key) % uint64(n))
}

// seededDay picks a day inside the window: from + draw % days, clamped to
// from when days is not positive.
func seededDay(seed int64, key string, from int64, days int) int64 {
	if days <= 0 {
		return from
	}
	return from + int64(seededHash(seed, key)%uint64(days))
}

/* ---------- chaining ---------- */

// advancedPlan returns the plan as the window left it: state, progress and
// reached states from the progression, status complete when every step is
// entered — the same rule faction's own persistence applies.
func advancedPlan(p faction.Plan, pr faction.Progression) faction.Plan {
	next := p
	next.CurrentState = pr.ToState
	next.Progress = pr.ToProgress
	next.Reached = append(append([]string{}, p.Reached...), movesStates(pr.Moves)...)
	if next.ActiveStep() == nil && len(p.Steps) > 0 {
		next.Status = faction.PlanComplete
	}
	next.LastAdvanced = nil // the store stamps the day on apply
	return next
}

func movesStates(moves []faction.StepMove) []string {
	out := make([]string, 0, len(moves))
	for _, m := range moves {
		out = append(out, m.To)
	}
	return out
}

// Trim renders a number without trailing zeros, matching the faction
// package's ledger prose.
func Trim(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}
