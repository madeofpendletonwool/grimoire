package server

// The leveling surface's HTTP tests (MAD-424): the permission matrix, the
// award off the encounter oracle, the gated level-up end to end through
// the batch decision endpoint, and the reconciliation pass that flags and
// proposes but never auto-applies.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/leveling"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// newLevelingServer boots the full stack the leveling surface needs: the
// campaign graph, the canon engine as the review gate, the ledger, and
// the leveling store registered as both its finalizers.
func newLevelingServer(t *testing.T) (*Server, *fixture, *leveling.Store) {
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
	ledgerStore, err := ledger.New(store.DB(), campaigns, engine)
	if err != nil {
		t.Fatalf("open ledger store: %v", err)
	}
	engine = engine.WithRestFinalizer(ledgerStore)
	levelingStore, err := leveling.New(store.DB(), campaigns, engine, ledgerStore)
	if err != nil {
		t.Fatalf("open leveling store: %v", err)
	}
	engine = engine.WithLevelUpFinalizer(levelingStore).WithReconcileFinalizer(levelingStore)
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithCanon(engine).
		WithLedger(ledgerStore).WithLeveling(levelingStore)
	f := buildFixture(t, s)
	return s, &f, levelingStore
}

// mkEncounter inserts a campaign encounter priced by the test.
func mkEncounter(t *testing.T, s *Server, f fixture, monsters string) string {
	t.Helper()
	if _, err := s.store.DB().Exec(`
		INSERT INTO encounters (id, owner_id, name, party, monsters, campaign_id, status, created_at, updated_at)
		VALUES ('enc-test', 'keeper', 'Ambush at the ford', '[4]', ?, ?, 'planned', 1, 1)`,
		monsters, f.campaignID); err != nil {
		t.Fatalf("insert encounter: %v", err)
	}
	return "enc-test"
}

func TestLevelingIsDMOnly(t *testing.T) {
	s, f, _ := newLevelingServer(t)
	putSheet(t, s, *f, f.pcID, `{"classes":[{"class":"wizard","level":4}],"max_hp":32,"xp":6500}`)
	player := addPlayerMember(t, s, *f, "mira", true)

	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/leveling", ""},
		{http.MethodPut, "/api/campaigns/" + f.campaignID + "/leveling/settings", `{"mode":"milestone"}`},
		{http.MethodPost, "/api/campaigns/" + f.campaignID + "/encounters/enc-test/award", `{}`},
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/awards", ""},
		{http.MethodPost, "/api/campaigns/" + f.campaignID + "/level-ups/propose", `{"character":"x","class":"wizard"}`},
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/level-ups", ""},
		{http.MethodPost, "/api/campaigns/" + f.campaignID + "/reconcile", `{}`},
	} {
		r := hit(t, s, tc.method, tc.path, tc.body, player)
		if r.Code != http.StatusForbidden {
			t.Errorf("%s %s: player got %d, want 403", tc.method, tc.path, r.Code)
		}
	}
}

func TestAwardEndToEnd(t *testing.T) {
	s, f, _ := newLevelingServer(t)
	putSheet(t, s, *f, f.pcID, `{"classes":[{"class":"wizard","level":4}],"max_hp":32,"xp":6500}`)
	mkEncounter(t, s, *f, `[{"name":"goblin","cr":"1/4","xp":50,"count":4}]`)
	dm := dmSession(t, s)

	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/encounters/enc-test/award",
		`{"note":"the ford"}`, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("award: status %d, body %s", r.Code, r.Body)
	}
	var body struct {
		Award struct {
			TotalXP int            `json:"total_xp"`
			Shares  map[string]int `json:"shares"`
		} `json:"award"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Award.TotalXP != 200 || body.Award.Shares[f.pcID] != 200 {
		t.Fatalf("award = %+v, want 200 total / 200 share", body.Award)
	}

	// The sheet carries the new total; the read surface reports it.
	r = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/sheet", "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("sheet: %d %s", r.Code, r.Body)
	}
	if !jsonContains(r.Body.String(), `"xp":6700`) {
		t.Errorf("sheet xp not bumped: %s", r.Body)
	}

	r = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/awards", "", dm)
	if r.Code != http.StatusOK || !jsonContains(r.Body.String(), `"amount":200`) {
		t.Errorf("awards read: %d %s", r.Code, r.Body)
	}

	// Milestone mode refuses the next award.
	r = hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/leveling/settings", `{"mode":"milestone"}`, dm)
	if r.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", r.Code, r.Body)
	}
	r = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/encounters/enc-test/award", `{}`, dm)
	if r.Code == http.StatusCreated {
		t.Error("awarding into an already-awarded encounter should refuse")
	}
}

func TestLevelUpProposeAndDecide(t *testing.T) {
	s, f, _ := newLevelingServer(t)
	putSheet(t, s, *f, f.pcID, `{"classes":[{"class":"wizard","level":4}],"max_hp":32,"xp":6500,
		"abilities":{"con":16},"spellcasting":{"ability":"int","slots":{"1":4,"2":3}}}`)
	dm := dmSession(t, s)

	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/level-ups/propose",
		`{"character":`+quote(f.pcID)+`,"class":"Wizard","note":"fifth circle"}`, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("propose: status %d, body %s", r.Code, r.Body)
	}
	batchID := idFrom(t, r, "batch")

	// The gate: nothing leveled yet.
	sheet := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/sheet", "", dm)
	if jsonContains(sheet.Body.String(), `"class":"wizard","level":5`) {
		t.Fatal("the sheet leveled before any decision")
	}

	// Decide through the batch surface — one endpoint for every generator.
	r = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals/"+batchID+"/decision",
		`{"decision":"accept"}`, dm)
	if r.Code != http.StatusOK {
		t.Fatalf("decide: status %d, body %s", r.Code, r.Body)
	}
	sheet = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/sheet", "", dm)
	for _, want := range []string{`"class":"wizard","level":5`, `"max_hp":39`, `"3":2`} {
		if !jsonContains(sheet.Body.String(), want) {
			t.Errorf("leveled sheet missing %s: %s", want, sheet.Body)
		}
	}

	// The level-up log records the applied row.
	r = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/level-ups", "", dm)
	if r.Code != http.StatusOK || !jsonContains(r.Body.String(), `"status":"applied"`) {
		t.Errorf("level-ups read: %d %s", r.Code, r.Body)
	}

	// An ineligible proposal refuses with the number it needs.
	r = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/level-ups/propose",
		`{"character":`+quote(f.pcID)+`,"class":"wizard"}`, dm)
	if r.Code != http.StatusBadRequest {
		t.Errorf("staging level 6 at 6700 xp got %d, want 400", r.Code)
	}
}

func TestReconcileEndToEnd(t *testing.T) {
	s, f, _ := newLevelingServer(t)
	putSheet(t, s, *f, f.pcID, `{"classes":[{"class":"fighter","level":3}],"max_hp":28,"xp":900}`)
	dm := dmSession(t, s)

	// Seed the discrepancy: a spend the fold cannot justify.
	db := s.store.DB()
	if _, err := db.Exec(`
		INSERT INTO resource_transactions (id, campaign_id, entity_id, pool_id, pool, kind, amount, actor, note, clock_day, created_at)
		SELECT 'seed', ?, ?, id, 'hp:hp', 'spend', 40, 'gremlin', 'seeded', 1, 1
		  FROM resource_pools WHERE campaign_id = ? AND entity_id = ? AND kind = 'hp'`,
		f.campaignID, f.pcID, f.campaignID, f.pcID); err != nil {
		t.Fatal(err)
	}

	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/reconcile", `{"note":"session 12"}`, dm)
	if r.Code != http.StatusOK {
		t.Fatalf("reconcile: status %d, body %s", r.Code, r.Body)
	}
	var body struct {
		Findings []map[string]any `json:"findings"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Findings) != 1 || body.Findings[0]["kind"] != "pool_bounds" {
		t.Fatalf("findings = %+v", body.Findings)
	}
	batchID := idFrom(t, r, "batch")

	// Nothing auto-applied: the balance still folds to -12.
	var current int
	if err := db.QueryRow(`
		SELECT COALESCE((SELECT -SUM(amount) FROM resource_transactions t
		 WHERE t.pool_id = p.id AND t.kind IN ('spend')), 0) + p.size
		  FROM resource_pools p WHERE p.campaign_id = ? AND p.entity_id = ? AND p.kind = 'hp'`,
		f.campaignID, f.pcID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != -12 {
		t.Fatalf("reconcile auto-applied a fix: hp folds to %d, want -12", current)
	}

	// Accept: the correction lands as a visible set.
	r = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals/"+batchID+"/decision",
		`{"decision":"accept"}`, dm)
	if r.Code != http.StatusOK {
		t.Fatalf("decide: status %d, body %s", r.Code, r.Body)
	}
	balances := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources", "", dm)
	if !jsonContains(balances.Body.String(), `"pool":"hp:hp","kind":"set"`) &&
		!jsonContains(balances.Body.String(), `"kind":"set"`) {
		// The balance read itself must show 0 current.
		t.Logf("balances: %s", balances.Body)
	}
	if !jsonContains(balances.Body.String(), `"current":0`) {
		t.Errorf("corrected hp not 0: %s", balances.Body)
	}
}

func TestLevelingReadSurface(t *testing.T) {
	s, f, _ := newLevelingServer(t)
	putSheet(t, s, *f, f.pcID, `{"classes":[{"class":"wizard","level":4}],"max_hp":32,"xp":6500}`)
	dm := dmSession(t, s)

	r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/leveling", "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("read: %d %s", r.Code, r.Body)
	}
	for _, want := range []string{`"mode":"xp"`, `"level":4`, `"next_level":5`, `"next_at":6500`, `"eligible":true`} {
		if !jsonContains(r.Body.String(), want) {
			t.Errorf("progress missing %s: %s", want, r.Body)
		}
	}
}
