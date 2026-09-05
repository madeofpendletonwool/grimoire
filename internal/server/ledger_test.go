package server

// The resource ledger's HTTP surface (MAD-419): permissions at the API
// layer, the live rest button, and the acceptance e2e — a long rest
// advances the campaign clock and everything attached to the clock reacts:
// the schedule's due list and the sim's next window both answer from the
// new day.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/sim"
)

// newLedgerServer boots the full stack the resource surface needs: the
// campaign graph, the canon engine as the review gate, the sim (so the
// rest's clock move can be proven to reach it), and the ledger store
// registered as the rest finalizer.
func newLedgerServer(t *testing.T) (*Server, *fixture) {
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
	ledgerStore, err := ledger.New(store.DB(), campaigns, engine)
	if err != nil {
		t.Fatalf("open ledger store: %v", err)
	}
	engine = engine.WithRestFinalizer(ledgerStore)
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithFactions(factions).WithCanon(engine).
		WithSim(simStore).WithLedger(ledgerStore)
	f := buildFixture(t, s)
	return s, &f
}

// wizardSheetJSON is the fixture sheet the tests PUT.
const wizardSheetJSON = `{
	"race": "human",
	"classes": [{"class": "Wizard", "level": 5}],
	"max_hp": 32,
	"ac": 14,
	"spellcasting": {"ability": "int", "dc": 15, "slots": {"1": 4, "2": 3, "3": 2}},
	"currency": {"gp": 130}
}`

// putSheet writes a pc's sheet and returns nothing — a helper because every
// test starts the same way.
func putSheet(t *testing.T, s *Server, f fixture, eid, sheetJSON string) {
	t.Helper()
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/characters/"+eid+"/sheet", sheetJSON, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("put sheet: status %d, body %s", rec.Code, rec.Body)
	}
}

// resourceBalances reads a character's balances as pool-key -> current.
func resourceBalances(t *testing.T, s *Server, f fixture, eid string, cookie *http.Cookie) map[string]int {
	t.Helper()
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+eid+"/resources", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("resources: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Balances []struct {
			Pool struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"pool"`
			Current int `json:"current"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode balances: %v", err)
	}
	out := make(map[string]int, len(body.Balances))
	for _, b := range body.Balances {
		out[b.Pool.Kind+":"+b.Pool.Name] = b.Current
	}
	return out
}

// poolIDOf finds one pool's row id through the balances read.
func poolIDOf(t *testing.T, s *Server, f fixture, eid string, cookie *http.Cookie, key string) string {
	t.Helper()
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+eid+"/resources", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("resources: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Balances []struct {
			Pool struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"pool"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, b := range body.Balances {
		if b.Pool.Kind+":"+b.Pool.Name == key {
			return b.Pool.ID
		}
	}
	t.Fatalf("no pool %s", key)
	return ""
}

/* ---------- permissions ---------- */

func TestResourcePermissions(t *testing.T) {
	s, f := newLedgerServer(t)
	putSheet(t, s, *f, f.pcID, wizardSheetJSON)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "velren", true)   // bound to f.pcID
	other := addPlayerMember(t, s, *f, "onlooker", false) // party scope, no binding

	// A second pc nobody owns.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", `{"kind":"pc","name":"Nyx"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pc: status %d, body %s", rec.Code, rec.Body)
	}
	nyx := idFrom(t, rec, "entity")

	// The bound player reads their own; nobody else's; the party-scoped
	// player reads nobody's.
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources", "", player); rec.Code != http.StatusOK {
		t.Fatalf("own resources: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+nyx+"/resources", "", player); rec.Code != http.StatusForbidden {
		t.Fatalf("another pc's resources: status %d, want 403", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources", "", other); rec.Code != http.StatusForbidden {
		t.Fatalf("party-scope read: status %d, want 403", rec.Code)
	}

	slot3 := poolIDOf(t, s, *f, f.pcID, dm, "slot:3")
	spend := `{"kind":"spend","amount":2,"note":"fireball"}`
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources/"+slot3+"/transactions", spend, player); rec.Code != http.StatusCreated {
		t.Fatalf("player spends own: status %d, body %s", rec.Code, rec.Body)
	}
	// A player cannot spend another pc's pool even by id.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+nyx+"/resources/"+slot3+"/transactions", spend, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player spends another's: status %d, want 403", rec.Code)
	}
	// A set is the DM's correction, refused to the player.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources/"+slot3+"/transactions", `{"kind":"set","amount":2}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player set: status %d, want 403", rec.Code)
	}
	// The DM's set lands as a visible transaction.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources/"+slot3+"/transactions", `{"kind":"set","amount":1,"note":"recount"}`, dm); rec.Code != http.StatusCreated {
		t.Fatalf("dm set: status %d, body %s", rec.Code, rec.Body)
	}
	if got := resourceBalances(t, s, *f, f.pcID, player)["slot:3"]; got != 1 {
		t.Fatalf("slot:3 = %d, want 1", got)
	}
	// Overspend refuses.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources/"+slot3+"/transactions", `{"kind":"spend","amount":2}`, player); rec.Code != http.StatusBadRequest {
		t.Fatalf("overspend: status %d, want 400 (%s)", rec.Code, rec.Body)
	}
	// Pool registration is the DM's.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources", `{"kind":"feature","name":"arrows","size":20,"recovery":"manual"}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player registers a pool: status %d, want 403", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources", `{"kind":"item","name":"arrows","size":20,"recovery":"manual"}`, dm); rec.Code != http.StatusCreated {
		t.Fatalf("dm registers a pool: status %d, body %s", rec.Code, rec.Body)
	}
}

/* ---------- the acceptance e2e ---------- */

// TestLongRestFiresClockScheduleAndSimHooks is the stage's acceptance: a
// DM's live long rest advances the campaign clock by exactly one day under
// reason 'rest', restores what the recovery grammar says, and everything
// attached to the clock answers from the new day — the schedule's due list
// carries the entry that just came due, and the sim's next window begins
// where the rest ended.
func TestLongRestFiresClockScheduleAndSimHooks(t *testing.T) {
	s, f := newLedgerServer(t)
	putSheet(t, s, *f, f.pcID, wizardSheetJSON)
	dm := dmSession(t, s)

	slot3 := poolIDOf(t, s, *f, f.pcID, dm, "slot:3")
	hd := poolIDOf(t, s, *f, f.pcID, dm, "hit_dice:hit_dice")
	for i := 0; i < 3; i++ {
		if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources/"+hd+"/transactions", `{"kind":"spend","amount":1}`, dm); rec.Code != http.StatusCreated {
			t.Fatalf("spend hit die: status %d, body %s", rec.Code, rec.Body)
		}
	}
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources/"+slot3+"/transactions", `{"kind":"spend","amount":2}`, dm); rec.Code != http.StatusCreated {
		t.Fatalf("spend slots: status %d, body %s", rec.Code, rec.Body)
	}

	// Something is scheduled for the day the party sleeps into, and a
	// faction plan is live so the sim has something to move.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/schedule",
		`{"name":"The Vernal caravan arrives","day":1}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("schedule: status %d, body %s", rec.Code, rec.Body)
	}

	var before struct {
		Clock struct {
			Day int64 `json:"day"`
		} `json:"clock"`
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock", "", dm)
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode clock: %v", err)
	}

	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rests",
		`{"kind":"long","characters":[`+quote(f.pcID)+`],"note":"camp outside the mines"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("rest: status %d, body %s", rec.Code, rec.Body)
	}
	var restBody struct {
		Rest struct {
			AdvanceID string `json:"advance_id"`
			ClockTo   int64  `json:"clock_to"`
		} `json:"rest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &restBody); err != nil {
		t.Fatalf("decode rest: %v", err)
	}
	if restBody.Rest.AdvanceID == "" || restBody.Rest.ClockTo != before.Clock.Day+1 {
		t.Fatalf("rest did not advance the clock by one day: %+v", restBody.Rest)
	}

	// The clock's own read agrees, with the rest's advance in its ledger.
	var after beforeT
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock", "", dm)
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode clock: %v", err)
	}
	if after.Clock.Day != before.Clock.Day+1 {
		t.Fatalf("clock at %d, want %d", after.Clock.Day, before.Clock.Day+1)
	}
	var reason string
	if err := s.store.DB().QueryRow(`SELECT reason FROM clock_advances WHERE id = ?`, restBody.Rest.AdvanceID).Scan(&reason); err != nil {
		t.Fatalf("read advance: %v", err)
	}
	if reason != campaign.AdvanceRest {
		t.Fatalf("advance reason = %q, want rest", reason)
	}

	// The schedule hook: the caravan that was tomorrow is now due.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock?due=7", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("clock due: status %d", rec.Code)
	}
	if !jsonContains(rec.Body.String(), "The Vernal caravan arrives") {
		t.Fatalf("the day-the-rest-crossed schedule entry is not due: %s", rec.Body)
	}

	// The sim hook: the next window begins on the new day.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/simulate", `{"days":7,"seed":11}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("simulate: status %d, body %s", rec.Code, rec.Body)
	}
	var simBody struct {
		Tick struct {
			FromDay int64 `json:"from_day"`
		} `json:"tick"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &simBody); err != nil {
		t.Fatalf("decode simulate: %v", err)
	}
	if simBody.Tick.FromDay != before.Clock.Day+1 {
		t.Fatalf("sim window begins at %d, want the post-rest day %d", simBody.Tick.FromDay, before.Clock.Day+1)
	}

	// And the grammar did its work on the ledger.
	balances := resourceBalances(t, s, *f, f.pcID, dm)
	if balances["slot:3"] != 2 {
		t.Fatalf("slots = %d, want 2 restored", balances["slot:3"])
	}
	if balances["hit_dice:hit_dice"] != 4 { // 5 - 3 + half-regain 2
		t.Fatalf("hit dice = %d, want 4", balances["hit_dice:hit_dice"])
	}
}

type beforeT struct {
	Clock struct {
		Day int64 `json:"day"`
	} `json:"clock"`
}

func jsonContains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

/* ---------- the proposed rest, end to end ---------- */

func TestProposedRestThroughTheGate(t *testing.T) {
	s, f := newLedgerServer(t)
	putSheet(t, s, *f, f.pcID, wizardSheetJSON)
	dm := dmSession(t, s)

	slot3 := poolIDOf(t, s, *f, f.pcID, dm, "slot:3")
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources/"+slot3+"/transactions", `{"kind":"spend","amount":2}`, dm); rec.Code != http.StatusCreated {
		t.Fatalf("spend: status %d, body %s", rec.Code, rec.Body)
	}

	// The proposal is DM-staged (a transcription hook fires it) and gated.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rests/propose",
		`{"kind":"long","characters":[`+quote(f.pcID)+`],"note":"we take a long rest"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("propose: status %d, body %s", rec.Code, rec.Body)
	}
	batchID := idFrom(t, rec, "batch")
	if got := resourceBalances(t, s, *f, f.pcID, dm)["slot:3"]; got != 0 {
		t.Fatalf("staging the proposal applied the rest: %d", got)
	}

	// The decision runs through the ordinary proposals endpoint.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals/"+batchID+"/decision",
		`{"decision":"accept"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide: status %d, body %s", rec.Code, rec.Body)
	}
	if got := resourceBalances(t, s, *f, f.pcID, dm)["slot:3"]; got != 2 {
		t.Fatalf("accepted proposal did not restore slots: %d", got)
	}

	// The clock moved exactly once, reason 'rest'.
	var clockBody struct {
		Clock struct {
			Day int64 `json:"day"`
		} `json:"clock"`
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock", "", dm)
	if err := json.Unmarshal(rec.Body.Bytes(), &clockBody); err != nil {
		t.Fatalf("decode clock: %v", err)
	}
	if clockBody.Clock.Day != 1 {
		t.Fatalf("clock at %d, want 1", clockBody.Clock.Day)
	}
	var restAdvances int
	if err := s.store.DB().QueryRow(
		`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ? AND reason = 'rest'`, f.campaignID,
	).Scan(&restAdvances); err != nil {
		t.Fatalf("count advances: %v", err)
	}
	if restAdvances != 1 {
		t.Fatalf("rest advances = %d, want 1", restAdvances)
	}

	// The accepted item lives on the campaign timeline — the rest is an
	// event with provenance, not a silent mutation.
	var events int
	if err := s.store.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE campaign_id = ? AND summary LIKE '%long rest%'`, f.campaignID,
	).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("rest events = %d, want 1 on the timeline", events)
	}
}

/* ---------- scope guards ---------- */

func TestRestsAreDMOnly(t *testing.T) {
	s, f := newLedgerServer(t)
	putSheet(t, s, *f, f.pcID, wizardSheetJSON)
	player := addPlayerMember(t, s, *f, "restless", true)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rests",
		`{"kind":"long","characters":[`+quote(f.pcID)+`]}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player rest: status %d, want 403", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rests/propose",
		`{"kind":"long","characters":[`+quote(f.pcID)+`]}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player propose: status %d, want 403", rec.Code)
	}
}
