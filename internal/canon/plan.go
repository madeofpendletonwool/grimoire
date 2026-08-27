package canon

// The story planner (MAD-362, stage 2 of MAD-314's design tools): plan the
// next session — or a whole act's worth — forward from the campaign as it
// actually stands.
//
// Where the skeleton generator works top-down from a premise, this works
// forward from what has already happened and what the party knows, and every
// load-bearing decision is computed before a prompt is built:
//
//   - Candidate material is selected, not invented. The engine snapshot
//     already holds the quests and the edges their state machines currently
//     allow, the secret facts nobody has reached, and the NPCs with
//     unresolved goals. The planner scores that material into shortlists and
//     the model chooses among real things — it never decides what the
//     campaign is about.
//   - The scene budget is arithmetic: story.ScenesPerSession prices a
//     session off the act's band and session count, and story.SceneMix fixes
//     the kinds from the act's structural position. A four-hour session is
//     not eleven scenes, and the mix is not a model preference.
//   - Secrets in play are rows, not paraphrases: chosen from the unreached
//     visibility='secret' set, linked by id through scene_secrets. A fact
//     the party's awareness already grants is not in the pool at all.
//   - Every outcome must name a real consequence — the next scene, a later
//     scene of the same session, a quest edge the machine actually has, or
//     an awareness change. An outcome that changes nothing is dropped before
//     the batch is staged.
//
// What lands where follows the split the skeleton generator set: scenes,
// cast, secrets, outcomes and new session plans are spine rows — plan, not
// canon — written the way a DM writing them by hand would, editable through
// the same story endpoints. The canon the plan would touch, the awareness
// changes its outcomes promise, is staged as ONE proposal batch behind the
// review gate (source 'story_plan'), because awareness is truth and truth is
// decided (ADR 3).
//
// Re-planning is non-destructive: a new plan for a session that already has
// one appends a second candidate set of scenes beside the first, stages a
// new batch, and leaves the existing plan row and the existing scenes
// exactly as they were — the DM compares and deletes the loser by hand,
// never an in-place overwrite of accepted material.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

/* ---------- input bounds ---------- */

const (
	planNotesMaxLen   = 1000
	planCastPoolCap   = 16
	planSecretPoolCap = 8
	planQuestPoolCap  = 6
	planAwarePoolCap  = 12
	planSettingCap    = 8
)

// outcomeLabels are the branch labels, in the order scenes are branched.
var outcomeLabels = []string{"A", "B", "C", "D"}

// Planning modes.
const (
	PlanModeSession = "session"
	PlanModeAct     = "act"
)

// PlanInput is one planning request: the mode (next session, or a whole
// act's worth of sessions), an optional act id, and the DM's notes on what
// tonight should be about.
type PlanInput struct {
	CampaignID string
	Mode       string
	ActID      string
	Notes      string
	CreatedBy  string
}

// PlannedSession is one session the planner wrote: the scenes it gained, the
// goal proposed for it, and what the generator dropped rather than staged.
type PlannedSession struct {
	SessionID string        `json:"session_id"`
	ActID     string        `json:"act_id"`
	Goal      string        `json:"goal,omitempty"`
	NewPlan   bool          `json:"new_plan"`
	Scenes    []story.Scene `json:"scenes"`
	Dropped   []string      `json:"dropped,omitempty"`

	// batchItems are the awareness changes this session's outcomes promise,
	// staged by the caller as one batch.
	batchItems []BatchItemInput
}

// PlanResult is what one planner run produced: the sessions planned, the
// awareness batch staged behind the review gate (nil when the plan promises
// no canon changes — nothing to gate), and the budget the scenes were held to.
type PlanResult struct {
	Act       story.Act        `json:"act"`
	Budget    int              `json:"budget"`
	Mix       []string         `json:"mix"`
	Sessions  []PlannedSession `json:"sessions"`
	Batch     *Batch           `json:"batch,omitempty"`
	Generated time.Time        `json:"generated_at"`
}

/* ---------- the deterministic shortlists ---------- */

// partyGrantedFacts is the set of fact ids the party's side already holds a
// granting stance on — party rows plus each pc's, the same join
// story.Validate's secret rule runs. A fact in this set is not secret
// material anymore: placing it in play wastes the scene's slot.
func partyGrantedFacts(snap *Snapshot) map[string]bool {
	pcs := map[string]bool{}
	for _, e := range snap.Entities {
		if e.Kind == campaign.KindPC && e.Status != campaign.StatusDeleted {
			pcs[e.ID] = true
		}
	}
	granted := map[string]bool{}
	for _, a := range snap.Awareness {
		if (a.Knower == campaign.PartyKnower || pcs[a.Knower]) && grantingStance(a.Stance) {
			granted[a.FactID] = true
		}
	}
	return granted
}

// liveFact reports whether a fact renders anywhere at all: not superseded,
// not a proposal awaiting a decision.
func liveFact(f campaign.Fact) bool {
	return f.SupersededBy == "" && f.Confidence != campaign.ConfidenceProposed
}

// unreachedSecrets lists the secret facts nobody on the party's side has
// reached, oldest first — the pool a scene's secrets are chosen from. Rows,
// not paraphrases: the planner hands the ids on and links them by id.
func unreachedSecrets(snap *Snapshot) []campaign.Fact {
	granted := partyGrantedFacts(snap)
	var out []campaign.Fact
	for _, f := range snap.Facts {
		if f.Visibility != campaign.VisibilitySecret || !liveFact(f) || granted[f.ID] {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if len(out) > planSecretPoolCap {
		out = out[:planSecretPoolCap]
	}
	return out
}

// awarenessCandidates lists the facts the party does not hold yet, any
// visibility — the pool an outcome's awareness change may name.
func awarenessCandidates(snap *Snapshot) []campaign.Fact {
	granted := partyGrantedFacts(snap)
	var out []campaign.Fact
	for _, f := range snap.Facts {
		if !liveFact(f) || granted[f.ID] {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if len(out) > planAwarePoolCap {
		out = out[:planAwarePoolCap]
	}
	return out
}

// questEdge is one legal next move of one quest, as the planner hands it to
// the model: the quest, where it sits, and where it may go.
type questEdge struct {
	quest campaign.Quest
	to    string
}

// readyQuestEdges lists the moves the quests' machines currently allow —
// edges out of each quest's present state, never a branch the quest cannot
// take tonight.
func readyQuestEdges(snap *Snapshot) []questEdge {
	var out []questEdge
	for _, q := range snap.Quests {
		for _, e := range q.Machine.Edges {
			if e.From != q.CurrentState {
				continue
			}
			out = append(out, questEdge{quest: q, to: e.To})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].quest.Name != out[j].quest.Name {
			return out[i].quest.Name < out[j].quest.Name
		}
		return out[i].to < out[j].to
	})
	if len(out) > planQuestPoolCap {
		out = out[:planQuestPoolCap]
	}
	return out
}

// castCandidate is one entity the planner will let the model seat on stage.
type castCandidate struct {
	entity campaign.Entity
	why    string
}

// planCastPool scores the campaign's people into the shortlist a scene's
// cast is chosen from: the party always (they are the ones being planned
// for), then the NPCs with unresolved goals — the minds with unfinished
// business — then every other live npc and faction in name order. Locations
// are settings, not cast, and stay out.
func planCastPool(snap *Snapshot) []castCandidate {
	var party, goals, rest []castCandidate
	for _, e := range snap.Entities {
		if e.Status == campaign.StatusDeleted {
			continue
		}
		switch e.Kind {
		case campaign.KindPC:
			party = append(party, castCandidate{entity: e, why: "the party"})
		case campaign.KindNPC:
			if agent := campaign.NPCAgentOf(&e); len(agent.Goals) > 0 {
				goals = append(goals, castCandidate{entity: e, why: "unresolved goal: " + agent.Goals[0]})
			} else {
				rest = append(rest, castCandidate{entity: e, why: "in the campaign"})
			}
		case campaign.KindFaction:
			rest = append(rest, castCandidate{entity: e, why: "a faction with stakes"})
		}
	}
	sort.Slice(goals, func(i, j int) bool { return goals[i].entity.Name < goals[j].entity.Name })
	sort.Slice(rest, func(i, j int) bool { return rest[i].entity.Name < rest[j].entity.Name })
	out := append(append(party, goals...), rest...)
	if len(out) > planCastPoolCap {
		out = out[:planCastPoolCap]
	}
	return out
}

// settingPool lists the locations a scene may happen in, most recently used
// first — the act's own scenes' settings lead, so a plan stays where the
// campaign already is.
func settingPool(snap *Snapshot, actID string) []campaign.Entity {
	useCount := map[string]int{}
	for i := range snap.Spine.Scenes {
		if sc := snap.Spine.Scenes[i]; sc.ActID == actID && sc.SettingEntity != "" {
			useCount[sc.SettingEntity]++
		}
	}
	var out []campaign.Entity
	for _, e := range snap.Entities {
		if e.Kind == campaign.KindLocation && e.Status != campaign.StatusDeleted {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ci, cj := useCount[out[i].ID], useCount[out[j].ID]
		if ci != cj {
			return ci > cj
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > planSettingCap {
		out = out[:planSettingCap]
	}
	return out
}

/* ---------- reference resolution against the pools ---------- */

// poolIndex resolves what the model wrote against one pool: ids exactly,
// names case-insensitively when unique. The pool is the authority — the
// model chooses among real things, it does not name new ones into being.
type poolIndex struct {
	byID   map[string]string
	byName map[string][]string
}

func newPoolIndex() *poolIndex {
	return &poolIndex{byID: map[string]string{}, byName: map[string][]string{}}
}

func (p *poolIndex) add(id, name string) {
	p.byID[id] = id
	key := strings.ToLower(strings.TrimSpace(name))
	if key != "" {
		p.byName[key] = append(p.byName[key], id)
	}
}

// resolve returns the id for a reference, and whether it resolved.
func (p *poolIndex) resolve(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	if id, ok := p.byID[ref]; ok {
		return id, true
	}
	if hits := p.byName[strings.ToLower(ref)]; len(hits) == 1 {
		return hits[0], true
	}
	return "", false
}

/* ---------- the planner ---------- */

// GeneratePlan runs one planning exchange: resolve where the campaign
// stands, compute the budget and the shortlists, fill them with one
// structured-generation call per session, write the scenes into the spine,
// and stage the promised awareness changes as one proposal batch. Nothing
// the batch carries becomes canon until DecideBatch accepts it, and nothing
// the spine already holds is overwritten.
func (s *Store) GeneratePlan(ctx context.Context, in PlanInput) (*PlanResult, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	switch in.Mode {
	case "", PlanModeSession:
		in.Mode = PlanModeSession
	case PlanModeAct:
	default:
		return nil, fmt.Errorf("%w: mode %q is not %s or %s", ErrInvalid, in.Mode, PlanModeSession, PlanModeAct)
	}
	in.Notes = strings.TrimSpace(in.Notes)
	if len([]rune(in.Notes)) > planNotesMaxLen {
		return nil, fmt.Errorf("%w: the notes are longer than %d characters", ErrInvalid, planNotesMaxLen)
	}
	if _, err := s.loadCampaign(ctx, in.CampaignID); err != nil {
		return nil, err
	}
	snap, err := LoadSnapshot(ctx, s.db, in.CampaignID)
	if err != nil {
		return nil, err
	}
	if len(snap.Spine.Acts) == 0 {
		return nil, fmt.Errorf("%w: the campaign has no acts to plan into; design a skeleton or create acts by hand first", ErrInvalid)
	}

	stories, err := story.New(s.db)
	if err != nil {
		return nil, err
	}
	sessionsStore, err := gamesession.New(s.db)
	if err != nil {
		return nil, err
	}

	act, err := resolvePlanAct(snap, in)
	if err != nil {
		return nil, err
	}

	// The sessions to plan, in ordinal order.
	planBySession := map[string]*story.SessionPlan{}
	for i := range snap.Spine.Plans {
		planBySession[snap.Spine.Plans[i].SessionID] = &snap.Spine.Plans[i]
	}
	hasScenes := func(sessionID string) bool {
		for i := range snap.Spine.Scenes {
			if snap.Spine.Scenes[i].SessionID == sessionID {
				return true
			}
		}
		return false
	}
	var targets []string
	switch in.Mode {
	case PlanModeSession:
		next := ""
		for _, ses := range snap.Sessions {
			if ses.Status == "planned" {
				next = ses.ID
				break
			}
		}
		if next == "" {
			ses, err := sessionsStore.CreateSession(ctx, in.CampaignID, "")
			if err != nil {
				return nil, err
			}
			next = ses.ID
		}
		targets = []string{next}
	default: // act
		var actSessions []string
		for _, ses := range snap.Sessions {
			if ses.Status != "planned" {
				continue
			}
			if p := planBySession[ses.ID]; p != nil && p.ActID == act.ID {
				actSessions = append(actSessions, ses.ID)
			}
		}
		if len(actSessions) == 0 {
			// A hand-built act with no sessions yet: create its paced
			// complement, the same arithmetic the skeleton generator used.
			count := story.Pace(act.LevelStart, act.LevelEnd, 1).PerAct[0].Sessions
			status := story.PlanStatusPlanned
			for i := 0; i < count; i++ {
				ses, err := sessionsStore.CreateSession(ctx, in.CampaignID, "")
				if err != nil {
					return nil, err
				}
				if _, err := stories.PutPlan(ctx, in.CampaignID, ses.ID, act.ID, "", "", &status); err != nil {
					return nil, err
				}
				actSessions = append(actSessions, ses.ID)
			}
		}
		for _, sid := range actSessions {
			if !hasScenes(sid) { // non-destructive: planned sessions keep their scenes
				targets = append(targets, sid)
			}
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("%w: every planned session of act %q already has scenes; re-plan one session at a time after deleting the set you are replacing", ErrInvalid, act.Name)
		}
	}

	// The budget: the act's session count prices each session's scenes, and
	// the act's structural position fixes the mix. Our arithmetic, not the
	// model's preference.
	actSessionCount := 0
	for _, ses := range snap.Sessions {
		if ses.Status != "planned" {
			continue
		}
		if p := planBySession[ses.ID]; p != nil && p.ActID == act.ID {
			actSessionCount++
		}
	}
	if actSessionCount == 0 {
		actSessionCount = len(targets)
	}
	budget := story.ScenesPerSession(act.LevelStart, act.LevelEnd, actSessionCount)
	actIndex, actCount := actOrdinalPosition(snap, act)
	mix := story.SceneMix(actIndex, actCount, budget)

	// The shortlists, computed before any prompt exists.
	secretPool := unreachedSecrets(snap)
	awarePool := awarenessCandidates(snap)
	questPool := readyQuestEdges(snap)
	castPool := planCastPool(snap)
	settings := settingPool(snap, act.ID)

	secretIdx := newPoolIndex()
	secretStmt := map[string]string{}
	for _, f := range secretPool {
		secretIdx.add(f.ID, f.ID)
		secretStmt[f.ID] = f.Statement
	}
	awareIdx := newPoolIndex()
	awareStmt := map[string]string{}
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
	// edgeOK carries the validated consequence a quest result names: keyed
	// quest id + destination state, carrying the full transition the
	// machine currently allows from its present state.
	edgeOK := map[string]story.QuestTransition{}
	for _, e := range questPool {
		edgeOK[e.quest.ID+"\x00"+e.to] = story.QuestTransition{
			QuestID: e.quest.ID, From: e.quest.CurrentState, To: e.to,
		}
	}

	res := &PlanResult{Act: *act, Budget: budget, Mix: mix, Generated: s.now()}
	usedSecrets := map[string]bool{}
	var items []BatchItemInput

	for _, sessionID := range targets {
		ps, err := s.planOneSession(ctx, planOneIn{
			snap: snap, stories: stories, act: act, sessionID: sessionID,
			budget: budget, mix: mix, notes: in.Notes,
			secretIdx: secretIdx, secretStmt: secretStmt, usedSecrets: usedSecrets,
			awareIdx: awareIdx, awareStmt: awareStmt, castIdx: castIdx,
			settingIdx: settingIdx, edgeOK: edgeOK,
			questPool: questPool, castPool: castPool, settings: settings,
			secretPool: secretPool, awarePool: awarePool,
		})
		if err != nil {
			return nil, err
		}
		res.Sessions = append(res.Sessions, *ps)
		items = append(items, ps.batchItems...)
	}

	if len(items) > 0 {
		var promptRecord strings.Builder
		fmt.Fprintf(&promptRecord, "mode: %s | act %d of %d %q (levels %d-%d, %d sessions) | %d scenes per session, mix [%s]",
			in.Mode, actIndex+1, actCount, act.Name, act.LevelStart, act.LevelEnd, actSessionCount,
			budget, strings.Join(mix, ", "))
		if len(questPool) > 0 {
			var names []string
			for _, e := range questPool {
				names = append(names, fmt.Sprintf("%s: %s -> %s", e.quest.Name, e.quest.CurrentState, e.to))
			}
			promptRecord.WriteString(" | quest edges: " + strings.Join(names, "; "))
		}
		if len(secretPool) > 0 {
			promptRecord.WriteString(fmt.Sprintf(" | %d unreached secrets", len(secretPool)))
		}
		if in.Notes != "" {
			promptRecord.WriteString(" | notes: " + in.Notes)
		}
		res.Batch, err = s.StageBatch(ctx, BatchInput{
			CampaignID: in.CampaignID, Source: BatchSourceStoryPlan,
			Prompt:    promptRecord.String(),
			CreatedBy: in.CreatedBy, Items: items,
		})
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

// actOrdinalPosition finds an act's zero-based index among the campaign's
// acts in ordinal order.
func actOrdinalPosition(snap *Snapshot, act *story.Act) (int, int) {
	acts := append([]story.Act(nil), snap.Spine.Acts...)
	sort.Slice(acts, func(i, j int) bool { return acts[i].Ordinal < acts[j].Ordinal })
	for i := range acts {
		if acts[i].ID == act.ID {
			return i, len(acts)
		}
	}
	return 0, len(acts)
}

// resolvePlanAct picks the act this run plans into: the named one when the
// input carries an id, else the next session's plan's act, else the first
// act that is not done.
func resolvePlanAct(snap *Snapshot, in PlanInput) (*story.Act, error) {
	acts := append([]story.Act(nil), snap.Spine.Acts...)
	sort.Slice(acts, func(i, j int) bool { return acts[i].Ordinal < acts[j].Ordinal })
	if in.ActID != "" {
		for i := range acts {
			if acts[i].ID == in.ActID {
				return &acts[i], nil
			}
		}
		return nil, fmt.Errorf("%w: act %s", ErrNotFound, in.ActID)
	}
	for _, ses := range snap.Sessions {
		if ses.Status != "planned" {
			continue
		}
		for i := range snap.Spine.Plans {
			if p := snap.Spine.Plans[i]; p.SessionID == ses.ID && p.ActID != "" {
				for j := range acts {
					if acts[j].ID == p.ActID {
						return &acts[j], nil
					}
				}
			}
		}
		break
	}
	for i := range acts {
		if acts[i].Status != story.StatusDone {
			return &acts[i], nil
		}
	}
	return nil, fmt.Errorf("%w: every act is done; add the next act before planning further", ErrInvalid)
}

/* ---------- one session ---------- */

// planOneIn carries everything one session's exchange needs, already
// computed by the caller.
type planOneIn struct {
	snap        *Snapshot
	stories     *story.Store
	act         *story.Act
	sessionID   string
	budget      int
	mix         []string
	notes       string
	secretIdx   *poolIndex
	secretStmt  map[string]string
	usedSecrets map[string]bool
	awareIdx    *poolIndex
	awareStmt   map[string]string
	castIdx     *poolIndex
	settingIdx  *poolIndex
	edgeOK      map[string]story.QuestTransition
	questPool   []questEdge
	castPool    []castCandidate
	settings    []campaign.Entity
	secretPool  []campaign.Fact
	awarePool   []campaign.Fact
}

// planOneSession plans a single session: one structured-generation call
// against the computed shape, the scenes written into the spine, and the
// awareness changes the outcomes promise returned as batch items.
func (s *Store) planOneSession(ctx context.Context, in planOneIn) (*PlannedSession, error) {
	structure := buildPlanStructure(in)
	fields := planFields(in)
	gen, err := s.Generate(ctx, GenerateInput{
		System:    planSystemPrompt,
		Structure: structure,
		Fields:    fields,
		Note:      "Fill every declared field. The structure block is the session you are planning — its scene budget and mix are already law, and every cast member, secret, quest move and fact you name comes from the listed pools.",
	})
	if err != nil {
		return nil, err
	}
	val := func(key string) string {
		sv, _ := gen.Values[key].(string)
		return strings.TrimSpace(sv)
	}

	ps := &PlannedSession{SessionID: in.sessionID, ActID: in.act.ID, Goal: val("session_goal")}

	// Parse and validate everything BEFORE anything is written, so a scene
	// the pools refuse leaves no half-written session behind.
	type parsedOutcome struct {
		label      string
		summary    string
		leadsTo    int // 1-based scene number within this session
		transition *story.QuestTransition
		aware      *awareChange
	}
	type parsedScene struct {
		name     string
		purpose  string
		setting  string
		cast     []castRef
		secrets  []string // resolved fact ids, in placement order
		disps    []string
		outcomes []parsedOutcome
	}
	parsed := make([]parsedScene, in.budget)
	for i := 1; i <= in.budget; i++ {
		p := &parsed[i-1]
		p.name = val(fmt.Sprintf("scene_%d_name", i))
		p.purpose = val(fmt.Sprintf("scene_%d_purpose", i))
		if raw := val(fmt.Sprintf("scene_%d_setting", i)); raw != "" {
			if id, ok := in.settingIdx.resolve(raw); ok {
				p.setting = id
			} else {
				ps.Dropped = append(ps.Dropped, fmt.Sprintf("scene %d setting %q is not a listed location; the scene has no setting", i, raw))
			}
		}
		var drops []string
		p.cast, drops = parseCastRefs(val(fmt.Sprintf("scene_%d_cast", i)), in.castIdx)
		ps.Dropped = append(ps.Dropped, drops...)
		if len(p.cast) == 0 {
			return nil, fmt.Errorf("%w: scene %d has no cast the pools recognise; a scene nobody is in is not plannable", ErrInvalid, i)
		}
		for _, sec := range parseFactRefs(val(fmt.Sprintf("scene_%d_secrets", i)), story.DispositionInPlay) {
			id, ok := in.secretIdx.resolve(sec.ref)
			if !ok {
				ps.Dropped = append(ps.Dropped, fmt.Sprintf("scene %d secret %q is not an unreached secret of this campaign; dropped", i, sec.ref))
				continue
			}
			if in.usedSecrets[id] {
				ps.Dropped = append(ps.Dropped, fmt.Sprintf("scene %d secret %q already plays in this plan; a secret is placed once", i, sec.ref))
				continue
			}
			in.usedSecrets[id] = true // claimed now, placed in the write pass
			p.secrets = append(p.secrets, id)
			p.disps = append(p.disps, sec.disposition)
		}
		for _, label := range outcomeLabels {
			summary := val(fmt.Sprintf("scene_%d_outcome_%s_summary", i, label))
			result := val(fmt.Sprintf("scene_%d_outcome_%s_result", i, label))
			if summary == "" && result == "" {
				continue // the model left this branch out; not ours to invent
			}
			leadsTo, transition, aware, why := parseOutcomeResult(i, in.budget, result, in.edgeOK, in.awareIdx)
			if why != "" {
				ps.Dropped = append(ps.Dropped, fmt.Sprintf("scene %d outcome %s dropped: %s", i, label, why))
				continue
			}
			p.outcomes = append(p.outcomes, parsedOutcome{
				label: label, summary: summary, leadsTo: leadsTo,
				transition: transition, aware: aware,
			})
		}
	}

	// The write pass: scenes first (the ids outcomes point at), then cast,
	// secrets and outcomes.
	sceneIDs := make([]string, in.budget)
	for i := 1; i <= in.budget; i++ {
		p := &parsed[i-1]
		sc, err := in.stories.CreateScene(ctx, in.snap.CampaignID, in.act.ID, in.sessionID,
			in.mix[i-1], p.name, p.purpose, p.setting)
		if err != nil {
			return nil, err
		}
		sceneIDs[i-1] = sc.ID
	}
	for i := 1; i <= in.budget; i++ {
		p := &parsed[i-1]
		sceneID := sceneIDs[i-1]
		for _, c := range p.cast {
			if _, err := in.stories.AddCast(ctx, in.snap.CampaignID, sceneID, c.id, c.role); err != nil {
				return nil, err
			}
		}
		for j, id := range p.secrets {
			if _, err := in.stories.SetSecret(ctx, in.snap.CampaignID, sceneID, id, p.disps[j]); err != nil {
				return nil, err
			}
		}
		for _, o := range p.outcomes {
			if _, err := in.stories.AddOutcome(ctx, in.snap.CampaignID, sceneID, o.label, o.summary,
				leadsToSceneRef(o.leadsTo, sceneIDs), o.transition); err != nil {
				return nil, err
			}
			if o.aware != nil {
				ps.batchItems = append(ps.batchItems, BatchItemInput{
					ID:      fmt.Sprintf("aware-%s-%d-%s", in.sessionID, i, o.label),
					Kind:    "discovery",
					Subject: fmt.Sprintf("The party %s: %s", o.aware.stance, in.awareStmt[o.aware.factID]),
					Summary: o.summary,
					Payload: map[string]any{
						"fact":          o.aware.factID,
						"discovered_by": campaign.PartyKnower,
						"stance":        o.aware.stance,
						"method":        fmt.Sprintf("Promised by outcome %s of a planned scene (session %s)", o.label, in.sessionID),
					},
				})
			}
		}
	}

	// Read the scenes back with their cast, secrets and outcomes attached,
	// so the result says exactly what landed.
	written, err := in.stories.ListScenes(ctx, campaign.ScopeDM, in.snap.CampaignID, in.act.ID)
	if err != nil {
		return nil, err
	}
	for i := range written {
		if written[i].SessionID == in.sessionID {
			sc, err := in.stories.GetScene(ctx, campaign.ScopeDM, in.snap.CampaignID, written[i].ID)
			if err != nil {
				return nil, err
			}
			ps.Scenes = append(ps.Scenes, *sc)
		}
	}

	// The session plan: written when the session has none, and filled in
	// when the row is a placeholder the skeleton generator left with no
	// goal — but a plan the DM has written a goal into is accepted material
	// a re-plan never overwrites.
	if existing, err := in.stories.GetPlan(ctx, campaign.ScopeDM, in.snap.CampaignID, in.sessionID); err == nil && existing.Goal != "" {
		ps.NewPlan = false
	} else {
		status := story.PlanStatusPlanned
		if _, err := in.stories.PutPlan(ctx, in.snap.CampaignID, in.sessionID, in.act.ID, ps.Goal, "", &status); err != nil {
			return nil, err
		}
		ps.NewPlan = true
	}
	return ps, nil
}

// leadsToSceneRef maps a parsed "scene j" consequence onto the id of the
// scene this run created.
func leadsToSceneRef(j int, sceneIDs []string) string {
	if j <= 0 || j > len(sceneIDs) {
		return ""
	}
	return sceneIDs[j-1]
}

/* ---------- the outcome consequence grammar ---------- */

// awareChange is one parsed awareness consequence: the fact and the stance
// the party would hold.
type awareChange struct {
	factID string
	stance string
}

// parseOutcomeResult parses one outcome's consequence string. Returns the
// intra-session scene number it leads to (0 when it does not), the quest
// transition, the awareness change, and — when non-empty — why the
// consequence is not a real one (the outcome is dropped).
func parseOutcomeResult(scene, budget int, result string, edgeOK map[string]story.QuestTransition, awareIdx *poolIndex) (int, *story.QuestTransition, *awareChange, string) {
	result = strings.TrimSpace(result)
	if result == "" {
		return 0, nil, nil, "names no consequence"
	}
	switch {
	case result == "next":
		if scene >= budget {
			return 0, nil, nil, "\"next\" on the session's last scene points nowhere"
		}
		return scene + 1, nil, nil, ""
	case strings.HasPrefix(result, "scene:"):
		var j int
		if _, err := fmt.Sscanf(result, "scene:%d", &j); err != nil || j <= scene || j > budget {
			return 0, nil, nil, fmt.Sprintf("%q is not a later scene of this session", result)
		}
		return j, nil, nil, ""
	case strings.HasPrefix(result, "quest:"):
		t, why := parseQuestConsequence(result, edgeOK)
		if why != "" {
			return 0, nil, nil, why
		}
		return 0, &t, nil, ""
	case strings.HasPrefix(result, "aware:"):
		aware, why := parseAwareConsequence(result, awareIdx)
		if why != "" {
			return 0, nil, nil, why
		}
		return 0, nil, aware, ""
	default:
		return 0, nil, nil, fmt.Sprintf("%q is none of next, scene:<n>, quest:<id>:<state>, aware:<id>:<stance>", result)
	}
}

// parseQuestConsequence parses the quest branch of an outcome result against
// the edges the machines currently allow.
func parseQuestConsequence(result string, edgeOK map[string]story.QuestTransition) (story.QuestTransition, string) {
	parts := strings.Split(strings.TrimPrefix(result, "quest:"), ":")
	if len(parts) != 2 {
		return story.QuestTransition{}, fmt.Sprintf("%q is not quest:<id>:<state>", result)
	}
	t, ok := edgeOK[parts[0]+"\x00"+strings.TrimSpace(parts[1])]
	if !ok {
		return story.QuestTransition{}, fmt.Sprintf("%q is not an edge the quest's machine currently allows", result)
	}
	return t, ""
}

// parseAwareConsequence parses the awareness branch of an outcome result.
func parseAwareConsequence(result string, awareIdx *poolIndex) (*awareChange, string) {
	parts := strings.Split(strings.TrimPrefix(result, "aware:"), ":")
	if len(parts) != 2 {
		return nil, fmt.Sprintf("%q is not aware:<id>:<stance>", result)
	}
	stance := strings.TrimSpace(parts[1])
	if stance != "knows" && stance != "suspects" {
		return nil, fmt.Sprintf("stance %q is not knows or suspects", stance)
	}
	id, ok := awareIdx.resolve(parts[0])
	if !ok {
		return nil, fmt.Sprintf("%q is not a fact the party could come to hold", result)
	}
	return &awareChange{factID: id, stance: stance}, ""
}

// castRef is one parsed cast entry.
type castRef struct {
	id   string
	role string
}

// parseCastRefs parses a scene's cast field — comma-separated pool refs,
// each optionally ":role" — validating every role against the vocabulary and
// every ref against the pool. Unrecognised entries are dropped and reported.
func parseCastRefs(field string, idx *poolIndex) ([]castRef, []string) {
	var out []castRef
	var drops []string
	seen := map[string]bool{}
	for _, entry := range strings.Split(field, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		ref, role := entry, story.RolePresent
		if head, tail, ok := strings.Cut(entry, ":"); ok {
			ref, role = strings.TrimSpace(head), strings.TrimSpace(tail)
		}
		if !story.CastRole(role) {
			drops = append(drops, fmt.Sprintf("cast %q has role %q, which is not focus, present, offstage or mentioned", entry, role))
			continue
		}
		id, ok := idx.resolve(ref)
		if !ok {
			drops = append(drops, fmt.Sprintf("cast %q is not in the candidate pool for this campaign", entry))
			continue
		}
		if seen[id] {
			continue // recasting within one scene is one row, not two
		}
		seen[id] = true
		out = append(out, castRef{id: id, role: role})
	}
	return out, drops
}

// factRef is one parsed fact reference with its optional disposition/stance.
type factRef struct {
	ref         string
	disposition string
}

// parseFactRefs parses a comma-separated fact field, each entry optionally
// carrying ":disposition" (secrets) — the caller validates the ids.
func parseFactRefs(field string, def string) []factRef {
	var out []factRef
	for _, entry := range strings.Split(field, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		ref, disp := entry, def
		if head, tail, ok := strings.Cut(entry, ":"); ok && !strings.Contains(head, " ") {
			ref, disp = strings.TrimSpace(head), strings.TrimSpace(tail)
		}
		if !story.SecretDisposition(disp) {
			disp = def
		}
		out = append(out, factRef{ref: ref, disposition: disp})
	}
	return out
}

/* ---------- the prompt ---------- */

// planStructure is the vetted shape one session's call fills into.
type planStructure struct {
	Act         planActView      `json:"act"`
	DMNotes     string           `json:"dm_notes,omitempty"`
	SceneBudget int              `json:"scene_budget"`
	SceneMix    []string         `json:"scene_mix"`
	Party       []planEntityView `json:"party"`
	Cast        []planCastView   `json:"candidate_cast"`
	Locations   []planEntityView `json:"locations"`
	Secrets     []planFactView   `json:"unreached_secrets"`
	Quests      []planQuestView  `json:"quests"`
	Awareness   []planFactView   `json:"awareness_candidates"`
}

type planActView struct {
	Ordinal    int    `json:"ordinal"`
	Of         int    `json:"of"`
	Name       string `json:"name"`
	Premise    string `json:"premise"`
	LevelStart int    `json:"level_start"`
	LevelEnd   int    `json:"level_end"`
}

type planEntityView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
}

type planCastView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Why     string `json:"why"`
	Summary string `json:"summary,omitempty"`
}

type planFactView struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
}

type planQuestView struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Current string   `json:"current_state"`
	Next    []string `json:"legal_next_states"`
}

func buildPlanStructure(in planOneIn) planStructure {
	st := planStructure{
		Act: planActView{
			Name: in.act.Name, Premise: in.act.Premise,
			LevelStart: in.act.LevelStart, LevelEnd: in.act.LevelEnd,
		},
		DMNotes:     in.notes,
		SceneBudget: in.budget,
		SceneMix:    in.mix,
	}
	idx, of := actOrdinalPosition(in.snap, in.act)
	st.Act.Ordinal, st.Act.Of = idx+1, of
	for _, c := range in.castPool {
		if c.entity.Kind == campaign.KindPC {
			st.Party = append(st.Party, planEntityView{ID: c.entity.ID, Name: c.entity.Name, Summary: c.entity.Summary})
		} else {
			st.Cast = append(st.Cast, planCastView{
				ID: c.entity.ID, Name: c.entity.Name, Kind: c.entity.Kind,
				Why: c.why, Summary: c.entity.Summary,
			})
		}
	}
	for _, e := range in.settings {
		st.Locations = append(st.Locations, planEntityView{ID: e.ID, Name: e.Name, Summary: e.Summary})
	}
	for _, f := range in.secretPool {
		st.Secrets = append(st.Secrets, planFactView{ID: f.ID, Statement: f.Statement})
	}
	byQuest := map[string]*planQuestView{}
	var order []string
	for _, e := range in.questPool {
		qv, ok := byQuest[e.quest.ID]
		if !ok {
			qv = &planQuestView{ID: e.quest.ID, Name: e.quest.Name, Current: e.quest.CurrentState}
			byQuest[e.quest.ID] = qv
			order = append(order, e.quest.ID)
		}
		qv.Next = append(qv.Next, e.to)
	}
	for _, id := range order {
		st.Quests = append(st.Quests, *byQuest[id])
	}
	for _, f := range in.awarePool {
		st.Awareness = append(st.Awareness, planFactView{ID: f.ID, Statement: f.Statement})
	}
	return st
}

// planFields declares the schema one session's call may fill.
func planFields(in planOneIn) []FieldSpec {
	fields := []FieldSpec{{
		Key: "session_goal", Required: true, MaxLen: 300,
		Desc: "What tonight's session is for, one sentence in the campaign's own language.",
	}}
	for i := 1; i <= in.budget; i++ {
		kind := in.mix[i-1]
		fields = append(fields,
			FieldSpec{Key: fmt.Sprintf("scene_%d_name", i), Required: true, MaxLen: 80,
				Desc: fmt.Sprintf("Name of scene %d of %d — a %s scene.", i, in.budget, kind)},
			FieldSpec{Key: fmt.Sprintf("scene_%d_purpose", i), Required: true, MaxLen: 400,
				Desc: fmt.Sprintf("Scene %d's purpose in two sentences: what it establishes and what it puts at stake.", i)},
			FieldSpec{Key: fmt.Sprintf("scene_%d_setting", i), MaxLen: 120,
				Desc: fmt.Sprintf("Where scene %d happens: one listed location's id or exact name, or empty for anywhere.", i)},
			FieldSpec{Key: fmt.Sprintf("scene_%d_cast", i), Required: true, MaxLen: 500,
				Desc: fmt.Sprintf("Scene %d's cast: comma-separated ids or exact names from the candidate pools, each optionally ':role' (focus, present, offstage, mentioned; default present). The party is assumed present — name the others.", i)},
			FieldSpec{Key: fmt.Sprintf("scene_%d_secrets", i), MaxLen: 300,
				Desc: fmt.Sprintf("Secrets scene %d puts in play: comma-separated fact ids from unreached_secrets, each optionally ':in_play', ':revealed_if' or ':withheld' (default in_play). A secret is placed once per plan.", i)},
		)
		for _, label := range outcomeLabels {
			fields = append(fields,
				FieldSpec{Key: fmt.Sprintf("scene_%d_outcome_%s_summary", i, label), MaxLen: 300,
					Desc: fmt.Sprintf("Branch %s of scene %d in one sentence — what happens if it goes this way. Empty when there is no branch %s.", label, i, label)},
				FieldSpec{Key: fmt.Sprintf("scene_%d_outcome_%s_result", i, label), MaxLen: 200,
					Desc: fmt.Sprintf("How branch %s of scene %d resolves: 'next', 'scene:<n>' (a later scene of this session), 'quest:<id>:<state>' (a listed quest's legal next state), or 'aware:<id>:<stance>' (the party gains knows/suspects on a listed fact). Empty when there is no branch %s.", label, i, label)},
			)
		}
	}
	return fields
}

// planSystemPrompt is the planner's system prompt.
const planSystemPrompt = `You are Grimoire's story planner. You are handed one session of an act of a campaign that already exists: its quests and the moves their state machines currently allow, the secrets nobody has reached, the people with unfinished business, and the scene budget and kind mix the table's pacing computed. Your job is to fill that session — names, purposes, cast, stakes and branches — choosing among the real things you were handed, nothing more.

STRICT RULES
1. Every field is a plain string. Fill every required one. No lists except where the field says comma-separated, no markdown, no commentary.
2. The scene count and each scene's kind are fixed by the structure. Do not add, merge or reorder scenes.
3. Cast, settings, secrets, quest moves and awareness facts come ONLY from the listed pools, by id or exact name. Never invent an entity, a fact or a quest move.
4. Every outcome branch must name a real consequence in its result field — the next scene, a later scene, a quest edge the machine allows, or an awareness change. A branch that changes nothing is not planned.
5. Honour the DM's notes where given, and the act's premise: the session should visibly advance what the campaign is about.
6. Prose is present tense, concrete, no filler.`
