package server

// The downtime HTTP surface (MAD-368): a player requests downtime for their
// own character and learns nothing but the request's identity; the DM gets
// the full deterministic result, stages it, and decides it through the
// ordinary proposals endpoint.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/downtime"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// newDowntimeServer boots a gated server with the full stack downtime
// needs: the campaign graph, the knowledge layer, the faction plans, and a
// canon engine wired both for graph writes and as the downtime finalizer.
func newDowntimeServer(t *testing.T) (*Server, *fixture, *faction.Store) {
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
	factions, err := faction.New(store.DB())
	if err != nil {
		t.Fatalf("open faction store: %v", err)
	}
	engine, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("open canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore).WithFactions(factions)
	downtimeStore, err := downtime.New(store.DB(), campaigns, factions, engine)
	if err != nil {
		t.Fatalf("open downtime store: %v", err)
	}
	engine = engine.WithDowntimeFinalizer(downtimeStore)
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithFactions(factions).
		WithCanon(engine).WithDowntime(downtimeStore)
	f := buildFixture(t, s)
	return s, &f, factions
}

// downtimeWorld is the downtime-shaped half of the world: the pc positioned
// in a town, a chatty npc standing there, a cult with one public fact the
// npc knows and one secret nobody does, and the cult's active public plan.
type downtimeWorld struct {
	townID   string
	cultID   string
	publicID string
	secretID string
	planID   string
}

func plantDowntimeWorld(t *testing.T, s *Server, factions *faction.Store, f fixture) *downtimeWorld {
	t.Helper()
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", `{"kind":"location","name":"Blackwater"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create town: status %d, body %s", rec.Code, rec.Body)
	}
	town := idFrom(t, rec, "entity")

	mk := func(kind, name string) string {
		t.Helper()
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities",
			`{"kind":`+quote(kind)+`,"name":`+quote(name)+`}`, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d, body %s", name, r.Code, r.Body)
		}
		return idFrom(t, r, "entity")
	}
	tom := mk("npc", "Tom the Innkeeper")
	cult := mk("faction", "The Vernal Cult")

	link := func(from, rel, to string) {
		t.Helper()
		body := `{"from":` + quote(from) + `,"rel_type":` + quote(rel) + `,"to":` + quote(to) + `}`
		if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/relationships", body, dm); r.Code != http.StatusCreated {
			t.Fatalf("link %s %s %s: status %d, body %s", from, rel, to, r.Code, r.Body)
		}
	}
	link(f.pcID, "located_in", town)
	link(tom, "located_in", town)

	mkFact := func(statement, visibility string) string {
		t.Helper()
		body := `{"subject":` + quote(cult) + `,"predicate":"recruits","object_literal":"among the camps","statement":` +
			quote(statement) + `,"visibility":` + quote(visibility) + `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/facts", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create fact: status %d, body %s", r.Code, r.Body)
		}
		return idFrom(t, r, "fact")
	}
	public := mkFact("The Vernal Cult recruits among the riverside camps.", "public")
	secret := mkFact("The Vernal Cult's rite needs the drowned bell.", "secret")
	// The party already knows the cult courts them — which is what makes
	// the cult itself a name the player's perspective can resolve, without
	// handing them the findable fact above.
	courts := mkFact("The Vernal Cult courts the riverside families.", "public")

	// Tom knows the public fact; nobody knows the secret; the party knows
	// the courts fact.
	aware := func(knower, fact, stance string) {
		t.Helper()
		body := `{"knower":` + quote(knower) + `,"fact_id":` + quote(fact) + `,"stance":` + quote(stance) + `}`
		if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/awareness", body, dm); r.Code != http.StatusOK {
			t.Fatalf("set awareness: status %d, body %s", r.Code, r.Body)
		}
	}
	aware(tom, public, "knows")
	aware("party", courts, "knows")

	plan, err := factions.CreatePlan(t.Context(), f.campaignID, cult, faction.PlanInput{
		Name: "The Vernal Rite",
		Machine: campaign.StateMachine{
			Initial: "gathering", States: campaign.States("gathering", "ritual", "ascension"),
			Edges: []campaign.StateEdge{{From: "gathering", To: "ritual"}, {From: "ritual", To: "ascension"}},
		},
		Steps: []faction.Step{
			{State: "ritual", Name: "Prepare the rite", Cost: 3},
			{State: "ascension", Name: "Complete it", Cost: 5},
		},
		RatePerDay: 1, Status: faction.PlanActive, Visibility: campaign.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return &downtimeWorld{townID: town, cultID: cult, publicID: public, secretID: secret, planID: plan.ID}
}

// TestDowntimePlayerRequestsOwnCharacterAndLearnsNothing is the player half
// of the surface: the request records for the bound character and the
// response carries no result — none of the computed findings, none of the
// statements the character has no path to yet.
func TestDowntimePlayerRequestsOwnCharacterAndLearnsNothing(t *testing.T) {
	s, f, factions := newDowntimeServer(t)
	w := plantDowntimeWorld(t, s, factions, *f)
	player := addPlayerMember(t, s, *f, "dtpl", true)

	body := `{"activity":"I spend three weeks researching the cult","subject":` + quote(w.cultID) + `,"days":21}`
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/downtime", body, player)
	if rec.Code != http.StatusCreated {
		t.Fatalf("player downtime: status %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["result"] != nil {
		t.Fatalf("the player response carries the computed result: %s", rec.Body)
	}
	req, ok := out["request"].(map[string]any)
	if !ok || req["character_id"] != f.pcID || req["status"] != "requested" {
		t.Fatalf("request view wrong: %s", rec.Body)
	}
	did, _ := req["id"].(string)
	if did == "" {
		t.Fatalf("request id missing: %s", rec.Body)
	}
	// The leak pin at the HTTP layer: neither statement may appear in the
	// player's response bytes — not the findable one, not the secret one.
	if strings.Contains(rec.Body.String(), "riverside camps") || strings.Contains(rec.Body.String(), "drowned bell") {
		t.Fatalf("the player response leaks finding text: %s", rec.Body)
	}

	// A player may not request anyone else's downtime.
	other := `{"activity":"research","subject":` + quote(w.cultID) + `,"days":7,"character":` + quote(w.townID) + `}`
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/downtime", other, player); rec.Code != http.StatusForbidden {
		t.Fatalf("another character's downtime: status %d, body %s", rec.Code, rec.Body)
	}

	// A player's subject that resolves nowhere at their scope is the same
	// 404 a missing entity produces — the endpoint cannot be probed.
	ghost := `{"activity":"research","subject":"00000000-0000-0000-0000-000000000000","days":7}`
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/downtime", ghost, player); rec.Code != http.StatusNotFound {
		t.Fatalf("a missing subject must 404: status %d, body %s", rec.Code, rec.Body)
	}

	// Staging is the DM's.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/downtime/"+did+"/stage", "", player); rec.Code != http.StatusForbidden {
		t.Fatalf("player stage: status %d, body %s", rec.Code, rec.Body)
	}
}

// reqID pulls the request id out of a downtime response.
func reqID(t *testing.T, rec *recorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("no request in response: %s", rec.Body)
	}
	id, _ := req["id"].(string)
	return id
}

// TestDowntimeDMGetsResultAndStages pins the DM half: the full deterministic
// result in the response, staging behind the review gate, and the clarifying
// question for an unmappable activity.
func TestDowntimeDMGetsResultAndStages(t *testing.T) {
	s, f, factions := newDowntimeServer(t)
	w := plantDowntimeWorld(t, s, factions, *f)
	dm := dmSession(t, s)

	// An unmappable activity is a clarifying question.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/downtime",
		`{"activity":"I meditate upon the void","days":7,"character":`+quote(f.pcID)+`}`, dm)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "could not read one activity") {
		t.Fatalf("clarify: status %d, body %s", rec.Code, rec.Body)
	}

	body := `{"activity":"I spend three weeks researching the cult","subject":` + quote(w.cultID) +
		`,"days":21,"seed":7,"character":` + quote(f.pcID) + `}`
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/downtime", body, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("dm downtime: status %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["result"] == nil {
		t.Fatalf("the dm response missing the result: %s", rec.Body)
	}
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("result shape: %s", rec.Body)
	}
	findings, _ := res["findings"].([]any)
	if len(findings) == 0 {
		t.Fatalf("no findings for the dm: %s", rec.Body)
	}
	if _, hasTick := res["tick"]; !hasTick {
		t.Fatalf("no world movement for the dm: %s", rec.Body)
	}
	did := reqID(t, rec)

	// The player cannot stage; the DM can.
	player := addPlayerMember(t, s, *f, "dtdm", true)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/downtime/"+did+"/stage", "", player); rec.Code != http.StatusForbidden {
		t.Fatalf("player stage: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/downtime/"+did+"/stage", "", dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("dm stage: status %d, body %s", rec.Code, rec.Body)
	}
	batchID := idFrom(t, rec, "batch")

	// The decision moves the clock by exactly the window, reason downtime.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals/"+batchID+"/decision",
		`{"decision":"accept"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide: status %d, body %s", rec.Code, rec.Body)
	}
	var clockBody struct {
		Clock struct {
			Day int64 `json:"day"`
		} `json:"clock"`
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock", "", dm)
	if err := json.Unmarshal(rec.Body.Bytes(), &clockBody); err != nil {
		t.Fatalf("decode clock: %v", err)
	}
	if clockBody.Clock.Day != 21 {
		t.Fatalf("clock at day %d, want 21", clockBody.Clock.Day)
	}
	var advances int
	if err := s.store.DB().QueryRow(
		`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ? AND reason = 'downtime'`, f.campaignID,
	).Scan(&advances); err != nil {
		t.Fatalf("count advances: %v", err)
	}
	if advances != 1 {
		t.Fatalf("expected exactly one downtime advance, got %d", advances)
	}
}
