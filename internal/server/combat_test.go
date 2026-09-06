package server

// The combat tracker's HTTP surface (MAD-422): permissions at the API
// layer — the tracker is the DM's screen — plus one battle driven over
// the routes themselves, and the 503 the unwired install answers.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

// newCombatServer boots the stack the combat surface needs: campaigns,
// sessions, the dice engine (initiative is rolls) and the combat store
// over a two-creature shelf.
func newCombatServer(t *testing.T) (*Server, *fixture, *combat.Store) {
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
	sessions, err := gamesession.New(store.DB())
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	roller, err := dice.New(store.DB(), campaigns, sessions)
	if err != nil {
		t.Fatalf("open dice store: %v", err)
	}
	combats, err := combat.New(store.DB(), campaigns, sessions, roller)
	if err != nil {
		t.Fatalf("open combat store: %v", err)
	}
	combats = combats.WithResolver(catalogShelf{})

	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithCombat(combats)
	f := buildFixture(t, s)
	return s, &f, combats
}

// catalogShelf is the resolver the server test resolves through: a
// wolf and a goblin, enough for a battle.
type catalogShelf struct{}

func (catalogShelf) ResolveStatblock(_ context.Context, _, _, name string) (encounter.Creature, bool) {
	switch name {
	case "Wolf":
		return encounter.Creature{
			Slug: "wolf", Name: "Wolf", CR: "1/4", XP: 50, AC: 13, HP: 11,
			Abilities: &statblock.Abilities{Str: 12, Dex: 15, Con: 12},
		}, true
	case "Goblin":
		return encounter.Creature{
			Slug: "goblin", Name: "Goblin", CR: "1/4", XP: 50, AC: 15, HP: 7,
			Abilities: &statblock.Abilities{Str: 8, Dex: 14, Con: 10},
		}, true
	}
	return encounter.Creature{}, false
}

/* ---------- permissions ---------- */

func TestCombatPermissions(t *testing.T) {
	s, f, _ := newCombatServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "thalia", true)
	// Every combat route is the DM's — a player reaches none of them,
	// read or write.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/combat"},
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/combats"},
		{http.MethodPost, "/api/campaigns/" + f.campaignID + "/combat"},
	} {
		if rec := hit(t, s, tc.method, tc.path, `{"pcs":[],"monsters":[]}`, player); rec.Code != http.StatusForbidden {
			t.Fatalf("player reached %s %s: status %d, want 403", tc.method, tc.path, rec.Code)
		}
	}

	// The DM reads the empty tracker and starts a battle.
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/combat", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("read active: status %d, body %s", rec.Code, rec.Body)
	}
	body := `{"pcs":["` + f.pcID + `"],"companions":[{"statblock":"Wolf","name":"Whiskers"}],"monsters":[{"name":"Goblin","count":2}]}`
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat", body, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body)
	}

	// A player cannot reach the battle's combatant writes either — the
	// path needs the combat id, so read it first as the DM.
	var started struct {
		Combat struct {
			ID string `json:"id"`
		} `json:"combat"`
		Order []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Side string `json:"side"`
		} `json:"order"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode started: %v", err)
	}
	cid := started.Combat.ID
	var goblinID string
	for _, c := range started.Order {
		if c.Side == "foe" {
			goblinID = c.ID
		}
	}
	if goblinID == "" {
		t.Fatalf("no foe in the order: %+v", started.Order)
	}
	if rec := hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/combats/"+cid+"/combatants/"+goblinID+"/damage",
		`{"amount":5}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player dealt damage: status %d, want 403", rec.Code)
	}
}

func TestCombatUnavailableWithoutWiring(t *testing.T) {
	s, f, _ := newCombatServer(t)
	dm := dmSession(t, s)
	s.combats = nil
	for _, path := range []string{
		"/api/campaigns/" + f.campaignID + "/combat",
	} {
		if rec := hit(t, s, http.MethodGet, path, "", dm); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("unwired GET %s: status %d, want 503", path, rec.Code)
		}
		if rec := hit(t, s, http.MethodPost, path, `{"pcs":[]}`, dm); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("unwired POST %s: status %d, want 503", path, rec.Code)
		}
	}
}

/* ---------- one battle over the routes ---------- */

func TestCombatBattleOverTheRoutes(t *testing.T) {
	s, f, _ := newCombatServer(t)
	dm := dmSession(t, s)

	body := `{"pcs":["` + f.pcID + `"],"companions":[{"statblock":"Wolf","name":"Whiskers"}],"monsters":[{"name":"Goblin","count":2}]}`
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat", body, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body)
	}
	var started struct {
		Combat struct {
			ID    string `json:"id"`
			Round int    `json:"round"`
		} `json:"combat"`
		Order []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Kind       string `json:"kind"`
			Side       string `json:"side"`
			MaxHP      int    `json:"max_hp"`
			AC         int    `json:"ac"`
			Initiative int    `json:"initiative"`
		} `json:"order"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode started: %v", err)
	}
	cid := started.Combat.ID
	if started.Combat.Round != 1 || len(started.Order) != 4 {
		t.Fatalf("started: %+v %+v", started.Combat, started.Order)
	}
	var pcID, wolfID, goblinID string
	for _, c := range started.Order {
		switch c.Kind {
		case "pc":
			pcID = c.ID
		case "companion":
			wolfID = c.ID
		case "monster":
			goblinID = c.ID
		}
	}
	if pcID == "" || wolfID == "" || goblinID == "" {
		t.Fatalf("the order's kinds: %+v", started.Order)
	}

	// The first turn, then a kill, then the wrap and the end.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combats/"+cid+"/next", `{}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("next: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/combats/"+cid+"/combatants/"+goblinID+"/damage",
		`{"amount":9,"damage_type":"slashing","note":"longsword"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("damage: status %d, body %s", rec.Code, rec.Body)
	}
	var hitOut struct {
		Combatant struct {
			Dead bool `json:"dead"`
			HP   int  `json:"hp"`
		} `json:"combatant"`
		Outcome struct {
			Died bool `json:"died"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hitOut); err != nil {
		t.Fatalf("decode damage: %v", err)
	}
	if !hitOut.Combatant.Dead || !hitOut.Outcome.Died {
		t.Fatalf("the goblin lived: %+v", hitOut)
	}

	// A heal and a condition refusal on the pc (pcs use the effects
	// API), then the battle's end.
	rec = hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/combats/"+cid+"/combatants/"+pcID+"/heal",
		`{"amount":3}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("heal: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/combats/"+cid+"/combatants/"+pcID+"/conditions",
		`{"name":"blinded","rounds":2}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("pc condition went local: status %d", rec.Code)
	}
	// The wolf takes a condition; the round wrap wears it.
	rec = hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/combats/"+cid+"/combatants/"+wolfID+"/conditions",
		`{"name":"poisoned","rounds":1}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("wolf condition: status %d, body %s", rec.Code, rec.Body)
	}
	for i := 0; i < 4; i++ {
		if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combats/"+cid+"/next", `{}`, dm); rec.Code != http.StatusOK {
			t.Fatalf("next %d: status %d, body %s", i, rec.Code, rec.Body)
		}
	}

	// The battle reads back whole: combat, order, journal.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/combats/"+cid, "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status %d, body %s", rec.Code, rec.Body)
	}
	var whole struct {
		Combat struct {
			Round int `json:"round"`
		} `json:"combat"`
		Log []struct {
			Kind string `json:"kind"`
		} `json:"log"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &whole); err != nil {
		t.Fatalf("decode whole: %v", err)
	}
	if whole.Combat.Round < 2 || len(whole.Log) < 8 {
		t.Fatalf("whole battle: round %d, %d log rows", whole.Combat.Round, len(whole.Log))
	}

	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combats/"+cid+"/end",
		`{"reason":"the goblins are done"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("end: status %d, body %s", rec.Code, rec.Body)
	}
	// The active read is empty again, and the list carries the battle.
	active := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/combat", "", dm)
	if active.Code != http.StatusOK {
		t.Fatalf("active after end: status %d", active.Code)
	}
	var activeOut struct {
		Combat *struct {
			ID string `json:"id"`
		} `json:"combat"`
	}
	if err := json.Unmarshal(active.Body.Bytes(), &activeOut); err != nil || activeOut.Combat != nil {
		t.Fatalf("active after end: %v %v", activeOut.Combat, err)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/combats", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("list: status %d", rec.Code)
	}

	// An unknown statblock is a 404, a second battle while one runs is
	// a 409, and an unknown combat is a 404.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat",
		`{"pcs":["`+f.pcID+`"],"monsters":[{"name":"Beholder"}]}`, dm); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown statblock: status %d, want 404", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat",
		`{"pcs":["`+f.pcID+`"]}`, dm); rec.Code != http.StatusCreated {
		t.Fatalf("second battle: status %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat",
		`{"pcs":["`+f.pcID+`"]}`, dm); rec.Code != http.StatusConflict {
		t.Fatalf("third battle while one runs: status %d, want 409", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combats/nope/next", `{}`, dm); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown combat: status %d", rec.Code)
	}
}
