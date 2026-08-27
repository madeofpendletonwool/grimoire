package canon

// One-click session prep's tests (MAD-364): the prep-time estimator against
// hand-built campaign states, the deterministic scoring over the seeded
// fixture, the offline guarantee (a ranked list with no model configured),
// the pitch exchange with a fake LLM, and the end-to-end build — the NPC
// sheet scope guarantee above all (ADR 8: e2e over a temp SQLite database
// with a fake LLM client replaying fixtures).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

/* ---------- the estimator ---------- */

func TestEstimatePrep_HandBuiltStates(t *testing.T) {
	cases := []struct {
		name string
		in   EstimateInput
		want int
	}{
		{
			name: "empty mix is just the base overhead",
			in:   EstimateInput{},
			want: prepBaseMinutes,
		},
		{
			name: "one quiet session, synced bestiary, no combat",
			in: EstimateInput{
				Mix:            []string{story.KindSocial, story.KindExploration, story.KindDowntime},
				BestiarySynced: true,
			},
			// 5 base + 5 social + 5 exploration + 3 downtime
			want: 5 + 5 + 5 + 3,
		},
		{
			name: "combat against a synced bestiary prices its roster",
			in: EstimateInput{
				Mix:                  []string{story.KindSocial, story.KindCombat, story.KindRevelation},
				BestiarySynced:       true,
				MonstersPerEncounter: 4,
			},
			// 5 + 5 + 12 + 8, plus 4 resolvable statblocks at 1 minute
			want: 5 + 5 + 12 + 8 + 4,
		},
		{
			name: "combat with no bestiary prices fetching statblocks by hand",
			in: EstimateInput{
				Mix: []string{story.KindCombat, story.KindCombat},
			},
			// 5 + 12 + 12, plus 8 per combat for the manual statblock work
			want: 5 + 12 + 12 + 8 + 8,
		},
		{
			name: "cast NPCs missing agent fields are priced",
			in: EstimateInput{
				Mix:              []string{story.KindSocial},
				NPCsMissingAgent: 3,
			},
			// 5 + 5 + three NPCs at 4 minutes each
			want: 5 + 5 + 3*prepNPCAgentMinute,
		},
		{
			name: "the pack shape's count defaults when unset",
			in: EstimateInput{
				Mix:            []string{story.KindCombat},
				BestiarySynced: true,
			},
			want: 5 + 12 + defaultMonstersPerEnc,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimatePrep(tc.in); got != tc.want {
				t.Fatalf("EstimatePrep(%+v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

/* ---------- the fixture stack ---------- */

// prepStack is the seeded fixture plus a spine to plan into, in the shape
// the prep tests need. acts=1 gives the default template (combat included);
// acts=4 mirrors the planner's four-act fixture.
type prepStack struct {
	db       *sql.DB
	fx       *campaign.Fixture
	s        *Store
	stories  *story.Store
	sessions *gamesession.Store
	ks       *knowledge.Store
	act1     story.Act
	secret   string
}

func prepStackSetup(t *testing.T, model ModelClient, acts int) *prepStack {
	t.Helper()
	db, fx, _ := seeded(t)
	st := &prepStack{
		db: db, fx: fx,
		stories:  mustStory(t, db),
		sessions: mustSessions(t, db),
		ks:       knowledgeStore(t, db),
	}
	if model != nil {
		st.s = skeletonStore(t, db, model)
	} else {
		offline, err := NewOffline(db)
		if err != nil {
			t.Fatalf("offline store: %v", err)
		}
		cs := campaignStore(t, db)
		st.s = offline.WithGraphStores(cs, st.ks)
	}
	ctx := context.Background()
	for i := 0; i < acts; i++ {
		lo := 1 + i*3
		act, err := st.stories.CreateAct(ctx, fx.Campaign.ID,
			fmt.Sprintf("Act %d", i+1), "The noose tightens.", lo, lo+2)
		if err != nil {
			t.Fatalf("create act: %v", err)
		}
		if i == 0 {
			st.act1 = *act
		}
	}
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
	// The fixture's own secret is one the party has reached; a fresh one is
	// tonight's buried material.
	mustDiscovery(t, st.ks, fx.Campaign.ID, fx.FactKeyOpensCrypt, campaign.PartyKnower, "knows")
	st.secret = mustFact(t, db, fx, "The crypt beneath Greyfall holds the Verdant God's dreaming heart.", true)
	return st
}

// giveVennAGoal arms the fixture's stalled-goal NPC.
func (st *prepStack) giveVennAGoal(t *testing.T) {
	t.Helper()
	if _, err := campaignStore(t, st.db).UpdateEntity(context.Background(), st.fx.Campaign.ID, st.fx.Venn, nil, nil, nil,
		map[string]any{"agent": map[string]any{
			"goals": []string{"find out who marched the miners off"},
			"voice": "soft-spoken, smells of lamp oil",
		}}); err != nil {
		t.Fatalf("give Venn a goal: %v", err)
	}
}

func directionByKind(dirs []PrepDirection, kind string) *PrepDirection {
	for i := range dirs {
		if dirs[i].Kind == kind {
			return &dirs[i]
		}
	}
	return nil
}

/* ---------- the deterministic ranking ---------- */

func TestDirections_OfflineScoredWithEvidenceAndEstimates(t *testing.T) {
	st := prepStackSetup(t, nil, 4)
	st.giveVennAGoal(t)
	ctx := context.Background()

	res, err := st.s.Directions(ctx, DirectionsInput{CampaignID: st.fx.Campaign.ID})
	if err != nil {
		t.Fatalf("Directions offline: %v", err)
	}
	if !res.Offline {
		t.Error("an offline store must report offline")
	}
	if len(res.Directions) == 0 || len(res.Directions) > prepDirectionCap {
		t.Fatalf("directions = %d, want 1..%d", len(res.Directions), prepDirectionCap)
	}

	// The fixture's four signals all surfaced: the quest's legal edges, the
	// buried secret, Venn's stalled goal, the live-but-offstage faction.
	for _, kind := range []string{DirectionAdvanceQuest, DirectionSurfaceSecret, DirectionPushGoal, DirectionFactionMoves} {
		if directionByKind(res.Directions, kind) == nil {
			t.Errorf("direction kind %s missing from %v", kind, idsOf(res.Directions))
		}
	}

	// Ranked by score, every score is its evidence's sum, every direction
	// carries a positive estimate and a deterministic pitch.
	last := 1 << 30
	for _, d := range res.Directions {
		sum := 0
		for _, ev := range d.Evidence {
			sum += ev.Weight
			if ev.Kind == "" || ev.Ref == "" || ev.Label == "" {
				t.Errorf("direction %s has a thin evidence row: %+v", d.ID, ev)
			}
		}
		if d.Score != sum {
			t.Errorf("direction %s score = %d, evidence sums %d", d.ID, d.Score, sum)
		}
		if d.PrepMinutes <= 0 {
			t.Errorf("direction %s prep minutes = %d", d.ID, d.PrepMinutes)
		}
		if d.Advances == "" || d.Title == "" || d.Pitch != d.Advances {
			t.Errorf("direction %s offline prose = %q/%q/%q", d.ID, d.Title, d.Advances, d.Pitch)
		}
		if d.Score > last {
			t.Errorf("directions not ranked: %d after %d", d.Score, last)
		}
		last = d.Score
	}

	// The quest direction names only edges its machine currently allows.
	qd := directionByKind(res.Directions, DirectionAdvanceQuest)
	var tos []string
	for _, ev := range qd.Evidence {
		if ev.Kind == evidenceQuestEdge {
			tos = strings.Split(afterColon(ev.Note), ", ")
		}
	}
	if len(tos) == 0 {
		t.Fatalf("quest evidence carries no edges: %+v", qd.Evidence)
	}
	for _, to := range tos {
		if !qd.quest.Machine.HasEdge(qd.quest.CurrentState, to) {
			t.Errorf("edge %q is not legal from %s", to, qd.quest.CurrentState)
		}
	}

	// The context the list was computed from.
	if res.Act == nil || res.Act.ID != st.act1.ID {
		t.Errorf("act context = %+v", res.Act)
	}
	if res.Budget == 0 || len(res.Mix) != res.Budget {
		t.Errorf("budget = %d, mix = %v", res.Budget, res.Mix)
	}
	if res.WhereThePartyIs == "" {
		t.Error("the timeline tail did not render")
	}
}

func idsOf(dirs []PrepDirection) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d.ID
	}
	return out
}

func afterColon(s string) string {
	if i := strings.Index(s, ":"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func TestDirections_UnknownCampaignAndLongNotes(t *testing.T) {
	st := prepStackSetup(t, nil, 1)
	ctx := context.Background()
	if _, err := st.s.Directions(ctx, DirectionsInput{CampaignID: "no-such"}); err == nil {
		t.Error("an unknown campaign should refuse")
	}
	if _, err := st.s.Directions(ctx, DirectionsInput{
		CampaignID: st.fx.Campaign.ID, Notes: strings.Repeat("x", planNotesMaxLen+1)}); err == nil {
		t.Error("over-long notes should refuse")
	}
}

/* ---------- the pitch exchange ---------- */

func TestDirections_ModelWritesPitchesOverOurRanking(t *testing.T) {
	st := prepStackSetup(t, &fakeModel{}, 4)
	st.giveVennAGoal(t)
	ctx := context.Background()

	// Script the pitch call for the four directions the fixture yields.
	script := func(n int) string {
		m := map[string]any{}
		for i := 1; i <= n; i++ {
			m[fmt.Sprintf("direction_%d_title", i)] = fmt.Sprintf("Model title %d", i)
			m[fmt.Sprintf("direction_%d_pitch", i)] = fmt.Sprintf("Model pitch %d in the kingdom's own words.", i)
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	// Two responses: Directions makes one call; the second proves the
	// ranking below came from the deterministic pass, not the model.
	model := &fakeModel{responses: []string{script(4), script(4)}}
	st.s.model = model

	res, err := st.s.Directions(ctx, DirectionsInput{CampaignID: st.fx.Campaign.ID, Notes: "lean into the mines"})
	if err != nil {
		t.Fatalf("Directions with a model: %v", err)
	}
	if res.Offline {
		t.Error("a wired model must not report offline")
	}
	if len(model.calls) != 1 {
		t.Fatalf("model calls = %d, want exactly the one pitch exchange", len(model.calls))
	}
	for i, d := range res.Directions {
		if d.Title != fmt.Sprintf("Model title %d", i+1) || d.Pitch != fmt.Sprintf("Model pitch %d in the kingdom's own words.", i+1) {
			t.Errorf("direction %d prose = %q / %q", i, d.Title, d.Pitch)
		}
	}
	// The model saw the evidence and the DM's notes, and never a reorder
	// instruction: the ranking is ours.
	prompt := model.calls[0]
	for _, marker := range []string{"lean into the mines", "The Missing Miners", "evidence"} {
		if !strings.Contains(prompt, marker) {
			t.Errorf("pitch prompt missing %q", marker)
		}
	}
	if !strings.Contains(prompt, "already ranked") && !strings.Contains(prepPitchSystemPrompt, "already-ranked") {
		t.Error("the pitch contract must pin the ranking")
	}
}

/* ---------- the offline build ---------- */

func TestBuildPrep_OfflineDeterministicScaffold(t *testing.T) {
	st := prepStackSetup(t, nil, 4)
	ctx := context.Background()

	dirs, err := st.s.Directions(ctx, DirectionsInput{CampaignID: st.fx.Campaign.ID})
	if err != nil {
		t.Fatalf("directions: %v", err)
	}
	qd := directionByKind(dirs.Directions, DirectionAdvanceQuest)

	res, err := st.s.BuildPrep(ctx, BuildInput{
		CampaignID: st.fx.Campaign.ID, DirectionID: qd.ID, CreatedBy: "keeper",
	})
	if err != nil {
		t.Fatalf("BuildPrep offline: %v", err)
	}
	if !res.Offline || res.Batch != nil {
		t.Errorf("offline build = online-shaped (offline=%v batch=%v)", res.Offline, res.Batch)
	}
	// The scaffold: the arithmetic's budget and mix, seated on the first
	// planned session with no scenes.
	if len(res.Scenes) != dirs.Budget || len(res.Scenes) != story.ScenesPerSession(st.act1.LevelStart, st.act1.LevelEnd, 4) {
		t.Fatalf("scenes = %d, want the budget", len(res.Scenes))
	}
	wantMix := story.SceneMix(0, 4, dirs.Budget)
	for i, sc := range res.Scenes {
		if sc.Kind != wantMix[i] || sc.SessionID != res.SessionID || sc.ActID != st.act1.ID {
			t.Errorf("scene %d = %s/%s/%s", i+1, sc.Kind, sc.SessionID, sc.ActID)
		}
	}
	if res.Goal == "" {
		t.Error("no session goal written")
	}
	// The quest direction's payoff is a validated transition on the last
	// scene; every earlier scene chains forward.
	var sawTransition bool
	for i, sc := range res.Scenes {
		last := i == len(res.Scenes)-1
		if len(sc.Outcomes) == 0 {
			t.Errorf("scene %d has no outcome", i+1)
		}
		for _, o := range sc.Outcomes {
			if o.QuestTransition != nil {
				sawTransition = true
				if o.QuestTransition.QuestID != st.fx.QuestID ||
					!qd.quest.Machine.HasEdge(o.QuestTransition.From, o.QuestTransition.To) {
					t.Errorf("outcome transition = %+v", o.QuestTransition)
				}
			}
			if !last && o.LeadsToScene == "" {
				t.Errorf("mid-session outcome %s resolves nowhere", o.Label)
			}
		}
	}
	if !sawTransition {
		t.Error("the advance_quest build promised no quest move")
	}

	// The setup template carries no combat scene: nothing to plan against.
	if len(res.Encounters) != 0 {
		t.Errorf("encounters = %d, want 0 for a combat-free mix", len(res.Encounters))
	}
	// Every branch of this build resolves somewhere: no contingencies.
	if len(res.Package.Contingencies) != 0 {
		t.Errorf("contingencies = %v, want none for the fully-chained scaffold", res.Package.Contingencies)
	}
}

func TestBuildPrep_StaleDirectionIDRefused(t *testing.T) {
	st := prepStackSetup(t, nil, 1)
	ctx := context.Background()
	if _, err := st.s.BuildPrep(ctx, BuildInput{
		CampaignID: st.fx.Campaign.ID, DirectionID: "advance_quest:ghost"}); err == nil ||
		!strings.Contains(err.Error(), "re-run") {
		t.Errorf("a stale direction id should refuse, got %v", err)
	}
	if _, err := st.s.BuildPrep(ctx, BuildInput{CampaignID: st.fx.Campaign.ID}); err == nil {
		t.Error("a missing direction id should refuse")
	}
	if _, err := st.s.BuildPrep(ctx, BuildInput{
		CampaignID: st.fx.Campaign.ID, DirectionID: "advance_quest:" + st.fx.QuestID,
		Band: "brutal"}); err == nil || !strings.Contains(err.Error(), "band") {
		t.Errorf("an unknown band should refuse, got %v", err)
	}
}

/* ---------- the NPC sheet scope guarantee ---------- */

func TestBuildPrep_NPCSheetsNeverLeaveNPCScope(t *testing.T) {
	st := prepStackSetup(t, nil, 1)
	st.giveVennAGoal(t)
	ctx := context.Background()

	// Tom knows something Venn does not, and Venn knows something Tom does
	// not — both secrets, both NPC-scoped.
	tomsSecret := mustFact(t, st.db, st.fx, "Tom's cellar hides the cult's tithe of silver.", true)
	vennsSecret := mustFact(t, st.db, st.fx, "Venn buried a miner who would not stay buried.", true)
	mustDiscovery(t, st.ks, st.fx.Campaign.ID, tomsSecret, st.fx.Tom, "knows")
	mustDiscovery(t, st.ks, st.fx.Campaign.ID, vennsSecret, st.fx.Venn, "knows")

	dirs, err := st.s.Directions(ctx, DirectionsInput{CampaignID: st.fx.Campaign.ID})
	if err != nil {
		t.Fatalf("directions: %v", err)
	}
	vd := directionByKind(dirs.Directions, DirectionPushGoal)
	if vd == nil {
		t.Fatalf("no push_goal direction: %v", idsOf(dirs.Directions))
	}
	res, err := st.s.BuildPrep(ctx, BuildInput{
		CampaignID: st.fx.Campaign.ID, DirectionID: vd.ID, CreatedBy: "keeper",
	})
	if err != nil {
		t.Fatalf("BuildPrep: %v", err)
	}

	// The cast NPC is Venn; his sheet exists and says what he knows.
	var vennSheet *NPCSheet
	for i := range res.Package.NPCSheets {
		if res.Package.NPCSheets[i].EntityID == st.fx.Venn {
			vennSheet = &res.Package.NPCSheets[i]
		}
	}
	if vennSheet == nil {
		t.Fatalf("no sheet for the cast NPC: %+v", res.Package.NPCSheets)
	}
	if vennSheet.Voice == "" || vennSheet.Goal == "" {
		t.Errorf("sheet voice/goal = %q/%q", vennSheet.Voice, vennSheet.Goal)
	}
	joined := strings.Join(vennSheet.Knows, "\n")
	if !strings.Contains(joined, "Venn buried a miner") {
		t.Errorf("Venn's sheet omits what Venn knows: %v", vennSheet.Knows)
	}
	if strings.Contains(joined, "Tom's cellar") {
		t.Error("LEAK: Venn's sheet contains a fact only Tom is aware of")
	}

	// The acceptance bar, mechanically: for every sheet on the package,
	// every statement on it is inside that NPC's own scoped read — the
	// sheet builder consumed the scoped read, and nothing else can appear.
	for _, sheet := range res.Package.NPCSheets {
		scoped, err := st.ks.Facts(ctx, knowledge.ScopeNPC(sheet.EntityID), st.fx.Campaign.ID, knowledge.FactFilter{})
		if err != nil {
			t.Fatalf("scoped read for %s: %v", sheet.Name, err)
		}
		allowed := map[string]bool{}
		for _, f := range scoped {
			allowed[f.Statement] = true
		}
		for _, k := range sheet.Knows {
			if !allowed[k] {
				t.Errorf("LEAK: sheet for %s carries %q, outside that NPC's awareness", sheet.Name, k)
			}
		}
		// And the DM-side secrets the NPC has no row for never reached the
		// sheet at all.
		if sheet.EntityID == st.fx.Venn && allowed["Tom's cellar hides the cult's tithe of silver."] {
			t.Error("the scoped read itself leaked — the store's filter is broken")
		}
	}
}

/* ---------- questions, contingencies, rulings ---------- */

func TestBuildPrep_PackageQuestionsContingenciesRulings(t *testing.T) {
	st := prepStackSetup(t, nil, 1)
	st.giveVennAGoal(t)
	ctx := context.Background()

	// The party is confidently wrong about the Duke's travels, and suspects
	// the charter fact.
	if _, err := st.ks.SetAwareness(ctx, st.fx.Campaign.ID, campaign.PartyKnower,
		st.fx.FactDukeVisited, knowledge.StanceBelievesFalse, 1, "", ""); err != nil {
		t.Fatalf("set believes_false: %v", err)
	}
	mustDiscovery(t, st.ks, st.fx.Campaign.ID, st.fx.FactMinesOwned, campaign.PartyKnower, "suspects")
	// A prior ruling whose words overlap the push_goal direction's scenes.
	if _, err := st.sessions.AddEvent(ctx, "sess-1", gamesession.EventRuling,
		"Ruling: the miners were marched off by the cult, not bandits — cultist captives talk.",
		"Standing ruling from the caravan session.", nil); err != nil {
		t.Fatalf("seed ruling: %v", err)
	}

	dirs, err := st.s.Directions(ctx, DirectionsInput{CampaignID: st.fx.Campaign.ID})
	if err != nil {
		t.Fatalf("directions: %v", err)
	}
	vd := directionByKind(dirs.Directions, DirectionPushGoal)
	res, err := st.s.BuildPrep(ctx, BuildInput{
		CampaignID: st.fx.Campaign.ID, DirectionID: vd.ID, CreatedBy: "keeper",
	})
	if err != nil {
		t.Fatalf("BuildPrep: %v", err)
	}

	// Questions: the believes_false stance leads, the suspected one follows.
	if len(res.Package.Questions) < 2 {
		t.Fatalf("questions = %v", res.Package.Questions)
	}
	if res.Package.Questions[0].Kind != "wrong" ||
		!strings.Contains(res.Package.Questions[0].Statement, "Duke personally visited") {
		t.Errorf("first question = %+v, want the believes_false gap", res.Package.Questions[0])
	}
	sawSuspected := false
	for _, q := range res.Package.Questions {
		if q.Kind == "suspected" && strings.Contains(q.Statement, "Eastern Mines") {
			sawSuspected = true
		}
	}
	if !sawSuspected {
		t.Errorf("no question for the suspected fact: %v", res.Package.Questions)
	}

	// Contingencies: the offline scaffold's last scene has no outcome —
	// that is exactly the branch nothing is planned for.
	if len(res.Package.Contingencies) == 0 {
		t.Fatal("the unplanned final scene surfaced no contingency")
	}
	sawBare := false
	for _, c := range res.Package.Contingencies {
		if c.Label == "" && strings.Contains(c.Note, "no branches") {
			sawBare = true
		}
	}
	if !sawBare {
		t.Errorf("contingencies = %+v", res.Package.Contingencies)
	}

	// Prior rulings: the FTS matcher, no model — the ruling's words overlap
	// the direction's scenes ("marched the miners off" is Venn's goal).
	if len(res.Package.PriorRulings) == 0 {
		t.Fatal("no prior ruling surfaced for the scenes")
	}
	found := false
	for _, r := range res.Package.PriorRulings {
		if strings.Contains(r.Summary, "cult, not bandits") {
			found = true
		}
	}
	if !found {
		t.Errorf("prior rulings = %+v", res.Package.PriorRulings)
	}
}

/* ---------- the Markdown export path ---------- */

func TestBuildPrep_MarkdownRidesTheSessionExport(t *testing.T) {
	st := prepStackSetup(t, nil, 1)
	ctx := context.Background()

	dirs, err := st.s.Directions(ctx, DirectionsInput{CampaignID: st.fx.Campaign.ID})
	if err != nil {
		t.Fatalf("directions: %v", err)
	}
	qd := directionByKind(dirs.Directions, DirectionAdvanceQuest)
	build := func() *BuildResult {
		res, err := st.s.BuildPrep(ctx, BuildInput{
			CampaignID: st.fx.Campaign.ID, DirectionID: qd.ID, CreatedBy: "keeper",
		})
		if err != nil {
			t.Fatalf("BuildPrep: %v", err)
		}
		return res
	}
	first := build()

	// The package is a dm_notes source the existing exporter renders.
	src, err := st.sessions.GetSource(ctx, first.MarkdownSourceID)
	if err != nil {
		t.Fatalf("prep source: %v", err)
	}
	if src.Kind != gamesession.SourceDMNotes || src.Title != "Session prep package" {
		t.Errorf("source = %s/%s", src.Kind, src.Title)
	}
	for _, heading := range []string{
		"# Session prep —", "## The plan", "## NPC quick reference",
		"## Likely player questions", "## Contingencies", "## Prior rulings likely to come up",
	} {
		if !strings.Contains(src.Content, heading) {
			t.Errorf("markdown missing %q", heading)
		}
	}
	exported, err := st.sessions.ExportMarkdown(ctx, first.SessionID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(exported, "Session prep package") {
		t.Error("the session export omits the prep package")
	}

	// A regenerate on the same session replaces instead of stacking: the
	// package is an artifact, not a record.
	if _, err := st.s.putPrepSource(ctx, st.sessions, first.SessionID, "keeper",
		"# Session prep — regenerated\n\ntoday the mines.\n"); err != nil {
		t.Fatalf("regenerate prep source: %v", err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM session_sources
		WHERE session_id = ? AND kind = 'dm_notes' AND title = 'Session prep package'`,
		first.SessionID).Scan(&n); err != nil {
		t.Fatalf("count prep sources: %v", err)
	}
	if n != 1 {
		t.Errorf("prep sources after regeneration = %d, want the replaced 1", n)
	}
}

/* ---------- the encounter arithmetic ---------- */

// seedBestiary fills the local mirror directly — the canon tests never touch
// the network — and returns a catalog loaded from it.
func seedBestiary(t *testing.T, db *sql.DB) *encounter.Catalog {
	t.Helper()
	cat, err := encounter.NewCatalog(db, "http://unused.local")
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}
	creatures := []encounter.Creature{
		{Slug: "goblin", Name: "Goblin", Doc: "srd-2024", CR: "1/4", CRNum: 0.25, XP: 50, Type: "Humanoid"},
		{Slug: "cultist", Name: "Cultist", Doc: "srd-2024", CR: "1/8", CRNum: 0.125, XP: 25, Type: "Humanoid"},
		{Slug: "cult-fanatic", Name: "Cult Fanatic", Doc: "srd-2024", CR: "2", CRNum: 2, XP: 450, Type: "Humanoid"},
	}
	for _, c := range creatures {
		blob, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal creature: %v", err)
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO bestiary
			(key, name, cr, cr_num, xp, type, data, synced_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			c.Slug, c.Name, c.CR, c.CRNum, c.XP, c.Type, string(blob), 1); err != nil {
			t.Fatalf("seed bestiary row: %v", err)
		}
	}
	if err := cat.Load(); err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return cat
}

func TestBuildPrep_EncountersThroughTheEncounterArithmetic(t *testing.T) {
	st := prepStackSetup(t, nil, 1) // one act: the default mix carries two combats
	ctx := context.Background()
	cat := seedBestiary(t, st.db)

	dirs, err := st.s.Directions(ctx, DirectionsInput{CampaignID: st.fx.Campaign.ID})
	if err != nil {
		t.Fatalf("directions: %v", err)
	}
	qd := directionByKind(dirs.Directions, DirectionAdvanceQuest)
	res, err := st.s.BuildPrep(ctx, BuildInput{
		CampaignID: st.fx.Campaign.ID, DirectionID: qd.ID,
		Band: "medium", CreatedBy: "keeper", Catalog: cat,
	})
	if err != nil {
		t.Fatalf("BuildPrep: %v", err)
	}
	// One act with no named shape: the default template, one combat scene.
	if len(res.Encounters) != 1 {
		t.Fatalf("encounters = %d, want 1 for the default mix", len(res.Encounters))
	}
	party := []int{5, 5, 5, 5} // the fixture's pcs
	for _, e := range res.Encounters {
		if e.HandBuilt || len(e.Monsters) == 0 {
			t.Fatalf("encounter %+v has no roster", e)
		}
		m := e.Monsters[0]
		if m.Count <= 0 || m.XP <= 0 || m.Name == "" {
			t.Errorf("monster = %+v", m)
		}
		if e.Verdict.Difficulty == "" {
			t.Errorf("no verdict for %+v", e)
		}
		// The budget was computed against the party's current levels — the
		// party_level_drift correction, by construction.
		for i, lvl := range e.Party {
			if lvl != party[i] {
				t.Errorf("party[%d] = %d, want %d", i, lvl, party[i])
			}
		}
	}

	// The events landed in the payload contract the canon engine reads:
	// this snapshot's planned encounters now include tonight's.
	snap := loadSnap(t, st.db, st.fx.Campaign.ID)
	found := 0
	for _, ref := range snap.Encounters {
		if ref.SessionID != res.SessionID {
			continue
		}
		found++
		if len(ref.Monsters) == 0 || len(ref.Party) != 4 {
			t.Errorf("planned encounter ref = %+v", ref)
		}
	}
	if found != 1 {
		t.Errorf("planned encounters on the session = %d, want 1", found)
	}
}

func TestBuildPrep_NoBestiaryLeavesTheRosterToTheDM(t *testing.T) {
	st := prepStackSetup(t, nil, 1)
	ctx := context.Background()

	dirs, err := st.s.Directions(ctx, DirectionsInput{CampaignID: st.fx.Campaign.ID})
	if err != nil {
		t.Fatalf("directions: %v", err)
	}
	qd := directionByKind(dirs.Directions, DirectionAdvanceQuest)
	res, err := st.s.BuildPrep(ctx, BuildInput{
		CampaignID: st.fx.Campaign.ID, DirectionID: qd.ID, CreatedBy: "keeper", // no catalog
	})
	if err != nil {
		t.Fatalf("BuildPrep: %v", err)
	}
	if len(res.Encounters) != 1 {
		t.Fatalf("encounters = %d", len(res.Encounters))
	}
	for _, e := range res.Encounters {
		if !e.HandBuilt || len(e.Monsters) != 0 {
			t.Errorf("encounter %+v should be hand-built", e)
		}
	}
}

/* ---------- the model build: through the scene designer ---------- */

func TestBuildPrep_ModelPathRunsTheSceneDesigner(t *testing.T) {
	st := prepStackSetup(t, &fakeModel{}, 4)
	ctx := context.Background()

	// The ranking is deterministic either way: compute it offline, then arm
	// the model for the build's single designer exchange.
	st.s.model = nil
	dirs, err := st.s.Directions(ctx, DirectionsInput{CampaignID: st.fx.Campaign.ID})
	if err != nil {
		t.Fatalf("directions: %v", err)
	}
	qd := directionByKind(dirs.Directions, DirectionAdvanceQuest)

	model := &fakeModel{responses: []string{planScriptFor(st, dirs.Budget)}}
	st.s.model = model
	res, err := st.s.BuildPrep(ctx, BuildInput{
		CampaignID: st.fx.Campaign.ID, DirectionID: qd.ID,
		Notes: "lean into the mines", CreatedBy: "keeper",
	})
	if err != nil {
		t.Fatalf("BuildPrep model path: %v", err)
	}
	if res.Offline || len(model.calls) != 1 {
		t.Fatalf("model calls = %d (offline=%v), want the one designer exchange", len(model.calls), res.Offline)
	}
	// The direction steered the designer: the prompt carries it.
	if !strings.Contains(model.calls[0], "Tonight's direction: Quest") {
		t.Errorf("designer prompt not steered by the direction: %q", model.calls[0][:min(200, len(model.calls[0]))])
	}
	if len(res.Scenes) != dirs.Budget {
		t.Fatalf("scenes = %d, want %d", len(res.Scenes), dirs.Budget)
	}
	// The script's awareness promise staged behind the review gate as a
	// session_prep batch.
	if res.Batch == nil || res.Batch.Source != BatchSourceSessionPrep {
		t.Fatalf("batch = %+v, want a session_prep batch", res.Batch)
	}
	if len(res.Batch.Items) != 1 || res.Batch.Items[0].Kind != ReviewProposedDiscovery {
		t.Errorf("batch items = %+v", res.Batch.Items)
	}
	// Nothing entered awareness before a decision (ADR 3).
	if rows := awarenessRows(t, st.db, st.fx.Campaign.ID, st.fx.FactDukeVisited); rows != 0 {
		t.Errorf("awareness rows before any decision = %d", rows)
	}
}

// planScriptFor scripts the scene designer's fill for a prep build: budget
// scenes chained by "next", the fixture's people on stage.
func planScriptFor(st *prepStack, budget int) string {
	m := map[string]any{
		"session_goal": "The party learns who marched the miners off, and the ledger names the wrong man.",
	}
	for i := 1; i <= budget; i++ {
		m[fmt.Sprintf("scene_%d_name", i)] = fmt.Sprintf("Scene %d", i)
		m[fmt.Sprintf("scene_%d_purpose", i)] = "Pressure lands on someone who lies about it."
		m[fmt.Sprintf("scene_%d_setting", i)] = "Blackwater"
		m[fmt.Sprintf("scene_%d_cast", i)] = "Duke Aldric Vane:focus, Tom the Innkeeper"
		m[fmt.Sprintf("scene_%d_secrets", i)] = ""
		for _, label := range []string{"A", "B", "C", "D"} {
			m[fmt.Sprintf("scene_%d_outcome_%s_summary", i, label)] = ""
			m[fmt.Sprintf("scene_%d_outcome_%s_result", i, label)] = ""
		}
		if i < budget {
			m[fmt.Sprintf("scene_%d_outcome_A_summary", i)] = "The party presses the lead."
			m[fmt.Sprintf("scene_%d_outcome_A_result", i)] = "next"
		} else {
			m[fmt.Sprintf("scene_%d_outcome_A_summary", i)] = "The Duke answers for the caravan."
			m[fmt.Sprintf("scene_%d_outcome_A_result", i)] = "quest:" + st.fx.QuestID + ":accused_survivor"
		}
	}
	// One awareness promise, so the batch has something to carry.
	m["scene_1_outcome_B_summary"] = "The steward's ledger surfaces."
	m["scene_1_outcome_B_result"] = "aware:" + st.fx.FactDukeVisited + ":knows"
	b, _ := json.Marshal(m)
	return string(b)
}
