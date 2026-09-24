package server

// The replay and post-game coach surfaces over HTTP (MAD-339): the
// scrub's read is a fold to an ordinal scoped like every other read,
// and the coach streams facts on meta then interpretation on delta —
// the judge's framing, asserted the same way. Prompt scoping is the
// leak gate's to prove (TestGamePodSurfacesLeakNothing covers this
// surface too); what is proven here is the contract: the guard, the
// deterministic facts, the stream, and the honest degrade when no
// model is configured.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/table/analysis"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

func TestGameReplayOverHTTP(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)
	gameAction(t, s, admin, game, `{"kind":"DRAW","seat":1,"count":2,"cards":["Forest","Cultivate"]}`)
	gameAction(t, s, admin, game, `{"kind":"PLAY_LAND","seat":1,"card":"Forest"}`)
	evs, latest := gameEvents(t, s, admin, game)

	// No cursor reads the present; it matches GET /api/games/{id}'s fold.
	rec := hit(t, s, http.MethodGet, "/api/games/"+game+"/replay", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay head: %d %s", rec.Code, rec.Body)
	}
	var head struct {
		State *engine.State `json:"state"`
		At    int64         `json:"at"`
		Head  int64         `json:"head"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &head); err != nil {
		t.Fatalf("replay body: %v", err)
	}
	if head.At != latest || head.Head != latest || head.State.LastOrd != latest {
		t.Fatalf("head read = at %d head %d folded %d, want %d", head.At, head.Head, head.State.LastOrd, latest)
	}

	// A mid-log scrub folds exactly the prefix — the acceptance
	// property, read through the surface.
	at := latest - 2
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/replay?at="+jsonInt(at), "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay at %d: %d %s", at, rec.Code, rec.Body)
	}
	var mid struct {
		State *engine.State `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mid); err != nil {
		t.Fatalf("replay body: %v", err)
	}
	// The draw was the second-to-last action; scrubbing past it must
	// not know the hand it produced.
	drawn := false
	for _, e := range evs {
		if e.Kind == engine.EventCardDrawn && e.Ord > at {
			drawn = true
		}
	}
	if drawn && mid.State.Seats[1].Hand.N >= head.State.Seats[1].Hand.N {
		t.Fatalf("scrub at %d folded the future: hand %+v", at, mid.State.Seats[1].Hand)
	}
	if mid.State.LastOrd != at {
		t.Fatalf("scrub folded through %d, want %d", mid.State.LastOrd, at)
	}

	// Past the head clamps to the present; a bad ordinal is a 400; a
	// stranger's game is the missing-game 404.
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/replay?at=9999", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("past head: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &head); err != nil || head.At != latest {
		t.Fatalf("past head = at %d err %v", head.At, err)
	}
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/replay?at=-3", "", admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative at: %d", rec.Code)
	}
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/replay", "", gameFriend(t, s, admin))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign replay: %d", rec.Code)
	}
}

func TestGameAnalysisOverHTTP(t *testing.T) {
	s, _, rec := newPodServer(t)
	owner := adminSession(t, s)
	game := gameCreate(t, s, owner)
	hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", `{"position":1,"name":"Collin"}`, owner)
	hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", `{"position":2,"name":"Bob"}`, owner)
	hit(t, s, http.MethodPost, "/api/games/"+game+"/start", `{}`, owner)
	gameAction(t, s, owner, game, `{"kind":"ADVANCE","seat":1}`)
	gameAction(t, s, owner, game, `{"kind":"DRAW","seat":1,"count":1,"cards":["Forest"]}`)
	gameAction(t, s, owner, game, `{"kind":"PLAY_LAND","seat":1,"card":"Forest"}`)

	// A setup game has nothing to coach — the honest 400.
	fresh := gameCreate(t, s, owner)
	if r := hit(t, s, http.MethodPost, "/api/games/"+fresh+"/analysis", `{"seat":1}`, owner); r.Code != http.StatusBadRequest {
		t.Fatalf("setup analysis: %d %s", r.Code, r.Body)
	}

	before := len(rec.all())
	r := hit(t, s, http.MethodPost, "/api/games/"+game+"/analysis", `{"seat":1}`, owner)
	if r.Code != http.StatusOK {
		t.Fatalf("analysis: %d %s", r.Code, r.Body)
	}
	body := r.Body.String()
	for _, frame := range []string{"event: meta", "event: delta", "event: done"} {
		if !strings.Contains(body, frame) {
			t.Fatalf("analysis stream missing %q:\n%s", frame, body)
		}
	}
	if !strings.Contains(body, `"summary"`) {
		t.Fatalf("meta carries no summary:\n%s", body)
	}
	// The prompt is facts + digest: the deterministic half is what the
	// model was handed, not a re-derivation the surface invented.
	prompts := rec.all()[before:]
	if len(prompts) == 0 {
		t.Fatal("the coach was never prompted")
	}
	for _, want := range []string{"SEAT: Collin", "plays Forest (#", "Never invent state"} {
		if !strings.Contains(strings.Join(prompts, "\n"), want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}

func TestGameAnalysisGuardAndDegrade(t *testing.T) {
	s, _, _ := newPodServer(t)
	owner := adminSession(t, s)
	game := gameCreate(t, s, owner)
	hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", `{"position":1,"name":"Collin","user_id":"`+podOwnerID(t, s)+`"}`, owner)
	friend := gameFriend(t, s, owner)
	var friendID string
	if err := s.games.DB().QueryRow(`SELECT id FROM users WHERE username = 'friend'`).Scan(&friendID); err != nil {
		t.Fatalf("friend id: %v", err)
	}
	hit(t, s, http.MethodPost, "/api/games/"+game+"/seats",
		`{"position":2,"name":"Bob","user_id":"`+friendID+`"}`, owner)
	hit(t, s, http.MethodPost, "/api/games/"+game+"/start", `{}`, owner)

	// The owner may read any seat's report.
	if r := hit(t, s, http.MethodPost, "/api/games/"+game+"/analysis", `{"seat":2}`, owner); r.Code != http.StatusOK {
		t.Fatalf("owner as seat 2: %d %s", r.Code, r.Body)
	}
	// A seated player asks as themselves only.
	if r := hit(t, s, http.MethodPost, "/api/games/"+game+"/analysis", `{"seat":1}`, friend); r.Code != http.StatusForbidden {
		t.Fatalf("friend as seat 1: %d", r.Code)
	}
	if r := hit(t, s, http.MethodPost, "/api/games/"+game+"/analysis", `{"seat":2}`, friend); r.Code != http.StatusOK {
		t.Fatalf("friend as seat 2: %d %s", r.Code, r.Body)
	}
	// A stranger sees the missing-game 404.
	if r := hit(t, s, http.MethodPost, "/api/games/"+game+"/analysis", `{"seat":1}`, gameFriend2(t, s, owner, "stranger")); r.Code != http.StatusNotFound {
		t.Fatalf("stranger: %d", r.Code)
	}

	// An install with no model still delivers the facts, then says
	// what is missing — the same honesty the rules judge keeps.
	bare, _ := newGamesServer(t)
	bareAdmin := adminSession(t, bare)
	bareGame := gameDrive(t, bare, bareAdmin)
	r := hit(t, bare, http.MethodPost, "/api/games/"+bareGame+"/analysis", `{"seat":1}`, bareAdmin)
	if r.Code != http.StatusOK {
		t.Fatalf("unconfigured analysis: %d %s", r.Code, r.Body)
	}
	if !strings.Contains(r.Body.String(), "event: meta") || !strings.Contains(r.Body.String(), "event: error") {
		t.Fatalf("unconfigured stream = %s", r.Body)
	}
	var meta struct {
		Summary *analysis.Summary `json:"summary"`
	}
	for _, line := range strings.Split(r.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var probe struct {
			Summary *analysis.Summary `json:"summary"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &probe) == nil && probe.Summary != nil {
			meta.Summary = probe.Summary
			break
		}
	}
	if meta.Summary == nil {
		t.Fatalf("no facts arrived:\n%s", r.Body)
	}
	if meta.Summary.Seat.Name != "Collin" || meta.Summary.Game.Status != "active" {
		t.Fatalf("facts = %+v", meta.Summary)
	}
}

// podOwnerID reads the admin session's user id for seating a bound seat.
func podOwnerID(t *testing.T, s *Server) string {
	t.Helper()
	var id string
	if err := s.games.DB().QueryRow(`SELECT id FROM users WHERE username = 'keeper'`).Scan(&id); err != nil {
		t.Fatalf("owner id: %v", err)
	}
	return id
}
