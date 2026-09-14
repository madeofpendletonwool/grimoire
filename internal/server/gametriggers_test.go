package server

// The trigger registry's handler tests (MAD-335): registration, list,
// delete, the model's proposal over HTTP (its 503-without-model
// contract, its honest NONE), the nudges read, and the account scope.
// Assertions are on the HTTP response; the engine's own suite covers
// the store and the firing.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

func TestTriggerRegistryOverHTTP(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)

	// Register (declared), read it back through the surface.
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/triggers",
		`{"card":"Rhystic Study","event_kind":"OPPONENT_CASTS_SPELL","effect":"may draw a card"}`, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/triggers?card=Rhystic+Study", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Triggers []engine.TriggerRow `json:"triggers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("list body: %v (%s)", err, rec.Body)
	}
	if len(body.Triggers) != 1 || body.Triggers[0].Origin != "declared" {
		t.Fatalf("rows = %+v", body.Triggers)
	}

	// The registration fires through the live surface: the source lands
	// first (cast + resolve), priority passes to seat 2, and their cast
	// fires the Rhystic in the same batch.
	gameAction(t, s, admin, game, `{"kind":"CAST","seat":1,"card":"Rhystic Study","base":{"types":["Enchantment"]}}`)
	gameAction(t, s, admin, game, `{"kind":"PASS_PRIORITY","seat":1}`)
	gameAction(t, s, admin, game, `{"kind":"PASS_PRIORITY","seat":2}`)
	gameAction(t, s, admin, game, `{"kind":"PASS_PRIORITY","seat":1}`)
	evs := gameAction(t, s, admin, game, `{"kind":"CAST","seat":2,"card":"Sol Ring"}`)
	sawFire := false
	for _, e := range evs {
		if e.Kind == engine.EventTriggerFired && e.Card == "Rhystic Study" {
			sawFire = true
		}
	}
	if !sawFire {
		t.Fatal("the registered trigger never fired on the opponent's cast")
	}

	// The nudge read carries the unresolved Rhystic.
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/nudges", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("nudges: status %d, body %s", rec.Code, rec.Body)
	}
	var nbody struct {
		Nudges []engine.Nudge `json:"nudges"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &nbody); err != nil {
		t.Fatalf("nudges body: %v (%s)", err, rec.Body)
	}
	sawNudge := false
	for _, n := range nbody.Nudges {
		if n.Kind == "unresolved" && n.Card == "Rhystic Study" {
			sawNudge = true
		}
	}
	if !sawNudge {
		t.Fatalf("nudges = %+v, want the unresolved Rhystic", nbody.Nudges)
	}

	// Delete, and the surface answers with the missing row's 404.
	rec = hit(t, s, http.MethodDelete,
		"/api/games/"+game+"/triggers?card=Rhystic+Study&event_kind=OPPONENT_CASTS_SPELL", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodDelete,
		"/api/games/"+game+"/triggers?card=Rhystic+Study&event_kind=OPPONENT_CASTS_SPELL", "", admin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: status %d", rec.Code)
	}
	// And a validation miss is a 400 that wrote nothing.
	rec = hit(t, s, http.MethodPost, "/api/games/"+game+"/triggers",
		`{"card":"X","event_kind":"NOT_A_KIND","effect":"e"}`, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: status %d", rec.Code)
	}
}

func TestTriggerProposalOverHTTP(t *testing.T) {
	s, _ := newIntentServer(t, &fakeIntentModel{t: t, responses: []string{
		"```json\n{\"kind\":\"UPKEEP\",\"effect\":\"lose 1 life, draw a card\",\"confidence\":0.9}\n```",
	}})
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)

	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/triggers/propose",
		`{"card":"Phyrexian Arena","seat":1}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("propose: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Proposal *struct {
			Card  string `json:"card"`
			Event string `json:"event_kind"`
		} `json:"proposal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("propose body: %v (%s)", err, rec.Body)
	}
	if body.Proposal == nil || body.Proposal.Event != "UPKEEP" {
		t.Fatalf("proposal = %+v", body.Proposal)
	}
	// Nothing was written by the proposal itself; the confirm tap is
	// the writer, origin confirmed.
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/triggers?card=Phyrexian+Arena", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-confirm list: status %d", rec.Code)
	}
	var rows struct {
		Triggers []engine.TriggerRow `json:"triggers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows.Triggers) != 0 {
		t.Fatalf("the proposal wrote rows: %+v", rows.Triggers)
	}
	rec = hit(t, s, http.MethodPost, "/api/games/"+game+"/triggers",
		`{"card":"Phyrexian Arena","event_kind":"UPKEEP","effect":"lose 1 life, draw a card","origin":"confirmed"}`, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("confirm: status %d, body %s", rec.Code, rec.Body)
	}
	// Proposing an already-registered card is a 400.
	rec = hit(t, s, http.MethodPost, "/api/games/"+game+"/triggers/propose",
		`{"card":"Phyrexian Arena","seat":1}`, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("re-propose: status %d", rec.Code)
	}
}

func TestTriggerProposalNONEAndModelless(t *testing.T) {
	// The model's honest NONE is a 200 with a note, not an error.
	s, _ := newIntentServer(t, &fakeIntentModel{t: t, responses: []string{
		"```json\n{\"kind\":\"NONE\"}\n```",
	}})
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/triggers/propose",
		`{"card":"Glorious Anthem","seat":1}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("NONE: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Proposal any    `json:"proposal"`
		Note     string `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Proposal != nil || body.Note == "" {
		t.Fatalf("NONE body = %+v", body)
	}

	// A grammar-only install answers 503 on propose — the manual strip
	// still works, the affordance is simply not there.
	bare, _ := newIntentServer(t, nil)
	bareAdmin := adminSession(t, bare)
	bareGame := gameDrive(t, bare, bareAdmin)
	rec = hit(t, bare, http.MethodPost, "/api/games/"+bareGame+"/triggers/propose",
		`{"card":"Phyrexian Arena","seat":1}`, bareAdmin)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("modelless propose: status %d", rec.Code)
	}
}

func TestTriggerSurfaceAccountScope(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game := gameDrive(t, s, admin)
	friend := gameFriend(t, s, admin)
	// Another owner's game answers like a missing one — the scope rule
	// every games endpoint keeps.
	for _, target := range []string{
		"/api/games/" + game + "/triggers",
		"/api/games/" + game + "/nudges",
	} {
		rec := hit(t, s, http.MethodGet, target, "", friend)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: friend scope status %d", target, rec.Code)
		}
	}
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/triggers",
		`{"card":"X","event_kind":"UPKEEP","effect":"e"}`, friend)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("friend register: status %d", rec.Code)
	}
}
