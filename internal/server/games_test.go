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
