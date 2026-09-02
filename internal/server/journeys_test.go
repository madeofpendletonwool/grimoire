package server

// The journeys HTTP surface (MAD-375): plan a road, read its day table,
// resolve days and the whole journey. DM-only end to end — a day table
// names what the DM planted along the road.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/journey"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// newJourneyServer boots a gated server with the stack journeys need: the
// campaign graph, the canon engine (offline — planning needs no model)
// wired for graph writes and as the journey finalizer.
func newJourneyServer(t *testing.T) (*Server, *fixture) {
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
	engine, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("open canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)
	journeys, err := journey.New(store.DB(), campaigns, engine, knowledgeStore)
	if err != nil {
		t.Fatalf("open journey store: %v", err)
	}
	engine = engine.WithJourneyFinalizer(journeys)
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithCanon(engine).WithJourneys(journeys)
	f := buildFixture(t, s)
	return s, &f
}

// roadFixture gives the campaign a road: Haven -3 road days-> Ford -4
// forest days-> Deepwood.
func roadFixture(t *testing.T, s *Server, f *fixture) (haven, ford, deep string) {
	t.Helper()
	dm := dmSession(t, s)
	mk := func(kind, name, payload string) string {
		body := `{"kind":` + quote(kind) + `,"name":` + quote(name)
		if payload != "" {
			body += `,"payload":` + payload
		}
		body += `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d, body %s", name, r.Code, r.Body)
		}
		return idFrom(t, r, "entity")
	}
	haven = mk("location", "Haven", "")
	ford = mk("location", "The Ford", `{"place":{"kind":"hamlet","services":["inn"]}}`)
	deep = mk("location", "Deepwood", `{"place":{"kind":"wilderness"}}`)
	patch := func(id, payload string) {
		body := `{"payload":` + payload + `}`
		r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/entities/"+id, body, dm)
		if r.Code != http.StatusOK {
			t.Fatalf("patch %s: status %d, body %s", id, r.Code, r.Body)
		}
	}
	patch(haven, `{"travel":{"routes":[{"to":"`+ford+`","days":3,"terrain":"road"},{"to":"`+deep+`","days":9,"terrain":"swamp"}]}}`)
	patch(ford, `{"travel":{"routes":[{"to":"`+deep+`","days":4,"terrain":"forest"}]}}`)
	return haven, ford, deep
}

func TestJourneysAreDMOnly(t *testing.T) {
	s, f := newJourneyServer(t)
	haven, _, deep := roadFixture(t, s, f)
	player := addPlayerMember(t, s, *f, "roadplayer", true)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/journeys",
		`{"from":`+quote(haven)+`,"to":`+quote(deep)+`,"density":"standard"}`, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player plan: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/journeys", "", player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player list: status %d, body %s", rec.Code, rec.Body)
	}

	dm := dmSession(t, s)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/journeys",
		`{"from":`+quote(haven)+`,"to":`+quote(deep)+`,"density":"standard","seed":42}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("dm plan: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Journey map[string]any   `json:"journey"`
		Days    []map[string]any `json:"days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Journey["id"] == nil || body.Journey["days"].(float64) != 7 {
		t.Fatalf("journey view wrong: %s", rec.Body)
	}
	if len(body.Days) != 7 {
		t.Fatalf("day table has %d days, want 7: %s", len(body.Days), rec.Body)
	}
	jid := body.Journey["id"].(string)

	// The day table reads back the same, DM only.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/journeys/"+jid, "", player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player read: status %d", rec.Code)
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/journeys/"+jid, "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("dm read: status %d, body %s", rec.Code, rec.Body)
	}

	// The whole surface refuses the player.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/journeys/"+jid+"/days/0/resolve", `{"detail":"x"}`, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player day resolve: status %d", rec.Code)
	}
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/journeys/"+jid+"/resolve", "", player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player resolve: status %d", rec.Code)
	}
}

// TestJourneyPlanDensityNoneIsOneLine: the hand-wave answers with the
// line, no day rows, and still resolves through the gate.
func TestJourneyPlanDensityNoneIsOneLine(t *testing.T) {
	s, f := newJourneyServer(t)
	haven, _, deep := roadFixture(t, s, f)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/journeys",
		`{"from":`+quote(haven)+`,"to":`+quote(deep)+`,"density":"none","seed":42}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plan: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Journey map[string]any   `json:"journey"`
		Days    []map[string]any `json:"days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	jid := body.Journey["id"].(string)
	if len(body.Days) != 0 {
		t.Fatalf("density none returned %d day rows: %s", len(body.Days), rec.Body)
	}
	if line, _ := body.Journey["line"].(string); line == "" {
		t.Fatalf("the hand-wave carried no line: %s", rec.Body)
	}

	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/journeys/"+jid+"/resolve", "", dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("resolve: status %d, body %s", rec.Code, rec.Body)
	}
	var staged struct {
		Batch map[string]any `json:"batch"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &staged); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	bid := staged.Batch["id"].(string)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals/"+bid+"/decision",
		`{"decision":"accept"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide: status %d, body %s", rec.Code, rec.Body)
	}

	var clock int64
	if err := s.store.DB().QueryRow(`SELECT clock FROM campaigns WHERE id = ?`, f.campaignID).Scan(&clock); err != nil {
		t.Fatalf("read clock: %v", err)
	}
	if clock != 7 {
		t.Fatalf("clock at %d, want 7", clock)
	}
	var advances int
	if err := s.store.DB().QueryRow(`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ? AND reason = 'travel'`,
		f.campaignID).Scan(&advances); err != nil {
		t.Fatalf("count advances: %v", err)
	}
	if advances != 1 {
		t.Fatalf("expected exactly one travel advance, got %d", advances)
	}
}

// TestJourneyDayResolveAndPatch: the DM settles a day at the table and
// can abandon the road; both render the updated journey.
func TestJourneyDayResolveAndPatch(t *testing.T) {
	s, f := newJourneyServer(t)
	haven, _, deep := roadFixture(t, s, f)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/journeys",
		`{"from":`+quote(haven)+`,"to":`+quote(deep)+`,"density":"standard","seed":42}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plan: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Journey map[string]any `json:"journey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	jid := body.Journey["id"].(string)

	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/journeys/"+jid+"/days/2/resolve",
		`{"detail":"Worse than the roll said."}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("day resolve: status %d, body %s", rec.Code, rec.Body)
	}
	var dayBody struct {
		Journey map[string]any `json:"journey"`
		Day     map[string]any `json:"day"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dayBody); err != nil {
		t.Fatalf("decode day: %v", err)
	}
	if dayBody.Journey["status"] != "underway" || dayBody.Day["detail"] != "Worse than the roll said." {
		t.Fatalf("day resolve wrong: %s", rec.Body)
	}

	rec = hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/journeys/"+jid,
		`{"status":"abandoned"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("abandon: status %d, body %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dayBody); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if dayBody.Journey["status"] != "abandoned" {
		t.Fatalf("abandon left status %v", dayBody.Journey["status"])
	}

	// An abandoned journey does not resolve.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/journeys/"+jid+"/resolve", "", dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("resolve abandoned: status %d, body %s", rec.Code, rec.Body)
	}
}

// TestJourneyNoRouteNeedsDays: a road the map does not hold is a refusal
// with the reason, and the DM's day count is the answer.
func TestJourneyNoRouteNeedsDays(t *testing.T) {
	s, f := newJourneyServer(t)
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities",
		`{"kind":"location","name":"The Far Island"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create island: status %d", rec.Code)
	}
	island := idFrom(t, rec, "entity")
	haven, _, _ := roadFixture(t, s, f)

	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/journeys",
		`{"from":`+quote(haven)+`,"to":`+quote(island)+`}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-route plan: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/journeys",
		`{"from":`+quote(haven)+`,"to":`+quote(island)+`,"days":12}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("override plan: status %d, body %s", rec.Code, rec.Body)
	}
}
