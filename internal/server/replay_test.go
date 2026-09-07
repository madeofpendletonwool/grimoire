package server

// The replay's HTTP surface (MAD-426): permissions — the replay is the
// DM's screen, like the tracker it reads — plus one battle driven over
// the routes themselves, replayed through them: the journal and its
// checksum, the scrub at positions along the way, the session's
// timeline, and the 503 the unwired install answers.

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/madeofpendletonwool/grimoire/internal/replay"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

// newReplayServer boots the stack the replay surface needs: campaigns,
// sessions, the dice engine, the combat tracker, and the replay over
// their windows.
func newReplayServer(t *testing.T) (*Server, *fixture, *combat.Store, *replay.Store) {
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
	combats = combats.WithResolver(replayShelf{})
	replays, err := replay.New(combats, sessions)
	if err != nil {
		t.Fatalf("open replay store: %v", err)
	}

	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithCampaign(campaigns, sessions)
	s = s.WithCombat(combats).WithReplay(replays)
	f := buildFixture(t, s)
	return s, &f, combats, replays
}

// replayShelf is the resolver the server test resolves through: one
// goblin is enough for a battle worth replaying.
type replayShelf struct{}

func (replayShelf) ResolveStatblock(_ context.Context, _, _, name string) (encounter.Creature, bool) {
	if name == "Goblin" {
		return encounter.Creature{
			Slug: "goblin", Name: "Goblin", CR: "1/4", XP: 50, AC: 15, HP: 7,
			Abilities: &statblock.Abilities{Str: 8, Dex: 14, Con: 10},
		}, true
	}
	return encounter.Creature{}, false
}

func decode(t *testing.T, rec *recorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return m
}

func dig(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("dig %v: %T is not an object at %q", path, cur, key)
		}
		cur = obj[key]
	}
	return cur
}

// startRoutedBattle opens a session and a fight through the routes,
// hits the goblin once, and ends the battle — the journal the replay
// answers with. It returns the ids the assertions need.
func startRoutedBattle(t *testing.T, s *Server, f fixture, dm *http.Cookie) (combatID, sessionID, goblinID string) {
	t.Helper()
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/sessions", `{"name":"The Ambush"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: status %d, body %s", rec.Code, rec.Body)
	}
	sid := idFrom(t, rec, "session")

	body := `{"pcs":["` + f.pcID + `"],"monsters":[{"name":"Goblin","count":1}],"session_id":"` + sid + `"}`
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat", body, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start combat: status %d, body %s", rec.Code, rec.Body)
	}
	cid, _ := dig(t, decode(t, rec), "combat", "id").(string)
	if cid == "" {
		t.Fatalf("combat id missing: %s", rec.Body)
	}
	order, _ := dig(t, decode(t, rec), "order").([]any)
	for _, o := range order {
		entry, _ := o.(map[string]any)
		if name, _ := entry["name"].(string); name == "Goblin" {
			goblinID, _ = entry["id"].(string)
		}
	}
	if goblinID == "" {
		t.Fatalf("goblin missing from the order: %s", rec.Body)
	}

	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combats/"+cid+"/combatants/"+goblinID+"/damage",
		`{"amount":4}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("damage: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combats/"+cid+"/next", `{}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combats/"+cid+"/end", `{"reason":"done"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("end: status %d, body %s", rec.Code, rec.Body)
	}
	return cid, sid, goblinID
}

/* ---------- permissions ---------- */

func TestReplayPermissions(t *testing.T) {
	s, f, _, _ := newReplayServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "thalia", true)
	cid, sid, _ := startRoutedBattle(t, s, *f, dm)

	// Every replay route is the DM's — a player reaches none of them.
	for _, path := range []string{
		"/api/campaigns/" + f.campaignID + "/combats/" + cid + "/replay",
		"/api/campaigns/" + f.campaignID + "/combats/" + cid + "/replay/state?at=1",
		"/api/campaigns/" + f.campaignID + "/sessions/" + sid + "/replay",
	} {
		if rec := hit(t, s, http.MethodGet, path, "", player); rec.Code != http.StatusForbidden {
			t.Fatalf("player reached %s: status %d, body %s", path, rec.Code, rec.Body)
		}
	}

	// The DM reads all three.
	for _, path := range []string{
		"/api/campaigns/" + f.campaignID + "/combats/" + cid + "/replay",
		"/api/campaigns/" + f.campaignID + "/combats/" + cid + "/replay/state",
		"/api/campaigns/" + f.campaignID + "/sessions/" + sid + "/replay",
	} {
		if rec := hit(t, s, http.MethodGet, path, "", dm); rec.Code != http.StatusOK {
			t.Fatalf("DM failed %s: status %d, body %s", path, rec.Code, rec.Body)
		}
	}
}

/* ---------- the battle, replayed over the routes ---------- */

func TestReplayBattleOverTheRoutes(t *testing.T) {
	s, f, _, _ := newReplayServer(t)
	dm := dmSession(t, s)
	cid, sid, goblinID := startRoutedBattle(t, s, *f, dm)

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/combats/"+cid+"/replay", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay: status %d, body %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)

	// The checksum assertion holds over the wire.
	if matches, _ := dig(t, body, "replay", "verify", "matches").(bool); !matches {
		t.Fatalf("verify over the wire: %v", dig(t, body, "replay", "verify"))
	}
	checksum, _ := dig(t, body, "replay", "final", "checksum").(string)
	if len(checksum) != 64 {
		t.Fatalf("checksum: %q", checksum)
	}
	if status, _ := dig(t, body, "replay", "final", "status").(string); status != combat.StatusEnded {
		t.Fatalf("final status: %v", dig(t, body, "replay", "final"))
	}
	journal, _ := dig(t, body, "replay", "journal").([]any)
	if len(journal) < 4 { // start, damage, turn, end
		t.Fatalf("journal too short: %s", rec.Body)
	}

	// The scrub: before anything, the opening lineup; on the damage
	// row, the goblin is hurt exactly as the table hurt him.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/combats/"+cid+"/replay/state?at=0", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrub 0: status %d, body %s", rec.Code, rec.Body)
	}
	frame := decode(t, rec)
	if round, _ := dig(t, frame, "frame", "round").(float64); round != 1 {
		t.Fatalf("opening frame round: %v", dig(t, frame, "frame"))
	}
	for _, o := range mustList(t, dig(t, frame, "frame", "order")) {
		entry, _ := o.(map[string]any)
		if name, _ := entry["name"].(string); name == "Goblin" {
			if hp, _ := entry["hp"].(float64); hp != 7 {
				t.Fatalf("goblin's opening hp over the wire: %v", hp)
			}
		}
	}

	var damageAt float64
	for _, j := range journal {
		entry, _ := j.(map[string]any)
		if kind, _ := entry["kind"].(string); kind == "damage" {
			damageAt, _ = entry["seq"].(float64)
		}
	}
	rec = hit(t, s, http.MethodGet,
		"/api/campaigns/"+f.campaignID+"/combats/"+cid+"/replay/state?at="+fmt.Sprintf("%d", int64(damageAt)), "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrub onto the damage row: status %d, body %s", rec.Code, rec.Body)
	}
	frame = decode(t, rec)
	for _, o := range mustList(t, dig(t, frame, "frame", "order")) {
		entry, _ := o.(map[string]any)
		if id, _ := entry["id"].(string); id == goblinID {
			if hp, _ := entry["hp"].(float64); hp != 3 {
				t.Fatalf("goblin's hp on the damage row: %v, want 3", hp)
			}
		}
	}

	// The session's timeline indexes the fight, with its journal length.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/sessions/"+sid+"/replay", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline: status %d, body %s", rec.Code, rec.Body)
	}
	tl := decode(t, rec)
	fights := mustList(t, dig(t, tl, "replay", "fights"))
	if len(fights) != 1 {
		t.Fatalf("fights: %v", fights)
	}
	fight, _ := fights[0].(map[string]any)
	if id, _ := fight["combat_id"].(string); id != cid {
		t.Fatalf("fight points at %v, want %s", fight["combat_id"], cid)
	}
	if n, _ := fight["log_len"].(float64); int(n) != len(journal) {
		t.Fatalf("log_len %v, journal %d — the index and the journal disagree", fight["log_len"], len(journal))
	}

	// Scrub positions are validated, and a battle that is not there is
	// not found.
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/combats/"+cid+"/replay/state?at=nope", "", dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("a bogus scrub position answered %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/combats/no-such-battle/replay", "", dm); rec.Code != http.StatusNotFound {
		t.Fatalf("a missing battle answered %d", rec.Code)
	}
}

/* ---------- the unwired install ---------- */

func TestReplayAnswers503Unwired(t *testing.T) {
	s, f, _, _ := newReplayServer(t)
	_ = s
	// A fresh install without WithReplay: every replay route answers
	// 503 before any campaign is even resolved.
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
	bare, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if rec := hit(t, bare, http.MethodPost, "/api/auth/setup", credsJSON("keeper", "a-fine-passphrase")); rec.Code != http.StatusCreated {
		t.Fatalf("setup keeper: status %d, body %s", rec.Code, rec.Body)
	}
	dm := dmSession(t, bare)
	for _, path := range []string{
		"/api/campaigns/" + f.campaignID + "/combats/x/replay",
		"/api/campaigns/" + f.campaignID + "/combats/x/replay/state",
		"/api/campaigns/" + f.campaignID + "/sessions/y/replay",
	} {
		if rec := hit(t, bare, http.MethodGet, path, "", dm); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("unwired %s answered %d, want 503", path, rec.Code)
		}
	}
}

func mustList(t *testing.T, v any) []any {
	t.Helper()
	out, ok := v.([]any)
	if !ok {
		t.Fatalf("want a list, got %T (%v)", v, v)
	}
	return out
}
