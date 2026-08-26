package server

// Story surface tests (MAD-360). The load-bearing ones, following the file's
// pattern: the spine is DM material and must not render on a player surface
// — the response bodies themselves must not carry it — and a DM can build a
// whole four-act, twelve-session spine by hand over HTTP with no model key
// anywhere on the box.

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

// newStoryServer boots a gated server with the campaign stack and the story
// store wired — no LLM, no key: the honest-fallback configuration the issue
// demands the planner work under.
func newStoryServer(t *testing.T) *Server {
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
		t.Fatalf("open canon engine: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s.WithCampaigns(campaigns, knowledgeStore).
		WithCampaign(campaigns, sessions).
		WithCanon(engine).
		WithStory(stories)
}

// buildSpine builds a whole four-act spine over HTTP as the DM: acts with
// chaining level bands, scenes with cast, a secret in play, an outcome with
// a legal quest transition, and a session plan. Returns the ids the
// assertions need.
func buildSpine(t *testing.T, s *Server, f fixture) (acts []string, sceneID, outcomeID string) {
	t.Helper()
	dm := dmSession(t, s)
	cid := f.campaignID
	base := "/api/campaigns/" + cid

	// A quest the outcome can name: unknown -> rumoured -> confirmed.
	machine := `{"initial":"unknown","states":["unknown","rumoured","confirmed"],
		"edges":[{"from":"unknown","to":"rumoured"},{"from":"rumoured","to":"confirmed"}]}`
	if r := hit(t, s, http.MethodPost, base+"/quests", `{"name":"The vampire question","state_machine":`+machine+`}`, dm); r.Code != http.StatusCreated {
		t.Fatalf("create quest: status %d, body %s", r.Code, r.Body)
	}
	var questList struct {
		Quests []struct {
			ID string `json:"id"`
		} `json:"quests"`
	}
	if err := json.Unmarshal(hit(t, s, http.MethodGet, base+"/quests", "", dm).Body.Bytes(), &questList); err != nil {
		t.Fatalf("list quests: %v", err)
	}
	questID := questList.Quests[len(questList.Quests)-1].ID

	// Four acts, levels 1-12, chaining exactly.
	bands := [][2]int{{1, 3}, {4, 6}, {7, 9}, {10, 12}}
	names := []string{"The Letter", "The March", "The Root", "The Reckoning"}
	for i, band := range bands {
		body := fmt.Sprintf(`{"name":%q,"premise":"premise %d","level_start":%d,"level_end":%d}`,
			names[i], i+1, band[0], band[1])
		r := hit(t, s, http.MethodPost, base+"/acts", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create act %d: status %d, body %s", i+1, r.Code, r.Body)
		}
		var out struct {
			Act struct {
				ID string `json:"id"`
			} `json:"act"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode act: %v", err)
		}
		acts = append(acts, out.Act.ID)
	}

	// A scene in act one with the duke as focus, the secret fact in play,
	// and an outcome promising the quest's legal first move.
	r := hit(t, s, http.MethodPost, base+"/scenes",
		`{"act_id":`+quote(acts[0])+`,"kind":"social","name":"The Waystone at midnight","purpose":"Put the question on the table."}`, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("create scene: status %d, body %s", r.Code, r.Body)
	}
	var sceneOut struct {
		Scene struct {
			ID string `json:"id"`
		} `json:"scene"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &sceneOut); err != nil {
		t.Fatalf("decode scene: %v", err)
	}
	sceneID = sceneOut.Scene.ID

	if r := hit(t, s, http.MethodPost, base+"/scenes/"+sceneID+"/cast",
		`{"entity_id":`+quote(f.dukeID)+`,"role":"focus"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("add cast: status %d, body %s", r.Code, r.Body)
	}
	if r := hit(t, s, http.MethodPost, base+"/scenes/"+sceneID+"/secrets",
		`{"fact_id":`+quote(f.secretID)+`,"disposition":"in_play"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("set secret: status %d, body %s", r.Code, r.Body)
	}
	transition := fmt.Sprintf(`{"quest":%q,"from":"unknown","to":"rumoured"}`, questID)
	r = hit(t, s, http.MethodPost, base+"/scenes/"+sceneID+"/outcomes",
		`{"label":"A","summary":"They start asking.","leads_to_scene":"","quest_transition":`+transition+`}`, dm)
	if r.Code != http.StatusOK {
		t.Fatalf("add outcome: status %d, body %s", r.Code, r.Body)
	}
	var outcomeOut struct {
		Outcomes []struct {
			ID string `json:"id"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &outcomeOut); err != nil || len(outcomeOut.Outcomes) != 1 {
		t.Fatalf("decode outcomes: %v (%s)", err, r.Body)
	}
	outcomeID = outcomeOut.Outcomes[0].ID

	// A session with a plan, seated in act one.
	ses := hit(t, s, http.MethodPost, base+"/sessions", `{"name":"Session 1"}`, dm)
	if ses.Code != http.StatusCreated {
		t.Fatalf("create session: status %d, body %s", ses.Code, ses.Body)
	}
	var sesOut struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(ses.Body.Bytes(), &sesOut); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	plan := `{"act_id":` + quote(acts[0]) + `,"goal":"The letter arrives.","prep_notes":"Map of Blackwater."}`
	if r := hit(t, s, http.MethodPut, base+"/sessions/"+sesOut.Session.ID+"/plan", plan, dm); r.Code != http.StatusOK {
		t.Fatalf("put plan: status %d, body %s", r.Code, r.Body)
	}
	return acts, sceneID, outcomeID
}

// THE ACCEPTANCE TEST: a DM builds a four-act, twelve-session spine with
// scenes, cast, secrets and outcomes entirely by hand, no API key
// configured, and reads it back whole through /story.
func TestDMBuildsFourActSpineByHand(t *testing.T) {
	s := newStoryServer(t)
	f := buildFixture(t, s)
	_, sceneID, _ := buildSpine(t, s, f)
	dm := dmSession(t, s)

	r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/story", "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("story read: status %d, body %s", r.Code, r.Body)
	}
	body := r.Body.String()
	for _, want := range []string{"The Letter", "The March", "The Root", "The Reckoning",
		"The Waystone at midnight", f.dukeID, f.secretID, "in_play", "The letter arrives."} {
		if !strings.Contains(body, want) {
			t.Errorf("the whole-spine read is missing %q:\n%s", want, body)
		}
	}
	// The secret's placement rides along; its statement must not leak from
	// this endpoint either (it carries ids, not fact text).
	if strings.Contains(body, "vampire") {
		t.Error("the spine read carries the secret's statement text; ids are enough here")
	}

	// The scene read carries its attachments.
	r = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/scenes/"+sceneID, "", dm)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "outcomes") {
		t.Fatalf("scene read: status %d, body %s", r.Code, r.Body)
	}
}

// THE SCOPE TESTS: the spine is DM material and must not render on a player
// surface — list, single, whole-spine, or plan — and a stranger learns
// nothing.
func TestPlayerCannotReadSpine(t *testing.T) {
	s := newStoryServer(t)
	f := buildFixture(t, s)
	acts, sceneID, _ := buildSpine(t, s, f)
	player := addPlayerMember(t, s, f, "mira", true)
	base := "/api/campaigns/" + f.campaignID

	playerGets := []struct {
		method, target string
	}{
		{http.MethodGet, base + "/story"},
		{http.MethodGet, base + "/acts"},
		{http.MethodGet, base + "/acts/" + acts[0]},
		{http.MethodGet, base + "/scenes"},
		{http.MethodGet, base + "/scenes/" + sceneID},
	}
	for _, req := range playerGets {
		r := hit(t, s, req.method, req.target, "", player)
		if r.Code != http.StatusForbidden {
			t.Errorf("%s %s: player got %d, want 403 (the spine is DM material)", req.method, req.target, r.Code)
		}
		if strings.Contains(r.Body.String(), "The Letter") || strings.Contains(r.Body.String(), "Waystone") {
			t.Errorf("%s %s: player response carries spine material: %s", req.method, req.target, r.Body)
		}
	}

	// Writes are refused the same way.
	r := hit(t, s, http.MethodPost, base+"/acts", `{"name":"Sneaky","level_start":1,"level_end":2}`, player)
	if r.Code != http.StatusForbidden {
		t.Errorf("player act write: %d, want 403", r.Code)
	}

	// An anonymous caller is gated before the route exists at all — 401,
	// which says nothing about the campaign. (A signed-in non-member gets
	// the same 404 a missing campaign produces; that path is asserted for
	// the campaign surface at large in campaign_test.go.)
	r = hit(t, s, http.MethodGet, base+"/story", "")
	if r.Code != http.StatusUnauthorized {
		t.Errorf("anonymous story read: %d, want 401 (the gate)", r.Code)
	}
	if strings.Contains(r.Body.String(), "The Letter") {
		t.Errorf("anonymous story read leaked spine material: %s", r.Body)
	}
}

// The deterministic helpers: shapes and pace answer with no campaign and no
// key at all.
func TestStoryHelpersArePure(t *testing.T) {
	s := newStoryServer(t)
	buildFixture(t, s) // claims the keeper account; the helpers need a session
	dm := dmSession(t, s)
	r := hit(t, s, http.MethodGet, "/api/story/shapes", "", dm)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "mid_turn") {
		t.Fatalf("shapes: status %d, body %s", r.Code, r.Body)
	}
	r = hit(t, s, http.MethodGet, "/api/story/pace?from=1&to=12&acts=4", "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("pace: status %d, body %s", r.Code, r.Body)
	}
	var out struct {
		Pace struct {
			TotalSessions int `json:"total_sessions"`
		} `json:"pace"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode pace: %v", err)
	}
	if out.Pace.TotalSessions != 26 {
		t.Errorf("pace(1,12,4) total = %d, want 26", out.Pace.TotalSessions)
	}
}

// The write-time quest-edge gate over HTTP: an outcome promising a move the
// machine does not have is a 400, not a stored lie.
func TestOutcomeWithIllegalQuestEdgeIsRejected(t *testing.T) {
	s := newStoryServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	machine := `{"initial":"unknown","states":["unknown","rumoured","confirmed"],
		"edges":[{"from":"unknown","to":"rumoured"},{"from":"rumoured","to":"confirmed"}]}`
	if r := hit(t, s, http.MethodPost, base+"/quests", `{"name":"Q","state_machine":`+machine+`}`, dm); r.Code != http.StatusCreated {
		t.Fatalf("create quest: %d %s", r.Code, r.Body)
	}
	var questList struct {
		Quests []struct {
			ID string `json:"id"`
		} `json:"quests"`
	}
	if err := json.Unmarshal(hit(t, s, http.MethodGet, base+"/quests", "", dm).Body.Bytes(), &questList); err != nil {
		t.Fatalf("list quests: %v", err)
	}
	questID := questList.Quests[len(questList.Quests)-1].ID

	r := hit(t, s, http.MethodPost, base+"/acts", `{"name":"A","level_start":1,"level_end":3}`, dm)
	var ab struct {
		Act struct {
			ID string `json:"id"`
		} `json:"act"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &ab); err != nil {
		t.Fatalf("decode act: %v", err)
	}
	r = hit(t, s, http.MethodPost, base+"/scenes", `{"act_id":`+quote(ab.Act.ID)+`,"kind":"social","name":"S"}`, dm)
	var sb struct {
		Scene struct {
			ID string `json:"id"`
		} `json:"scene"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &sb); err != nil {
		t.Fatalf("decode scene: %v", err)
	}

	// unknown -> confirmed skips the machine's only path.
	transition := fmt.Sprintf(`{"quest":%q,"from":"unknown","to":"confirmed"}`, questID)
	r = hit(t, s, http.MethodPost, base+"/scenes/"+sb.Scene.ID+"/outcomes",
		`{"label":"A","summary":"A shortcut.","quest_transition":`+transition+`}`, dm)
	if r.Code != http.StatusBadRequest {
		t.Errorf("illegal quest edge over HTTP: %d, want 400 (%s)", r.Code, r.Body)
	}
}

// The canon engine picks the spine's rules up: a spine with an overlap and
// an empty scene surfaces act_level_mismatch and scene_without_cast in a
// canon check run.
func TestCanonCheckRunsSpineRules(t *testing.T) {
	s := newStoryServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	for _, band := range []string{
		`{"name":"One","level_start":1,"level_end":5}`,
		`{"name":"Two","level_start":4,"level_end":9}`, // overlap
	} {
		if r := hit(t, s, http.MethodPost, base+"/acts", band, dm); r.Code != http.StatusCreated {
			t.Fatalf("create act: %d %s", r.Code, r.Body)
		}
	}
	r := hit(t, s, http.MethodGet, base+"/acts", "", dm)
	var list struct {
		Acts []struct {
			ID string `json:"id"`
		} `json:"acts"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &list); err != nil || len(list.Acts) != 2 {
		t.Fatalf("list acts: %v (%s)", err, r.Body)
	}
	if r := hit(t, s, http.MethodPost, base+"/scenes",
		`{"act_id":`+quote(list.Acts[0].ID)+`,"kind":"social","name":"Nobody here"}`, dm); r.Code != http.StatusCreated {
		t.Fatalf("create scene: %d %s", r.Code, r.Body)
	}

	r = hit(t, s, http.MethodPost, base+"/canon/check", "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("canon check: status %d, body %s", r.Code, r.Body)
	}
	for _, want := range []string{"act_level_mismatch", "scene_without_cast"} {
		if !strings.Contains(r.Body.String(), want) {
			t.Errorf("canon check output missing %q:\n%s", want, r.Body)
		}
	}
}
