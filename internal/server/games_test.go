package server

// The Magic table's handler tests (MAD-326): the lifecycle over HTTP,
// the account scope (another owner's game answers like a missing one),
// the rejected-action contract (400, zero rows), rewind as
// truncate-and-refold, and the ordinal stream — live push, the
// reconnect-from-ordinal replay, the rewind control frame, and the
// acceptance fold: a client folding what the stream delivered holds the
// same state the server does. The board stream's harness shape
// (TestBoardStreamPushesLiveAndLeaksNothing) is the pattern throughout.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// newGamesServer wires the engine the way runServe does: one store on
// the shared handle, one server, one keeper.
func newGamesServer(t *testing.T) (*Server, *engine.Store) {
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
	games, err := engine.New(store.DB())
	if err != nil {
		t.Fatalf("open engine store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithGames(games)
	return s, games
}

// gameFriend registers a second account through the invite flow and
// returns its session cookie — the scoping tests' other owner.
func gameFriend(t *testing.T, s *Server, admin *http.Cookie) *http.Cookie {
	t.Helper()
	inv := createInvite(t, s, admin, "")
	code, _ := inv["code"].(string)
	rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("friend", "a-fine-passphrase", code))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register friend: status %d, body %s", rec.Code, rec.Body)
	}
	return sessionFrom(t, rec)
}

// gameCreate makes a game through the surface and returns its id.
func gameCreate(t *testing.T, s *Server, cookie *http.Cookie) string {
	t.Helper()
	rec := hit(t, s, http.MethodPost, "/api/games", `{"name":"Friday Commander"}`, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create game: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Game struct {
			ID string `json:"id"`
		} `json:"game"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("create body: %v (%s)", err, rec.Body)
	}
	return body.Game.ID
}

// gameAction submits an action and asserts it landed.
func gameAction(t *testing.T, s *Server, cookie *http.Cookie, game, body string) []engine.Event {
	t.Helper()
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/actions", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("action %s: status %d, body %s", body, rec.Code, rec.Body)
	}
	var resp struct {
		Events []engine.Event `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("action body: %v (%s)", err, rec.Body)
	}
	return resp.Events
}

// gameEvents reads the whole log window through the surface.
func gameEvents(t *testing.T, s *Server, cookie *http.Cookie, game string) ([]engine.Event, int64) {
	t.Helper()
	rec := hit(t, s, http.MethodGet, "/api/games/"+game+"/events", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("events: status %d, body %s", rec.Code, rec.Body)
	}
	var resp struct {
		Events []engine.Event `json:"events"`
		Latest int64          `json:"latest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("events body: %v (%s)", err, rec.Body)
	}
	return resp.Events, resp.Latest
}

// gameState reads one game's folded state through the surface.
func gameState(t *testing.T, s *Server, cookie *http.Cookie, game string) *engine.State {
	t.Helper()
	rec := hit(t, s, http.MethodGet, "/api/games/"+game, "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("get game: status %d, body %s", rec.Code, rec.Body)
	}
	var resp struct {
		State *engine.State `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("get body: %v (%s)", err, rec.Body)
	}
	return resp.State
}

// gameDrive seats two players, starts, and walks seat 1 into its
// precombat main: the standing start every log test plays from.
func gameDrive(t *testing.T, s *Server, cookie *http.Cookie) string {
	t.Helper()
	game := gameCreate(t, s, cookie)
	for i, seat := range []string{`{"position":1,"name":"Collin"}`, `{"position":2,"name":"Bob"}`} {
		rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", seat, cookie)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seat %d: status %d, body %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/start", `{}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body)
	}
	gameAction(t, s, cookie, game, `{"kind":"ADVANCE","seat":1}`)
	gameAction(t, s, cookie, game, `{"kind":"ADVANCE","seat":1}`)
	return game
}

func TestGameLifecycleOverHTTP(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)

	// The writer accepts the table's common vocabulary: draws with spoken
	// identities, land drops, casts, sourced life changes.
	gameAction(t, s, admin, game, `{"kind":"DRAW","seat":1,"count":2,"cards":["Forest","Cultivate"]}`)
	gameAction(t, s, admin, game, `{"kind":"PLAY_LAND","seat":1,"card":"Forest"}`)
	gameAction(t, s, admin, game, `{"kind":"CAST","seat":1,"card":"Cultivate"}`)
	gameAction(t, s, admin, game, `{"kind":"CHANGE_LIFE","seat":2,"target_seat":2,"delta":-3,"source_card":"Lightning Bolt"}`)

	evs, latest := gameEvents(t, s, admin, game)
	if latest != int64(len(evs)) {
		t.Fatalf("latest %d, log length %d — ordinals must be contiguous from 1", latest, len(evs))
	}
	for i, e := range evs {
		if e.Ord != int64(i+1) {
			t.Fatalf("ord %d at position %d", e.Ord, i+1)
		}
	}
	st := gameState(t, s, admin, game)
	if st.Status != engine.StatusActive {
		t.Fatalf("status = %s", st.Status)
	}
	if p := st.Seats[2]; p.Life != 37 {
		t.Fatalf("seat 2 life = %d, want 37 (sourced change folded)", p.Life)
	}
	if len(st.Battlefield(1)) != 1 {
		t.Fatalf("seat 1 battlefield = %d objects, want the Forest", len(st.Battlefield(1)))
	}

	// The acceptance fold: a client folding the log the wire carried
	// holds exactly the state the server holds.
	folded := engine.Fold(evs)
	a, b := mustJSON(t, folded), mustJSON(t, st)
	if !bytes.Equal(a, b) {
		t.Fatalf("client fold != server state:\n%s\n%s", a, b)
	}

	// Setup writes are closed once the log begins.
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", `{"position":3,"name":"Late"}`, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("seating after start: status %d, body %s", rec.Code, rec.Body)
	}
}

// TestGameSetupSeatsRead covers the play surface's setup read (MAD-327):
// while the game is in setup, GET carries the seat rows so a reloaded
// client can rebuild an unfinished table; once the log begins the fold's
// GAME_STARTED echo is the seating and the field is absent.
func TestGameSetupSeatsRead(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)

	game := gameCreate(t, s, admin)
	for _, seat := range []string{
		`{"position":1,"name":"Collin","commander":"Atraxa, Praetors' Voice"}`,
		`{"position":2,"name":"Bob","commander":"Krenko, Mob Boss"}`,
	} {
		rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", seat, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seat: status %d, body %s", rec.Code, rec.Body)
		}
	}

	readSeats := func() *[]engine.SeatConfig {
		t.Helper()
		rec := hit(t, s, http.MethodGet, "/api/games/"+game, "", admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("get game: status %d, body %s", rec.Code, rec.Body)
		}
		var body struct {
			Game struct {
				Seats *[]engine.SeatConfig `json:"seats"`
			} `json:"game"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("get body: %v (%s)", err, rec.Body)
		}
		return body.Game.Seats
	}

	seats := readSeats()
	if seats == nil || len(*seats) != 2 {
		t.Fatalf("setup seats = %v, want the two seated players", seats)
	}
	if sc := (*seats)[0]; sc.Name != "Collin" || sc.Seat != 1 || sc.Commander != "Atraxa, Praetors' Voice" {
		t.Fatalf("first seat = %+v", sc)
	}

	// The seat response carries the same read — seating answers with the
	// table it just changed.
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", `{"position":3,"name":"Alice"}`, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("third seat: status %d, body %s", rec.Code, rec.Body)
	}
	var seated struct {
		Game struct {
			Seats *[]engine.SeatConfig `json:"seats"`
		} `json:"game"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &seated); err != nil {
		t.Fatalf("seat body: %v (%s)", err, rec.Body)
	}
	if seated.Game.Seats == nil || len(*seated.Game.Seats) != 3 {
		t.Fatalf("seat response carries %v, want three seats", seated.Game.Seats)
	}

	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/start", `{}`, admin); rec.Code != http.StatusOK {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body)
	}
	if seats = readSeats(); seats != nil {
		t.Fatalf("active game still carries setup seats: %v", *seats)
	}
	st := gameState(t, s, admin, game)
	if len(st.Order) != 3 {
		t.Fatalf("folded order = %v, want the three seats GAME_STARTED echoed", st.Order)
	}
}

// TestGameFullCommanderGameByTap drives a three-seat Commander game
// through the exact action sequence the play surface's taps emit
// (MAD-327's acceptance): setup, turns, priority rotation, the stack,
// the commander's one-tap cast with its declared base, tokens, counters,
// combat with blockers, and the correction paths — undo and amend over
// the same surface.
func TestGameFullCommanderGameByTap(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)

	game := gameCreate(t, s, admin)
	for _, seat := range []string{
		`{"position":1,"name":"Collin","commander":"Atraxa, Praetors' Voice"}`,
		`{"position":2,"name":"Bob","commander":"Krenko, Mob Boss"}`,
		`{"position":3,"name":"Alice","commander":"The Ur-Dragon"}`,
	} {
		rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", seat, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seat: status %d, body %s", rec.Code, rec.Body)
		}
	}
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/start", `{}`, admin); rec.Code != http.StatusOK {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body)
	}

	st := gameState(t, s, admin, game)
	if st.Status != engine.StatusActive || st.TurnSeat != 1 || st.Step != "untap" {
		t.Fatalf("post-start position: %s T%d seat%d %s", st.Status, st.Turn, st.TurnSeat, st.Step)
	}

	// T1 Collin: untap grants no priority; two advances reach precombat
	// main (the first turn's draw is skipped), then a land, a dork to
	// block with later, and a Sol Ring on the stack.
	gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":1,"source":"tap"}`)
	gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":1,"source":"tap"}`)
	st = gameState(t, s, admin, game)
	if st.Step != "main" || st.Phase != "precombat_main" || st.PrioritySeat != 1 {
		t.Fatalf("T1 main: %s/%s priority %d", st.Phase, st.Step, st.PrioritySeat)
	}
	gameAction(t, s, admin, game, `{"kind":"DRAW","seat":1,"count":7,"source":"tap"}`) // the opening hand
	gameAction(t, s, admin, game, `{"kind":"PLAY_LAND","seat":1,"card":"Forest","base":{"name":"Forest","types":["Land"]},"source":"tap"}`)
	gameAction(t, s, admin, game, `{"kind":"CAST","seat":1,"card":"Sakura-Tribe Elder","from_zone":"hand","base":{"name":"Sakura-Tribe Elder","types":["Creature","Human","Shaman"],"power":1,"toughness":1},"source":"tap"}`)
	for range 3 {
		st = gameState(t, s, admin, game)
		gameAction(t, s, admin, game, fmt.Sprintf(`{"kind":"PASS_PRIORITY","seat":%d,"source":"tap"}`, st.PrioritySeat))
	}
	gameAction(t, s, admin, game, `{"kind":"CAST","seat":1,"card":"Sol Ring","from_zone":"hand","base":{"name":"Sol Ring","types":["Artifact"]},"source":"tap"}`)
	st = gameState(t, s, admin, game)
	if len(st.Stack) != 1 || st.Stack[0].Card != "Sol Ring" {
		t.Fatalf("stack after cast: %+v", st.Stack)
	}

	// The tracker's resolve button: passes in rotation until the top
	// resolves. Three seats, three passes, priority returning to the
	// active player after the resolution.
	for range 3 {
		gameAction(t, s, admin, game, fmt.Sprintf(`{"kind":"PASS_PRIORITY","seat":%d,"source":"tap"}`, st.PrioritySeat))
		st = gameState(t, s, admin, game)
	}
	if len(st.Stack) != 0 {
		t.Fatalf("stack after rotation: %+v", st.Stack)
	}
	if n := len(st.Battlefield(1)); n != 3 {
		t.Fatalf("battlefield after resolve = %d, want Forest + Elder + Sol Ring", n)
	}
	elder := int64(0)
	for _, o := range st.Battlefield(1) {
		if o.Identity.Card == "Sakura-Tribe Elder" {
			elder = o.ID
		}
	}
	if elder == 0 {
		t.Fatal("the Elder did not resolve to the battlefield")
	}

	// T2 Bob: the commander's one-tap cast from the command zone, with
	// the declared base the fold adopts.
	for {
		st = gameState(t, s, admin, game)
		if st.Turn == 2 && st.Step == "untap" {
			break
		}
		gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":1,"source":"tap"}`)
	}
	for range 3 { // upkeep, draw, precombat main
		gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":2,"source":"tap"}`)
	}
	gameAction(t, s, admin, game, `{"kind":"DRAW","seat":2,"count":7,"source":"tap"}`) // the opening hand
	gameAction(t, s, admin, game, `{"kind":"DRAW","seat":2,"count":1,"source":"tap"}`) // the turn's draw
	gameAction(t, s, admin, game, `{"kind":"PLAY_LAND","seat":2,"card":"Mountain","base":{"name":"Mountain","types":["Land"]},"source":"tap"}`)
	gameAction(t, s, admin, game, `{"kind":"CAST","seat":2,"card":"Krenko, Mob Boss","from_zone":"command","base":{"name":"Krenko, Mob Boss","types":["Creature","Goblin","Rogue"],"power":3,"toughness":3},"source":"tap"}`)
	for range 3 {
		st = gameState(t, s, admin, game)
		gameAction(t, s, admin, game, fmt.Sprintf(`{"kind":"PASS_PRIORITY","seat":%d,"source":"tap"}`, st.PrioritySeat))
	}
	st = gameState(t, s, admin, game)
	if tax := st.CommanderTax("Krenko, Mob Boss"); tax != 2 {
		t.Fatalf("commander tax after one cast = %d, want 2", tax)
	}
	var krenko int64
	for _, o := range st.Battlefield(2) {
		if o.Identity.Card == "Krenko, Mob Boss" {
			krenko = o.ID
		}
	}
	if krenko == 0 {
		t.Fatal("the commander did not resolve to the battlefield")
	}
	if c := st.Characteristics(krenko); c.Power == nil || *c.Power != 3 {
		t.Fatalf("commander computed power = %v, want the declared 3 (base adopted on cast)", c.Power)
	}

	// Tokens (the composer's Make), a counter, tap and untap — the board
	// values, all through the same action path.
	gameAction(t, s, admin, game, `{"kind":"CREATE_TOKEN","seat":2,"count":2,"token":{"name":"Goblin","types":["Creature","Goblin"],"power":1,"toughness":1},"source":"tap"}`)
	gameAction(t, s, admin, game, `{"kind":"ADJUST_COUNTERS","seat":2,"target_seat":1,"counter_name":"poison","delta":1,"source":"tap"}`)
	gameAction(t, s, admin, game, `{"kind":"TAP","seat":2,"all":true,"source":"tap"}`)
	gameAction(t, s, admin, game, `{"kind":"UNTAP","seat":2,"all":true,"source":"tap"}`)
	st = gameState(t, s, admin, game)
	if n := len(st.Battlefield(2)); n != 4 {
		t.Fatalf("battlefield after tokens = %d, want commander + Mountain + 2 Goblins", n)
	}
	if st.Seats[1].Counters["poison"] != 1 {
		t.Fatalf("poison on seat 1 = %v", st.Seats[1].Counters)
	}

	// Combat: Bob swings the commander and a Goblin at Collin; Collin
	// blocks the Goblin with the Elder; damage resolves.
	for {
		st = gameState(t, s, admin, game)
		if st.Phase == "combat" && st.Step == "declare_attackers" {
			break
		}
		gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":2,"source":"tap"}`)
	}
	var goblin int64
	for _, o := range st.Battlefield(2) {
		if o.Identity.Token != nil {
			goblin = o.ID
		}
	}
	if goblin == 0 {
		t.Fatal("no token on the battlefield to attack with")
	}
	gameAction(t, s, admin, game, fmt.Sprintf(`{"kind":"DECLARE_ATTACKERS","seat":2,"source":"tap","attackers":[{"object":%d,"target_seat":1},{"object":%d,"target_seat":1}]}`, krenko, goblin))
	gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":2,"source":"tap"}`) // → declare_blockers
	st = gameState(t, s, admin, game)
	if st.Step != "declare_blockers" {
		t.Fatalf("at %s, want declare_blockers", st.Step)
	}
	gameAction(t, s, admin, game, fmt.Sprintf(`{"kind":"DECLARE_BLOCKERS","seat":1,"source":"tap","blockers":[{"blocker":%d,"attackers":[%d]}]}`, elder, goblin))
	gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":2,"source":"tap"}`) // → combat_damage
	gameAction(t, s, admin, game, `{"kind":"RESOLVE_COMBAT","seat":2,"source":"tap"}`)
	st = gameState(t, s, admin, game)
	if !st.CombatResolved {
		t.Fatal("combat did not resolve")
	}
	if st.Seats[1].Life != 37 {
		t.Fatalf("seat 1 life after 3 commander damage = %d, want 37", st.Seats[1].Life)
	}
	if st.Seats[1].CommanderDamage["Krenko, Mob Boss"] != 3 {
		t.Fatalf("commander damage = %v, want 3", st.Seats[1].CommanderDamage)
	}

	// Correction: undo the resolve (rewind to the blockers declaration —
	// COMBAT_RESOLVED anchors the end of its batch, the damage rows
	// precede it), then a manual life edit through the same action path —
	// the easy-adjust contract.
	evs, latest := gameEvents(t, s, admin, game)
	var blockersAt int64
	for _, e := range evs {
		if e.Kind == engine.EventBlockersDeclared {
			blockersAt = e.Ord
		}
	}
	if blockersAt == 0 {
		t.Fatal("no BLOCKERS_DECLARED row to rewind to")
	}
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/rewind",
		fmt.Sprintf(`{"to":%d}`, blockersAt), admin); rec.Code != http.StatusOK {
		t.Fatalf("undo rewind: status %d, body %s", rec.Code, rec.Body)
	}
	st = gameState(t, s, admin, game)
	if st.Seats[1].Life != 40 || st.CombatResolved {
		t.Fatalf("after undo: life %d combatResolved %v, want the pre-combat-damage position", st.Seats[1].Life, st.CombatResolved)
	}
	_, after := gameEvents(t, s, admin, game)
	if after >= latest {
		t.Fatalf("log did not shrink on rewind: %d → %d", latest, after)
	}
	gameAction(t, s, admin, game, `{"kind":"CHANGE_LIFE","seat":1,"target_seat":1,"delta":-2,"source_card":"Thoughtseize","source":"tap"}`)
	st = gameState(t, s, admin, game)
	if st.Seats[1].Life != 38 {
		t.Fatalf("manual life edit = %d, want 38", st.Seats[1].Life)
	}
}

func TestGameScopingAndAvailability(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)

	friend := gameFriend(t, s, admin)
	if rec := hit(t, s, http.MethodGet, "/api/games", "", friend); rec.Code != http.StatusOK {
		t.Fatalf("friend list: status %d", rec.Code)
	} else {
		var body struct {
			Games []any `json:"games"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if len(body.Games) != 0 {
			t.Fatalf("a new account saw someone else's games: %s", rec.Body)
		}
	}
	for _, target := range []string{
		"/api/games/" + game,
		"/api/games/" + game + "/events",
		"/api/games/" + game + "/stream",
	} {
		if rec := hit(t, s, http.MethodGet, target, "", friend); rec.Code != http.StatusNotFound {
			t.Fatalf("friend %s: status %d, want 404 (not-yours is not-found)", target, rec.Code)
		}
	}
	for _, target := range []string{
		"/api/games/" + game + "/seats",
		"/api/games/" + game + "/actions",
		"/api/games/" + game + "/rewind",
		"/api/games/" + game + "/amend",
	} {
		if rec := hit(t, s, http.MethodPost, target, `{}`, friend); rec.Code != http.StatusNotFound {
			t.Fatalf("friend %s: status %d, want 404", target, rec.Code)
		}
	}

	// An install without the engine answers 503, never panics.
	bare, _ := newGamesServer(t)
	bare = withoutGames(bare)
	keeper := adminSession(t, bare)
	if rec := hit(t, bare, http.MethodGet, "/api/games", "", keeper); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired list: status %d, want 503", rec.Code)
	}
	if rec := hit(t, bare, http.MethodGet, "/api/games/"+game+"/stream", "", keeper); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired stream: status %d, want 503", rec.Code)
	}
}

// withoutGames rewires a server with no engine store, the 503 posture.
func withoutGames(s *Server) *Server {
	s.games = nil
	return s
}

func TestGameRejectedActionWritesNothing(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)
	_, latest := gameEvents(t, s, admin, game)

	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/actions", `{"kind":"DRAW","seat":9}`, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rejected action: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost, "/api/games/"+game+"/actions", `{"seat":1}`, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("kindless action: status %d, body %s", rec.Code, rec.Body)
	}
	evs, after := gameEvents(t, s, admin, game)
	if after != latest || len(evs) != int(latest) {
		t.Fatalf("a rejected action wrote rows: latest %d → %d", latest, after)
	}
}

func TestGameRewindOverHTTP(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)
	gameAction(t, s, admin, game, `{"kind":"CHANGE_LIFE","seat":2,"target_seat":2,"delta":-3}`)

	var resp struct {
		Head  int64         `json:"head"`
		State *engine.State `json:"state"`
	}
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/rewind", `{"to":3}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("rewind: status %d, body %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("rewind body: %v", err)
	}
	if resp.Head != 3 || resp.State.LastOrd != 3 {
		t.Fatalf("rewind head = %d, state ord = %d, want 3", resp.Head, resp.State.LastOrd)
	}
	if p := resp.State.Seats[2]; p.Life != 40 {
		t.Fatalf("seat 2 life after rewind = %d, want 40 restored", p.Life)
	}
	evs, after := gameEvents(t, s, admin, game)
	if after != 3 || len(evs) != 3 {
		t.Fatalf("log after rewind: %d rows, latest %d — want 3", len(evs), after)
	}
	// The truncate still plays: new events append past the new head with
	// no gap and no duplicate.
	newEvs := gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":1}`)
	if len(newEvs) == 0 || newEvs[0].Ord != 4 {
		t.Fatalf("post-rewind append = %v, want ord 4", ordsOf(newEvs))
	}
	// Past-the-head and negative ordinals are rejected, not clamped.
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/rewind", `{"to":99}`, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("rewind past head: status %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/rewind", `{"to":-1}`, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative rewind: status %d", rec.Code)
	}
}

// TestGameAmendOverHTTP is the misidentified-card contract (MAD-328):
// amending the cast's log entry with the right card — one request, the
// log telling both halves — and a rejected correction leaving the log
// exactly as it was.
func TestGameAmendOverHTTP(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)
	miscast := gameAction(t, s, admin, game, `{"kind":"CAST","seat":1,"card":"Rhystic Study","from_zone":"hand"}`)
	gameAction(t, s, admin, game, `{"kind":"CHANGE_LIFE","seat":2,"target_seat":2,"delta":-3,"source_card":"Lightning Bolt"}`)
	before, latestBefore := gameEvents(t, s, admin, game)

	amendBody := fmt.Sprintf(`{"at":%d,"action":{"kind":"CAST","seat":1,"card":"Smothering Tithe","from_zone":"hand","source":"tap"}}`, miscast[0].Ord)
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/amend", amendBody, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("amend: status %d, body %s", rec.Code, rec.Body)
	}
	var resp struct {
		Events []engine.Event `json:"events"`
		State  *engine.State  `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("amend body: %v", err)
	}
	evs, latest := gameEvents(t, s, admin, game)
	// The correction lands where the amended batch began; ordinals stay
	// contiguous from 1; the later life change is gone with the batch.
	if resp.Events[0].Ord != miscast[0].Ord {
		t.Fatalf("amended rows start at %d, want %d", resp.Events[0].Ord, miscast[0].Ord)
	}
	for i, e := range evs {
		if e.Ord != int64(i+1) {
			t.Fatalf("ord %d at position %d — amend must keep ordinals contiguous", e.Ord, i)
		}
	}
	if p := resp.State.Seats[2]; p.Life != 40 {
		t.Fatalf("seat 2 life = %d, want 40 — the change after the amended batch must not survive", p.Life)
	}
	if len(stateStackCards(resp.State)) != 1 || stateStackCards(resp.State)[0] != "Smothering Tithe" {
		t.Fatalf("stack = %v, want the corrected card", stateStackCards(resp.State))
	}
	if latest == latestBefore || len(evs) >= len(before) {
		t.Fatalf("log did not shrink and regrow around the amend: %d → %d rows", len(before), len(evs))
	}

	// A rejected correction writes nothing — the log is byte-identical.
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/amend",
		`{"at":1,"action":{"kind":"DRAW","seat":9}}`, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("rejected amend: status %d", rec.Code)
	}
	afterReject, latestAfterReject := gameEvents(t, s, admin, game)
	if latestAfterReject != latest || len(afterReject) != len(evs) {
		t.Fatalf("a rejected amend changed the log: %d → %d rows", len(evs), len(afterReject))
	}
	for i := range evs {
		if evs[i].ID != afterReject[i].ID {
			t.Fatal("a rejected amend rewrote rows")
		}
	}

	// Malformed requests answer 400 and touch nothing.
	for _, body := range []string{
		`{"action":{"kind":"ADVANCE","seat":1}}`, // no ordinal
		`{"at":2}`,                               // no action
		`{"at":2,"action":{}}`,                   // no kind
		`{"at":999,"action":{"kind":"ADVANCE","seat":1}}`, // past the head
	} {
		if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/amend", body, admin); rec.Code != http.StatusBadRequest {
			t.Fatalf("amend %s: status %d, want 400", body, rec.Code)
		}
	}

	// Another account's game answers like a missing one.
	friend := gameFriend(t, s, admin)
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/amend", amendBody, friend); rec.Code != http.StatusNotFound {
		t.Fatalf("friend amend: status %d, want 404", rec.Code)
	}
}

// stateStackCards spells a state's stack for assertions.
func stateStackCards(st *engine.State) []string {
	out := make([]string, 0, len(st.Stack))
	for _, it := range st.Stack {
		out = append(out, it.Card)
	}
	return out
}

// TestGameAmendAnnouncesToAttachedClients is MAD-328's multi-client
// acceptance: a client live on the stream sees the amend as a rewind
// control frame — never a half-corrected game — and everything the wire
// carries afterwards folds to exactly the state the server holds.
func TestGameAmendAnnouncesToAttachedClients(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)
	miscast := gameAction(t, s, admin, game, `{"kind":"CAST","seat":1,"card":"Rhystic Study","from_zone":"hand"}`)

	// Two clients attached: one at the head, one parked on an old cursor.
	recLive, endLive, doneLive := openGameStream(t, s, admin, "/api/games/"+game+"/stream")
	recParked, endParked, doneParked := openGameStream(t, s, admin, "/api/games/"+game+"/stream?after=2")
	time.Sleep(150 * time.Millisecond)

	amendBody := fmt.Sprintf(`{"at":%d,"action":{"kind":"CAST","seat":1,"card":"Smothering Tithe","from_zone":"hand","source":"tap"}}`, miscast[0].Ord)
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/amend", amendBody, admin); rec.Code != http.StatusOK {
		t.Fatalf("amend: status %d, body %s", rec.Code, rec.Body)
	}
	// Play continues after the correction (a manual action — the
	// corrected spell is still on the stack, and that is the point).
	tail := gameAction(t, s, admin, game, `{"kind":"CHANGE_LIFE","seat":2,"target_seat":2,"delta":-1,"source":"tap"}`)
	time.Sleep(500 * time.Millisecond)
	endLive()
	cancelAndWait(t, doneLive)
	endParked()
	cancelAndWait(t, doneParked)

	// The head client — holding the rows the amend replaced — must hear
	// the rewind, never a half-corrected game.
	if !strings.Contains(recLive.Body.String(), "event: rewind") {
		t.Fatalf("live client never learned the log was amended:\n%s", recLive.Body.String())
	}

	// The parked client, replaying from an old cursor, must receive a
	// consistent log: every row the wire carried is a row the log holds
	// — no ghost of the amended batch, and the fresh tail arrives.
	current, _ := gameEvents(t, s, admin, game)
	byID := map[string]int64{}
	for _, e := range current {
		byID[e.ID] = e.Ord
	}
	frames := sseEvents(t, recParked.Body.String())
	var fresh bool
	for _, f := range frames {
		if f.Event != "event" {
			continue
		}
		e := decodeGameEvent(t, f.Data)
		if byID[e.ID] != e.Ord {
			t.Fatalf("parked client received a row the log does not hold (ord %d) — a ghost of the amend:\n%s", e.Ord, recParked.Body.String())
		}
		if e.Ord == tail[0].Ord && e.ID == tail[0].ID {
			fresh = true
		}
	}
	if !fresh {
		t.Fatalf("parked client stalled after the amend — the fresh tail never arrived:\n%s", recParked.Body.String())
	}

	// The acceptance fold: re-reading from the stream's cue holds the
	// same state the server does.
	folded := engine.Fold(current)
	a, b := mustJSON(t, folded), mustJSON(t, gameState(t, s, admin, game))
	if !bytes.Equal(a, b) {
		t.Fatalf("post-amend fold != server state:\n%s\n%s", a, b)
	}
	if len(stateStackCards(folded)) != 1 || stateStackCards(folded)[0] != "Smothering Tithe" {
		t.Fatalf("folded stack = %v, want the corrected card", stateStackCards(folded))
	}
}

// openGameStream starts a stream request in its own goroutine and
// returns a recorder plus the cancel that ends it.
func openGameStream(t *testing.T, s *Server, cookie *http.Cookie, target string) (*httptest.ResponseRecorder, context.CancelFunc, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(ctx)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rec, req)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("game stream did not return after cancel")
		}
	})
	return rec, cancel, done
}

func TestGameStreamLivePushAndReconnect(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin) // ords 1..5: start + two advances
	_, latest := gameEvents(t, s, admin, game)
	if latest != 5 {
		t.Fatalf("standing start = %d ords, want 5", latest)
	}

	// First connection: the whole log replays in order, then live writes
	// push the moment they land.
	rec, end, done := openGameStream(t, s, admin, "/api/games/"+game+"/stream?after=0")
	time.Sleep(150 * time.Millisecond)
	live := gameAction(t, s, admin, game, `{"kind":"DRAW","seat":1,"count":1,"cards":["Forest"]}`)
	time.Sleep(500 * time.Millisecond)
	end()
	cancelAndWait(t, done)

	frames := sseEvents(t, rec.Body.String())
	if len(frames) == 0 || frames[0].Event != "open" {
		t.Fatalf("first frame = %+v, want open", frames[0])
	}
	var seen []engine.Event
	for _, f := range frames {
		if f.Event != "event" {
			continue
		}
		seen = append(seen, decodeGameEvent(t, f.Data))
	}
	if len(seen) != int(latest)+len(live) {
		t.Fatalf("first stream delivered %d events, want %d (replay + live push)", len(seen), int(latest)+len(live))
	}
	for i, e := range seen {
		if e.Ord != int64(i+1) {
			t.Fatalf("first stream ord %d at position %d", e.Ord, i)
		}
	}

	// Dropped, writes land the client cannot observe, then it reconnects
	// from the last ordinal it saw: the replay must be exactly the
	// missed rows, in order.
	last := seen[len(seen)-1].Ord
	missedLive := gameAction(t, s, admin, game, `{"kind":"PLAY_LAND","seat":1,"card":"Island"}`)
	missedLive = append(missedLive, gameAction(t, s, admin, game, `{"kind":"CHANGE_LIFE","seat":2,"target_seat":2,"delta":-3,"source_card":"Lightning Bolt"}`)...)

	rec2, end2, done2 := openGameStream(t, s, admin, "/api/games/"+game+"/stream?after="+strconv.FormatInt(last, 10))
	time.Sleep(150 * time.Millisecond)
	// One more write while reconnected: replay first, then the live tail.
	tail := gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":1}`)
	time.Sleep(500 * time.Millisecond)
	end2()
	cancelAndWait(t, done2)
	frames = sseEvents(t, rec2.Body.String())
	if len(frames) == 0 || frames[0].Event != "open" {
		t.Fatalf("reconnect first frame = %+v, want open", frames[0])
	}
	var replay []engine.Event
	for _, f := range frames {
		if f.Event != "event" {
			continue
		}
		replay = append(replay, decodeGameEvent(t, f.Data))
	}
	want := append(append([]engine.Event{}, missedLive...), tail...)
	if len(replay) != len(want) {
		t.Fatalf("reconnect replayed %d events, want the %d it missed plus the live tail", len(replay), len(want))
	}
	for i := range want {
		if replay[i].Ord != want[i].Ord || replay[i].ID != want[i].ID {
			t.Fatalf("reconnect position %d = ord %d, want ord %d — the missed rows, in order", i, replay[i].Ord, want[i].Ord)
		}
	}

	// The acceptance fold over the wire: everything the two connections
	// observed folds to the state the server holds.
	evs, _ := gameEvents(t, s, admin, game)
	all := append(append([]engine.Event{}, seen...), replay...)
	if len(all) != len(evs) {
		t.Fatalf("wire delivered %d events, log holds %d", len(all), len(evs))
	}
	folded := engine.Fold(all)
	a, b := mustJSON(t, folded), mustJSON(t, gameState(t, s, admin, game))
	if !bytes.Equal(a, b) {
		t.Fatalf("stream fold != server state:\n%s\n%s", a, b)
	}
}

func TestGameStreamAnnouncesRewind(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)

	// Attached at the present: no replay, just the live feed.
	rec, end, done := openGameStream(t, s, admin, "/api/games/"+game+"/stream")
	time.Sleep(150 * time.Millisecond)

	// The writer truncates: the stream must say so, not go quiet.
	if r := hit(t, s, http.MethodPost, "/api/games/"+game+"/rewind", `{"to":3}`, admin); r.Code != http.StatusOK {
		t.Fatalf("rewind: status %d, body %s", r.Code, r.Body)
	}
	time.Sleep(500 * time.Millisecond)
	// New rows append past the truncated head; the stream keeps feeding
	// them after the control frame.
	gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":1}`)
	time.Sleep(500 * time.Millisecond)
	end()
	cancelAndWait(t, done)

	body := rec.Body.String()
	if !strings.Contains(body, "event: rewind") {
		t.Fatalf("stream never announced the rewind:\n%s", body)
	}
	if !strings.Contains(body, `"head":3`) {
		t.Fatalf("rewind frame lacks the truncated head:\n%s", body)
	}
	frames := sseEvents(t, body)
	sawRewind, sawFresh := false, false
	for _, f := range frames {
		switch f.Event {
		case "rewind":
			sawRewind = true
		case "event":
			if e := decodeGameEvent(t, f.Data); e.Ord == 4 {
				sawFresh = true
			}
		}
	}
	if !sawRewind || !sawFresh {
		t.Fatalf("rewind announcement %v, fresh ord-4 event %v — a re-folding client would stall:\n%s", sawRewind, sawFresh, body)
	}
}

/* ---------- tiny helpers ---------- */

func decodeGameEvent(t *testing.T, data string) engine.Event {
	t.Helper()
	var wrap struct {
		Event engine.Event `json:"event"`
	}
	if err := json.Unmarshal([]byte(data), &wrap); err != nil {
		t.Fatalf("decode stream event: %v (%s)", err, data)
	}
	return wrap.Event
}

func ordsOf(evs []engine.Event) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.Ord
	}
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func cancelAndWait(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("game stream did not return")
	}
}
