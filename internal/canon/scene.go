package canon

// The scene designer (MAD-362, the second half): one scene — purpose, cast,
// secrets present, outcomes A-D — designed forward from the campaign as it
// stands.
//
// The load-bearing rule is the cast pool: a scene's cast is drawn from
// entities that can plausibly BE there, not the whole roster. The pool is
// computed deterministically from three signals the database already holds:
//
//   - the setting, when one is named — everyone the graph ties to that place
//     (located_in / contains edges both ways);
//   - the timeline's tail — the participants and locations of the campaign's
//     most recent events, the party's actual last known whereabouts;
//   - unfinished business — the NPCs whose agent fields carry goals, and the
//     factions, who are always up to something.
//
// The party is in every pool: the session is being planned for them.
//
// Secrets come from the unreached set exactly as the planner's do, quest
// moves from the edges the machines currently allow, and every outcome must
// name a real consequence — here a later scene of the same act (an existing
// one, by id), a quest edge, or an awareness change. The scene, its cast,
// its secrets and its outcomes are spine rows, written the way a DM writing
// them by hand would; the awareness changes the outcomes promise are staged
// as one small proposal batch (source 'scene') behind the review gate.
// Re-designing is designing again: nothing already written is touched.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

// Scene pool bounds.
const (
	sceneCastPoolCap  = 16
	sceneActScenesCap = 12
)

// SceneInput is one scene-design request: the act (and optionally the
// session) it belongs to, an optional setting and kind, and the DM's notes.
type SceneInput struct {
	CampaignID string
	ActID      string
	SessionID  string
	Setting    string
	Kind       string
	Notes      string
	CreatedBy  string
}

// SceneResult is what one scene-design run produced.
type SceneResult struct {
	Scene     story.Scene `json:"scene"`
	Batch     *Batch      `json:"batch,omitempty"`
	Dropped   []string    `json:"dropped,omitempty"`
	Generated time.Time   `json:"generated_at"`
}

// sceneCastPool computes who can plausibly be in this scene: the party, the
// setting's people, the timeline tail's participants, then the NPCs with
// unresolved goals and the factions — in that priority, capped.
func sceneCastPool(snap *Snapshot, settingID string) []castCandidate {
	score := map[string]int{}
	why := map[string]string{}
	var order []string
	rank := func(id, reason string, points int) {
		if id == "" {
			return
		}
		if _, seen := score[id]; !seen {
			order = append(order, id)
		}
		score[id] += points
		if _, ok := why[id]; !ok || points >= 3 {
			why[id] = reason
		}
	}
	byID := map[string]campaign.Entity{}
	for _, e := range snap.Entities {
		if e.Status == campaign.StatusDeleted {
			continue
		}
		byID[e.ID] = e
		switch e.Kind {
		case campaign.KindPC:
			rank(e.ID, "the party", 5)
		case campaign.KindNPC:
			if agent := campaign.NPCAgentOf(&e); len(agent.Goals) > 0 {
				rank(e.ID, "unresolved goal: "+agent.Goals[0], 2)
			}
		case campaign.KindFaction:
			rank(e.ID, "a faction with stakes", 1)
		}
	}
	// The setting's people: everything the graph ties to that place.
	if settingID != "" {
		for _, r := range snap.Relationships {
			switch r.RelType {
			case "located_in":
				if r.ToEntity == settingID {
					rank(r.FromEntity, "tied to the setting", 4)
				}
			case "contains":
				if r.FromEntity == settingID {
					rank(r.ToEntity, "inside the setting", 4)
				}
			}
		}
	}
	// The timeline tail: the campaign's most recent events, newest first —
	// who was there, and where it happened.
	events := append([]campaign.Event(nil), snap.Events...)
	sort.SliceStable(events, func(i, j int) bool {
		if (events[i].RealOrdinal != 0 || events[j].RealOrdinal != 0) && events[i].RealOrdinal != events[j].RealOrdinal {
			return events[i].RealOrdinal > events[j].RealOrdinal
		}
		return events[i].CreatedAt.After(events[j].CreatedAt)
	})
	if len(events) > 3 {
		events = events[:3]
	}
	for _, ev := range events {
		rank(ev.LocationEntity, "the last place anything happened", 3)
		for _, p := range ev.Participants {
			rank(p.EntityID, "last seen in the timeline's tail", 3)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return score[order[i]] > score[order[j]] })
	var out []castCandidate
	for _, id := range order {
		e, ok := byID[id]
		if !ok {
			continue
		}
		if e.Kind != campaign.KindPC && e.Kind != campaign.KindNPC && e.Kind != campaign.KindFaction {
			continue // locations are settings, not cast
		}
		out = append(out, castCandidate{entity: e, why: why[id]})
		if len(out) >= sceneCastPoolCap {
			break
		}
	}
	return out
}

// GenerateScene runs one scene-design exchange: compute the plausible cast,
// the unreached secrets and the legal quest moves, fill the scene with one
// structured-generation call, write it into the spine, and stage the
// awareness changes its outcomes promise behind the review gate.
func (s *Store) GenerateScene(ctx context.Context, in SceneInput) (*SceneResult, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
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
		return nil, fmt.Errorf("%w: the campaign has no acts to design a scene for; design a skeleton or create acts by hand first", ErrInvalid)
	}
	stories, err := story.New(s.db)
	if err != nil {
		return nil, err
	}

	// The act this scene belongs to.
	act, err := resolvePlanAct(snap, PlanInput{ActID: in.ActID})
	if err != nil {
		return nil, err
	}
	actIndex, actCount := actOrdinalPosition(snap, act)
	if in.SessionID != "" {
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM game_sessions WHERE id = ? AND campaign_id = ?`, in.SessionID, in.CampaignID).Scan(&one)
		if err != nil {
			return nil, fmt.Errorf("%w: session %s", ErrNotFound, in.SessionID)
		}
	}

	// The kind: the DM's when given, else the act's template deals the next
	// one in order — the scene count the act already holds picks the slot.
	kind := strings.TrimSpace(in.Kind)
	actScenes := 0
	actSceneIDs := map[string]bool{}
	for i := range snap.Spine.Scenes {
		if snap.Spine.Scenes[i].ActID == act.ID {
			actScenes++
			actSceneIDs[snap.Spine.Scenes[i].ID] = true
		}
	}
	if kind == "" {
		kind = story.SceneMix(actIndex, actCount, actScenes+1)[actScenes]
	} else if !story.SceneKind(kind) {
		return nil, fmt.Errorf("%w: scene kind %q is not one of %s", ErrInvalid, kind, strings.Join(story.SceneKinds, ", "))
	}

	// The pools: setting (input, resolved against the locations), plausible
	// cast, unreached secrets, legal quest moves, awareness candidates.
	locations := settingPool(snap, act.ID)
	settingIdx := newPoolIndex()
	for _, e := range locations {
		settingIdx.add(e.ID, e.Name)
	}
	settingID := ""
	if in.Setting != "" {
		id, ok := settingIdx.resolve(in.Setting)
		if !ok {
			return nil, fmt.Errorf("%w: setting %q is not a location of this campaign", ErrInvalid, in.Setting)
		}
		settingID = id
	}
	castPool := sceneCastPool(snap, settingID)
	secretPool := unreachedSecrets(snap)
	awarePool := awarenessCandidates(snap)
	questPool := readyQuestEdges(snap)

	castIdx := newPoolIndex()
	for _, c := range castPool {
		castIdx.add(c.entity.ID, c.entity.Name)
	}
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
	edgeOK := map[string]story.QuestTransition{}
	for _, e := range questPool {
		edgeOK[e.quest.ID+"\x00"+e.to] = story.QuestTransition{
			QuestID: e.quest.ID, From: e.quest.CurrentState, To: e.to,
		}
	}

	// The act's existing scenes an outcome may branch into, capped by name
	// order so the model sees a bounded list.
	var actSceneList []planEntityView
	for i := range snap.Spine.Scenes {
		if actSceneIDs[snap.Spine.Scenes[i].ID] {
			actSceneList = append(actSceneList, planEntityView{
				ID: snap.Spine.Scenes[i].ID, Name: snap.Spine.Scenes[i].Name,
			})
		}
	}
	sort.Slice(actSceneList, func(i, j int) bool { return actSceneList[i].Name < actSceneList[j].Name })
	if len(actSceneList) > sceneActScenesCap {
		actSceneList = actSceneList[:sceneActScenesCap]
	}
	actSceneIdx := newPoolIndex()
	for _, sc := range actSceneList {
		actSceneIdx.add(sc.ID, sc.Name)
	}

	structure := buildSceneStructure(sceneStructureIn{
		act: act, actIndex: actIndex, actCount: actCount, kind: kind, notes: in.Notes,
		settingID: settingID, locations: locations, castPool: castPool,
		secretPool: secretPool, awarePool: awarePool, questPool: questPool,
		actScenes: actSceneList,
	})
	fields := sceneFields(actSceneList)
	gen, err := s.Generate(ctx, GenerateInput{
		System:    sceneSystemPrompt,
		Structure: structure,
		Fields:    fields,
		Note:      "Fill every declared field. The structure block is the scene you are designing — its kind is already law, and every cast member, secret, quest move, fact and target scene you name comes from the listed pools.",
	})
	if err != nil {
		return nil, err
	}
	val := func(key string) string {
		sv, _ := gen.Values[key].(string)
		return strings.TrimSpace(sv)
	}

	res := &SceneResult{Generated: s.now()}

	// The cast is parsed and pool-validated BEFORE the scene exists, so a
	// cast the pools refuse leaves nothing written.
	cast, drops := parseCastRefs(val("scene_cast"), castIdx)
	res.Dropped = append(res.Dropped, drops...)
	if len(cast) == 0 {
		return nil, fmt.Errorf("%w: the scene has no cast the pools recognise; a scene nobody is in is not plannable", ErrInvalid)
	}
	sc, err := stories.CreateScene(ctx, in.CampaignID, act.ID, in.SessionID,
		kind, val("scene_name"), val("scene_purpose"), settingID)
	if err != nil {
		return nil, err
	}
	for _, c := range cast {
		if _, err := stories.AddCast(ctx, in.CampaignID, sc.ID, c.id, c.role); err != nil {
			return nil, err
		}
	}
	used := map[string]bool{}
	for _, sec := range parseFactRefs(val("scene_secrets"), story.DispositionInPlay) {
		id, ok := secretIdx.resolve(sec.ref)
		if !ok {
			res.Dropped = append(res.Dropped, fmt.Sprintf("secret %q is not an unreached secret of this campaign; dropped", sec.ref))
			continue
		}
		if used[id] {
			continue
		}
		if _, err := stories.SetSecret(ctx, in.CampaignID, sc.ID, id, sec.disposition); err != nil {
			return nil, err
		}
		used[id] = true
	}

	var items []BatchItemInput
	for _, label := range outcomeLabels {
		summary := val(fmt.Sprintf("outcome_%s_summary", label))
		result := val(fmt.Sprintf("outcome_%s_result", label))
		if summary == "" && result == "" {
			continue
		}
		leadsTo, transition, aware, why := parseSceneOutcomeResult(result, edgeOK, awareIdx, actSceneIdx)
		if why != "" {
			res.Dropped = append(res.Dropped, fmt.Sprintf("outcome %s dropped: %s", label, why))
			continue
		}
		if _, err := stories.AddOutcome(ctx, in.CampaignID, sc.ID, label, summary, leadsTo, transition); err != nil {
			return nil, err
		}
		if aware != nil {
			items = append(items, BatchItemInput{
				ID:      fmt.Sprintf("aware-%s-%s", sc.ID, label),
				Kind:    "discovery",
				Subject: fmt.Sprintf("The party %s: %s", aware.stance, awareStmt[aware.factID]),
				Summary: summary,
				Payload: map[string]any{
					"fact":          aware.factID,
					"discovered_by": campaign.PartyKnower,
					"stance":        aware.stance,
					"method":        fmt.Sprintf("Promised by outcome %s of a designed scene", label),
				},
			})
		}
	}

	if len(items) > 0 {
		res.Batch, err = s.StageBatch(ctx, BatchInput{
			CampaignID: in.CampaignID, Source: BatchSourceScene,
			Prompt: fmt.Sprintf("scene %q (%s) in act %d of %d %q | cast pool %d | %d unreached secrets | notes: %s",
				val("scene_name"), kind, actIndex+1, actCount, act.Name, len(castPool), len(secretPool), in.Notes),
			CreatedBy: in.CreatedBy, Items: items,
		})
		if err != nil {
			return nil, err
		}
	}
	full, err := stories.GetScene(ctx, campaign.ScopeDM, in.CampaignID, sc.ID)
	if err != nil {
		return nil, err
	}
	res.Scene = *full
	return res, nil
}

// parseSceneOutcomeResult parses one designed scene's outcome consequence:
// a later scene of the same act (an existing one, by id or exact name), a
// quest edge the machine currently allows, or an awareness change. There is
// no "next" here — a lone scene has no session ordering behind it.
func parseSceneOutcomeResult(result string, edgeOK map[string]story.QuestTransition, awareIdx *poolIndex, actSceneIdx *poolIndex) (string, *story.QuestTransition, *awareChange, string) {
	result = strings.TrimSpace(result)
	if result == "" {
		return "", nil, nil, "names no consequence"
	}
	switch {
	case strings.HasPrefix(result, "scene:"):
		ref := strings.TrimSpace(strings.TrimPrefix(result, "scene:"))
		id, ok := actSceneIdx.resolve(ref)
		if !ok {
			return "", nil, nil, fmt.Sprintf("%q is not a scene of this act", result)
		}
		return id, nil, nil, ""
	case strings.HasPrefix(result, "quest:"):
		t, why := parseQuestConsequence(result, edgeOK)
		if why != "" {
			return "", nil, nil, why
		}
		return "", &t, nil, ""
	case strings.HasPrefix(result, "aware:"):
		aware, why := parseAwareConsequence(result, awareIdx)
		if why != "" {
			return "", nil, nil, why
		}
		return "", nil, aware, ""
	default:
		return "", nil, nil, fmt.Sprintf("%q is none of scene:<id>, quest:<id>:<state>, aware:<id>:<stance>", result)
	}
}

/* ---------- the prompt ---------- */

type sceneStructureIn struct {
	act        *story.Act
	actIndex   int
	actCount   int
	kind       string
	notes      string
	settingID  string
	locations  []campaign.Entity
	castPool   []castCandidate
	secretPool []campaign.Fact
	awarePool  []campaign.Fact
	questPool  []questEdge
	actScenes  []planEntityView
}

type sceneStructure struct {
	Act       planActView      `json:"act"`
	DMNotes   string           `json:"dm_notes,omitempty"`
	Kind      string           `json:"kind"`
	Setting   planEntityView   `json:"setting,omitempty"`
	Party     []planEntityView `json:"party"`
	Cast      []planCastView   `json:"candidate_cast"`
	Locations []planEntityView `json:"locations,omitempty"`
	Secrets   []planFactView   `json:"unreached_secrets"`
	Quests    []planQuestView  `json:"quests"`
	Awareness []planFactView   `json:"awareness_candidates"`
	ActScenes []planEntityView `json:"later_scenes_of_this_act"`
}

func buildSceneStructure(in sceneStructureIn) sceneStructure {
	st := sceneStructure{
		Act: planActView{
			Ordinal: in.actIndex + 1, Of: in.actCount,
			Name: in.act.Name, Premise: in.act.Premise,
			LevelStart: in.act.LevelStart, LevelEnd: in.act.LevelEnd,
		},
		DMNotes:   in.notes,
		Kind:      in.kind,
		ActScenes: in.actScenes,
	}
	for _, e := range in.locations {
		v := planEntityView{ID: e.ID, Name: e.Name, Summary: e.Summary}
		if e.ID == in.settingID {
			st.Setting = v
		}
		st.Locations = append(st.Locations, v)
	}
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
	for _, f := range in.secretPool {
		st.Secrets = append(st.Secrets, planFactView{ID: f.ID, Statement: f.Statement})
	}
	for _, f := range in.awarePool {
		st.Awareness = append(st.Awareness, planFactView{ID: f.ID, Statement: f.Statement})
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
	return st
}

// sceneFields declares the schema one scene-design call may fill.
func sceneFields(actScenes []planEntityView) []FieldSpec {
	fields := []FieldSpec{
		{Key: "scene_name", Required: true, MaxLen: 80,
			Desc: "The scene's name — what the DM would write on the index card."},
		{Key: "scene_purpose", Required: true, MaxLen: 400,
			Desc: "The scene's purpose in two sentences: what it establishes and what it puts at stake."},
		{Key: "scene_cast", Required: true, MaxLen: 500,
			Desc: "The cast: comma-separated ids or exact names from the candidate pools, each optionally ':role' (focus, present, offstage, mentioned; default present). The party is assumed present — name the others."},
		{Key: "scene_secrets", MaxLen: 300,
			Desc: "Secrets the scene puts in play: comma-separated fact ids from unreached_secrets, each optionally ':in_play', ':revealed_if' or ':withheld' (default in_play)."},
	}
	for _, label := range outcomeLabels {
		fields = append(fields,
			FieldSpec{Key: fmt.Sprintf("outcome_%s_summary", label), MaxLen: 300,
				Desc: fmt.Sprintf("Branch %s in one sentence — what happens if the scene goes this way. Empty when there is no branch %s.", label, label)},
			FieldSpec{Key: fmt.Sprintf("outcome_%s_result", label), MaxLen: 200,
				Desc: fmt.Sprintf("How branch %s resolves: 'scene:<id>' (a later scene of this act, from later_scenes_of_this_act), 'quest:<id>:<state>' (a listed quest's legal next state), or 'aware:<id>:<stance>' (the party gains knows/suspects on a listed fact). Empty when there is no branch %s.", label, label)},
		)
	}
	return fields
}

// sceneSystemPrompt is the scene designer's system prompt.
const sceneSystemPrompt = `You are Grimoire's scene designer. You are handed one scene of one act of a campaign that already exists: who can plausibly be there (computed from the setting, the timeline's tail and unfinished business), which secrets nobody has reached, the moves the quests' machines allow, and the act's own later scenes. Your job is to fill that scene — purpose, cast, secrets and branches A-D — choosing among the real things you were handed, nothing more.

STRICT RULES
1. Every field is a plain string. Fill every required one. No lists except where the field says comma-separated, no markdown, no commentary.
2. Cast, secrets, quest moves, awareness facts and target scenes come ONLY from the listed pools, by id or exact name. Never invent an entity, a fact, a quest move or a scene.
3. Every outcome branch must name a real consequence in its result field — a later scene of this act, a quest edge the machine allows, or an awareness change. A branch that changes nothing is not planned.
4. Honour the DM's notes where given, and the act's premise: the scene should visibly advance what the campaign is about.
5. Prose is present tense, concrete, no filler.`
