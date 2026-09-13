package server

// The rules judge's handler tests (MAD-333): a question asked mid-game is
// grounded in the live folded state — board, stack, priority, step — and
// answered through the resolver's prompt with citations, over the same
// fake-LLM/fake-Scryfall seam the resolver tests use (ADR 8). Assertions
// are on the SSE response and on the prompt the model actually received.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

// newJudgeServer wires the engine, the universe and a configured LLM whose
// Messages endpoint streams the given answer, capturing the prompt it
// received. Scryfall answers from the given oracle map.
func newJudgeServer(t *testing.T, oracle map[string]string, answer string, captured *capturedRequest) (*Server, *http.Cookie) {
	t.Helper()
	scryfall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cards/named" {
			http.NotFound(w, r)
			return
		}
		name := r.URL.Query().Get("fuzzy")
		text, ok := oracle[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"name":` + jsonString(name) + `,"type_line":"Creature","oracle_text":` + jsonString(text) + `}`))
	}))
	t.Cleanup(scryfall.Close)

	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if captured != nil {
			_ = json.Unmarshal(raw, captured)
		}
		w.Header().Set("content-type", "text/event-stream")
		half := len(answer) / 2
		_, _ = w.Write([]byte("data: " + sseDelta(answer[:half]) + "\n\n"))
		_, _ = w.Write([]byte("data: " + sseDelta(answer[half:]) + "\n\n"))
	}))
	t.Cleanup(llmSrv.Close)

	store, err := index.Open(t.TempDir() + "/ask.db")
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
	s, err := New(store, llm.New(llm.Config{BaseURL: llmSrv.URL, APIKey: "k", Model: "test"}),
		cards.NewWithBase(scryfall.URL), nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithGames(games).WithUniverse(universeStore)
	return s, adminSession(t, s)
}

// doAsk posts a judge question through the surface.
func doAsk(t *testing.T, s *Server, cookie *http.Cookie, game, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/games/"+game+"/ask", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// passPriority passes as whoever holds priority, once.
func passPriority(t *testing.T, s *Server, cookie *http.Cookie, game string) {
	t.Helper()
	st := gameState(t, s, cookie, game)
	if st.PrioritySeat == 0 {
		t.Fatalf("no one holds priority; cannot pass")
	}
	gameAction(t, s, cookie, game,
		fmt.Sprintf(`{"kind":"PASS_PRIORITY","seat":%d,"source":"tap"}`, st.PrioritySeat))
}

func TestHandleGameAsk_AnswersAgainstTheLiveBoard(t *testing.T) {
	oracle := map[string]string{
		"Lightning Bolt": "Lightning Bolt deals 3 damage to any target.",
		"Atraxa, Praetors' Voice": "Flying, vigilance, deathtouch, lifelink. " +
			"At the beginning of your end step, proliferate.",
	}
	var captured capturedRequest
	answer := "1. Bob's Lightning Bolt is the top of the stack (117.4). 2. You receive priority and may respond (117.3c). Result: yes."
	s, cookie := newJudgeServer(t, oracle, answer, &captured)
	indexResolveRules(t, s)

	// The standing start, advanced to a live question: Collin resolves an
	// Atraxa, passes, and Bob casts a Lightning Bolt at it.
	game := gameDrive(t, s, cookie)
	gameAction(t, s, cookie, game, `{"kind":"CAST","seat":1,"card":"Atraxa, Praetors' Voice","from_zone":"hand",`+
		`"base":{"name":"Atraxa, Praetors' Voice","types":["Creature"],"power":4,"toughness":4},"source":"tap"}`)
	passPriority(t, s, cookie, game) // Collin passes
	passPriority(t, s, cookie, game) // Bob passes — Atraxa resolves
	passPriority(t, s, cookie, game) // Collin passes priority to Bob
	st := gameState(t, s, cookie, game)
	if st.PrioritySeat != 2 {
		t.Fatalf("priority = %d, want Bob holding it before his cast", st.PrioritySeat)
	}
	gameAction(t, s, cookie, game, `{"kind":"CAST","seat":2,"card":"Lightning Bolt","from_zone":"hand",`+
		`"base":{"name":"Lightning Bolt","types":["Instant"]},"targets":[{"object":1}],"source":"tap"}`)

	rec := doAsk(t, s, cookie, game, `{"seat":1,"question":"can I respond to this?"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body %s", rec.Code, rec.Body)
	}

	frames := sseFrames(rec.Body.String())
	want := []string{"meta", "delta", "delta", "done"}
	var got []string
	for _, f := range frames {
		got = append(got, f["__event"].(string))
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v (body %s)", got, want, rec.Body)
	}
	for i, ev := range want {
		if got[i] != ev {
			t.Errorf("frame %d = %s, want %s", i, got[i], ev)
		}
	}
	var answerText strings.Builder
	for _, f := range frames {
		if f["__event"] == "delta" {
			answerText.WriteString(f["text"].(string))
		}
	}
	if answerText.String() != answer {
		t.Errorf("concatenated answer = %q, want %q", answerText.String(), answer)
	}

	// The prompt the model received: the resolver's exchange, fed the live
	// state — the board card, the stack in resolution order, the position
	// line, the grounded oracle text, and the question itself.
	user := captured.Messages[0].Content
	for _, want := range []string{
		"Atraxa, Praetors' Voice", "casts Lightning Bolt targeting Atraxa",
		"Bob holds priority", "QUESTION: can I respond to this?",
		"Lightning Bolt deals 3 damage", // grounded oracle text
		"117.4",                         // the wholesale timing chapter
	} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt missing %q:\n%s", want, user)
		}
	}
	if !strings.Contains(captured.System, "HONESTY") {
		t.Errorf("resolver system prompt not used; system = %q", captured.System)
	}

	// meta carries the citations the answer grounded in.
	meta := frames[0]
	if cs, _ := meta["cards"].([]any); len(cs) == 0 {
		t.Errorf("meta should list resolved cards, got %v", meta)
	}
}

func TestHandleGameAsk_RequiresAQuestion(t *testing.T) {
	s, cookie := newJudgeServer(t, nil, "", nil)
	game := gameDrive(t, s, cookie)
	rec := doAsk(t, s, cookie, game, `{"seat":1,"question":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for an empty question", rec.Code)
	}
}

func TestHandleGameAsk_AnotherOwnersGameIsAMissingOne(t *testing.T) {
	s, cookie := newJudgeServer(t, nil, "", nil)
	game := gameDrive(t, s, cookie)
	friend := gameFriend(t, s, cookie)
	rec := doAsk(t, s, friend, game, `{"seat":1,"question":"what resolves next?"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404 for another owner's game", rec.Code)
	}
}

func TestHandleGameAsk_NotConfigured(t *testing.T) {
	// The games harness without an LLM: not configured says so over SSE.
	s, _ := newGamesServer(t)
	cookie := adminSession(t, s)
	game := gameDrive(t, s, cookie)
	rec := doAsk(t, s, cookie, game, `{"seat":1,"question":"what resolves next?"}`)
	frames := sseFrames(rec.Body.String())
	var sawError bool
	for _, f := range frames {
		if f["__event"] == "error" {
			sawError = true
			if msg, _ := f["error"].(string); !strings.Contains(msg, "not configured") {
				t.Errorf("error = %q, want not-configured notice", msg)
			}
		}
	}
	if !sawError {
		t.Errorf("expected an SSE error event for an unconfigured judge")
	}
}
