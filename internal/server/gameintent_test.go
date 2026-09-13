package server

// The intent surface over HTTP (MAD-331): table talk through the
// pipeline, the pending tray's reads and answers, the 503-without-wiring
// contract, and the account scope. The pipeline's own behaviour has its
// e2e suite in internal/table/intent; these tests assert the HTTP half
// — what a client of the surface sees.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/intent"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

// fakeIntentModel replays scripted responses for the intent pipeline's
// model seam.
type fakeIntentModel struct {
	t         *testing.T
	responses []string
	calls     int
}

func (f *fakeIntentModel) ModelName() string { return "fake-intent" }

func (f *fakeIntentModel) Complete(ctx context.Context, system, user string) (intent.Completion, error) {
	i := f.calls
	f.calls++
	if i >= len(f.responses) {
		f.t.Fatalf("unexpected model call %d", i+1)
	}
	return intent.Completion{Text: f.responses[i]}, nil
}

// newIntentServer wires the games surface with the intent pipeline, the
// way runServe does. model may be nil.
func newIntentServer(t *testing.T, model intent.ModelClient) (*Server, *engine.Store) {
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
	universeStore, err := universe.NewStore(store.DB(), nil)
	if err != nil {
		t.Fatalf("open universe store: %v", err)
	}
	intentStore, err := intent.New(store.DB(), model, games, universeStore)
	if err != nil {
		t.Fatalf("open intent store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithGames(games).WithUniverse(universeStore).WithIntent(intentStore)
	return s, games
}

// intentGame builds a started two-seat game through the surface. Seat 1
// carries an attached deck so identification has a universe to resolve
// against — the parked-answer path caches through it.
func intentGame(t *testing.T, s *Server, games *engine.Store, cookie *http.Cookie) string {
	t.Helper()
	seedGameDeck(t, games, "intent-deck-1", "Atraxa, Praetors' Voice")
	id := gameCreate(t, s, cookie)
	rec := hit(t, s, http.MethodPost, "/api/games/"+id+"/seats",
		`{"position":1,"name":"Collin","deck_id":"intent-deck-1"}`, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seat 1: status %d body %s", rec.Code, rec.Body)
	}
	if rec := hit(t, s, http.MethodPost, "/api/games/"+id+"/seats",
		`{"position":2,"name":"Bob"}`, cookie); rec.Code != http.StatusCreated {
		t.Fatalf("seat 2: status %d body %s", rec.Code, rec.Body)
	}
	if rec := hit(t, s, http.MethodPost, "/api/games/"+id+"/start", "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("start: status %d body %s", rec.Code, rec.Body)
	}
	// Walk to the first main so casts and talk land cleanly.
	for i := 0; i < 12; i++ {
		state := gameState(t, s, cookie, id)
		if state.Status == "active" && state.PrioritySeat == 1 && state.Phase == "precombat_main" {
			return id
		}
		gameAction(t, s, cookie, id, `{"kind":"ADVANCE","seat":1}`)
	}
	t.Fatal("never reached the first main")
	return ""
}

// talk posts one utterance and asserts it answered.
func talk(t *testing.T, s *Server, cookie *http.Cookie, game, body string) *intent.Reply {
	t.Helper()
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/intent", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("intent %s: status %d body %s", body, rec.Code, rec.Body)
	}
	var resp struct {
		Reply *intent.Reply `json:"reply"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("intent body: %v (%s)", err, rec.Body)
	}
	if resp.Reply == nil {
		t.Fatal("nil reply")
	}
	return resp.Reply
}

func TestIntentGrammarAutoOverHTTP(t *testing.T) {
	s, games := newIntentServer(t, nil)
	admin := adminSession(t, s)
	game := intentGame(t, s, games, admin)
	reply := talk(t, s, admin, game, `{"seat":1,"text":"-3"}`)
	if !reply.Parsed || reply.Disposition != intent.Auto || !reply.Applied {
		t.Fatalf("reply = %+v, want auto applied", reply)
	}
	if state := gameState(t, s, admin, game); state.Seats[1].Life != 37 {
		t.Errorf("life = %d, want 37", state.Seats[1].Life)
	}
}

func TestIntentAskParksAndAnswersOverHTTP(t *testing.T) {
	s, games := newIntentServer(t, nil)
	admin := adminSession(t, s)
	game := intentGame(t, s, games, admin)
	reply := talk(t, s, admin, game, `{"seat":1,"text":"cast blorptidious"}`)
	if reply.Disposition != intent.Ask || reply.Applied {
		t.Fatalf("reply = %+v, want ask, not applied", reply)
	}
	// The tray holds the parked question.
	rec := hit(t, s, http.MethodGet, "/api/games/"+game+"/pending", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("pending: status %d", rec.Code)
	}
	var list struct {
		Pending []intent.PendingRow `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Pending) != 1 {
		t.Fatalf("pending = %s err %v, want one open row", rec.Body, err)
	}
	id := list.Pending[0].ID

	// Answer it with free text that names a card in the universe:
	// recorded, nothing applied, and the span is resolved from then on.
	ans := hit(t, s, http.MethodPost, "/api/games/"+game+"/pending/"+id,
		`{"answer":"Rhystic Study","seat":1}`, admin)
	if ans.Code != http.StatusOK {
		t.Fatalf("answer: status %d body %s", ans.Code, ans.Body)
	}
	again := talk(t, s, admin, game, `{"seat":1,"text":"cast blorptidious"}`)
	if !again.Applied || again.From != "grammar" {
		t.Fatalf("second talk = %+v, want the grammar answering from the cache", again)
	}
}

func TestIntentFallbackOverHTTP(t *testing.T) {
	model := &fakeIntentModel{t: t, responses: []string{
		"```json\n{\"kind\":\"CHANGE_LIFE\",\"target\":\"Collin\",\"delta\":-3,\"confidence\":0.9}\n```",
	}}
	s, games := newIntentServer(t, model)
	admin := adminSession(t, s)
	game := intentGame(t, s, games, admin)
	reply := talk(t, s, admin, game, `{"seat":1,"text":"so about that whole situation"}`)
	if reply.From != "llm" || !reply.Applied || reply.Disposition != intent.Confirm {
		t.Fatalf("reply = %+v, want the fallback's confirm", reply)
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1", model.calls)
	}
	if state := gameState(t, s, admin, game); state.Seats[1].Life != 37 {
		t.Errorf("life = %d, want 37", state.Seats[1].Life)
	}
}

func TestIntentVoiceSourceRidesTheAction(t *testing.T) {
	s, games := newIntentServer(t, nil)
	admin := adminSession(t, s)
	game := intentGame(t, s, games, admin)
	reply := talk(t, s, admin, game, `{"seat":1,"text":"-3","source":"voice"}`)
	a := reply.Action
	if a == nil || a.Source != "voice" {
		t.Fatalf("action = %+v, want the voice entry channel", a)
	}
}

func TestIntentUnavailableWithoutWiring(t *testing.T) {
	// The games server without WithIntent answers 503 on the new
	// endpoints — the same degradation every optional store carries.
	store, err := index.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(store.DB()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	games, err := engine.New(store.DB())
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	plain, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	plain = plain.WithGames(games)
	admin := adminSession(t, plain)
	game := intentGame(t, plain, games, admin)
	if rec := hit(t, plain, http.MethodPost, "/api/games/"+game+"/intent", `{"seat":1,"text":"-3"}`, admin); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("intent: status %d, want 503", rec.Code)
	}
	if rec := hit(t, plain, http.MethodGet, "/api/games/"+game+"/pending", "", admin); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("pending: status %d, want 503", rec.Code)
	}
}

func TestIntentScopedToOwner(t *testing.T) {
	s, games := newIntentServer(t, nil)
	admin := adminSession(t, s)
	game := intentGame(t, s, games, admin)
	friend := gameFriend(t, s, admin)
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/intent", `{"seat":1,"text":"-3"}`, friend)
	if rec.Code != http.StatusNotFound {
		t.Errorf("friend intent: status %d, want 404", rec.Code)
	}
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/pending", "", friend)
	if rec.Code != http.StatusNotFound {
		t.Errorf("friend pending: status %d, want 404", rec.Code)
	}
}

func TestRewindDismissesOpenQuestionsOverHTTP(t *testing.T) {
	s, games := newIntentServer(t, nil)
	admin := adminSession(t, s)
	game := intentGame(t, s, games, admin)
	reply := talk(t, s, admin, game, `{"seat":1,"text":"cast blorptidious"}`)
	if reply.Question == nil {
		t.Fatal("expected a parked question")
	}
	ord, err := games.LatestOrd(t.Context(), game)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/rewind", fmt.Sprintf(`{"to":%d}`, ord-1), admin); rec.Code != http.StatusOK {
		t.Fatalf("rewind: status %d body %s", rec.Code, rec.Body)
	}
	var list struct {
		Pending []intent.PendingRow `json:"pending"`
	}
	rec := hit(t, s, http.MethodGet, "/api/games/"+game+"/pending", "", admin)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Pending) != 0 {
		t.Fatalf("pending after rewind = %s err %v, want empty", rec.Body, err)
	}
}
