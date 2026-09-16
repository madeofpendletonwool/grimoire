package server

// The multiplayer pod's leak gate over HTTP (MAD-337): the shipgate
// pattern applied to the Magic table — a real four-seat pod with
// marker decks, marker draws and marker notes, then every read surface
// and every assembled prompt scanned for a seat's hidden identities
// from a viewer not entitled to them.
//
//   - TestGamePodSurfacesLeakNothing: each seated cookie hits every
//     read surface; the response bodies, the intent fallback's prompt
//     and the rules judge's prompt are scanned with the engine's
//     hidden_zone_leak check. Zero findings, sweep proven non-vacuous.
//   - TestGamePodGateHasTeeth: the owner's full payload scanned at a
//     seat's scope fires — a missing filter cannot pass silently.
//   - TestGamePodWriteEnforcement: a participant acts only as their own
//     seat; host controls answer 404/403; notes stay seat-private even
//     from the owner; a stranger sees nothing at all.
//   - TestGamePodFourClientsFoldOneGame: the acceptance fold — four
//     seated clients and the owner fold their own streams and hold the
//     same public game.
//   - TestGameJoinByCode: the pod's front door over HTTP.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/intent"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

/* ---------- the harness ---------- */

// capturingModel is the intent fallback's model seam as a recorder: the
// exact prompt the pipeline assembled is the thing under test.
type capturingModel struct {
	mu    sync.Mutex
	turns []string
}

func (c *capturingModel) ModelName() string { return "capturing" }

func (c *capturingModel) Complete(ctx context.Context, system, user string) (intent.Completion, error) {
	c.mu.Lock()
	c.turns = append(c.turns, system+"\n"+user)
	c.mu.Unlock()
	return intent.Completion{Text: "```json\n{\"kind\":\"NONE\"}\n```", InputTokens: 10, OutputTokens: 5}, nil
}

func (c *capturingModel) turns_() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.turns...)
}

// newPodServer wires the table the way runServe does: engine, universe,
// intent with a capturing fallback model, and an llm client pointed at
// a stub endpoint that records every ask prompt and streams a harmless
// answer back.
func newPodServer(t *testing.T) (*Server, *capturingModel, *promptRecorder) {
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
	uni, err := universe.NewStore(store.DB(), nil)
	if err != nil {
		t.Fatalf("open universe store: %v", err)
	}
	model := &capturingModel{}
	pipeline, err := intent.New(store.DB(), model, games, uni)
	if err != nil {
		t.Fatalf("open intent store: %v", err)
	}
	rec := &promptRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.add(string(raw))
		if strings.Contains(string(raw), `"stream":true`) {
			w.Header().Set("content-type", "text/event-stream")
			payload, _ := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"delta": map[string]any{"type": "text_delta", "text": "the stack is empty"},
			})
			fmt.Fprintf(w, "data: %s\n\n", payload)
			return
		}
		noneAnswer := "```json\n{\"kind\":\"NONE\"}\n```"
		fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}],"usage":{"input_tokens":10,"output_tokens":5}}`, noneAnswer)
	}))
	t.Cleanup(up.Close)
	cfg := llm.Config{BaseURL: up.URL, APIKey: "test-key", Model: "test-model"}
	s, err := New(store, llm.New(cfg), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithGames(games).WithUniverse(uni).WithIntent(pipeline)
	return s, model, rec
}

// promptRecorder collects every request body the stub llm endpoint saw.
type promptRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (p *promptRecorder) add(body string) {
	p.mu.Lock()
	p.bodies = append(p.bodies, body)
	p.mu.Unlock()
}

func (p *promptRecorder) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.bodies...)
}

// podFixture is the four-seat acceptance game: the owner plus three
// seated accounts, marker decks, marker draws, marker notes.
type podFixture struct {
	s      *Server
	game   string
	owner  *http.Cookie
	seats  map[int]*http.Cookie // seat → session
	userID map[int]string       // seat → registered user id
	notes  map[int]string       // seat → marker note body
	model  *capturingModel      // the intent fallback's recorded prompts
	rec    *promptRecorder      // the llm stub's recorded request bodies
}

// podFriend registers a named account through the invite flow and
// returns its session cookie and user id (read from the row — the
// register response carries the username, not the id).
func podFriend(t *testing.T, s *Server, admin *http.Cookie, name string) (*http.Cookie, string) {
	t.Helper()
	inv := createInvite(t, s, admin, "")
	code, _ := inv["code"].(string)
	rec := call(s, http.MethodPost, "/api/auth/register", registerJSON(name, "a-fine-passphrase", code))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s: status %d, body %s", name, rec.Code, rec.Body)
	}
	var id string
	if err := s.games.DB().QueryRow(`SELECT id FROM users WHERE username = ?`, name).Scan(&id); err != nil {
		t.Fatalf("load %s id: %v", name, err)
	}
	return sessionFrom(t, rec), id
}

// seedPodDeck writes one marker deck row for the seats to attach.
func seedPodDeck(t *testing.T, s *Server, id string, names map[string]int) {
	t.Helper()
	type entry struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	var entries []entry
	for name, n := range names {
		entries = append(entries, entry{Name: name, Count: n})
	}
	cards, _ := json.Marshal(entries)
	// A deck owned by whoever runs the server's admin session; the
	// engine reads cards and commander only.
	if _, err := s.games.DB().Exec(`INSERT INTO decks (id, owner_id, name, commander, cards, notes, created_at, updated_at)
		VALUES (?, 'deckless', ?, 'Pod Commander', ?, '', ?, ?)`,
		id, id, string(cards), time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed deck %s: %v", id, err)
	}
}

// buildPod drives the whole table into its started, drawn, noted state.
func buildPod(t *testing.T) *podFixture {
	t.Helper()
	s, model, rec := newPodServer(t)
	owner := adminSession(t, s)
	f := &podFixture{s: s, game: "", owner: owner, model: model, rec: rec,
		seats: map[int]*http.Cookie{}, userID: map[int]string{}, notes: map[int]string{}}
	// The owner's account id, straight from the row: seating binds a
	// real user, and the FK means it.
	var ownerID string
	if err := s.games.DB().QueryRow(`SELECT id FROM users WHERE username = 'keeper'`).Scan(&ownerID); err != nil {
		t.Fatalf("load owner id: %v", err)
	}
	f.seats[1], f.userID[1] = owner, ownerID
	for i, name := range []string{"podbob", "podalice", "poddave"} {
		seat := i + 2
		f.seats[seat], f.userID[seat] = podFriend(t, s, owner, name)
	}
	hr := hit(t, s, http.MethodPost, "/api/games", `{"name":"Pod Night"}`, owner)
	if hr.Code != http.StatusCreated {
		t.Fatalf("create game: %d %s", hr.Code, hr.Body)
	}
	var created struct {
		Game struct {
			ID string `json:"id"`
		} `json:"game"`
	}
	_ = json.Unmarshal(hr.Body.Bytes(), &created)
	f.game = created.Game.ID

	seedPodDeck(t, s, "podA", map[string]int{"Seat One Secret": 4, "Forest": 9})
	seedPodDeck(t, s, "podB", map[string]int{"Seat Two Secret": 4, "Island": 9})
	seedPodDeck(t, s, "podC", map[string]int{"Seat Three Secret": 4, "Swamp": 9})
	seedPodDeck(t, s, "podD", map[string]int{"Seat Four Secret": 4, "Mountain": 9})
	type seatRow struct {
		pos    int
		name   string
		userID string
		deck   string
	}
	for _, row := range []seatRow{
		{1, "Collin", f.userID[1], "podA"},
		{2, "Bob", f.userID[2], "podB"},
		{3, "Alice", f.userID[3], "podC"},
		{4, "Dave", f.userID[4], "podD"},
	} {
		body, _ := json.Marshal(map[string]any{
			"position": row.pos, "name": row.name, "user_id": row.userID, "deck_id": row.deck})
		rec := hit(t, s, http.MethodPost, "/api/games/"+f.game+"/seats", string(body), owner)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seat %d: %d %s", row.pos, rec.Code, rec.Body)
		}
	}
	if rec := hit(t, s, http.MethodPost, "/api/games/"+f.game+"/start", `{}`, owner); rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	// Advance out of untap so plays are legal, then seat 2 draws two
	// marker cards and seat 1 plays a public land.
	gameAction(t, s, owner, f.game, `{"kind":"ADVANCE","seat":1}`)
	gameAction(t, s, owner, f.game, `{"kind":"ADVANCE","seat":1}`)
	gameAction(t, s, f.seats[2], f.game, `{"kind":"DRAW","seat":2,"count":2,"cards":["Seat Two Secret","Island"]}`)
	gameAction(t, s, owner, f.game, `{"kind":"PLAY_LAND","seat":1,"card":"Forest"}`)
	// Marker notes, one per seat.
	for seat := 1; seat <= 4; seat++ {
		note := fmt.Sprintf("NOTE MARKER SEAT %d PRIVATE", seat)
		f.notes[seat] = note
		body, _ := json.Marshal(map[string]any{"seat": seat, "body": note})
		rec := hit(t, s, http.MethodPut, "/api/games/"+f.game+"/notes", string(body), f.seats[seat])
		if rec.Code != http.StatusOK {
			t.Fatalf("note seat %d: %d %s", seat, rec.Code, rec.Body)
		}
	}
	_ = model
	return f
}

// leakIndex builds the engine's hidden_zone_leak join from the owner's
// full log read.
func (f *podFixture) leakIndex(t *testing.T) *engine.LeakIndex {
	t.Helper()
	evs, latest := gameEvents(t, f.s, f.owner, f.game)
	if latest == 0 && len(evs) == 0 {
		t.Fatal("no events to index")
	}
	return engine.IndexLeaks(evs)
}

// assertClean scans one rendered surface for a seat viewer and fails on
// any finding, reporting the surface's name.
func assertClean(t *testing.T, x *engine.LeakIndex, seat int, surface string, rendered string) {
	t.Helper()
	findings := x.Scan(engine.SeatViewer(seat), fmt.Sprintf("seat%d", seat), rendered)
	if len(findings) > 0 {
		t.Fatalf("%s leaked at seat %d: %+v", surface, seat, findings)
	}
}

// scanNotes asserts no seat's marker note appears anywhere it should
// not: another seat's surfaces and every prompt.
func assertNoNoteMarkers(t *testing.T, seat int, surface string, rendered string, f *podFixture) {
	t.Helper()
	for other, note := range f.notes {
		if other == seat {
			continue
		}
		if strings.Contains(rendered, note) {
			t.Fatalf("%s carried seat %d's private note at seat %d", surface, other, seat)
		}
	}
}

/* ---------- the gate ---------- */

// TestGamePodSurfacesLeakNothing is the hard gate: every read surface a
// participant can reach, hit as each seated account, plus both prompt
// assemblies, scanned by the hidden_zone_leak check. Zero findings.
func TestGamePodSurfacesLeakNothing(t *testing.T) {
	f := buildPod(t)
	x := f.leakIndex(t)

	for seat := 2; seat <= 4; seat++ {
		cookie := f.seats[seat]
		get := func(path string) string {
			rec := hit(t, f.s, http.MethodGet, path, "", cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("seat %d GET %s: %d %s", seat, path, rec.Code, rec.Body)
			}
			return rec.Body.String()
		}
		surfaces := map[string]string{
			"game":      get("/api/games/" + f.game),
			"events":    get("/api/games/" + f.game + "/events?after=0"),
			"library":   get(fmt.Sprintf("/api/games/%s/library?seat=%d", f.game, seat)),
			"pending":   get("/api/games/" + f.game + "/pending"),
			"nudges":    get("/api/games/" + f.game + "/nudges"),
			"notes":     get("/api/games/" + f.game + "/notes"),
			"turns":     get("/api/games/" + f.game + "/turns/1"),
			"game-list": get("/api/games"),
		}
		for name, body := range surfaces {
			assertClean(t, x, seat, name, body)
			assertNoNoteMarkers(t, seat, name, body, f)
		}
		// The own-seat library really is the own seat's: known, exact,
		// and carrying this seat's own markers.
		if !strings.Contains(surfaces["library"], `"known":true`) {
			t.Fatalf("library read did not answer seat %d: %s", seat, surfaces["library"])
		}
		// The odds question over the own library.
		oddsBody := fmt.Sprintf(`{"seat":%d,"card":"Forest","draws":3}`, seat)
		rec := hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/odds", oddsBody, cookie)
		if rec.Code == http.StatusOK {
			assertClean(t, x, seat, "odds", rec.Body.String())
		}
		// The intent fallback's prompt: grammar refuses the utterance,
		// the model is prompted with the scoped fold. The turn captured
		// between this seat's markers is this seat's prompt, and only
		// this seat's scope may judge it — a prompt rendered for seat 2
		// carries seat 2's own cards by right.
		before := len(f.model.turns_())
		rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/intent",
			fmt.Sprintf(`{"seat":%d,"text":"is the moon made of cheese tonight"}`, seat), cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("intent: %d %s", rec.Code, rec.Body)
		}
		assertClean(t, x, seat, "intent-reply", rec.Body.String())
		assertNoNoteMarkers(t, seat, "intent-reply", rec.Body.String(), f)
		for _, turn := range f.model.turns_()[before:] {
			assertClean(t, x, seat, "intent-prompt", turn)
			assertNoNoteMarkers(t, seat, "intent-prompt", turn, f)
		}
		// The rules judge's prompt over SSE, segmented the same way.
		beforeBodies := len(f.rec.all())
		rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/ask",
			fmt.Sprintf(`{"seat":%d,"question":"what resolves next?"}`, seat), cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("ask: %d %s", rec.Code, rec.Body)
		}
		assertClean(t, x, seat, "ask-reply", rec.Body.String())
		assertNoNoteMarkers(t, seat, "ask-reply", rec.Body.String(), f)
		for _, body := range f.rec.all()[beforeBodies:] {
			assertClean(t, x, seat, "ask-prompt", body)
			assertNoNoteMarkers(t, seat, "ask-prompt", body, f)
		}
	}
	// Both prompt assemblies really fired, or the gates above were
	// vacuous.
	if turns := f.model.turns_(); len(turns) == 0 {
		t.Fatal("the fallback model was never prompted — the prompt gate is vacuous")
	}
	if bodies := f.rec.all(); len(bodies) == 0 {
		t.Fatal("the judge was never prompted — the ask gate is vacuous")
	}
}

// TestGamePodGateHasTeeth proves the sweep can fail: the owner's full
// payload — every deck, every identity — scanned at a seat's scope
// fires. A missing filter in any surface cannot pass silently, because
// the check demonstrably catches exactly that shape.
func TestGamePodGateHasTeeth(t *testing.T) {
	f := buildPod(t)
	x := f.leakIndex(t)
	rec := hit(t, f.s, http.MethodGet, "/api/games/"+f.game, "", f.owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner read: %d", rec.Code)
	}
	findings := x.Scan(engine.SeatViewer(3), "seat3", rec.Body.String())
	if len(findings) == 0 {
		t.Fatal("the owner's full payload scanned at a seat's scope did not fire — the gate has no teeth")
	}
	// The same payload is clean at the owner's scope.
	if got := x.Scan(engine.OwnerViewer(), "owner", rec.Body.String()); len(got) != 0 {
		t.Fatalf("owner payload fired at the owner's scope: %+v", got)
	}
}

// TestGamePodWriteEnforcement: a participant acts only as their own
// seat, host controls stay the host's, notes stay seat-private even
// from the owner, and a stranger sees nothing at all.
func TestGamePodWriteEnforcement(t *testing.T) {
	f := buildPod(t)
	bob := f.seats[2]

	// Bob cannot act as seat 1.
	rec := hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/actions",
		`{"kind":"CHANGE_LIFE","seat":1,"target_seat":1,"delta":-3}`, bob)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob acting as seat 1: %d %s", rec.Code, rec.Body)
	}
	// Bob acts as himself freely.
	rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/actions",
		`{"kind":"CHANGE_LIFE","seat":2,"target_seat":2,"delta":-3,"source_card":"Seat Two Secret"}`, bob)
	if rec.Code != http.StatusOK {
		t.Fatalf("bob acting as seat 2: %d %s", rec.Code, rec.Body)
	}
	// Host controls answer 404 for a participant: not-yours and
	// missing are the same answer.
	for _, path := range []string{"/rewind", "/amend", "/start", "/seats", "/settings"} {
		rec := hit(t, f.s, http.MethodPost, "/api/games/"+f.game+path, `{}`, bob)
		if rec.Code == http.StatusOK {
			t.Fatalf("bob reached host control %s", path)
		}
	}
	// Reads of another seat's zones are 403, not silent.
	rec = hit(t, f.s, http.MethodGet, "/api/games/"+f.game+"/library?seat=1", "", bob)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob reading seat 1's library: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/odds", `{"seat":1,"card":"Forest","draws":1}`, bob)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob asking odds for seat 1: %d %s", rec.Code, rec.Body)
	}
	// Bob's own note is his; seat 3's is not, and neither is the
	// owner's to read.
	rec = hit(t, f.s, http.MethodGet, "/api/games/"+f.game+"/notes", "", bob)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), f.notes[2]) {
		t.Fatalf("bob's own note: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, f.s, http.MethodGet, "/api/games/"+f.game+"/notes?seat=3", "", bob)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob reading seat 3's note: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, f.s, http.MethodGet, "/api/games/"+f.game+"/notes?seat=2", "", f.owner)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("the owner read a seated player's note: %d %s", rec.Code, rec.Body)
	}
	// A stranger — an account with no seat — gets the missing-game 404.
	stranger := gameFriend(t, f.s, f.owner)
	rec = hit(t, f.s, http.MethodGet, "/api/games/"+f.game, "", stranger)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stranger read the game: %d", rec.Code)
	}
	// The join code rides only the owner's view.
	rec = hit(t, f.s, http.MethodGet, "/api/games/"+f.game, "", f.owner)
	var ownerView struct {
		Game struct {
			JoinCode string `json:"join_code"`
		} `json:"game"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ownerView)
	if ownerView.Game.JoinCode == "" {
		t.Fatal("owner view carries no join code")
	}
	rec = hit(t, f.s, http.MethodGet, "/api/games/"+f.game, "", bob)
	if strings.Contains(rec.Body.String(), ownerView.Game.JoinCode) {
		t.Fatal("a participant's view carries the join code")
	}
}

// TestGamePodFourClientsFoldOneGame is the acceptance fold: four seated
// clients and the owner each read their own stream and fold it, and
// every one of them holds the same public game — same seats, same
// life, same hand counts, same battlefield, same turn and step.
func TestGamePodFourClientsFoldOneGame(t *testing.T) {
	f := buildPod(t)

	snapshot := func(st *engine.State) string {
		type seatSnap struct {
			Life    int          `json:"life"`
			Alive   bool         `json:"alive"`
			Hand    engine.Count `json:"hand"`
			Library engine.Count `json:"library"`
		}
		type snap struct {
			Status   string              `json:"status"`
			Turn     int                 `json:"turn"`
			TurnSeat int                 `json:"turn_seat"`
			Phase    string              `json:"phase"`
			Step     string              `json:"step"`
			Priority int                 `json:"priority_seat"`
			Objects  int                 `json:"objects"`
			Stack    int                 `json:"stack"`
			Seats    map[string]seatSnap `json:"seats"`
		}
		out := snap{Status: string(st.Status), Turn: st.Turn, TurnSeat: st.TurnSeat,
			Phase: st.Phase, Step: st.Step, Priority: st.PrioritySeat,
			Objects: len(st.Objects), Stack: len(st.Stack), Seats: map[string]seatSnap{}}
		for seat, p := range st.Seats {
			out.Seats[fmt.Sprintf("%d", seat)] = seatSnap{p.Life, p.Alive, p.Hand, p.Library}
		}
		b, _ := json.Marshal(out)
		return string(b)
	}

	viewers := map[string]*http.Cookie{"owner": f.owner,
		"seat1": f.seats[1], "seat2": f.seats[2], "seat3": f.seats[3], "seat4": f.seats[4]}
	want := ""
	for name, cookie := range viewers {
		rec := hit(t, f.s, http.MethodGet, "/api/games/"+f.game+"/events?after=0", "", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s events: %d %s", name, rec.Code, rec.Body)
		}
		var body struct {
			Events []engine.Event `json:"events"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s parse: %v", name, err)
		}
		if len(body.Events) == 0 {
			t.Fatalf("%s folded an empty stream", name)
		}
		st := engine.Fold(body.Events)
		got := snapshot(st)
		if want == "" {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("%s's public game diverged:\n%s\nvs\n%s", name, got, want)
		}
	}
}

// TestGameJoinByCode drives the pod's front door over HTTP: the owner
// shares a code, a friend redeems it and is bound to the next seat, the
// redeemed game shows up in their list, and the door closes when play
// begins.
func TestGameJoinByCode(t *testing.T) {
	s, _, _ := newPodServer(t)
	owner := adminSession(t, s)
	rec := hit(t, s, http.MethodPost, "/api/games", `{"name":"Open Table"}`, owner)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		Game struct {
			ID       string `json:"id"`
			JoinCode string `json:"join_code"`
		} `json:"game"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Game.JoinCode == "" {
		t.Fatal("create minted no join code")
	}

	friend := gameFriend(t, s, owner)
	rec = hit(t, s, http.MethodPost, "/api/games/join",
		`{"code":"`+strings.ToLower(created.Game.JoinCode)+`"}`, friend)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body)
	}
	var joined map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &joined)
	seat, _ := joined["seat"].(float64)
	isOwner, _ := joined["owner"].(bool)
	if int(seat) != 1 || isOwner {
		t.Fatalf("join → seat %v owner %v body %s", seat, isOwner, rec.Body)
	}
	// The game is now in the friend's list.
	rec = hit(t, s, http.MethodGet, "/api/games", "", friend)
	if !strings.Contains(rec.Body.String(), created.Game.ID) {
		t.Fatalf("joined game absent from friend's list: %s", rec.Body)
	}
	// A rejoin is the same seat.
	rec = hit(t, s, http.MethodPost, "/api/games/join", `{"code":"`+created.Game.JoinCode+`"}`, friend)
	var again map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if seat, _ := again["seat"].(float64); int(seat) != 1 {
		t.Fatalf("rejoin → seat %v", again["seat"])
	}
	// A bad code is a missing game.
	rec = hit(t, s, http.MethodPost, "/api/games/join", `{"code":"ZZZZZZ"}`, friend)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad code: %d %s", rec.Code, rec.Body)
	}
	// Once play begins the door closes.
	rec = hit(t, s, http.MethodPost, "/api/games/"+created.Game.ID+"/start", `{}`, owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	late := gameFriend2(t, s, owner, "latecomer")
	rec = hit(t, s, http.MethodPost, "/api/games/join", `{"code":"`+created.Game.JoinCode+`"}`, late)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("late join: %d %s", rec.Code, rec.Body)
	}
}

// gameFriend2 registers a second named friend (gameFriend's username
// is fixed).
func gameFriend2(t *testing.T, s *Server, admin *http.Cookie, name string) *http.Cookie {
	t.Helper()
	inv := createInvite(t, s, admin, "")
	code, _ := inv["code"].(string)
	rec := call(s, http.MethodPost, "/api/auth/register", registerJSON(name, "a-fine-passphrase", code))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s: %d %s", name, rec.Code, rec.Body)
	}
	return sessionFrom(t, rec)
}
