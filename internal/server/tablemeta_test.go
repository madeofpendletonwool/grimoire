package server

// The table meta surface's HTTP contract (MAD-428): the inspiration
// award and its spend through the roll flow (advantage, once, on an
// attack roll, saving throw or ability check — the 2014 rule enforced
// as written), and the stats fold — the endcap view that shows the DM
// the secret rolls a player's fold never sees.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/stats"
)

// newTableMetaServer boots the full stack table meta needs: the ledger
// (inspiration), the dice engine (the roll flow), the combat tracker
// (attributed damage for the fold) and the stats store.
func newTableMetaServer(t *testing.T) (*Server, *fixture) {
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
	ledgerStore, err := ledger.New(store.DB(), campaigns, engine)
	if err != nil {
		t.Fatalf("open ledger store: %v", err)
	}
	engine = engine.WithRestFinalizer(ledgerStore)
	sessions, err := gamesession.New(store.DB())
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	diceStore, err := dice.New(store.DB(), campaigns, sessions)
	if err != nil {
		t.Fatalf("open dice store: %v", err)
	}
	combatStore, err := combat.New(store.DB(), campaigns, sessions, diceStore)
	if err != nil {
		t.Fatalf("open combat store: %v", err)
	}
	statsStore, err := stats.New(store.DB())
	if err != nil {
		t.Fatalf("open stats store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaign(campaigns, sessions).
		WithCampaigns(campaigns, knowledgeStore).WithFactions(factions).WithCanon(engine).
		WithLedger(ledgerStore).WithDice(diceStore).WithCombat(combatStore).WithStats(statsStore)
	f := buildFixture(t, s)
	return s, &f
}

/* ---------- the award ---------- */

func TestInspirationAwardRoute(t *testing.T) {
	s, f := newTableMetaServer(t)
	dm := dmSession(t, s)
	putSheet(t, s, *f, f.pcID, wizardSheetJSON)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/inspiration",
		`{"note":"the goblin plan was excellent"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("award: status %d, body %s", rec.Code, rec.Body)
	}
	if held := resourceBalance(t, s, *f, f.pcID, "feature:inspiration", dm); held != 1 {
		t.Fatalf("after award: %d, want 1", held)
	}

	// 2014: it does not stack — the second award refuses.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/inspiration", `{}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("second award: status %d, body %s", rec.Code, rec.Body)
	}

	// The award is the DM's word.
	player := addPlayerMember(t, s, *f, "mira", true)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/inspiration", `{}`, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player award: status %d, body %s", rec.Code, rec.Body)
	}
}

// resourceBalance reads one pool's derived current value.
func resourceBalance(t *testing.T, s *Server, f fixture, eid, key string, cookie *http.Cookie) int {
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
		t.Fatalf("decode: %v", err)
	}
	for _, b := range body.Balances {
		if b.Pool.Kind+":"+b.Pool.Name == key {
			return b.Current
		}
	}
	return -1
}

/* ---------- the spend, through the roll flow ---------- */

func TestRollSpendsInspiration(t *testing.T) {
	s, f := newTableMetaServer(t)
	dm := dmSession(t, s)
	putSheet(t, s, *f, f.pcID, wizardSheetJSON)
	player := addPlayerMember(t, s, *f, "mira", true)

	award := func(cookie *http.Cookie) int {
		rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/inspiration", `{}`, cookie)
		return rec.Code
	}
	rollWithSpend := func(body string, cookie *http.Cookie) *recorder {
		return hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls", body, cookie)
	}

	// The player spends their own, on an attack, for advantage.
	if award(dm) != http.StatusCreated {
		t.Fatal("award failed")
	}
	rec := rollWithSpend(`{"formula":"1d20+5","context":"attack","spend_inspiration":true}`, player)
	if rec.Code != http.StatusCreated {
		t.Fatalf("inspired roll: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Roll struct {
			Mode        string `json:"mode"`
			Inspiration bool   `json:"inspiration"`
			Formula     string `json:"formula"`
			Notation    string `json:"notation"`
		} `json:"roll"`
		Inspiration struct {
			Pool string `json:"pool"`
			Kind string `json:"kind"`
		} `json:"inspiration"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Roll.Mode != "advantage" || !body.Roll.Inspiration {
		t.Fatalf("the roll did not carry advantage from inspiration: %+v", body.Roll)
	}
	if !strings.Contains(body.Roll.Notation, "2d20kh1") {
		t.Fatalf("the engine rewrote the formula: %+v", body.Roll)
	}
	if body.Inspiration.Pool != "feature:inspiration" || body.Inspiration.Kind != "spend" {
		t.Fatalf("the spend txn: %+v", body.Inspiration)
	}
	if held := resourceBalance(t, s, *f, f.pcID, "feature:inspiration", dm); held != 0 {
		t.Fatalf("after the spend: %d, want 0", held)
	}

	// Spent is spent: no inspiration, no advantage.
	rec = rollWithSpend(`{"formula":"1d20+5","context":"attack","spend_inspiration":true}`, player)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("spend without holding: status %d, body %s", rec.Code, rec.Body)
	}

	// The 2014 rule's edges, each refused before anything moves. One
	// award stands for the lot: a refused spend never consumes it.
	if award(dm) != http.StatusCreated {
		t.Fatal("award before the edges failed")
	}
	for name, req := range map[string]string{
		"initiative": `{"formula":"1d20+5","context":"initiative","spend_inspiration":true}`,
		"damage":     `{"formula":"1d20+5","context":"damage","spend_inspiration":true}`,
		"a mode":     `{"formula":"1d20+5","context":"attack","mode":"disadvantage","spend_inspiration":true}`,
		"no d20":     `{"formula":"2d6+3","context":"attack","spend_inspiration":true}`,
	} {
		rec = rollWithSpend(req, player)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, body %s", name, rec.Code, rec.Body)
		}
		if held := resourceBalance(t, s, *f, f.pcID, "feature:inspiration", dm); held != 1 {
			t.Fatalf("%s ate the inspiration: held = %d", name, held)
		}
	}
}

/* ---------- the fold, over HTTP ---------- */

func TestStatsEndpoint(t *testing.T) {
	s, f := newTableMetaServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "mira", true)
	putSheet(t, s, *f, f.pcID, wizardSheetJSON)

	// One public and one secret roll, directly through the store — the
	// endpoint under test is the fold, not the roller.
	ctx := context.Background()
	for _, in := range []dice.Input{
		{Formula: "1d20+5", ContextKind: "attack", CharacterID: f.pcID, Actor: "keeper"},
		{Formula: "1d20+7", ContextKind: "check", CharacterID: f.pcID, Actor: "keeper", Visibility: dice.VisibilitySecret},
	} {
		if _, err := s.dice.Roll(ctx, f.campaignID, in); err != nil {
			t.Fatalf("roll: %v", err)
		}
	}

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/stats", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("dm stats: status %d, body %s", rec.Code, rec.Body)
	}
	var dmBody struct {
		Stats struct {
			Rolls struct {
				Total int `json:"total"`
			} `json:"rolls"`
		} `json:"stats"`
		Markdown string `json:"markdown"`
		Scope    struct {
			DM bool `json:"dm"`
		} `json:"scope"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dmBody); err != nil {
		t.Fatalf("decode dm: %v", err)
	}
	if dmBody.Stats.Rolls.Total != 2 || !dmBody.Scope.DM || dmBody.Markdown == "" {
		t.Fatalf("dm fold: %+v markdown %q", dmBody.Stats.Rolls, dmBody.Markdown)
	}

	// The player's fold runs the feed's queries: the secret roll is
	// absent — the leak line, one more surface.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/stats", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player stats: status %d, body %s", rec.Code, rec.Body)
	}
	var playerBody struct {
		Stats struct {
			Rolls struct {
				Total int `json:"total"`
			} `json:"rolls"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &playerBody); err != nil {
		t.Fatalf("decode player: %v", err)
	}
	if playerBody.Stats.Rolls.Total != 1 {
		t.Fatalf("player fold saw the secret: %d rolls", playerBody.Stats.Rolls.Total)
	}

	// A foreign session is not this campaign's to fold.
	otherServer, otherFixture := newTableMetaServer(t)
	ses, err := otherServer.sessions.CreateSession(ctx, otherFixture.campaignID, "Another Table")
	if err != nil {
		t.Fatalf("foreign session: %v", err)
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/stats?session="+ses.ID, "", dm)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign session: status %d, body %s", rec.Code, rec.Body)
	}
}
