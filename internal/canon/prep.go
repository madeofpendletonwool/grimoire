package canon

// One-click session prep (MAD-364, the stage-3 capstone of MAD-314's
// generators): "I have 30 minutes before my players arrive."
//
// The direction list is scored, not generated. Every input is already in the
// database and every one of them is free: the quests and the edges their
// state machines allow next, the secrets nobody can reach and the threads
// gone cold (the canon engine's own checks, run pure), the NPC goals nothing
// has advanced, the factions whose plans are live but offstage, where the
// party is (the timeline's real_ordinal tail) and the party levels that set
// the encounter budget through encounter.Plan. Each candidate carries a
// prep-time estimate computed from what building it actually needs — scenes
// to write, encounters to build, statblocks to resolve, NPCs missing agent
// fields — never a number a model produced. The model, when configured, only
// names and pitches the ranked directions in the campaign's own language.
//
// Building the chosen direction reuses the machinery that already exists:
// the session plan and its scenes go through the scene designer's own fill
// path (planOneSession — one structured-generation call against the same
// validated pools, offline writes a deterministic scaffold), encounters go
// through the encounter designer's arithmetic (encounter.Plan, BuildPool,
// Evaluate) and land as planned-encounter session events, and the awareness
// changes the outcomes promise stage behind the review gate as one proposal
// batch (source 'session_prep').
//
// The prep package itself is read-only, regenerable, and never staged: NPC
// quick-reference sheets read what each NPC knows at npc:<id> scope — the
// SQL-enforced awareness join is the leak guarantee (ADR 2), so a sheet
// cannot contain something its NPC could not say. Likely player questions
// come from the gap between what the party believes and what is true (the
// believes_false stances above all). Contingencies are the scene outcomes
// with nothing downstream. Prior rulings come from the session layer's FTS
// matcher — no model anywhere. The package exports as Markdown through the
// session's own export path: a dm_notes source the existing exporter renders
// verbatim, replaced on every rebuild.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

/* ---------- vocabularies ---------- */

// Direction kinds: which deterministic signal a direction came from. The
// evidence rows carry the engine's own check codes alongside these.
const (
	DirectionAdvanceQuest  = "advance_quest"
	DirectionSurfaceSecret = "surface_secret"
	DirectionReviveThread  = "revive_thread"
	DirectionPushGoal      = "push_goal"
	DirectionFactionMoves  = "faction_moves"
)

// prepDirectionCap is how many directions one run proposes — the issue's
// five, and never padded with filler to reach it.
const prepDirectionCap = 5

// Evidence kinds: the deterministic signals a score is summed from. The
// engine's check codes (unreachable_secret, orphan_thread, dormant_clue,
// unused_npc) are reused verbatim where they are the signal; the rest name
// the joins this package runs.
const (
	evidenceQuestEdge    = "quest_open_edge"
	evidenceNPCGoal      = "npc_goal_stalled"
	evidenceFactionLive  = "faction_plan_live"
	evidencePartyHere    = "party_here"
	evidenceUnusedNPC    = CheckUnusedNPC
	evidenceThreadChecks = "orphan_thread|dormant_clue" // documentation only
)

// directionKindRank orders equal-score directions stably.
var directionKindRank = map[string]int{
	DirectionAdvanceQuest: 0, DirectionSurfaceSecret: 1, DirectionReviveThread: 2,
	DirectionPushGoal: 3, DirectionFactionMoves: 4,
}

/* ---------- the shapes ---------- */

// EvidenceRow is one row of the evidence behind a direction's score: the
// signal, the record it points at, and the weight it contributed. The score
// is the sum of the weights — the ranking is inspectable by construction.
type EvidenceRow struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Label  string `json:"label"`
	Note   string `json:"note,omitempty"`
	Weight int    `json:"weight"`
}

// PrepDirection is one plausible next-session direction: what it advances,
// what it costs in prep minutes, and the evidence rows behind its score.
// Pitch is model prose when a model is configured; without one the
// deterministic Advances sentence carries the idea.
type PrepDirection struct {
	ID          string        `json:"id"`
	Kind        string        `json:"kind"`
	Title       string        `json:"title"`
	Pitch       string        `json:"pitch,omitempty"`
	Advances    string        `json:"advances"`
	Score       int           `json:"score"`
	PrepMinutes int           `json:"prep_minutes"`
	Evidence    []EvidenceRow `json:"evidence"`

	// quest carries the advance_quest candidate's quest, and tos its legal
	// next states, so build needs no second derivation.
	quest campaign.Quest   `json:"-"`
	tos   []string         `json:"-"`
	fact  *campaign.Fact   `json:"-"`
	npc   *campaign.Entity `json:"-"`
}

// DirectionsInput is one direction-list request. Notes, when given, are
// handed to the pitch writer as the DM's steering.
type DirectionsInput struct {
	CampaignID string
	Notes      string
}

// DirectionsResult is the ranked list plus the context it was computed from.
type DirectionsResult struct {
	CampaignID       string          `json:"campaign_id"`
	CampaignName     string          `json:"campaign_name"`
	Act              *story.Act      `json:"act,omitempty"`
	Budget           int             `json:"budget"`
	Mix              []string        `json:"mix"`
	Party            []int           `json:"party"`
	WhereThePartyIs  string          `json:"where_the_party_is,omitempty"`
	DriftedEncounter int             `json:"drifted_encounters"`
	Directions       []PrepDirection `json:"directions"`
	Offline          bool            `json:"offline"`
	GeneratedMS      int64           `json:"generated_at"`
}

// BuildInput is one build request: which direction (by id, from a fresh
// /prep/directions run), optional steering notes, and the encounter band the
// combat scenes are budgeted against (default Medium).
type BuildInput struct {
	CampaignID  string
	DirectionID string
	Notes       string
	Band        string
	CreatedBy   string
	// Catalog is the encounter designer's local bestiary mirror; nil means
	// no mirror and combat scenes get no deterministic roster.
	Catalog *encounter.Catalog
}

// PrepEncounter is one combat scene's planned encounter, written as a
// planned-encounter session event — the payload contract the canon engine's
// encounter checks read.
type PrepEncounter struct {
	SceneID   string              `json:"scene_id"`
	EventID   string              `json:"event_id"`
	Name      string              `json:"name"`
	Party     []int               `json:"party"`
	Monsters  []encounter.Monster `json:"monsters"`
	Band      string              `json:"band"`
	Verdict   encounter.Verdict   `json:"verdict"`
	HandBuilt bool                `json:"hand_built,omitempty"` // no roster: build by hand
}

// NPCSheet is one cast NPC's quick reference. Knows is read at npc:<id>
// scope; the SQL awareness join is the guarantee a sheet holds nothing its
// NPC could not say.
type NPCSheet struct {
	EntityID string   `json:"entity_id"`
	Name     string   `json:"name"`
	Voice    string   `json:"voice,omitempty"`
	Goal     string   `json:"goal,omitempty"`
	Fears    string   `json:"fears,omitempty"`
	Knows    []string `json:"knows"`
}

// PlayerQuestion is one question the table is likely to ask, drawn from the
// gap between what the party believes and what is true.
type PlayerQuestion struct {
	Kind      string `json:"kind"` // wrong | suspected
	Statement string `json:"statement"`
	Note      string `json:"note,omitempty"`
}

// Contingency is one scene outcome with nothing planned downstream.
type Contingency struct {
	SceneID string `json:"scene_id"`
	Scene   string `json:"scene"`
	Label   string `json:"label,omitempty"`
	Summary string `json:"summary,omitempty"`
	Note    string `json:"note"`
}

// PriorRulingRef is one past ruling the FTS matcher surfaced for a scene.
type PriorRulingRef struct {
	SceneID string `json:"scene_id"`
	EventID string `json:"event_id"`
	Session string `json:"session_name"`
	Ordinal int64  `json:"session_ordinal"`
	Summary string `json:"summary"`
}

// PrepPackage is the read-only prep material: never staged, regenerable on
// every build.
type PrepPackage struct {
	WhereThePartyIs string           `json:"where_the_party_is,omitempty"`
	Party           []int            `json:"party"`
	NPCSheets       []NPCSheet       `json:"npc_sheets"`
	Questions       []PlayerQuestion `json:"player_questions"`
	Contingencies   []Contingency    `json:"contingencies"`
	PriorRulings    []PriorRulingRef `json:"prior_rulings"`
}

// BuildResult is everything one build produced: the session it planned, the
// scenes and encounters it wrote, the batch it staged (when the outcomes
// promised awareness changes), and the prep package.
type BuildResult struct {
	Direction        PrepDirection   `json:"direction"`
	SessionID        string          `json:"session_id"`
	Act              story.Act       `json:"act"`
	Goal             string          `json:"goal"`
	Scenes           []story.Scene   `json:"scenes"`
	Encounters       []PrepEncounter `json:"encounters"`
	Batch            *Batch          `json:"batch,omitempty"`
	Package          PrepPackage     `json:"package"`
	MarkdownSourceID string          `json:"markdown_source_id"`
	Offline          bool            `json:"offline"`
	Dropped          []string        `json:"dropped,omitempty"`
	GeneratedMS      int64           `json:"generated_at"`
}

/* ---------- the prep-time estimator (pure) ---------- */

// scenePrepMinutes prices writing one scene of each kind. Combat and
// revelation cost more because they carry rosters and branches, not just
// prose.
var scenePrepMinutes = map[string]int{
	story.KindSocial: 5, story.KindExploration: 5, story.KindTravel: 3,
	story.KindDowntime: 3, story.KindRevelation: 8, story.KindCombat: 12,
}

const (
	prepBaseMinutes       = 5 // reading the state, seating the plan
	prepStatblockMinute   = 1 // per monster already resolvable in the local bestiary
	prepNoBestiaryMinute  = 8 // per combat the mirror cannot serve: fetch statblocks by hand
	prepNPCAgentMinute    = 4 // per cast NPC with no voice or no goal: write them before the table does
	defaultMonstersPerEnc = 4 // the pack shape's count, the commonest build
)

// EstimateInput is what a direction's prep minutes are computed from — the
// scene mix, the party the encounters are budgeted against, whether the
// local bestiary can resolve rosters, and how many cast NPCs are missing
// agent fields.
type EstimateInput struct {
	Mix                  []string
	Party                []int
	Band                 string
	BestiarySynced       bool
	MonstersPerEncounter int
	NPCsMissingAgent     int
}

// EstimatePrep prices one direction's prep in minutes. Pure arithmetic over
// what building it actually needs; no model is asked, here or anywhere in
// the ranking.
func EstimatePrep(in EstimateInput) int {
	total := prepBaseMinutes
	combats := 0
	for _, k := range in.Mix {
		total += scenePrepMinutes[k]
		if k == story.KindCombat {
			combats++
		}
	}
	if in.BestiarySynced {
		n := in.MonstersPerEncounter
		if n <= 0 {
			n = defaultMonstersPerEnc
		}
		total += combats * n * prepStatblockMinute
	} else {
		total += combats * prepNoBestiaryMinute
	}
	total += in.NPCsMissingAgent * prepNPCAgentMinute
	return total
}

/* ---------- the deterministic state ---------- */

// prepState is everything the candidate rules read, loaded once: the engine
// snapshot, the pure findings, the pacing context, and the joins the
// "no progress" rules need.
type prepState struct {
	snap     *Snapshot
	act      *story.Act // nil when the campaign has no act to plan into
	actIdx   int
	actCount int
	sessions int // the act's planned session count, for the budget arithmetic
	budget   int
	mix      []string
	tail     *campaign.Event
	findings []campaign.Finding
	flagged  map[string]string // entity id -> unused_npc finding message
	seated   map[string]bool   // entity seated in any scene (any status)
	played   map[string]bool   // entity participated in any timeline event
	drifted  int
}

func loadPrepState(ctx context.Context, s *Store, campaignID string) (*prepState, error) {
	if _, err := s.loadCampaign(ctx, campaignID); err != nil {
		return nil, err
	}
	snap, err := LoadSnapshot(ctx, s.db, campaignID)
	if err != nil {
		return nil, err
	}
	st := &prepState{snap: snap, flagged: map[string]string{}, seated: map[string]bool{}, played: map[string]bool{}}

	// The pure engine pass: the same checks the health report carries
	// (MAD-312 is merged), without the ledger refresh — a direction list is
	// a read.
	st.findings = CheckSnapshot(snap, DefaultCheckOptions())
	for _, f := range st.findings {
		switch f.Check {
		case CheckUnusedNPC:
			st.flagged[f.RecordID] = f.Message
		case CheckPartyLevelDrift:
			st.drifted++
		}
	}

	// The pacing context: the act the next session plans into, its budget
	// and mix. A campaign with no acts still gets directions — build
	// refuses until one exists.
	if act, err := resolvePlanAct(snap, PlanInput{}); err == nil && act != nil {
		st.act = act
		st.actIdx, st.actCount = actOrdinalPosition(snap, act)
		planBySession := map[string]*story.SessionPlan{}
		for i := range snap.Spine.Plans {
			planBySession[snap.Spine.Plans[i].SessionID] = &snap.Spine.Plans[i]
		}
		for _, ses := range snap.Sessions {
			if ses.Status != "planned" {
				continue
			}
			if p := planBySession[ses.ID]; p != nil && p.ActID == act.ID {
				st.sessions++
			}
		}
	}
	if st.sessions == 0 {
		st.sessions = 1
	}
	if st.act != nil {
		st.budget = story.ScenesPerSession(st.act.LevelStart, st.act.LevelEnd, st.sessions)
	} else {
		st.budget = 4
	}
	st.mix = story.SceneMix(st.actIdx, st.actCount, st.budget)

	// Where the party is: the timeline's real_ordinal tail.
	for i := range snap.Events {
		if st.tail == nil || snap.Events[i].RealOrdinal > st.tail.RealOrdinal {
			e := snap.Events[i]
			st.tail = &e
		}
	}

	for i := range snap.Spine.Scenes {
		for _, c := range snap.Spine.Scenes[i].Cast {
			st.seated[c.EntityID] = true
		}
	}
	for i := range snap.Events {
		for _, p := range snap.Events[i].Participants {
			st.played[p.EntityID] = true
		}
	}
	return st, nil
}

// whereThePartyIs renders the tail as a sentence.
func (st *prepState) whereThePartyIs() string {
	if st.tail == nil {
		return ""
	}
	if st.tail.LocationEntity != "" {
		if e, ok := entityByID(st.snap, st.tail.LocationEntity); ok {
			return fmt.Sprintf("%s — %s", e.Name, st.tail.Summary)
		}
	}
	return st.tail.Summary
}

// inTail names the entities present in the timeline's last event — the
// "what were they last chasing" signal.
func (st *prepState) inTail(id string) bool {
	if st.tail == nil {
		return false
	}
	for _, p := range st.tail.Participants {
		if p.EntityID == id {
			return true
		}
	}
	return false
}

/* ---------- the candidates ---------- */

// candidates lists every direction the campaign's current state supports,
// with its evidence rows, score and estimate. Pure over the prep state.
func (st *prepState) candidates() []PrepDirection {
	var out []PrepDirection

	// Quests: the moves their machines currently allow.
	for _, q := range st.snap.Quests {
		var tos []string
		for _, e := range q.Machine.Edges {
			if e.From == q.CurrentState {
				tos = append(tos, e.To)
			}
		}
		if len(tos) == 0 {
			continue
		}
		sort.Strings(tos)
		d := PrepDirection{
			ID: DirectionAdvanceQuest + ":" + q.ID, Kind: DirectionAdvanceQuest,
			quest: q, tos: tos,
		}
		d.Evidence = append(d.Evidence, EvidenceRow{
			Kind: evidenceQuestEdge, Ref: q.ID, Label: q.Name,
			Note:   fmt.Sprintf("edges from %q: %s", q.CurrentState, strings.Join(tos, ", ")),
			Weight: 30,
		})
		d.Advances = fmt.Sprintf("Quest %q advances from %q along an edge its machine allows: %s.",
			q.Name, q.CurrentState, strings.Join(tos, " or "))
		d.Title = "Advance " + q.Name
		out = append(out, d)
	}

	// The dropped threads: orphan_thread and dormant_clue findings, merged
	// per fact so one fact surfaces one direction.
	threads := map[string]*PrepDirection{}
	for _, f := range st.findings {
		if f.Check != CheckOrphanThread && f.Check != CheckDormantClue {
			continue
		}
		fact, ok := factByID(st.snap, f.RecordID)
		if !ok {
			continue
		}
		d := threads[fact.ID]
		if d == nil {
			d = &PrepDirection{
				ID: DirectionReviveThread + ":" + fact.ID, Kind: DirectionReviveThread,
				fact: &fact,
			}
			threads[fact.ID] = d
		}
		weight := 18
		note := f.Message
		if f.Check == CheckOrphanThread {
			weight = 24
			if last := lastTouchSession(st.snap, fact.ID); last != nil {
				quiet := maxOrdinal(st.snap) - last.Ordinal
				if quiet > 0 {
					weight += int(min(quiet*2, 10))
					note = fmt.Sprintf("went quiet %d sessions ago", quiet)
				}
			}
		}
		d.Evidence = append(d.Evidence, EvidenceRow{
			Kind: f.Check, Ref: fact.ID, Label: truncate(fact.Statement, 80), Note: note, Weight: weight,
		})
		if st.inTail(fact.SubjectEntity) {
			d.Evidence = append(d.Evidence, EvidenceRow{
				Kind: evidencePartyHere, Ref: fact.SubjectEntity, Label: "the party was last chasing this",
				Weight: 8,
			})
		}
	}
	for _, d := range threads {
		d.Advances = fmt.Sprintf("A thread gone cold comes back into play: %s.", d.fact.Statement)
		d.Title = "Pick up the dropped thread"
		out = append(out, *d)
	}

	// Secrets nobody can reach: the unreachable_secret findings.
	for _, f := range st.findings {
		if f.Check != CheckUnreachableSecret {
			continue
		}
		fact, ok := factByID(st.snap, f.RecordID)
		if !ok {
			continue
		}
		d := PrepDirection{
			ID: DirectionSurfaceSecret + ":" + fact.ID, Kind: DirectionSurfaceSecret,
			fact: &fact,
		}
		d.Evidence = append(d.Evidence, EvidenceRow{
			Kind: CheckUnreachableSecret, Ref: fact.ID, Label: truncate(fact.Statement, 80),
			Note:   "no awareness row and no scene placement leads here",
			Weight: 28,
		})
		if st.inTail(fact.SubjectEntity) {
			d.Evidence = append(d.Evidence, EvidenceRow{
				Kind: evidencePartyHere, Ref: fact.SubjectEntity, Label: "the party was last chasing this",
				Weight: 8,
			})
		}
		d.Advances = fmt.Sprintf("A secret nobody can reach finally meets the party: %s.", fact.Statement)
		d.Title = "Surface the buried secret"
		if e, ok := entityByID(st.snap, fact.SubjectEntity); ok {
			d.Title = "Surface what " + e.Name + " hides"
		}
		out = append(out, d)
	}

	// NPC goals with no progress since they were set: not seated in any
	// scene, not present in any recorded event.
	for i := range st.snap.Entities {
		e := st.snap.Entities[i]
		if e.Kind != campaign.KindNPC || e.Status == campaign.StatusDeleted {
			continue
		}
		agent := campaign.NPCAgentOf(&e)
		if len(agent.Goals) == 0 || st.seated[e.ID] || st.played[e.ID] {
			continue
		}
		d := PrepDirection{
			ID: DirectionPushGoal + ":" + e.ID, Kind: DirectionPushGoal,
			npc: &st.snap.Entities[i],
		}
		d.Evidence = append(d.Evidence, EvidenceRow{
			Kind: evidenceNPCGoal, Ref: e.ID, Label: e.Name,
			Note:   fmt.Sprintf("goal: %s — no scene or event has advanced it", agent.Goals[0]),
			Weight: 22,
		})
		if msg, flagged := st.flagged[e.ID]; flagged {
			d.Evidence = append(d.Evidence, EvidenceRow{
				Kind: evidenceUnusedNPC, Ref: e.ID, Label: e.Name, Note: msg, Weight: 6,
			})
		}
		d.Advances = fmt.Sprintf("%s pursues %q — unresolved since it was set.", e.Name, agent.Goals[0])
		d.Title = "Push " + e.Name + "'s stalled goal"
		out = append(out, d)
	}

	// Factions whose plans are live (the graph says so) but which are
	// seated in no planned scene.
	for i := range st.snap.Entities {
		e := st.snap.Entities[i]
		if e.Kind != campaign.KindFaction || e.Status == campaign.StatusDeleted {
			continue
		}
		live := false
		for _, f := range st.snap.Facts {
			if liveFact(f) && (f.SubjectEntity == e.ID || f.ObjectEntity == e.ID) {
				live = true
				break
			}
		}
		if !live {
			for _, r := range st.snap.Relationships {
				if r.FromEntity == e.ID || r.ToEntity == e.ID {
					live = true
					break
				}
			}
		}
		if !live {
			continue
		}
		seatedPlanned := false
		for j := range st.snap.Spine.Scenes {
			sc := st.snap.Spine.Scenes[j]
			if sc.Status == story.StatusDone {
				continue
			}
			for _, c := range sc.Cast {
				if c.EntityID == e.ID {
					seatedPlanned = true
				}
			}
		}
		if seatedPlanned {
			continue
		}
		d := PrepDirection{
			ID: DirectionFactionMoves + ":" + e.ID, Kind: DirectionFactionMoves,
			npc: &st.snap.Entities[i], // the entity the build seats as focus
		}
		d.Evidence = append(d.Evidence, EvidenceRow{
			Kind: evidenceFactionLive, Ref: e.ID, Label: e.Name,
			Note:   "live in the graph, seated in no planned scene",
			Weight: 20,
		})
		if st.inTail(e.ID) {
			d.Evidence = append(d.Evidence, EvidenceRow{
				Kind: evidencePartyHere, Ref: e.ID, Label: "present in the party's last event",
				Weight: 8,
			})
		}
		d.Advances = fmt.Sprintf("%s moves on its plan offstage, and tonight the party is in the way.", e.Name)
		d.Title = e.Name + " moves"
		out = append(out, d)
	}

	// Score, estimate, and the deterministic pitch.
	for i := range out {
		d := &out[i]
		for _, ev := range d.Evidence {
			d.Score += ev.Weight
		}
		d.PrepMinutes = st.estimate(d)
		d.Pitch = d.Advances
	}
	return out
}

// estimate prices one direction: the scene mix it would be built into, the
// statblock outlook of the local bestiary, and the cast NPCs whose agent
// fields are missing.
func (st *prepState) estimate(d *PrepDirection) int {
	npcs := st.directionNPCs(d)
	missing := 0
	for _, id := range npcs {
		for i := range st.snap.Entities {
			if st.snap.Entities[i].ID != id {
				continue
			}
			agent := campaign.NPCAgentOf(&st.snap.Entities[i])
			if agent.Voice == "" || len(agent.Goals) == 0 {
				missing++
			}
		}
	}
	monsters := defaultMonstersPerEnc
	if b := encounter.Plan(st.snap.Party, "", encounter.Objective{}); len(b.Shapes) > 0 {
		for _, sh := range b.Shapes {
			if sh.Key == "pack" {
				monsters = sh.Count
			}
		}
	}
	return EstimatePrep(EstimateInput{
		Mix: st.mix, Party: st.snap.Party, Band: encounter.DefaultBand,
		BestiarySynced:       len(st.snap.Bestiary) > 0,
		MonstersPerEncounter: monsters, NPCsMissingAgent: missing,
	})
}

// directionNPCs lists the NPC entities a direction would seat as cast, for
// the estimate's missing-agent count and the offline build's cast.
func (st *prepState) directionNPCs(d *PrepDirection) []string {
	var ids []string
	switch d.Kind {
	case DirectionPushGoal, DirectionFactionMoves:
		if d.npc != nil {
			ids = append(ids, d.npc.ID)
		}
	case DirectionSurfaceSecret, DirectionReviveThread:
		if d.fact != nil && d.fact.SubjectEntity != "" {
			if e, ok := entityByID(st.snap, d.fact.SubjectEntity); ok &&
				(e.Kind == campaign.KindNPC || e.Kind == campaign.KindFaction) {
				ids = append(ids, e.ID)
			}
		}
	}
	return ids
}

/* ---------- the directions endpoint's engine ---------- */

// Directions computes the ranked direction list. The ranking, the evidence
// and the estimates are deterministic and need no model; a configured model
// only fills the prose titles and pitches.
func (s *Store) Directions(ctx context.Context, in DirectionsInput) (*DirectionsResult, error) {
	in.Notes = strings.TrimSpace(in.Notes)
	if len([]rune(in.Notes)) > planNotesMaxLen {
		return nil, fmt.Errorf("%w: the notes are longer than %d characters", ErrInvalid, planNotesMaxLen)
	}
	st, err := loadPrepState(ctx, s, in.CampaignID)
	if err != nil {
		return nil, err
	}

	res := &DirectionsResult{
		CampaignID:       in.CampaignID,
		CampaignName:     campaignName(ctx, s.db, in.CampaignID),
		Act:              st.act,
		Budget:           st.budget,
		Mix:              st.mix,
		Party:            st.snap.Party,
		WhereThePartyIs:  st.whereThePartyIs(),
		DriftedEncounter: st.drifted,
		Offline:          s.model == nil,
		Directions:       []PrepDirection{},
	}
	res.GeneratedMS = s.now().UnixMilli()

	cands := st.candidates()
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		ri, rj := directionKindRank[cands[i].Kind], directionKindRank[cands[j].Kind]
		if ri != rj {
			return ri < rj
		}
		return cands[i].Title < cands[j].Title
	})
	if len(cands) > prepDirectionCap {
		cands = cands[:prepDirectionCap]
	}

	if s.model != nil {
		if err := s.writePitches(ctx, st, in.Notes, cands); err != nil {
			return nil, err
		}
	}
	res.Directions = cands
	return res, nil
}

// writePitches asks the model for the prose coat over the ranked list: one
// title and one pitch per direction, in the campaign's own language. The
// model chooses words, never order — the ranking is already fixed.
func (s *Store) writePitches(ctx context.Context, st *prepState, notes string, dirs []PrepDirection) error {
	if len(dirs) == 0 {
		return nil
	}
	type pitchDirection struct {
		Kind         string   `json:"kind"`
		WorkingTitle string   `json:"working_title"`
		Advances     string   `json:"advances"`
		PrepMinutes  int      `json:"prep_minutes"`
		Evidence     []string `json:"evidence"`
	}
	type pitchStructure struct {
		Campaign        string           `json:"campaign"`
		Premise         string           `json:"premise,omitempty"`
		WhereThePartyIs string           `json:"where_the_party_is,omitempty"`
		DMNotes         string           `json:"dm_notes,omitempty"`
		Directions      []pitchDirection `json:"directions"`
	}
	pst := pitchStructure{
		Campaign:        campaignName(ctx, s.db, st.snap.CampaignID),
		WhereThePartyIs: st.whereThePartyIs(),
		DMNotes:         notes,
	}
	// The premise, straight off the row the pitch writer needs for tone.
	_ = s.db.QueryRowContext(ctx,
		`SELECT premise FROM campaigns WHERE id = ?`, st.snap.CampaignID).Scan(&pst.Premise)
	for i := range dirs {
		pd := pitchDirection{
			Kind: dirs[i].Kind, WorkingTitle: dirs[i].Title,
			Advances: dirs[i].Advances, PrepMinutes: dirs[i].PrepMinutes,
		}
		for _, ev := range dirs[i].Evidence {
			pd.Evidence = append(pd.Evidence, ev.Label)
		}
		pst.Directions = append(pst.Directions, pd)
	}
	fields := make([]FieldSpec, 0, len(dirs)*2)
	for i := range dirs {
		fields = append(fields,
			FieldSpec{Key: fmt.Sprintf("direction_%d_title", i+1), Required: true, MaxLen: 60,
				Desc: fmt.Sprintf("A title for direction %d (%s), in the campaign's own language.", i+1, dirs[i].Kind)},
			FieldSpec{Key: fmt.Sprintf("direction_%d_pitch", i+1), Required: true, MaxLen: 280,
				Desc: fmt.Sprintf("One or two sentences pitching direction %d to the DM: what tonight looks like if they run it.", i+1)},
		)
	}
	gen, err := s.Generate(ctx, GenerateInput{
		System:    prepPitchSystemPrompt,
		Structure: pst,
		Fields:    fields,
		Note:      "Fill every declared field. The directions are already ranked; you are naming and pitching them, not reordering or inventing new ones.",
	})
	if err != nil {
		return err
	}
	for i := range dirs {
		title, _ := gen.Values[fmt.Sprintf("direction_%d_title", i+1)].(string)
		pitch, _ := gen.Values[fmt.Sprintf("direction_%d_pitch", i+1)].(string)
		if t := strings.TrimSpace(title); t != "" {
			dirs[i].Title = t
		}
		if p := strings.TrimSpace(pitch); p != "" {
			dirs[i].Pitch = p
		}
	}
	return nil
}

const prepPitchSystemPrompt = `You are Grimoire's session-prep pitch writer. You are handed a campaign's state and five already-ranked directions for tonight's session — what each advances, what it costs in prep minutes, and the evidence rows behind its score. Your job is words: a short title and a one-or-two-sentence pitch per direction, in the campaign's own language and tone.

STRICT RULES
1. Every field is a plain string. Fill every required one. No markdown, no commentary.
2. Pitch the direction you were handed, in the order you were handed it. Never invent a sixth direction, never merge two, never reoder.
3. Ground every pitch in the advances sentence and the evidence labels: real quests, real names, real secrets. Nothing outside what you were handed.
4. Present tense, concrete, no filler.`

/* ---------- the build ---------- */

// BuildPrep turns one chosen direction into a runnable session: the plan and
// its scenes (through the scene designer's fill path when a model is wired,
// a deterministic scaffold when not), planned encounters for the combat
// scenes, the read-only prep package, and its Markdown export as a session
// source the existing exporter renders.
func (s *Store) BuildPrep(ctx context.Context, in BuildInput) (*BuildResult, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	in.DirectionID = strings.TrimSpace(in.DirectionID)
	if in.DirectionID == "" {
		return nil, fmt.Errorf("%w: direction_id is required (run /prep/directions first)", ErrInvalid)
	}
	in.Notes = strings.TrimSpace(in.Notes)
	if len([]rune(in.Notes)) > planNotesMaxLen {
		return nil, fmt.Errorf("%w: the notes are longer than %d characters", ErrInvalid, planNotesMaxLen)
	}
	in.Band = strings.TrimSpace(in.Band)
	if in.Band == "" {
		in.Band = encounter.DefaultBand
	} else if !prepBandOK(in.Band) {
		return nil, fmt.Errorf("%w: band %q is not one of %s", ErrInvalid, in.Band, strings.Join(encounter.Bands, ", "))
	}

	st, err := loadPrepState(ctx, s, in.CampaignID)
	if err != nil {
		return nil, err
	}
	if st.act == nil {
		return nil, fmt.Errorf("%w: the campaign has no acts to plan into; design a skeleton or create acts by hand first", ErrInvalid)
	}

	// The chosen direction is looked up in a freshly computed list — the id
	// is only meaningful against the state it was scored from.
	cands := st.candidates()
	var chosen *PrepDirection
	for i := range cands {
		if cands[i].ID == in.DirectionID {
			chosen = &cands[i]
			break
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("%w: direction %q is not one this campaign currently offers; re-run /prep/directions", ErrInvalid, in.DirectionID)
	}

	stories, err := story.New(s.db)
	if err != nil {
		return nil, err
	}
	sessions, err := gamesession.New(s.db)
	if err != nil {
		return nil, err
	}

	// The session: the first planned session with no scenes, else a new one
	// — the same non-destructive rule the planner follows.
	sessionID := ""
	for _, ses := range st.snap.Sessions {
		if ses.Status != "planned" {
			continue
		}
		has := false
		for i := range st.snap.Spine.Scenes {
			if st.snap.Spine.Scenes[i].SessionID == ses.ID {
				has = true
				break
			}
		}
		if !has {
			sessionID = ses.ID
			break
		}
	}
	if sessionID == "" {
		ses, err := sessions.CreateSession(ctx, in.CampaignID, "")
		if err != nil {
			return nil, err
		}
		sessionID = ses.ID
	}

	res := &BuildResult{
		Direction: *chosen, SessionID: sessionID, Act: *st.act,
		Offline: s.model == nil,
	}
	res.GeneratedMS = s.now().UnixMilli()

	var batchItems []BatchItemInput
	if s.model != nil {
		// Through the scene designer: the same pools, the same validation,
		// the same spine writes the planner uses — steered by the
		// direction's notes.
		notes := "Tonight's direction: " + chosen.Advances
		if in.Notes != "" {
			notes += " DM notes: " + in.Notes
		}
		ps, err := s.planOneSession(ctx, prepPlanOneIn(st, stories, st.act, sessionID, notes))
		if err != nil {
			return nil, err
		}
		res.Goal = ps.Goal
		res.Scenes = ps.Scenes
		res.Dropped = ps.Dropped
		batchItems = ps.batchItems
	} else {
		goal, scenes, dropped, err := s.buildOfflineScenes(ctx, st, stories, sessionID, chosen, in.Notes)
		if err != nil {
			return nil, err
		}
		res.Goal, res.Scenes, res.Dropped = goal, scenes, dropped
	}

	// The encounters: one per combat scene, budgeted against the party's
	// current levels through the encounter designer's own arithmetic.
	res.Encounters = []PrepEncounter{}
	for _, sc := range res.Scenes {
		if sc.Kind != story.KindCombat {
			continue
		}
		enc, err := s.buildPrepEncounter(ctx, sessions, sessionID, sc, *chosen, st.snap.Party, in.Band, in.Catalog)
		if err != nil {
			return nil, err
		}
		res.Encounters = append(res.Encounters, *enc)
	}

	if len(batchItems) > 0 {
		var promptRecord strings.Builder
		fmt.Fprintf(&promptRecord, "direction %s (%s) | %s", chosen.ID, chosen.Kind, chosen.Advances)
		for _, ev := range chosen.Evidence {
			fmt.Fprintf(&promptRecord, " | %s: %s", ev.Kind, ev.Label)
		}
		batch, err := s.StageBatch(ctx, BatchInput{
			CampaignID: in.CampaignID, Source: BatchSourceSessionPrep,
			Prompt:    promptRecord.String(),
			CreatedBy: in.CreatedBy, Items: batchItems,
		})
		if err != nil {
			return nil, err
		}
		res.Batch = batch
	}

	// The prep package, and its Markdown through the session's own export
	// path: a dm_notes source the exporter renders verbatim.
	pkg, err := s.assemblePrepPackage(ctx, st, sessions, res.Scenes)
	if err != nil {
		return nil, err
	}
	res.Package = *pkg
	srcID, err := s.putPrepSource(ctx, sessions, sessionID, in.CreatedBy, renderPrepMarkdown(res))
	if err != nil {
		return nil, err
	}
	res.MarkdownSourceID = srcID
	return res, nil
}

// prepPlanOneIn assembles the scene designer's input from the prep state —
// the same pools GeneratePlan computes, with the quest edges taken from
// every quest so the chosen direction's edge always validates.
func prepPlanOneIn(st *prepState, stories *story.Store, act *story.Act, sessionID, notes string) planOneIn {
	snap := st.snap
	secretPool := unreachedSecrets(snap)
	awarePool := awarenessCandidates(snap)
	questPool := readyQuestEdges(snap)
	castPool := planCastPool(snap)
	settings := settingPool(snap, act.ID)

	secretIdx, secretStmt := newPoolIndex(), map[string]string{}
	for _, f := range secretPool {
		secretIdx.add(f.ID, f.ID)
		secretStmt[f.ID] = f.Statement
	}
	awareIdx, awareStmt := newPoolIndex(), map[string]string{}
	for _, f := range awarePool {
		awareIdx.add(f.ID, f.ID)
		awareStmt[f.ID] = f.Statement
	}
	castIdx := newPoolIndex()
	for _, c := range castPool {
		castIdx.add(c.entity.ID, c.entity.Name)
	}
	settingIdx := newPoolIndex()
	for _, e := range settings {
		settingIdx.add(e.ID, e.Name)
	}
	edgeOK := map[string]story.QuestTransition{}
	for _, q := range snap.Quests {
		for _, e := range q.Machine.Edges {
			if e.From != q.CurrentState {
				continue
			}
			edgeOK[q.ID+"\x00"+e.To] = story.QuestTransition{
				QuestID: q.ID, From: q.CurrentState, To: e.To,
			}
		}
	}
	return planOneIn{
		snap: snap, stories: stories, act: act, sessionID: sessionID,
		budget: st.budget, mix: st.mix, notes: notes,
		secretIdx: secretIdx, secretStmt: secretStmt, usedSecrets: map[string]bool{},
		awareIdx: awareIdx, awareStmt: awareStmt, castIdx: castIdx,
		settingIdx: settingIdx, edgeOK: edgeOK,
		questPool: questPool, castPool: castPool, settings: settings,
		secretPool: secretPool, awarePool: awarePool,
	}
}

// buildOfflineScenes writes the deterministic scaffold when no model is
// configured: the budget and mix are the arithmetic's, the cast is the
// direction's own NPCs, and the outcomes point somewhere real — the next
// scene, or the quest edge the direction is about.
func (s *Store) buildOfflineScenes(ctx context.Context, st *prepState, stories *story.Store, sessionID string, d *PrepDirection, notes string) (string, []story.Scene, []string, error) {
	cid := st.snap.CampaignID
	goal := d.Title + ": " + d.Advances
	if notes != "" {
		goal += " " + notes
	}
	setting := ""
	if st.tail != nil && st.tail.LocationEntity != "" {
		if e, ok := entityByID(st.snap, st.tail.LocationEntity); ok && e.Kind == campaign.KindLocation && e.Status != campaign.StatusDeleted {
			setting = e.ID
		}
	}
	sceneIDs := make([]string, len(st.mix))
	for i, kind := range st.mix {
		sc, err := stories.CreateScene(ctx, cid, st.act.ID, sessionID, kind,
			fmt.Sprintf("%s (%s)", d.Title, kind), d.Advances, setting)
		if err != nil {
			return "", nil, nil, err
		}
		sceneIDs[i] = sc.ID
	}
	var dropped []string
	for i := range st.mix {
		for _, id := range st.directionNPCs(d) {
			role := story.RoleFocus
			if e, ok := entityByID(st.snap, id); ok && e.Kind == campaign.KindFaction {
				role = story.RoleOffstage
			}
			if _, err := stories.AddCast(ctx, cid, sceneIDs[i], id, role); err != nil {
				dropped = append(dropped, fmt.Sprintf("cast %s: %v", id, err))
			}
		}
		if d.Kind == DirectionSurfaceSecret && d.fact != nil && i == 0 {
			if _, err := stories.SetSecret(ctx, cid, sceneIDs[0], d.fact.ID, story.DispositionInPlay); err != nil {
				dropped = append(dropped, fmt.Sprintf("secret: %v", err))
			}
		}
		if i < len(st.mix)-1 {
			if _, err := stories.AddOutcome(ctx, cid, sceneIDs[i], "A",
				"The direction holds; the session moves on.", sceneIDs[i+1], nil); err != nil {
				dropped = append(dropped, fmt.Sprintf("outcome scene %d: %v", i+1, err))
			}
		}
	}
	// The advance_quest direction's payoff: the last scene promises the
	// quest move the direction is about, validated against the machine by
	// AddOutcome itself.
	if d.Kind == DirectionAdvanceQuest && len(d.tos) > 0 {
		tr := story.QuestTransition{QuestID: d.quest.ID, From: d.quest.CurrentState, To: d.tos[0]}
		if _, err := stories.AddOutcome(ctx, cid, sceneIDs[len(sceneIDs)-1], "A",
			fmt.Sprintf("The quest turns: %s.", d.tos[0]), "", &tr); err != nil {
			dropped = append(dropped, fmt.Sprintf("quest outcome: %v", err))
		}
	}
	status := story.PlanStatusPlanned
	if _, err := stories.PutPlan(ctx, cid, sessionID, st.act.ID, goal, "", &status); err != nil {
		return "", nil, nil, err
	}
	scenes, err := stories.ListScenes(ctx, campaign.ScopeDM, cid, st.act.ID)
	if err != nil {
		return "", nil, nil, err
	}
	var mine []story.Scene
	for i := range scenes {
		if scenes[i].SessionID != sessionID {
			continue
		}
		sc, err := stories.GetScene(ctx, campaign.ScopeDM, cid, scenes[i].ID)
		if err != nil {
			return "", nil, nil, err
		}
		mine = append(mine, *sc)
	}
	return goal, mine, dropped, nil
}

// buildPrepEncounter designs one combat scene's encounter through the
// encounter designer's arithmetic — Plan budgets it against the party's
// current levels, BuildPool shortlists what the local bestiary vouches for,
// the pick is deterministic, Evaluate verifies — and writes it as a
// planned-encounter session event, the payload contract the canon engine's
// encounter checks read.
func (s *Store) buildPrepEncounter(ctx context.Context, sessions *gamesession.Store, sessionID string, sc story.Scene, d PrepDirection, party []int, band string, cat *encounter.Catalog) (*PrepEncounter, error) {
	budget := encounter.Plan(party, band, encounter.Objective{})
	enc := &PrepEncounter{SceneID: sc.ID, Party: budget.Party, Band: budget.Band}

	monsters := pickPrepRoster(cat, budget, sc.Name+" "+sc.Purpose+" "+d.Advances)
	if len(monsters) == 0 {
		// No mirror or empty pool: the scene keeps its slot, the roster is
		// the DM's to build — the estimate priced exactly this.
		enc.HandBuilt = true
		name := sc.Name + " — roster to build"
		ev, err := sessions.AddEvent(ctx, sessionID, gamesession.EventEncounter, name,
			fmt.Sprintf("Session prep planned this combat against %s; the local bestiary offered no roster.", budget.Band),
			map[string]any{"name": name, "party": budget.Party})
		if err != nil {
			return nil, err
		}
		enc.EventID, enc.Name = ev.ID, name
		return enc, nil
	}
	enc.Monsters = monsters
	enc.Verdict = encounter.Evaluate(budget.Party, monsters)
	enc.Name = fmt.Sprintf("%s — %s", sc.Name, encounter.Describe(monsters))
	detail := fmt.Sprintf("Planned by session prep: %s (target %d adjusted XP, band %s).",
		enc.Verdict.Difficulty, budget.TargetXP, budget.Band)
	ev, err := sessions.AddEvent(ctx, sessionID, gamesession.EventEncounter, enc.Name, detail,
		map[string]any{"name": enc.Name, "party": budget.Party, "monsters": monsters})
	if err != nil {
		return nil, err
	}
	enc.EventID = ev.ID
	return enc, nil
}

// pickPrepRoster deterministically picks a roster from the pool: the pack
// shape's count of each standard candidate in turn, scored by how close the
// evaluated encounter lands to the budget's target.
func pickPrepRoster(cat *encounter.Catalog, budget encounter.Budget, idea string) []encounter.Monster {
	if cat == nil {
		return nil
	}
	hints := encounter.ReadIdea(idea)
	pool := encounter.BuildPool(cat, nil, budget, hints, nil)
	if pool.Len() == 0 {
		pool = encounter.BuildPool(cat, nil, budget, encounter.Hints{}, nil)
	}
	count := defaultMonstersPerEnc
	for _, sh := range budget.Shapes {
		if sh.Key == "pack" {
			count = sh.Count
		}
	}
	// Standard first (the workhorse build), then minions, then a boss, and
	// the flavour tier — the direction's own words — as the last resort.
	var candidates []encounter.Creature
	candidates = append(candidates, pool.Standard...)
	candidates = append(candidates, pool.Minion...)
	candidates = append(candidates, pool.Boss...)
	candidates = append(candidates, pool.Flavour...)
	best, bestGap := 0, 1<<62
	for i, c := range candidates {
		if i >= 8 || c.XP <= 0 {
			continue
		}
		monsters := []encounter.Monster{{Name: c.Name, CR: c.CR, XP: c.XP, Count: count}}
		v := encounter.Evaluate(budget.Party, monsters)
		gap := v.AdjustedXP - budget.TargetXP
		if gap < 0 {
			gap = -gap
		}
		if gap < bestGap {
			best, bestGap = i, gap
		}
	}
	if len(candidates) == 0 || candidates[best].XP <= 0 {
		return nil
	}
	c := candidates[best]
	return []encounter.Monster{{Name: c.Name, CR: c.CR, XP: c.XP, Count: count}}
}

/* ---------- the prep package ---------- */

const (
	prepQuestionsCap    = 5
	prepRulingsScenes   = 3
	prepRulingsPerScene = 2
)

// assemblePrepPackage builds the read-only prep material. The NPC sheets
// read at npc:<id> scope through the knowledge store — the SQL awareness
// join is the guarantee, not a filter applied after the fact.
func (s *Store) assemblePrepPackage(ctx context.Context, st *prepState, sessions *gamesession.Store, scenes []story.Scene) (*PrepPackage, error) {
	pkg := &PrepPackage{
		WhereThePartyIs: st.whereThePartyIs(),
		Party:           st.snap.Party,
		NPCSheets:       []NPCSheet{}, Questions: []PlayerQuestion{},
		Contingencies: []Contingency{}, PriorRulings: []PriorRulingRef{},
	}

	// The cast NPCs, in first-appearance order.
	var order []string
	seen := map[string]bool{}
	for _, sc := range scenes {
		for _, c := range sc.Cast {
			if seen[c.EntityID] {
				continue
			}
			seen[c.EntityID] = true
			if e, ok := entityByID(st.snap, c.EntityID); ok && e.Kind == campaign.KindNPC && e.Status != campaign.StatusDeleted {
				order = append(order, c.EntityID)
			}
		}
	}
	for _, id := range order {
		e, _ := entityByID(st.snap, id)
		agent := campaign.NPCAgentOf(&e)
		sheet := NPCSheet{EntityID: id, Name: e.Name, Voice: agent.Voice, Knows: []string{}}
		if len(agent.Goals) > 0 {
			sheet.Goal = agent.Goals[0]
		}
		if len(agent.Fears) > 0 {
			sheet.Fears = agent.Fears[0]
		}
		facts, err := s.knowledge.Facts(ctx, knowledge.ScopeNPC(id), st.snap.CampaignID, knowledge.FactFilter{})
		if err != nil {
			return nil, fmt.Errorf("npc sheet for %s: %w", e.Name, err)
		}
		for _, f := range facts {
			sheet.Knows = append(sheet.Knows, f.Statement)
		}
		pkg.NPCSheets = append(pkg.NPCSheets, sheet)
	}

	// Likely player questions: the gap between what the party believes and
	// what is true. believes_false is the valuable one — the party is
	// confidently wrong and will act on it.
	partySide := map[string]bool{campaign.PartyKnower: true}
	for _, e := range st.snap.Entities {
		if e.Kind == campaign.KindPC && e.Status != campaign.StatusDeleted {
			partySide[e.ID] = true
		}
	}
	believed := map[string]bool{}
	var wrong, suspected []PlayerQuestion
	for _, a := range st.snap.Awareness {
		if !partySide[a.Knower] || believed[a.FactID] {
			continue
		}
		switch a.Stance {
		case knowledge.StanceBelievesFalse:
			believed[a.FactID] = true
			if f, ok := factByID(st.snap, a.FactID); ok {
				wrong = append(wrong, PlayerQuestion{
					Kind: "wrong", Statement: f.Statement,
					Note: "the party is confidently wrong about this — expect the question, and the argument",
				})
			}
		case knowledge.StanceSuspects:
			believed[a.FactID] = true
			if f, ok := factByID(st.snap, a.FactID); ok {
				suspected = append(suspected, PlayerQuestion{
					Kind: "suspected", Statement: f.Statement,
					Note: "the party suspects but has not confirmed — expect them to push",
				})
			}
		}
	}
	for _, q := range append(wrong, suspected...) {
		if len(pkg.Questions) >= prepQuestionsCap {
			break
		}
		pkg.Questions = append(pkg.Questions, q)
	}

	// Contingencies: scene outcomes with nothing downstream.
	for _, sc := range scenes {
		if len(sc.Outcomes) == 0 {
			pkg.Contingencies = append(pkg.Contingencies, Contingency{
				SceneID: sc.ID, Scene: sc.Name,
				Note: "no branches planned — anything can happen here",
			})
			continue
		}
		for _, o := range sc.Outcomes {
			if o.LeadsToScene != "" || o.QuestTransition != nil {
				continue
			}
			pkg.Contingencies = append(pkg.Contingencies, Contingency{
				SceneID: sc.ID, Scene: sc.Name, Label: o.Label, Summary: o.Summary,
				Note: "resolves to nothing planned yet",
			})
		}
	}

	// Prior rulings likely to come up, per scene, through the session
	// layer's FTS matcher. No model.
	seenRuling := map[string]bool{}
	for i, sc := range scenes {
		if i >= prepRulingsScenes {
			break
		}
		matches, err := sessions.MatchPriorRulings(ctx, st.snap.CampaignID, sc.Name+" "+sc.Purpose, "", prepRulingsPerScene)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if seenRuling[m.EventID] {
				continue
			}
			seenRuling[m.EventID] = true
			pkg.PriorRulings = append(pkg.PriorRulings, PriorRulingRef{
				SceneID: sc.ID, EventID: m.EventID, Session: m.Session,
				Ordinal: m.Ordinal, Summary: m.Summary,
			})
		}
	}
	return pkg, nil
}

// prepSourceTitle names the dm_notes source the prep package exports as, so
// a rebuild replaces the previous one instead of stacking copies. Prep
// sources are generated artifacts, not records: nothing ever targets a span
// into them, so replacement loses nothing.
const prepSourceTitle = "Session prep package"

// putPrepSource replaces the session's prep-package source with a fresh
// render and returns its id.
func (s *Store) putPrepSource(ctx context.Context, sessions *gamesession.Store, sessionID, author, markdown string) (string, error) {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM session_sources WHERE session_id = ? AND kind = ? AND title = ?`,
		sessionID, gamesession.SourceDMNotes, prepSourceTitle); err != nil {
		return "", fmt.Errorf("replace prep source: %w", err)
	}
	src, err := sessions.AddSource(ctx, sessionID, gamesession.SourceDMNotes, author, prepSourceTitle, markdown, nil)
	if err != nil {
		return "", err
	}
	return src.ID, nil
}

/* ---------- the Markdown render ---------- */

// renderPrepMarkdown lays the package out for the session's export path.
func renderPrepMarkdown(res *BuildResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Session prep — %s\n\n", res.Direction.Title)
	fmt.Fprintf(&b, "- **Session:** %s\n", res.SessionID)
	fmt.Fprintf(&b, "- **Act:** %s (levels %d-%d)\n", res.Act.Name, res.Act.LevelStart, res.Act.LevelEnd)
	fmt.Fprintf(&b, "- **Prep estimate:** %d minutes\n", res.Direction.PrepMinutes)
	if res.Package.WhereThePartyIs != "" {
		fmt.Fprintf(&b, "- **Where the party is:** %s\n", res.Package.WhereThePartyIs)
	}
	if len(res.Package.Party) > 0 {
		fmt.Fprintf(&b, "- **Party levels:** %s\n", intList(res.Package.Party))
	}
	fmt.Fprintf(&b, "\n%s\n\n", res.Direction.Pitch)

	b.WriteString("## The plan\n\n")
	for i, sc := range res.Scenes {
		fmt.Fprintf(&b, "%d. **%s** (%s)", i+1, sc.Name, sc.Kind)
		if sc.Purpose != "" {
			fmt.Fprintf(&b, " — %s", sc.Purpose)
		}
		b.WriteString("\n")
		for _, o := range sc.Outcomes {
			fmt.Fprintf(&b, "   - %s: %s\n", o.Label, o.Summary)
		}
	}
	b.WriteString("\n")

	if len(res.Encounters) > 0 {
		b.WriteString("## Encounters\n\n")
		for _, e := range res.Encounters {
			if e.HandBuilt {
				fmt.Fprintf(&b, "- **%s** — roster to build by hand\n", e.Name)
				continue
			}
			fmt.Fprintf(&b, "- **%s** — %s (%s, %d adjusted XP)\n",
				e.Name, encounter.Describe(e.Monsters), e.Verdict.Difficulty, e.Verdict.AdjustedXP)
		}
		b.WriteString("\n")
	}

	b.WriteString("## NPC quick reference\n\n")
	if len(res.Package.NPCSheets) == 0 {
		b.WriteString("_No cast NPCs in tonight's scenes._\n\n")
	}
	for _, sheet := range res.Package.NPCSheets {
		fmt.Fprintf(&b, "### %s\n\n", sheet.Name)
		if sheet.Voice != "" {
			fmt.Fprintf(&b, "- **Voice:** %s\n", sheet.Voice)
		}
		if sheet.Goal != "" {
			fmt.Fprintf(&b, "- **Goal:** %s\n", sheet.Goal)
		}
		if sheet.Fears != "" {
			fmt.Fprintf(&b, "- **Fears:** %s\n", sheet.Fears)
		}
		if len(sheet.Knows) == 0 {
			b.WriteString("- **Knows:** nothing recorded at this NPC's awareness\n")
		} else {
			b.WriteString("- **Knows:**\n")
			for _, k := range sheet.Knows {
				fmt.Fprintf(&b, "  - %s\n", k)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("## Likely player questions\n\n")
	if len(res.Package.Questions) == 0 {
		b.WriteString("_No gaps between what the party believes and what is true._\n\n")
	}
	for _, q := range res.Package.Questions {
		fmt.Fprintf(&b, "- (%s) %s — %s\n", q.Kind, q.Statement, q.Note)
	}
	b.WriteString("\n")

	b.WriteString("## Contingencies\n\n")
	if len(res.Package.Contingencies) == 0 {
		b.WriteString("_Every branch resolves somewhere._\n\n")
	}
	for _, c := range res.Package.Contingencies {
		line := fmt.Sprintf("- **%s**", c.Scene)
		if c.Label != "" {
			line += fmt.Sprintf(" %s", c.Label)
		}
		if c.Summary != "" {
			line += fmt.Sprintf(": %s", c.Summary)
		}
		fmt.Fprintf(&b, "%s — %s\n", line, c.Note)
	}
	b.WriteString("\n")

	b.WriteString("## Prior rulings likely to come up\n\n")
	if len(res.Package.PriorRulings) == 0 {
		b.WriteString("_No prior rulings match tonight's scenes._\n\n")
	}
	for _, r := range res.Package.PriorRulings {
		fmt.Fprintf(&b, "- Session %d (%s): %s\n", r.Ordinal, r.Session, r.Summary)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

/* ---------- small helpers ---------- */

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func intList(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return strings.Join(parts, ", ")
}

// prepBandOK reports whether band names one of the encounter designer's
// difficulty bands, case-insensitively.
func prepBandOK(band string) bool {
	for _, b := range encounter.Bands {
		if strings.EqualFold(band, b) {
			return true
		}
	}
	return false
}
