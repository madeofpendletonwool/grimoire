package canon

// The story planner and scene designer's tests (MAD-362): the deterministic
// shortlists, the consequence grammar, the plausible-cast pool, and the
// end-to-end exchanges over a temp SQLite database with a fake LLM client
// replaying a fixture (ADR 8) — planning, staging, acceptance and the canon
// check's verdict on what landed.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

/* ---------- the deterministic shortlists ---------- */

func TestPartyGrantedFacts_OnlyPartySideGrantingStances(t *testing.T) {
	db, fx, _ := seeded(t)
	ks := knowledgeStore(t, db)
	// The party knows the key's secret; the Duke suspects the mines fact;
	// the party is deliberately unaware of the visit fact.
	mustDiscovery(t, ks, fx.Campaign.ID, fx.FactKeyOpensCrypt, campaign.PartyKnower, "knows")
	mustDiscovery(t, ks, fx.Campaign.ID, fx.FactDukeVisited, fx.Duke, "suspects")
	if _, err := ks.SetAwareness(context.Background(), fx.Campaign.ID, campaign.PartyKnower,
		fx.FactDukeNever, knowledge.StanceUnaware, 1, "", ""); err != nil {
		t.Fatalf("set unaware: %v", err)
	}
	// Thalia (a pc) knows the mines fact.
	mustDiscovery(t, ks, fx.Campaign.ID, fx.FactMinesOwned, fx.Thalia, "knows")

	snap := loadSnap(t, db, fx.Campaign.ID)
	granted := partyGrantedFacts(snap)
	if !granted[fx.FactKeyOpensCrypt] {
		t.Error("the party's knows row must grant")
	}
	if !granted[fx.FactMinesOwned] {
		t.Error("a pc's knows row must grant")
	}
	if granted[fx.FactDukeVisited] {
		t.Error("an NPC's stance must not grant for the party's reachability")
	}
	if granted[fx.FactDukeNever] {
		t.Error("a deliberate unaware must not grant")
	}
}

func TestUnreachedSecrets_ExcludesGrantedAndNonSecrets(t *testing.T) {
	db, fx, _ := seeded(t)
	ks := knowledgeStore(t, db)
	// The party has reached the key's secret — it is no longer material.
	mustDiscovery(t, ks, fx.Campaign.ID, fx.FactKeyOpensCrypt, campaign.PartyKnower, "knows")
	// A fresh secret nobody has touched.
	fresh := mustFact(t, db, fx, "The crypt beneath Greyfall holds the Verdant God's dreaming heart.", true)

	snap := loadSnap(t, db, fx.Campaign.ID)
	got := unreachedSecrets(snap)
	ids := map[string]bool{}
	for _, f := range got {
		ids[f.ID] = true
		if f.Visibility != campaign.VisibilitySecret {
			t.Errorf("non-secret %s in the unreached pool", f.ID)
		}
	}
	if ids[fx.FactKeyOpensCrypt] {
		t.Error("a secret the party already holds is not unreached")
	}
	if ids[fx.FactMinesOwned] {
		t.Error("a public fact is not secret material")
	}
	if !ids[fresh] {
		t.Error("the fresh secret must be in the pool")
	}
}

func TestReadyQuestEdges_OnlyFromCurrentState(t *testing.T) {
	db, fx, _ := seeded(t)
	snap := loadSnap(t, db, fx.Campaign.ID)
	edges := readyQuestEdges(snap)
	if len(edges) == 0 {
		t.Fatal("the fixture's quest sits at survivors_found with two edges out")
	}
	for _, e := range edges {
		if e.quest.CurrentState != "survivors_found" {
			t.Errorf("edge %s -> %s offered from %s, not the current state", e.quest.Name, e.to, e.quest.CurrentState)
		}
		if !e.quest.Machine.HasEdge(e.quest.CurrentState, e.to) {
			t.Errorf("edge to %s is not in the machine", e.to)
		}
	}
	var tos []string
	for _, e := range edges {
		tos = append(tos, e.to)
	}
	for _, want := range []string{"trusted_survivor", "accused_survivor"} {
		if !contains(tos, want) {
			t.Errorf("legal next state %q missing from %v", want, tos)
		}
	}
}

func TestSceneCastPool_PlausiblePresence(t *testing.T) {
	db, fx, _ := seeded(t)
	ctx := context.Background()
	cs := campaignStore(t, db)
	// Brother Venn has an unresolved goal; the party was in the ambush at
	// Blackwater; Tom keeps the Waystone in Blackwater.
	if _, err := cs.UpdateEntity(ctx, fx.Campaign.ID, fx.Venn, nil, nil, nil,
		map[string]any{"agent": map[string]any{"goals": []string{"find out who marched the miners off"}}}); err != nil {
		t.Fatalf("give Venn a goal: %v", err)
	}
	snap := loadSnap(t, db, fx.Campaign.ID)

	pool := sceneCastPool(snap, fx.Blackwater)
	in := map[string]castCandidate{}
	for _, c := range pool {
		in[c.entity.ID] = c
	}
	for _, id := range []string{fx.Thalia, fx.Tom, fx.Venn, fx.Cult} {
		if _, ok := in[id]; !ok {
			t.Errorf("entity %s missing from the setting pool (have %v)", id, poolNames(pool))
		}
	}
	if _, ok := in[fx.Verdant]; ok {
		t.Error("the deity is neither party, cast kind, tied to the setting nor in the timeline tail")
	}
	// Ranked: the party outranks the goal-NPC, who outranks the faction
	// (a lower index is a better rank).
	if rank(pool, fx.Thalia) > rank(pool, fx.Venn) || rank(pool, fx.Venn) > rank(pool, fx.Cult) {
		t.Errorf("pool ranking wrong: thalia=%d venn=%d cult=%d", rank(pool, fx.Thalia), rank(pool, fx.Venn), rank(pool, fx.Cult))
	}

	// Without a setting the timeline tail still seats the ambush's
	// participants and its location.
	pool = sceneCastPool(snap, "")
	in = map[string]castCandidate{}
	for _, c := range pool {
		in[c.entity.ID] = c
	}
	for _, id := range []string{fx.Thalia, fx.Venn} {
		if _, ok := in[id]; !ok {
			t.Errorf("entity %s missing from the unsetting pool", id)
		}
	}
}

func poolNames(pool []castCandidate) []string {
	var out []string
	for _, c := range pool {
		out = append(out, c.entity.Name)
	}
	return out
}

func rank(pool []castCandidate, id string) int {
	for i, c := range pool {
		if c.entity.ID == id {
			return i
		}
	}
	return -1
}

/* ---------- the consequence grammar ---------- */

func TestParseOutcomeResult(t *testing.T) {
	edgeOK := map[string]story.QuestTransition{
		"q1\x00trusted_survivor": {QuestID: "q1", From: "survivors_found", To: "trusted_survivor"},
	}
	aware := newPoolIndex()
	aware.add("f1", "f1")

	// next, forward scene refs, quest edges, awareness.
	if j, tr, aw, why := parseOutcomeResult(1, 4, "next", edgeOK, aware); j != 2 || tr != nil || aw != nil || why != "" {
		t.Errorf("next: %d %v %v %q", j, tr, aw, why)
	}
	if j, _, _, why := parseOutcomeResult(1, 4, "scene:3", edgeOK, aware); j != 3 || why != "" {
		t.Errorf("scene:3 -> %d %q", j, why)
	}
	if j, _, _, why := parseOutcomeResult(4, 4, "next", edgeOK, aware); j != 0 || why == "" {
		t.Error("next on the last scene must be dropped")
	}
	if j, _, _, why := parseOutcomeResult(2, 4, "scene:2", edgeOK, aware); j != 0 || why == "" {
		t.Error("a self-pointing scene must be dropped")
	}
	if j, _, _, why := parseOutcomeResult(2, 4, "scene:9", edgeOK, aware); j != 0 || why == "" {
		t.Error("a scene past the session must be dropped")
	}
	if _, tr, _, why := parseOutcomeResult(1, 4, "quest:q1:trusted_survivor", edgeOK, aware); tr == nil || why != "" {
		t.Errorf("quest edge: %v %q", tr, why)
	}
	if _, tr, _, why := parseOutcomeResult(1, 4, "quest:q1:cult_revealed", edgeOK, aware); tr != nil || why == "" {
		t.Error("an edge the machine does not currently allow must be dropped")
	}
	if _, _, aw, why := parseOutcomeResult(1, 4, "aware:f1:suspects", edgeOK, aware); aw == nil || aw.stance != "suspects" || why != "" {
		t.Errorf("aware: %v %q", aw, why)
	}
	if _, _, aw, why := parseOutcomeResult(1, 4, "aware:f1:believes_false", edgeOK, aware); aw != nil || why == "" {
		t.Error("believes_false is not a plannable stance")
	}
	if _, _, _, why := parseOutcomeResult(1, 4, "", edgeOK, aware); why == "" {
		t.Error("an empty result names no consequence")
	}
	if _, _, _, why := parseOutcomeResult(1, 4, "everyone dies", edgeOK, aware); why == "" {
		t.Error("prose is not a consequence")
	}
}

func TestParseCastRefs(t *testing.T) {
	idx := newPoolIndex()
	idx.add("e1", "Duke Aldric Vane")
	idx.add("e2", "Tom the Innkeeper")
	idx.add("e3", "Lady Elara")
	idx.add("e4", "Elara of the Marches") // same given name, two entities

	cast, drops := parseCastRefs("Duke Aldric Vane:focus, e2, tom the innkeeper:offstage, ghost", idx)
	if len(drops) != 1 || !strings.Contains(drops[0], "ghost") {
		t.Fatalf("drops = %v", drops)
	}
	if len(cast) != 2 {
		t.Fatalf("cast = %+v", cast)
	}
	if cast[0].id != "e1" || cast[0].role != story.RoleFocus {
		t.Errorf("cast[0] = %+v", cast[0])
	}
	if cast[1].role != story.RolePresent {
		t.Errorf("default role = %q", cast[1].role)
	}
	// An ambiguous name resolves nowhere; a duplicate entity is one row,
	// keeping the first role it was given.
	cast, drops = parseCastRefs("Elara, e3:mentioned, e3", idx)
	if len(drops) != 1 || !strings.Contains(drops[0], "Elara") {
		t.Fatalf("ambiguous/dup drops = %v", drops)
	}
	if len(cast) != 1 || cast[0].role != story.RoleMentioned {
		t.Fatalf("cast = %+v", cast)
	}
	// A bad role is dropped, not guessed.
	if _, drops = parseCastRefs("e1:star", idx); len(drops) != 1 {
		t.Fatalf("bad role drops = %v", drops)
	}
}

/* ---------- the end-to-end planning exchange ---------- */

// planTestStack builds the fixture plus a hand-authored four-act spine and
// returns the store and the ids the scripts need.
type planStack struct {
	s        *Store
	db       *sql.DB
	fx       *campaign.Fixture
	stories  *story.Store
	sessions *gamesession.Store
	act1     story.Act
	acts     []story.Act
	secret   string // a fresh, unreached secret
}

func planStackSetup(t *testing.T, model ModelClient) *planStack {
	t.Helper()
	db, fx, _ := seeded(t)
	st := &planStack{
		s:  skeletonStore(t, db, model),
		db: db, fx: fx,
		stories:  mustStory(t, db),
		sessions: mustSessions(t, db),
	}
	ctx := context.Background()
	for _, band := range [][2]int{{1, 3}, {4, 6}, {7, 9}, {10, 12}} {
		act, err := st.stories.CreateAct(ctx, fx.Campaign.ID,
			fmt.Sprintf("Act %d", len(st.acts)+1), "The noose tightens.", band[0], band[1])
		if err != nil {
			t.Fatalf("create act: %v", err)
		}
		st.acts = append(st.acts, *act)
	}
	st.act1 = st.acts[0]
	// Four planned sessions seated on act 1 — the pacing the budget reads.
	status := story.PlanStatusPlanned
	for i := 0; i < 4; i++ {
		ses, err := st.sessions.CreateSession(ctx, fx.Campaign.ID, "")
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		if _, err := st.stories.PutPlan(ctx, fx.Campaign.ID, ses.ID, st.act1.ID, "", "", &status); err != nil {
			t.Fatalf("put plan: %v", err)
		}
	}
	// The fixture's own secret is one the party has already reached; a
	// fresh one is the material tonight's plan can actually put in play.
	ks := knowledgeStore(t, db)
	mustDiscovery(t, ks, fx.Campaign.ID, fx.FactKeyOpensCrypt, campaign.PartyKnower, "knows")
	st.secret = mustFact(t, db, fx, "The crypt beneath Greyfall holds the Verdant God's dreaming heart.", true)
	return st
}

// planScript scripts one session's fill: `budget` scenes with the fixture's
// people, the fresh secret on the chosen scene, and a spread of outcome
// consequences — the legal ones the assertions expect, plus the deliberate
// failures the drop rules must catch.
func planScript(st *planStack, budget int) string {
	m := map[string]any{
		"session_goal": "The party learns who marched the miners off, and the ledger names the wrong man.",
	}
	for i := 1; i <= budget; i++ {
		m[fmt.Sprintf("scene_%d_name", i)] = fmt.Sprintf("Scene %d", i)
		m[fmt.Sprintf("scene_%d_purpose", i)] = "Pressure lands on someone who lies about it."
		m[fmt.Sprintf("scene_%d_setting", i)] = "Blackwater"
		m[fmt.Sprintf("scene_%d_cast", i)] = "Duke Aldric Vane:focus, Tom the Innkeeper"
		m[fmt.Sprintf("scene_%d_secrets", i)] = ""
		last := i == budget
		for _, label := range []string{"A", "B", "C", "D"} {
			m[fmt.Sprintf("scene_%d_outcome_%s_summary", i, label)] = ""
			m[fmt.Sprintf("scene_%d_outcome_%s_result", i, label)] = ""
		}
		if !last {
			m[fmt.Sprintf("scene_%d_outcome_A_summary", i)] = "The party presses the lead."
			m[fmt.Sprintf("scene_%d_outcome_A_result", i)] = "next"
		} else {
			m[fmt.Sprintf("scene_%d_outcome_A_summary", i)] = "The Duke answers for the caravan."
			m[fmt.Sprintf("scene_%d_outcome_A_result", i)] = "quest:" + st.fx.QuestID + ":accused_survivor"
		}
	}
	// Consequence spreads the tests assert on.
	m["scene_1_outcome_B_summary"] = "A survivor talks."
	m["scene_1_outcome_B_result"] = "quest:" + st.fx.QuestID + ":trusted_survivor"
	m["scene_2_outcome_B_summary"] = "The party follows the robed figures."
	m["scene_2_outcome_B_result"] = "scene:4"
	m["scene_3_outcome_B_summary"] = "The charter is a forgery."
	m["scene_3_outcome_B_result"] = "aware:" + st.fx.FactDukeVisited + ":knows"
	// The fresh secret plays in scene 3 — by id, a row not a paraphrase —
	// and scene 4 names it again, which the once-per-plan rule drops.
	m["scene_3_secrets"] = st.secret + ":in_play"
	// The deliberate failures the drop rules must catch.
	m["scene_1_secrets"] = st.fx.FactKeyOpensCrypt // already granted — not in the pool
	m["scene_2_secrets"] = "not-a-fact"            // never existed
	m["scene_4_secrets"] = st.secret               // a secret is placed once per plan
	m["scene_4_outcome_B_summary"] = "The quest jumps ahead of its machine."
	m["scene_4_outcome_B_result"] = "quest:" + st.fx.QuestID + ":cult_revealed" // edge not from the current state
	m[fmt.Sprintf("scene_%d_outcome_C_summary", budget)] = "Nothing changes."
	m[fmt.Sprintf("scene_%d_outcome_C_result", budget)] = "" // no consequence — dropped
	b, _ := json.Marshal(m)
	return string(b)
}

func TestGeneratePlan_Session(t *testing.T) {
	st := planStackSetup(t, &fakeModel{})
	st.s.model = &fakeModel{responses: []string{planScript(st, 5)}}
	ctx := context.Background()

	res, err := st.s.GeneratePlan(ctx, PlanInput{CampaignID: st.fx.Campaign.ID, Notes: "lean into the mines"})
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(res.Sessions))
	}
	if res.Budget != 5 {
		t.Fatalf("budget = %d, want 5 (act 1-3 across 4 sessions)", res.Budget)
	}
	ps := res.Sessions[0]
	if len(ps.Scenes) != 5 {
		t.Fatalf("scenes = %d, want 5", len(ps.Scenes))
	}
	if !ps.NewPlan || ps.Goal == "" {
		t.Errorf("the session's plan was not written (new=%v goal=%q)", ps.NewPlan, ps.Goal)
	}
	var dead []string // outcomes whose consequence is the batch's awareness change

	// The mix is the setup template's: our arithmetic, not the model's.
	wantMix := story.SceneMix(0, 4, 5)
	for i, sc := range ps.Scenes {
		if sc.Kind != wantMix[i] {
			t.Errorf("scene %d kind = %s, want %s", i+1, sc.Kind, wantMix[i])
		}
		if sc.SessionID != ps.SessionID || sc.ActID != st.act1.ID {
			t.Errorf("scene %d seated wrong: session %s act %s", i+1, sc.SessionID, sc.ActID)
		}
		if sc.SettingEntity != st.fx.Blackwater {
			t.Errorf("scene %d setting = %s, want Blackwater", i+1, sc.SettingEntity)
		}
		// The cast: the Duke in focus, Tom present — resolved against the
		// pool, never invented.
		roles := map[string]string{}
		for _, c := range sc.Cast {
			roles[c.EntityID] = c.Role
		}
		if roles[st.fx.Duke] != story.RoleFocus || roles[st.fx.Tom] != story.RolePresent {
			t.Errorf("scene %d cast = %v", i+1, roles)
		}
		// Every outcome resolves to a real consequence — a scene, a quest
		// edge, or (for exactly one, asserted below) the awareness change
		// the batch carries.
		for _, o := range sc.Outcomes {
			if o.LeadsToScene == "" && o.QuestTransition == nil {
				dead = append(dead, fmt.Sprintf("%d/%s", i+1, o.Label))
			}
		}
	}

	// The secret set: only the fresh secret, only once, and — the issue's
	// own bar — no fact the party's awareness already grants.
	snap := loadSnap(t, st.db, st.fx.Campaign.ID)
	granted := partyGrantedFacts(snap)
	var secretIDs []string
	for _, sc := range ps.Scenes {
		for _, sec := range sc.Secrets {
			secretIDs = append(secretIDs, sec.FactID)
			if granted[sec.FactID] {
				t.Errorf("scene %q puts fact %s in play, but the party already holds it", sc.Name, sec.FactID)
			}
			if sec.FactID != st.secret {
				t.Errorf("secret %s is not the fresh unreached secret", sec.FactID)
			}
		}
	}
	if len(secretIDs) != 1 {
		t.Errorf("secrets placed = %v, want exactly the fresh one", secretIDs)
	}

	// The quest outcomes carry validated transitions from the current state.
	if len(dead) != 1 || dead[0] != "3/B" {
		t.Errorf("outcomes resolving to nothing: %v (want only the awareness change, scene 3 outcome B)", dead)
	}
	quests := map[string]campaign.Quest{}
	for _, q := range snap.Quests {
		quests[q.ID] = q
	}
	sawQuest := false
	for _, sc := range ps.Scenes {
		for _, o := range sc.Outcomes {
			if tr := o.QuestTransition; tr != nil {
				sawQuest = true
				q := quests[tr.QuestID]
				if !q.Machine.HasEdge(tr.From, tr.To) || tr.From != q.CurrentState {
					t.Errorf("outcome %s transition %v is not a current edge of %s", o.Label, tr, q.Name)
				}
			}
		}
	}
	if !sawQuest {
		t.Error("no quest outcome landed")
	}

	// The drops: the granted secret, the invented secret, the illegal quest
	// edge and the consequence-less outcome are all named and gone.
	joined := strings.Join(ps.Dropped, "\n")
	for _, marker := range []string{"already", "not an unreached secret", "machine currently allows", "names no consequence"} {
		if !strings.Contains(joined, marker) {
			t.Errorf("drops missing %q: %v", marker, ps.Dropped)
		}
	}

	// The batch: one proposed discovery — the awareness change outcome B of
	// scene 3 promised — staged behind the gate, nothing else.
	if res.Batch == nil {
		t.Fatal("no batch staged for the awareness outcome")
	}
	if res.Batch.Source != BatchSourceStoryPlan {
		t.Errorf("source = %s", res.Batch.Source)
	}
	if len(res.Batch.Items) != 1 {
		t.Fatalf("batch items = %d, want the single awareness change", len(res.Batch.Items))
	}
	item := res.Batch.Items[0]
	if item.Kind != ReviewProposedDiscovery {
		t.Errorf("item kind = %s", item.Kind)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(item.Detail), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["fact"] != st.fx.FactDukeVisited || payload["discovered_by"] != campaign.PartyKnower || payload["stance"] != "knows" {
		t.Errorf("payload = %v", payload)
	}

	// Nothing in the awareness tables moved yet: the gate holds.
	if rows := awarenessRows(t, st.db, st.fx.Campaign.ID, st.fx.FactDukeVisited); rows != 0 {
		t.Errorf("awareness rows for the promised fact = %d before any decision", rows)
	}

	// Accept, and the party holds the fact.
	if _, err := st.s.DecideBatch(ctx, st.fx.Campaign.ID, res.Batch.ID, DecisionAccept, nil, "keeper"); err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	if rows := awarenessRows(t, st.db, st.fx.Campaign.ID, st.fx.FactDukeVisited); rows == 0 {
		t.Error("accepting the batch wrote no awareness row")
	}

	// The spine the plan wrote passes its own rules: no new error findings.
	before := findingSet(t, st.s, st.fx.Campaign.ID) // after acceptance, for stability
	snap = loadSnap(t, st.db, st.fx.Campaign.ID)
	for _, f := range story.Validate(snap.Spine) {
		if f.Severity == campaign.SeverityError {
			t.Errorf("spine rule fired: %s: %s", f.Check, f.Message)
		}
	}
	if f, ok := before[CheckUnreachableSecret+"/"+st.secret]; ok {
		t.Errorf("the planned secret is unreachable: %s", f.Message)
	}
}

func TestGeneratePlan_ReplanIsNonDestructive(t *testing.T) {
	st := planStackSetup(t, &fakeModel{})
	st.s.model = &fakeModel{responses: []string{planScript(st, 5), planScript(st, 5)}}
	ctx := context.Background()

	first, err := st.s.GeneratePlan(ctx, PlanInput{CampaignID: st.fx.Campaign.ID})
	if err != nil {
		t.Fatalf("first plan: %v", err)
	}
	origIDs := map[string]bool{}
	var origGoal string
	for _, sc := range first.Sessions[0].Scenes {
		origIDs[sc.ID] = true
	}
	origGoal = first.Sessions[0].Goal
	origPlan, err := st.stories.GetPlan(ctx, campaign.ScopeDM, st.fx.Campaign.ID, first.Sessions[0].SessionID)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}

	// The re-plan: a second batch beside the first, a second set of scenes
	// appended, and the first set untouched.
	second, err := st.s.GeneratePlan(ctx, PlanInput{CampaignID: st.fx.Campaign.ID, Notes: "again, darker"})
	if err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	if second.Sessions[0].SessionID != first.Sessions[0].SessionID {
		t.Fatalf("re-plan moved to session %s", second.Sessions[0].SessionID)
	}
	if second.Sessions[0].NewPlan {
		t.Error("the re-plan overwrote the session's plan row")
	}
	if second.Batch == nil || second.Batch.ID == first.Batch.ID {
		t.Errorf("re-plan batch = %+v, want a new one", second.Batch)
	}
	scenes, err := st.stories.ListScenes(ctx, campaign.ScopeDM, st.fx.Campaign.ID, st.act1.ID)
	if err != nil {
		t.Fatalf("list scenes: %v", err)
	}
	var surviving int
	for _, sc := range scenes {
		if origIDs[sc.ID] {
			surviving++
		}
	}
	if surviving != len(origIDs) {
		t.Fatalf("the first plan's scenes were touched: %d of %d remain", surviving, len(origIDs))
	}
	if len(scenes) != len(origIDs)*2 {
		t.Fatalf("act scenes = %d, want the two candidate sets (%d)", len(scenes), len(origIDs)*2)
	}
	nowPlan, err := st.stories.GetPlan(ctx, campaign.ScopeDM, st.fx.Campaign.ID, first.Sessions[0].SessionID)
	if err != nil {
		t.Fatalf("read plan again: %v", err)
	}
	if nowPlan.Goal != origGoal || nowPlan.UpdatedAt.UnixMilli() != origPlan.UpdatedAt.UnixMilli() {
		t.Error("the session's plan row changed under a re-plan")
	}
}

func TestGeneratePlan_ActModePlansTheWholeAct(t *testing.T) {
	// Act 2 (levels 4-6) has no sessions: the run creates its paced
	// complement and plans every one of them.
	st := planStackSetup(t, &fakeModel{})
	ctx := context.Background()
	want := story.Pace(4, 6, 1).PerAct[0].Sessions
	budget := story.ScenesPerSession(4, 6, want)
	script := planScript(st, budget)
	st.s.model = &fakeModel{responses: repeatStrings(script, want)}

	res, err := st.s.GeneratePlan(ctx, PlanInput{CampaignID: st.fx.Campaign.ID, Mode: PlanModeAct, ActID: st.acts[1].ID})
	if err != nil {
		t.Fatalf("GeneratePlan act mode: %v", err)
	}
	if len(res.Sessions) != want {
		t.Fatalf("sessions planned = %d, want the paced %d", len(res.Sessions), want)
	}
	mix := story.SceneMix(1, 4, budget) // act 2 of 4: the complication template
	for _, ps := range res.Sessions {
		if len(ps.Scenes) != budget {
			t.Fatalf("session %s scenes = %d, want %d", ps.SessionID, len(ps.Scenes), budget)
		}
		for i, sc := range ps.Scenes {
			if sc.Kind != mix[i] {
				t.Errorf("scene kind = %s, want %s", sc.Kind, mix[i])
			}
			if sc.ActID != st.acts[1].ID {
				t.Errorf("scene seated on act %s", sc.ActID)
			}
		}
		if !ps.NewPlan {
			t.Errorf("session %s plan was not written", ps.SessionID)
		}
	}
	// Act mode over an act whose sessions all have scenes refuses rather
	// than overwriting.
	if _, err := st.s.GeneratePlan(ctx, PlanInput{CampaignID: st.fx.Campaign.ID, Mode: PlanModeAct, ActID: st.acts[1].ID}); err == nil {
		t.Error("act mode over an already-planned act should refuse")
	}
}

func TestGeneratePlan_ValidationAndOffline(t *testing.T) {
	st := planStackSetup(t, &fakeModel{})
	st.s.model = &fakeModel{responses: []string{planScript(st, 5)}}
	ctx := context.Background()
	base := PlanInput{CampaignID: st.fx.Campaign.ID}

	bad := base
	bad.Mode = "week"
	if _, err := st.s.GeneratePlan(ctx, bad); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("a bad mode should refuse, got %v", err)
	}
	long := base
	long.Notes = strings.Repeat("x", planNotesMaxLen+1)
	if _, err := st.s.GeneratePlan(ctx, long); err == nil {
		t.Error("over-long notes should refuse")
	}
	missing := base
	missing.ActID = "no-such-act"
	if _, err := st.s.GeneratePlan(ctx, missing); err == nil {
		t.Error("an unknown act should refuse")
	}

	// A campaign with no acts has nothing to plan into.
	db, fx2, _ := seeded(t)
	if _, err := db.Exec(`DELETE FROM acts`); err != nil {
		t.Fatal(err)
	}
	s2 := skeletonStore(t, db, &fakeModel{responses: []string{planScript(st, 5)}})
	if _, err := s2.GeneratePlan(ctx, PlanInput{CampaignID: fx2.Campaign.ID}); err == nil || !strings.Contains(err.Error(), "no acts") {
		t.Errorf("a spineless campaign should refuse, got %v", err)
	}

	// The offline store refuses with the model-driven error.
	offline, err := NewOffline(st.db)
	if err != nil {
		t.Fatalf("offline store: %v", err)
	}
	offline = offline.WithGraphStores(st.s.campaigns, st.s.knowledge)
	if _, err := offline.GeneratePlan(ctx, base); err == nil {
		t.Error("the offline store should refuse to plan")
	}
	if _, err := offline.GenerateScene(ctx, SceneInput{CampaignID: st.fx.Campaign.ID}); err == nil {
		t.Error("the offline store should refuse to design")
	}
}

/* ---------- the scene-designer exchange ---------- */

func TestGenerateScene_DesignsOneScene(t *testing.T) {
	st := planStackSetup(t, &fakeModel{})
	ctx := context.Background()
	// Venn needs a signal to be plausible cast: an unresolved goal.
	if _, err := campaignStore(t, st.db).UpdateEntity(ctx, st.fx.Campaign.ID, st.fx.Venn, nil, nil, nil,
		map[string]any{"agent": map[string]any{"goals": []string{"tend the mining camps' sick"}}}); err != nil {
		t.Fatalf("give Venn a goal: %v", err)
	}
	// One existing scene in the act for a leads_to target, with the fresh
	// secret's discoverer on stage.
	existing, err := st.stories.CreateScene(ctx, st.fx.Campaign.ID, st.act1.ID, "",
		story.KindSocial, "The Waystone at dusk", "Tom counts the cups and says nothing.", st.fx.Blackwater)
	if err != nil {
		t.Fatalf("seed scene: %v", err)
	}
	if _, err := st.stories.AddCast(ctx, st.fx.Campaign.ID, existing.ID, st.fx.Tom, story.RoleFocus); err != nil {
		t.Fatal(err)
	}

	script := func() string {
		m := map[string]any{
			"scene_name":        "The steward's ledger",
			"scene_purpose":     "The charter the Duke's steward signed is dated wrong, and Tom saw who delivered it.",
			"scene_cast":        "Tom the Innkeeper:focus, Brother Venn, Cult of the Root:offstage",
			"scene_secrets":     st.secret + ":revealed_if",
			"outcome_A_summary": "The party copies the ledger.",
			"outcome_A_result":  "aware:" + st.fx.FactMinesOwned + ":suspects",
			"outcome_B_summary": "The party follows the courier instead.",
			"outcome_B_result":  "scene:" + existing.ID,
			"outcome_C_summary": "Nothing comes of it.",
			"outcome_C_result":  "",
			"outcome_D_summary": "The cult intervenes.",
			"outcome_D_result":  "quest:" + st.fx.QuestID + ":wreckage_found", // not an edge from the current state
		}
		b, _ := json.Marshal(m)
		return string(b)
	}()
	st.s.model = &fakeModel{responses: []string{script}}

	res, err := st.s.GenerateScene(ctx, SceneInput{
		CampaignID: st.fx.Campaign.ID, ActID: st.act1.ID,
		Setting: "Blackwater", Notes: "a quiet scene before the mines",
	})
	if err != nil {
		t.Fatalf("GenerateScene: %v", err)
	}
	sc := res.Scene
	if sc.ActID != st.act1.ID || sc.SettingEntity != st.fx.Blackwater {
		t.Fatalf("scene seated wrong: %+v", sc)
	}
	// The kind defaults to the act's template slot: the act holds one
	// social scene, so slot 2 deals exploration.
	if sc.Kind != story.KindExploration {
		t.Errorf("kind = %s, want the template's next slot (exploration)", sc.Kind)
	}
	roles := map[string]string{}
	for _, c := range sc.Cast {
		roles[c.EntityID] = c.Role
	}
	if roles[st.fx.Tom] != story.RoleFocus || roles[st.fx.Venn] != story.RolePresent || roles[st.fx.Cult] != story.RoleOffstage {
		t.Errorf("cast = %v", roles)
	}
	if len(sc.Secrets) != 1 || sc.Secrets[0].FactID != st.secret || sc.Secrets[0].Disposition != story.DispositionRevealedIf {
		t.Errorf("secrets = %+v", sc.Secrets)
	}
	// Outcome A stages the awareness change; B points at the existing
	// scene; C and D are dropped with their reasons.
	var leads, awareStaged bool
	for _, o := range sc.Outcomes {
		if o.Label == "B" {
			if o.LeadsToScene == existing.ID {
				leads = true
			}
		}
	}
	joined := strings.Join(res.Dropped, "\n")
	if !strings.Contains(joined, "names no consequence") || !strings.Contains(joined, "machine currently allows") {
		t.Errorf("drops missing the dead outcomes: %v", res.Dropped)
	}
	if !leads {
		t.Error("outcome B does not lead to the existing scene")
	}
	if res.Batch != nil {
		awareStaged = true
		if len(res.Batch.Items) != 1 || res.Batch.Items[0].Kind != ReviewProposedDiscovery {
			t.Errorf("batch = %+v", res.Batch.Items)
		}
		if res.Batch.Source != BatchSourceScene {
			t.Errorf("source = %s", res.Batch.Source)
		}
	}
	if !awareStaged {
		t.Error("the awareness outcome staged no batch")
	}

	// The cast pool kept the deity out and the setting's people in: the
	// prompt the model saw proves it.
	prompt := st.s.model.(*fakeModel).calls[0]
	if strings.Contains(prompt, "The Verdant God") {
		t.Error("the deity leaked into the cast pool")
	}
	for _, marker := range []string{"Tom the Innkeeper", st.secret, "trusted_survivor"} {
		if !strings.Contains(prompt, marker) {
			t.Errorf("prompt missing %q", marker)
		}
	}
}

func TestGenerateScene_Validation(t *testing.T) {
	st := planStackSetup(t, &fakeModel{responses: []string{`{}`}})
	ctx := context.Background()
	base := SceneInput{CampaignID: st.fx.Campaign.ID}

	badSetting := base
	badSetting.Setting = "Atlantis"
	if _, err := st.s.GenerateScene(ctx, badSetting); err == nil || !strings.Contains(err.Error(), "not a location") {
		t.Errorf("an unknown setting should refuse, got %v", err)
	}
	badKind := base
	badKind.Kind = "musical"
	if _, err := st.s.GenerateScene(ctx, badKind); err == nil || !strings.Contains(err.Error(), "scene kind") {
		t.Errorf("an unknown kind should refuse, got %v", err)
	}
	badSession := base
	badSession.SessionID = "no-such-session"
	if _, err := st.s.GenerateScene(ctx, badSession); err == nil {
		t.Error("an unknown session should refuse")
	}
}

/* ---------- helpers ---------- */

func knowledgeStore(t *testing.T, db *sql.DB) *knowledge.Store {
	t.Helper()
	ks, err := knowledge.New(db)
	if err != nil {
		t.Fatalf("knowledge store: %v", err)
	}
	return ks
}

func mustDiscovery(t *testing.T, ks *knowledge.Store, campaignID, factID, knower, stance string) {
	t.Helper()
	if _, err := ks.RecordDiscovery(context.Background(), knowledge.RecordDiscoveryInput{
		CampaignID: campaignID, FactID: factID, DiscoveredBy: knower,
		Stance: stance, Confidence: 1, AcceptedBy: "keeper",
		Method: "test",
	}); err != nil {
		t.Fatalf("record discovery: %v", err)
	}
}

func mustFact(t *testing.T, db *sql.DB, fx *campaign.Fixture, statement string, secret bool) string {
	t.Helper()
	visibility := campaign.VisibilityPublic
	if secret {
		visibility = campaign.VisibilitySecret
	}
	cs := campaignStore(t, db)
	f, err := cs.CreateFact(context.Background(), fx.Campaign.ID, fx.Monastery, "hides", "", "a dreaming heart",
		statement, campaign.ConfidenceCanon, visibility, "keeper", []campaign.ProvenanceInput{{
			Method: campaign.MethodDMAuthored,
		}})
	if err != nil {
		t.Fatalf("create fact: %v", err)
	}
	return f.ID
}

func campaignStore(t *testing.T, db *sql.DB) *campaign.Store {
	t.Helper()
	cs, err := campaign.New(db)
	if err != nil {
		t.Fatalf("campaign store: %v", err)
	}
	return cs
}

func mustStory(t *testing.T, db *sql.DB) *story.Store {
	t.Helper()
	st, err := story.New(db)
	if err != nil {
		t.Fatalf("story store: %v", err)
	}
	return st
}

func mustSessions(t *testing.T, db *sql.DB) *gamesession.Store {
	t.Helper()
	ss, err := gamesession.New(db)
	if err != nil {
		t.Fatalf("gamesession store: %v", err)
	}
	return ss
}

func loadSnap(t *testing.T, db *sql.DB, campaignID string) *Snapshot {
	t.Helper()
	snap, err := LoadSnapshot(context.Background(), db, campaignID)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	return snap
}

func awarenessRows(t *testing.T, db *sql.DB, campaignID, factID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM awareness WHERE campaign_id = ? AND fact_id = ?`, campaignID, factID).Scan(&n); err != nil {
		t.Fatalf("count awareness: %v", err)
	}
	return n
}

func repeatStrings(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}
