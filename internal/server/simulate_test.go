package server

// The simulation tick HTTP surface (MAD-367): preview a window, stage it,
// and let the ordinary batch decision move the clock. DM-only end to end.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/sim"
)

// newSimServer boots a gated server with the full stack the tick needs: the
// campaign graph, the faction plans, and a canon engine wired both for
// graph writes and as the tick's finalizer.
func newSimServer(t *testing.T) (*Server, *fixture, *faction.Store) {
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
	simStore, err := sim.New(store.DB(), campaigns, factions, engine)
	if err != nil {
		t.Fatalf("open sim store: %v", err)
	}
	engine = engine.WithTickFinalizer(simStore)
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithFactions(factions).WithCanon(engine).WithSim(simStore)
	f := buildFixture(t, s)
	return s, &f, factions
}

// simPlanFixture adds a faction entity and one active, public plan to the
// campaign so a preview has something to move.
func simPlanFixture(t *testing.T, s *Server, factions *faction.Store, f fixture) string {
	t.Helper()
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities",
		`{"kind":"faction","name":"The Vernal Cult"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create faction: status %d, body %s", rec.Code, rec.Body)
	}
	factionID := idFrom(t, rec, "entity")

	ctx := t.Context()
	plan, err := factions.CreatePlan(ctx, f.campaignID, factionID, faction.PlanInput{
		Name:       "The Vernal Rite",
		Machine:    campaign.StateMachine{Initial: "gathering", States: []string{"gathering", "ritual"}, Edges: []campaign.StateEdge{{From: "gathering", To: "ritual"}}},
		Steps:      []faction.Step{{State: "ritual", Name: "Prepare the rite", Cost: 3}},
		RatePerDay: 1, Status: faction.PlanActive, Visibility: campaign.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return plan.ID
}

func TestSimulateIsDMOnly(t *testing.T) {
	s, f, factions := newSimServer(t)
	simPlanFixture(t, s, factions, *f)
	player := addPlayerMember(t, s, *f, "simplayer", true)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/simulate", `{"days":7}`, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player simulate: status %d, body %s", rec.Code, rec.Body)
	}

	dm := dmSession(t, s)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/simulate", `{"days":7}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("dm simulate: status %d, body %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["result"] == nil || body["tick"] == nil {
		t.Fatalf("preview response missing tick/result: %s", rec.Body)
	}

	tid := idFrom(t, rec, "tick")
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/simulate/"+tid+"/stage", "", player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player stage: status %d, body %s", rec.Code, rec.Body)
	}
}

// TestSimulatePreviewWritesNothing pins the acceptance criterion at the
// HTTP layer: a preview's row counts on entities, facts, events and
// relationships are unchanged before and after.
func TestSimulatePreviewWritesNothing(t *testing.T) {
	s, f, factions := newSimServer(t)
	simPlanFixture(t, s, factions, *f)
	dm := dmSession(t, s)

	before := countGraph(t, s, f.campaignID)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/simulate", `{"days":14}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("simulate: status %d, body %s", rec.Code, rec.Body)
	}
	after := countGraph(t, s, f.campaignID)
	if before != after {
		t.Fatalf("preview changed graph row counts: %d -> %d", before, after)
	}
}

func countGraph(t *testing.T, s *Server, campaignID string) int {
	t.Helper()
	var n int
	for _, table := range []string{"entities", "facts", "events"} {
		var c int
		if err := s.store.DB().QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE campaign_id = ?`, campaignID).Scan(&c); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		n += c
	}
	return n
}

// TestSimulateStageAndAcceptMovesClockOnce is the end-to-end acceptance
// path: preview, stage, decide through the ordinary proposals endpoint —
// the surface a DM actually uses — and the clock moves exactly once by
// exactly the window.
func TestSimulateStageAndAcceptMovesClockOnce(t *testing.T) {
	s, f, factions := newSimServer(t)
	simPlanFixture(t, s, factions, *f)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/simulate", `{"days":14,"seed":7}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("simulate: status %d, body %s", rec.Code, rec.Body)
	}
	tid := idFrom(t, rec, "tick")

	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/simulate/"+tid+"/stage", "", dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("stage: status %d, body %s", rec.Code, rec.Body)
	}
	batchID := idFrom(t, rec, "batch")

	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals/"+batchID+"/decision",
		`{"decision":"accept"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide: status %d, body %s", rec.Code, rec.Body)
	}

	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("clock: status %d, body %s", rec.Code, rec.Body)
	}
	var clockBody struct {
		Clock struct {
			Day int64 `json:"day"`
		} `json:"clock"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &clockBody); err != nil {
		t.Fatalf("decode clock: %v", err)
	}
	if clockBody.Clock.Day != 14 {
		t.Fatalf("clock at day %d, want 14", clockBody.Clock.Day)
	}

	var advances int
	if err := s.store.DB().QueryRow(
		`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ? AND reason = 'tick'`, f.campaignID,
	).Scan(&advances); err != nil {
		t.Fatalf("count advances: %v", err)
	}
	if advances != 1 {
		t.Fatalf("expected exactly one tick advance, got %d", advances)
	}
}
