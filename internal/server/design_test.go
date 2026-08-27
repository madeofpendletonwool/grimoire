package server

// The story planner and scene designer's handler tests (MAD-362): scope
// enforcement end to end — the planned scene must be invisible at party and
// character:<id> scope, on every surface that could render it — plus the
// offline gate and the assembled prompt (ADR 8).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

// newDesignServer boots a gated server whose canon engine carries a fake
// model and whose spine is wired, so both design endpoints and every story
// read are live end to end.
func newPlannerServer(t *testing.T) (*Server, fixture, *fakeCanonModel, *index.Store) {
	t.Helper()
	store, err := index.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(store.DB()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	campaigns, err := campaign.New(store.DB())
	if err != nil {
		t.Fatalf("open campaign store: %v", err)
	}
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatalf("open knowledge store: %v", err)
	}
	stories, err := story.New(store.DB())
	if err != nil {
		t.Fatalf("open story store: %v", err)
	}
	sessions, err := gamesession.New(store.DB())
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	model := &fakeCanonModel{}
	engine, err := canon.New(store.DB(), model, canon.Config{MaxCandidates: 500, BatchSize: 8})
	if err != nil {
		t.Fatalf("open canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)
	s, err := New(store, llm.New(llm.Config{APIKey: "test"}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).
		WithCampaign(campaigns, sessions).
		WithCanon(engine).
		WithStory(stories)
	f := buildFixture(t, s)
	return s, f, model, store
}

// seedSpine gives the fixture campaign one act and one planned session
// seated on it — the minimum the planner plans into. It returns the scene
// budget the pacing arithmetic will hold the run to.
func seedSpine(t *testing.T, s *Server, f fixture, dm *http.Cookie) int {
	t.Helper()
	act := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/acts",
		`{"name":"The Gathering Dark","premise":"The marches lean toward the wood.","level_start":1,"level_end":3}`, dm)
	if act.Code != http.StatusCreated {
		t.Fatalf("create act: status %d, body %s", act.Code, act.Body)
	}
	ses := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/sessions", `{}`, dm)
	if ses.Code != http.StatusCreated {
		t.Fatalf("create session: status %d, body %s", ses.Code, ses.Body)
	}
	sid := idFrom(t, ses, "session")
	body := `{"act_id":` + quote(idFrom(t, act, "act")) + `}`
	if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/sessions/"+sid+"/plan", body, dm); r.Code != http.StatusOK {
		t.Fatalf("put plan: status %d, body %s", r.Code, r.Body)
	}
	// One session across levels 1-3: the arithmetic's ceiling, six scenes.
	return 6
}

// planModelJSON scripts one session's fill for the handler fixture's world.
func planModelJSON(f fixture, budget int) string {
	m := map[string]any{
		"session_goal": "The party learns what the Duke's charter really buys.",
	}
	for i := 1; i <= budget; i++ {
		m[fmt.Sprintf("scene_%d_name", i)] = fmt.Sprintf("Scene %d", i)
		m[fmt.Sprintf("scene_%d_purpose", i)] = "The Duke's people lie about the charter."
		m[fmt.Sprintf("scene_%d_setting", i)] = ""
		m[fmt.Sprintf("scene_%d_cast", i)] = "Duke Aldric Vane:focus"
		m[fmt.Sprintf("scene_%d_secrets", i)] = ""
		m[fmt.Sprintf("scene_%d_outcome_A_summary", i)] = "The party presses the lead."
		m[fmt.Sprintf("scene_%d_outcome_A_result", i)] = ""
		if i < budget {
			m[fmt.Sprintf("scene_%d_outcome_A_result", i)] = "next"
		}
	}
	// The last scene has no later scene, no quest and no ungranted fact in
	// this fixture: its dead branch is dropped, which the test asserts.
	b, _ := json.Marshal(m)
	return string(b)
}

func TestDesignPlan_PlansTheNextSession(t *testing.T) {
	s, f, model, _ := newPlannerServer(t)
	dm := dmSession(t, s)
	budget := seedSpine(t, s, f, dm)
	model.response = planModelJSON(f, budget)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/plan",
		`{"notes":"lean on the charter"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plan: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Budget   int      `json:"budget"`
		Mix      []string `json:"mix"`
		Sessions []struct {
			SessionID string `json:"session_id"`
			Goal      string `json:"goal"`
			NewPlan   bool   `json:"new_plan"`
			Scenes    []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
				Cast []struct {
					EntityID string `json:"entity_id"`
					Role     string `json:"role"`
				} `json:"cast"`
			} `json:"scenes"`
			Dropped []string `json:"dropped"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if len(body.Sessions) != 1 || len(body.Sessions[0].Scenes) != budget {
		t.Fatalf("sessions = %d, scenes = %d, want 1 and %d", len(body.Sessions), len(body.Sessions[0].Scenes), budget)
	}
	if body.Budget != budget {
		t.Errorf("budget = %d, want %d", body.Budget, budget)
	}
	if !body.Sessions[0].NewPlan || body.Sessions[0].Goal == "" {
		t.Errorf("plan row not written: %+v", body.Sessions[0])
	}
	sc := body.Sessions[0].Scenes[0]
	if len(sc.Cast) != 1 || sc.Cast[0].EntityID != f.dukeID || sc.Cast[0].Role != "focus" {
		t.Errorf("scene 1 cast = %+v", sc.Cast)
	}
	// The dead branch on the last scene is dropped and reported.
	if dropped := body.Sessions[0].Dropped; len(dropped) != 1 || !strings.Contains(dropped[0], "names no consequence") {
		t.Errorf("dropped = %v", dropped)
	}

	// The prompt carried the computed structure: the budget, the mix and
	// the candidate cast the model chose among.
	if len(model.prompts) != 1 {
		t.Fatalf("model calls = %d, want 1", len(model.prompts))
	}
	prompt := model.prompts[0]
	for _, marker := range []string{fmt.Sprintf(`"scene_budget":%d`, budget), "Duke Aldric Vane", `"candidate_cast"`, "The Gathering Dark"} {
		if !strings.Contains(prompt, marker) {
			t.Errorf("prompt missing %q", marker)
		}
	}

	// The DM reads what the planner wrote.
	if r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/scenes/"+sc.ID, "", dm); r.Code != http.StatusOK {
		t.Errorf("DM scene read: status %d, body %s", r.Code, r.Body)
	}
}

// TestDesignPlan_PlannedScenesInvisibleToPlayers is the issue's own bar: a
// planned scene renders on no player surface, at party scope or at
// character:<id> scope — so spoiler_leak can never fire because a planned
// scene leaked (ADR 2).
func TestDesignPlan_PlannedScenesInvisibleToPlayers(t *testing.T) {
	s, f, model, _ := newPlannerServer(t)
	dm := dmSession(t, s)
	budget := seedSpine(t, s, f, dm)
	model.response = planModelJSON(f, budget)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/plan", `{}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plan: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			Scenes    []struct {
				ID string `json:"id"`
			} `json:"scenes"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Sessions) != 1 || len(body.Sessions[0].Scenes) == 0 {
		t.Fatalf("plan body: %v (%s)", err, rec.Body)
	}
	sceneID := body.Sessions[0].Scenes[0].ID
	sessionID := body.Sessions[0].SessionID

	// A player bound to their pc — the character:<id> scope.
	character := addPlayerMember(t, s, f, "mira", true)
	// A player with no character — the party scope.
	party := addPlayerMember(t, s, f, "ronan", false)

	reads := []string{
		"/api/campaigns/" + f.campaignID + "/scenes/" + sceneID,
		"/api/campaigns/" + f.campaignID + "/scenes",
		"/api/campaigns/" + f.campaignID + "/acts",
		"/api/campaigns/" + f.campaignID + "/story",
		"/api/campaigns/" + f.campaignID + "/sessions/" + sessionID + "/plan",
	}
	for _, who := range []struct {
		name   string
		cookie *http.Cookie
	}{{"character scope", character}, {"party scope", party}} {
		for _, target := range reads {
			if r := hit(t, s, http.MethodGet, target, "", who.cookie); r.Code != http.StatusForbidden {
				t.Errorf("%s GET %s: status %d, want 403", who.name, target, r.Code)
			}
		}
		for _, target := range []string{"/design/plan", "/design/scene"} {
			if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+target, `{}`, who.cookie); r.Code != http.StatusForbidden {
				t.Errorf("%s POST %s: status %d, want 403", who.name, target, r.Code)
			}
		}
	}
}

func TestDesignScene_DesignsOneScene(t *testing.T) {
	s, f, model, _ := newPlannerServer(t)
	dm := dmSession(t, s)
	seedSpine(t, s, f, dm)
	model.response = func() string {
		m := map[string]any{
			"scene_name":        "The midnight audit",
			"scene_purpose":     "The Duke's steward counts coin that is not his.",
			"scene_cast":        "Duke Aldric Vane:focus, Mira Thorn:present",
			"scene_secrets":     "",
			"outcome_A_summary": "The party copies the ledger.",
			"outcome_A_result":  "",
		}
		b, _ := json.Marshal(m)
		return string(b)
	}()
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/scene",
		`{"notes":"quiet, before the mines"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("scene: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Scene struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
			Cast []struct {
				EntityID string `json:"entity_id"`
				Role     string `json:"role"`
			} `json:"cast"`
		} `json:"scene"`
		Dropped []string `json:"dropped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if body.Scene.ID == "" || len(body.Scene.Cast) != 1 || body.Scene.Cast[0].EntityID != f.pcID {
		t.Fatalf("scene = %+v (the party is the only plausible cast in this fixture)", body.Scene)
	}
	// The Duke has no signal tying him to anywhere in this fixture — no
	// setting, no timeline, no goal — so the pool refuses him.
	if dropped := strings.Join(body.Dropped, "\n"); !strings.Contains(dropped, "Duke Aldric Vane") {
		t.Errorf("the unmoored Duke should be dropped and reported: %v", body.Dropped)
	}
	// A player cannot even ask for it.
	player := addPlayerMember(t, s, f, "mira", true)
	if r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/scenes/"+body.Scene.ID, "", player); r.Code != http.StatusForbidden {
		t.Errorf("player scene read: status %d, want 403", r.Code)
	}
}

func TestDesign_OfflineAndBadInput(t *testing.T) {
	s, f, model, store := newPlannerServer(t)
	dm := dmSession(t, s)
	seedSpine(t, s, f, dm)
	model.response = planModelJSON(f, 6)

	// Bad input names its problem.
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/plan", `{"mode":"week"}`, dm); r.Code != http.StatusBadRequest {
		t.Fatalf("bad mode status = %d, want 400 (body %s)", r.Code, r.Body)
	}
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/scene", `{"kind":"musical"}`, dm); r.Code != http.StatusBadRequest {
		t.Fatalf("bad kind status = %d, want 400 (body %s)", r.Code, r.Body)
	}

	// Swap in an offline canon store: no model, no planning.
	offline, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("offline engine: %v", err)
	}
	campaigns, err := campaign.New(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	s.canon = offline.WithGraphStores(campaigns, knowledgeStore)
	for _, target := range []string{"/design/plan", "/design/scene"} {
		if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+target, `{}`, dm); r.Code != http.StatusServiceUnavailable {
			t.Errorf("offline POST %s: status %d, want 503 (body %s)", target, r.Code, r.Body)
		}
	}
}
