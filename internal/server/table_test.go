package server

// The table screen's handler tests (MAD-425): the token lifecycle, the
// public snapshot's leak posture (asserted on the wire with no session
// at all — the token is the whole access model), the live stream under
// writes and a reconnect, and the revocation that closes it. The dice
// and board streams' harness shape (TestRollStreamPushesLive,
// TestBoardStreamPushesLiveAndLeaksNothing) is the pattern throughout.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

/* ---------- helpers ---------- */

// mintTableScreen opens a projector link as the DM and returns its token.
func mintTableScreen(t *testing.T, s *Server, campaignID string, dm *http.Cookie) string {
	t.Helper()
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+campaignID+"/table-screen", "", dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	return body.Token
}

// startedBattle is one combat's ids off the start response.
type startedBattle struct {
	CombatID string
	PC       string
	Foes     []string
}

// startBattle starts the fixture fight: the wizard, three goblins. The
// initiative rolls land in the public feed on the way.
func startBattle(t *testing.T, s *Server, f *fixture, dm *http.Cookie) startedBattle {
	t.Helper()
	body := `{"pcs":["` + f.pcID + `"],"monsters":[{"name":"Goblin","count":3}]}`
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat", body, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start combat: status %d, body %s", rec.Code, rec.Body)
	}
	var started struct {
		Combat struct {
			ID string `json:"id"`
		} `json:"combat"`
		Order []struct {
			ID   string `json:"id"`
			Side string `json:"side"`
		} `json:"order"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode started: %v", err)
	}
	out := startedBattle{CombatID: started.Combat.ID}
	for _, c := range started.Order {
		if c.Side == "foe" {
			out.Foes = append(out.Foes, c.ID)
		} else {
			out.PC = c.ID
		}
	}
	if out.CombatID == "" || out.PC == "" || len(out.Foes) != 3 {
		t.Fatalf("battle missing pieces: %+v", out)
	}
	return out
}

// revealFoe sets one foe's table-screen exposure as the DM.
func revealFoe(t *testing.T, s *Server, f *fixture, dm *http.Cookie, b startedBattle, foe int, mode string) {
	t.Helper()
	rec := hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/combats/"+b.CombatID+"/combatants/"+b.Foes[foe]+"/reveal",
		`{"mode":"`+mode+`"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("reveal %d %q: status %d, body %s", foe, mode, rec.Code, rec.Body)
	}
}

// tableBoard reads the public snapshot with no session at all — exactly
// what a projector does.
func tableBoard(t *testing.T, s *Server, token string) (map[string]any, string) {
	t.Helper()
	rec := hit(t, s, http.MethodGet, "/t/"+token+"/board", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("table board: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Table map[string]any `json:"table"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode table board: %v", err)
	}
	return body.Table, rec.Body.String()
}

/* ---------- the token lifecycle ---------- */

func TestTableScreenTokenLifecycle(t *testing.T) {
	s, f := newBoardServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "mira", true)

	// Minting is the DM's; listing is the DM's; a player gets neither.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/table-screen", "", player); rec.Code != http.StatusForbidden {
		t.Fatalf("player minted a screen: status %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/table-screen", "", player); rec.Code != http.StatusForbidden {
		t.Fatalf("player listed screens: status %d", rec.Code)
	}

	token := mintTableScreen(t, s, f.campaignID, dm)

	// The list carries the link, with the absolute URL to cast.
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/table-screen", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body %s", rec.Code, rec.Body)
	}
	var listed struct {
		Screens []struct {
			Token string `json:"token"`
			URL   string `json:"url"`
		} `json:"table_screens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Screens) != 1 || listed.Screens[0].Token != token || !strings.Contains(listed.Screens[0].URL, "/t/"+token) {
		t.Fatalf("list disagrees with the mint: %+v", listed.Screens)
	}

	// The page renders with no session: the token is the whole access
	// model.
	rec = hit(t, s, http.MethodGet, "/t/"+token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("page: status %d, body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `data-token="`+token+`"`) {
		t.Fatal("the page does not carry its token to the painter")
	}

	// Unknown and malformed tokens are the same 404.
	if rec := hit(t, s, http.MethodGet, "/t/aaaaaaaaaaaaaaaaaaaaaa/board", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token: status %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, "/t/not-a-token/board", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("malformed token: status %d", rec.Code)
	}

	// Revoking is idempotent for this campaign's tokens, and unknown
	// tokens stay 404.
	if rec := hit(t, s, http.MethodDelete, "/api/campaigns/"+f.campaignID+"/table-screen/"+token, "", dm); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := hit(t, s, http.MethodDelete, "/api/campaigns/"+f.campaignID+"/table-screen/"+token, "", dm); rec.Code != http.StatusNoContent {
		t.Fatalf("second revoke: status %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodDelete, "/api/campaigns/"+f.campaignID+"/table-screen/aaaaaaaaaaaaaaaaaaaaaa", "", dm); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown: status %d", rec.Code)
	}

	// A closed screen answers 410 on both surfaces — the link says it
	// was revoked rather than pretending it never existed.
	if rec := hit(t, s, http.MethodGet, "/t/"+token+"/board", ""); rec.Code != http.StatusGone {
		t.Fatalf("closed board: status %d, want 410", rec.Code)
	}
	rec = hit(t, s, http.MethodGet, "/t/"+token, "")
	if rec.Code != http.StatusGone {
		t.Fatalf("closed page: status %d, want 410", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "closed") {
		t.Fatal("the closed page does not say so")
	}
}

/* ---------- the leak posture ---------- */

// TestTableScreenSnapshotLeak is the issue's acceptance test: zero
// secrets reachable from the table-screen URL. The campaign runs word +
// private (the most restrictive shape), a battle runs with one foe
// revealed as numbers, one as a word, one hidden, and the dice hold a
// public and a secret roll — the projector's read, with no session at
// all, must carry exactly the public shape.
func TestTableScreenSnapshotLeak(t *testing.T) {
	s, f := newBoardServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "mira", true)

	// Word + private: nobody's numbers, nobody's slots.
	if rec := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/board/settings",
		`{"hp":"word","slots":"private"}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("settings: status %d, body %s", rec.Code, rec.Body)
	}

	b := startBattle(t, s, f, dm)
	revealFoe(t, s, f, dm, b, 0, "hp")
	revealFoe(t, s, f, dm, b, 1, "word")
	// foe 2 stays hidden — the DM's numbers alone.

	// The wizard takes a wound the room will read as a word; every foe
	// takes three so the bars move.
	if rec := hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/combats/"+b.CombatID+"/combatants/"+b.PC+"/damage",
		`{"amount":22,"note":"the trap"}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("hurt the wizard: status %d, body %s", rec.Code, rec.Body)
	}

	// A public roll the room made together, and a secret the DM made
	// alone.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"3d6+2","context":"damage","detail":"the fireball"}`, player); rec.Code != http.StatusCreated {
		t.Fatalf("public roll: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20+7","visibility":"secret","detail":"the whispered truth"}`, dm); rec.Code != http.StatusCreated {
		t.Fatalf("secret roll: status %d, body %s", rec.Code, rec.Body)
	}

	token := mintTableScreen(t, s, f.campaignID, dm)
	table, raw := tableBoard(t, s, token)

	// The party strip: word mode, observer shape — keys checked on the
	// generic map (the shape the server serialized), so a hidden field
	// that shipped as a zero still fails here.
	members, ok := table["members"].([]any)
	if !ok || len(members) != 1 {
		t.Fatalf("the screen shows %v members, want the one bound pc", table["members"])
	}
	strip := members[0].(map[string]any)
	for _, leak := range []string{"hp", "max_hp", "temp_hp", "slots", "warnings"} {
		if _, has := strip[leak]; has {
			t.Fatalf("the projector's strip for %v leaked %q — a leak", strip["name"], leak)
		}
	}
	if strip["health"] != "bloodied" {
		t.Fatalf("the wounded wizard's word = %v, want bloodied", strip["health"])
	}

	// The battle is public: round, order, the names.
	combat, ok := table["combat"].(map[string]any)
	if !ok {
		t.Fatalf("the screen shows no battle: %v", table["combat"])
	}
	if _, has := combat["round"]; !has {
		t.Fatal("the battle carries no round")
	}

	// The other side: exactly the two revealed foes, each in its mode.
	monsters, ok := table["monsters"].([]any)
	if !ok || len(monsters) != 2 {
		t.Fatalf("the screen shows %v monsters, want the two revealed", table["monsters"])
	}
	var hpFoe, wordFoe map[string]any
	for _, m := range monsters {
		mo := m.(map[string]any)
		if _, has := mo["hp"]; has {
			hpFoe = mo
		} else if mo["health"] != nil {
			wordFoe = mo
		}
	}
	if hpFoe == nil || wordFoe == nil {
		t.Fatalf("the revealed foes came in the wrong shapes: %v", monsters)
	}
	if _, has := hpFoe["health"]; has {
		t.Fatal("a numbers-revealed foe also carried a word")
	}
	if _, has := wordFoe["hp"]; has {
		t.Fatal("a word-revealed foe also carried numbers — a leak")
	}
	if hpFoe["hp"] != float64(7) || hpFoe["max_hp"] != float64(7) {
		t.Fatalf("the revealed foe's numbers = %v/%v, want 7/7", hpFoe["hp"], hpFoe["max_hp"])
	}

	// The dice: the public roll is there, the secret one is not — the
	// absence asserted on the raw wire, the way the leak tests read it.
	rolls, ok := table["rolls"].([]any)
	if !ok || len(rolls) < 1 {
		t.Fatalf("the screen shows no rolls: %v", table["rolls"])
	}
	var sawPublic bool
	for _, r := range rolls {
		roll := r.(map[string]any)
		if roll["detail"] == "the fireball" {
			sawPublic = true
		}
	}
	if !sawPublic {
		t.Fatal("the public roll never reached the projector")
	}
	for _, leak := range []string{"the whispered truth", `"visibility":"secret"`, `"visibility":"public"`} {
		if strings.Contains(raw, leak) {
			t.Fatalf("the wire carries %q — a leak:\n%s", leak, raw)
		}
	}

	// And nothing else: no dm flag, no presence.
	if strings.Contains(raw, `"dm":true`) {
		t.Fatal("the projector's read says dm")
	}
	if strings.Contains(raw, `"present":true`) {
		t.Fatal("the projector's read carries presence")
	}
}

/* ---------- the live stream ---------- */

// TestTableScreenStreamLiveReconnectGone drives the braided stream end
// to end: the open frame paints, damage re-renders the board live, a
// public roll lands and a secret one never does, a dropped connection
// re-enters through a fresh open frame, and a revocation closes the
// stream with a gone event — all with no session at all.
func TestTableScreenStreamLiveReconnectGone(t *testing.T) {
	s, f := newBoardServer(t)
	dm := dmSession(t, s)
	addPlayerMember(t, s, *f, "mira", true) // the wizard's player — the strip exists because of the binding

	if rec := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/board/settings",
		`{"hp":"word","slots":"private"}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("settings: status %d, body %s", rec.Code, rec.Body)
	}
	b := startBattle(t, s, f, dm)
	revealFoe(t, s, f, dm, b, 0, "hp")
	token := mintTableScreen(t, s, f.campaignID, dm)

	open := func() (*httptest.ResponseRecorder, context.CancelFunc, <-chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "/t/"+token+"/stream", nil)
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.Handler().ServeHTTP(rec, req)
		}()
		return rec, cancel, done
	}

	rec, cancel, done := open()
	time.Sleep(150 * time.Millisecond)

	// The DM hurts the wizard: the room's screen re-renders with the
	// word, never the number.
	if r := hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/combats/"+b.CombatID+"/combatants/"+b.PC+"/damage",
		`{"amount":22,"note":"the trap"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("hurt the wizard: status %d, body %s", r.Code, r.Body)
	}
	// And the revealed foe takes a hit: its bar moves on the wire.
	if r := hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/combats/"+b.CombatID+"/combatants/"+b.Foes[0]+"/damage",
		`{"amount":3}`, dm); r.Code != http.StatusOK {
		t.Fatalf("hurt the goblin: status %d, body %s", r.Code, r.Body)
	}
	// A public roll lands; a secret one must not.
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"3d6+2","context":"damage","detail":"the fireball"}`, dm); r.Code != http.StatusCreated {
		t.Fatalf("public roll: status %d, body %s", r.Code, r.Body)
	}
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20+7","visibility":"secret","detail":"the whispered truth"}`, dm); r.Code != http.StatusCreated {
		t.Fatalf("secret roll: status %d, body %s", r.Code, r.Body)
	}
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(rec.Header().Get("content-type"), "text/event-stream") {
		t.Fatalf("content-type %q", rec.Header().Get("content-type"))
	}
	if !strings.Contains(body, "event: open") {
		t.Fatal("the stream never opened")
	}
	if !strings.Contains(body, "event: board") {
		t.Fatal("a combat write never re-rendered the screen")
	}
	if !strings.Contains(body, "bloodied") {
		t.Fatalf("the word-mode change (10/32) never arrived as a word:\n%s", body)
	}
	if !strings.Contains(body, `"hp":4`) {
		t.Fatal("the revealed foe's bar (4/7) never arrived")
	}
	if !strings.Contains(body, "event: roll") {
		t.Fatal("the public roll never landed on the stream")
	}
	if !strings.Contains(body, "the fireball") {
		t.Fatal("the public roll's detail never landed")
	}
	for _, leak := range []string{"the whispered truth", `"max_hp":32`, `"temp_hp":`, `"slots":[`} {
		if strings.Contains(body, leak) {
			t.Fatalf("the stream leaked %q — a leak:\n%s", leak, body)
		}
	}

	// A dropped projector re-enters through a fresh open frame.
	rec2, cancel2, done2 := open()
	time.Sleep(200 * time.Millisecond)
	cancel2()
	<-done2
	if !strings.Contains(rec2.Body.String(), "event: open") {
		t.Fatal("a reconnecting screen never re-opened")
	}
	if !strings.Contains(rec2.Body.String(), "bloodied") {
		t.Fatal("the re-opened frame lost the word")
	}

	// And revocation closes the stream with a gone event, promptly. The
	// body is read only after the handler returns — a recorder is not
	// safe to read while it writes.
	rec3, cancel3, done3 := open()
	time.Sleep(150 * time.Millisecond)
	if rec := hit(t, s, http.MethodDelete, "/api/campaigns/"+f.campaignID+"/table-screen/"+token, "", dm); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status %d, body %s", rec.Code, rec.Body)
	}
	select {
	case <-done3:
	case <-time.After(3 * time.Second):
		cancel3()
		<-done3
		t.Fatal("the revoked screen never closed its stream")
	}
	cancel3()
	if !strings.Contains(rec3.Body.String(), "event: gone") {
		t.Fatalf("the closed stream never said gone:\n%s", rec3.Body)
	}
}

/* ---------- the reveal's own rules ---------- */

func TestCombatRevealPermissionsAndJournal(t *testing.T) {
	s, f := newBoardServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "mira", true)

	b := startBattle(t, s, f, dm)
	revealPath := func(ctid string) string {
		return "/api/campaigns/" + f.campaignID + "/combats/" + b.CombatID + "/combatants/" + ctid + "/reveal"
	}

	// A player cannot reveal anything.
	if rec := hit(t, s, http.MethodPost, revealPath(b.Foes[0]), `{"mode":"hp"}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player revealed a foe: status %d", rec.Code)
	}
	// Nonsense modes are errors, never guesses.
	if rec := hit(t, s, http.MethodPost, revealPath(b.Foes[0]), `{"mode":"whenever"}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode accepted: status %d", rec.Code)
	}
	// The party's numbers are not the DM's to reveal per-monster: the
	// board's visibility config governs them.
	if rec := hit(t, s, http.MethodPost, revealPath(b.PC), `{"mode":"hp"}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("revealed a pc: status %d", rec.Code)
	}

	revealFoe(t, s, f, dm, b, 0, "hp")

	// The DM's own board carries the reveal beside the numbers — the
	// toggle's read path.
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/board", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("dm board: status %d", rec.Code)
	}
	var dmBoard struct {
		Board struct {
			Monsters []struct {
				ID       string `json:"id"`
				Reveal   string `json:"reveal"`
				CombatID string `json:"combat_id"`
			} `json:"monsters"`
		} `json:"board"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dmBoard); err != nil {
		t.Fatalf("decode dm board: %v", err)
	}
	if len(dmBoard.Board.Monsters) != 3 {
		t.Fatalf("dm monsters = %d, want 3", len(dmBoard.Board.Monsters))
	}
	for _, mo := range dmBoard.Board.Monsters {
		if mo.ID == b.Foes[0] {
			if mo.Reveal != "hp" || mo.CombatID != b.CombatID {
				t.Fatalf("revealed foe's row = %+v", mo)
			}
		} else if mo.Reveal != "" {
			t.Fatalf("an unrevealed foe carries %q", mo.Reveal)
		}
	}

	// The journal records the DM's act — the story the session log
	// tells includes what the room was shown.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/combats/"+b.CombatID, "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("get combat: status %d, body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"kind":"reveal"`) {
		t.Fatal("the journal has no reveal entry")
	}
	if !strings.Contains(rec.Body.String(), "the table sees") {
		t.Fatal("the journal's reveal entry does not say what the table sees")
	}

	// Switching modes and turning it back off both land.
	revealFoe(t, s, f, dm, b, 0, "word")
	revealFoe(t, s, f, dm, b, 0, "")
	rec = hit(t, s, http.MethodGet, "/t/"+mintTableScreen(t, s, f.campaignID, dm)+"/board", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("table board: status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"monsters":[{`) {
		t.Fatal("a turned-off reveal still shows a monster")
	}
}
