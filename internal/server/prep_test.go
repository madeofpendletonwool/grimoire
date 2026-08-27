package server

// One-click session prep's handler tests (MAD-364): scope enforcement, the
// no-model-key guarantee (the ranked list still comes back — only the prose
// pitches are missing), and the offline build end to end over HTTP.

import (
	"encoding/json"
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

// newPrepServer boots a server with no model key at all — the canon engine
// is the offline store, the exact configuration a self-hosted box with
// nothing configured runs. Prep must still work there; the /design/* gates
// 503 in this same configuration, which the test asserts for contrast.
func newPrepServer(t *testing.T) (*Server, fixture, *index.Store) {
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
	engine, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("offline canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithCampaign(campaigns, sessions).
		WithStory(stories).WithCanon(engine)
	f := buildFixture(t, s)
	return s, f, store
}

// seedPrepMaterial gives the fixture campaign a spine and a quest, so the
// direction engine has something to score.
func seedPrepMaterial(t *testing.T, s *Server, f fixture, dm *http.Cookie) (actID, questID string) {
	t.Helper()
	act := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/acts",
		`{"name":"The Gathering Dark","premise":"The marches lean toward the wood.","level_start":1,"level_end":3}`, dm)
	if act.Code != http.StatusCreated {
		t.Fatalf("create act: status %d, body %s", act.Code, act.Body)
	}
	actID = idFrom(t, act, "act")
	machine := map[string]any{
		"initial": "rumour",
		"states":  []string{"rumour", "hunted", "ended"},
		"edges":   []map[string]string{{"from": "rumour", "to": "hunted"}, {"from": "hunted", "to": "ended"}},
	}
	mb, _ := json.Marshal(machine)
	q := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/quests",
		`{"name":"The Ashen Debt","state_machine":`+string(mb)+`}`, dm)
	if q.Code != http.StatusCreated {
		t.Fatalf("create quest: status %d, body %s", q.Code, q.Body)
	}
	return actID, idFrom(t, q, "quest")
}

func TestPrepDirections_WorksWithoutAModelKey(t *testing.T) {
	s, f, _ := newPrepServer(t)
	dm := dmSession(t, s)
	seedPrepMaterial(t, s, f, dm)

	// The contrast: the model-gated design surface refuses in this
	// configuration; prep does not.
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/plan", `{}`, dm); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("design/plan status = %d, want 503 without a key", r.Code)
	}

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/prep/directions", `{}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("prep/directions status = %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Offline    bool              `json:"offline"`
		Directions []json.RawMessage `json:"directions"`
		Budget     int               `json:"budget"`
		Mix        []string          `json:"mix"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Offline {
		t.Error("offline flag missing")
	}
	if len(body.Directions) == 0 {
		t.Fatal("no directions without a model key — the scoring must not depend on a model")
	}
	if body.Budget == 0 || len(body.Mix) != body.Budget {
		t.Errorf("budget/mix = %d/%v", body.Budget, body.Mix)
	}
	// Every direction carries the evidence and the estimate.
	for _, raw := range body.Directions {
		var d struct {
			ID          string `json:"id"`
			Score       int    `json:"score"`
			PrepMinutes int    `json:"prep_minutes"`
			Evidence    []struct {
				Kind string `json:"kind"`
			} `json:"evidence"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("decode direction: %v", err)
		}
		if d.ID == "" || d.Score <= 0 || d.PrepMinutes <= 0 || len(d.Evidence) == 0 {
			t.Errorf("direction = %+v", d)
		}
	}
}

func TestPrepDirections_ScopeEnforcement(t *testing.T) {
	s, f, _ := newPrepServer(t)
	dm := dmSession(t, s)
	seedPrepMaterial(t, s, f, dm)

	player := addPlayerMember(t, s, f, "prep-player", true)
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/prep/directions", `{}`, player); r.Code != http.StatusForbidden {
		t.Fatalf("player status = %d, want 403", r.Code)
	}
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/prep/build", `{"direction_id":"x"}`, player); r.Code != http.StatusForbidden {
		t.Fatalf("player build status = %d, want 403", r.Code)
	}
	if r := hit(t, s, http.MethodPost, "/api/campaigns/no-such-campaign/prep/directions", `{}`, dm); r.Code != http.StatusNotFound {
		t.Fatalf("foreign campaign status = %d, want 404", r.Code)
	}
}

func TestPrepBuild_OfflineBuildEndToEnd(t *testing.T) {
	s, f, _ := newPrepServer(t)
	dm := dmSession(t, s)
	actID, questID := seedPrepMaterial(t, s, f, dm)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/prep/directions", `{}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("directions: status %d, body %s", rec.Code, rec.Body)
	}
	var dirs struct {
		Directions []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"directions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dirs); err != nil {
		t.Fatalf("decode directions: %v", err)
	}
	var directionID string
	for _, d := range dirs.Directions {
		if d.Kind == "advance_quest" {
			directionID = d.ID
		}
	}
	if directionID == "" {
		t.Fatalf("no advance_quest direction: %+v", dirs.Directions)
	}
	if !strings.Contains(directionID, questID) {
		t.Errorf("direction id %q does not name quest %s", directionID, questID)
	}

	build := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/prep/build",
		`{"direction_id":`+quote(directionID)+`}`, dm)
	if build.Code != http.StatusCreated {
		t.Fatalf("prep/build status = %d, body %s", build.Code, build.Body)
	}
	var res struct {
		Offline   bool             `json:"offline"`
		SessionID string           `json:"session_id"`
		Goal      string           `json:"goal"`
		Scenes    []map[string]any `json:"scenes"`
		Package   struct {
			NPCSheets     []map[string]any `json:"npc_sheets"`
			Contingencies []map[string]any `json:"contingencies"`
		} `json:"package"`
		MarkdownSourceID string `json:"markdown_source_id"`
	}
	if err := json.Unmarshal(build.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode build: %v", err)
	}
	if !res.Offline || res.Goal == "" || len(res.Scenes) == 0 || res.MarkdownSourceID == "" {
		t.Fatalf("build = offline:%v goal:%q scenes:%d src:%q", res.Offline, res.Goal, len(res.Scenes), res.MarkdownSourceID)
	}
	if got, _ := res.Scenes[0]["act_id"].(string); got != actID {
		t.Errorf("scene act = %v, want %s", res.Scenes[0]["act_id"], actID)
	}

	// The quest's payoff landed on the spine and the export carries the
	// package: the whole loop closes through the existing surfaces.
	scenes := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/scenes", "", dm)
	if scenes.Code != http.StatusOK || !strings.Contains(scenes.Body.String(), "The Ashen Debt") {
		t.Errorf("scenes read-back: %d %s", scenes.Code, scenes.Body.String())
	}
	export := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/sessions/"+res.SessionID+"/export", "", dm)
	if export.Code != http.StatusOK || !strings.Contains(export.Body.String(), "Session prep package") {
		t.Errorf("export: %d %s", export.Code, export.Body.String()[:min(300, export.Body.Len())])
	}

	// A stale direction id is a clean 400, not a 500.
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/prep/build",
		`{"direction_id":"advance_quest:gone"}`, dm); r.Code != http.StatusBadRequest {
		t.Fatalf("stale direction status = %d, want 400", r.Code)
	}
}
