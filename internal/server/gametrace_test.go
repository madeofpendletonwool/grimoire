package server

// The provenance endpoints' handler tests (MAD-334): the three
// deterministic questions over HTTP — the characteristic trace (with
// its at-an-ordinal historical read), the death walk, the turn slice —
// plus the account scope and the reject-a-row-that-isn't-there
// contract. Every assertion is on the HTTP response, the store already
// covered by the engine's own tests.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// traceGame drives the standing provenance board: an Anthem, a 2/2 bear
// under it with a +1/+1 counter and a Giant Growth (the 7/7), and a
// second bear that dies to a -3/-3 at 3/3. Returns the game, the two
// bears, and the dying bear's DIED ordinal.
func traceGame(t *testing.T, s *Server, cookie *http.Cookie) (game string, bear, victim int64, diedOrd int64) {
	t.Helper()
	game = gameDrive(t, s, cookie) // seat 1's precombat main
	anthem := gameAction(t, s, cookie, game,
		`{"kind":"CREATE_TOKEN","seat":1,"token":{"name":"Glorious Anthem","types":["Enchantment"]}}`)
	bearObj := anthem[len(anthem)-1].Object
	bearTokens := gameAction(t, s, cookie, game,
		`{"kind":"CREATE_TOKEN","seat":1,"token":{"name":"Grizzly Bears","types":["Creature"],"power":2,"toughness":2}}`)
	bear = bearTokens[len(bearTokens)-1].Object
	gameAction(t, s, cookie, game, `{"kind":"ADD_MODIFIER","seat":1,"object":`+jsonInt(bear)+`,"modifier":{`+
		`"layer":"pt_modify","duration":"while_source_present","source_obj":`+jsonInt(bearObj)+`,`+
		`"delta":{"power":1,"toughness":1}}}`)
	gameAction(t, s, cookie, game,
		`{"kind":"ADJUST_COUNTERS","seat":1,"on_object":`+jsonInt(bear)+`,"counter_name":"+1/+1","delta":1}`)
	gameAction(t, s, cookie, game, `{"kind":"ADD_MODIFIER","seat":1,"object":`+jsonInt(bear)+`,"modifier":{`+
		`"layer":"pt_modify","duration":"until_end_of_turn","source_card":"Giant Growth",`+
		`"delta":{"power":3,"toughness":3}}}`)
	victimTokens := gameAction(t, s, cookie, game,
		`{"kind":"CREATE_TOKEN","seat":2,"token":{"name":"Snapdax","types":["Creature"],"power":2,"toughness":2}}`)
	victim = victimTokens[len(victimTokens)-1].Object
	gameAction(t, s, cookie, game,
		`{"kind":"ADJUST_COUNTERS","seat":2,"on_object":`+jsonInt(victim)+`,"counter_name":"+1/+1","delta":1}`)
	gasp := gameAction(t, s, cookie, game, `{"kind":"ADD_MODIFIER","seat":1,"object":`+jsonInt(victim)+`,"modifier":{`+
		`"layer":"pt_modify","duration":"until_end_of_turn","source_card":"Last Gasp",`+
		`"delta":{"power":-3,"toughness":-3}}}`)
	for _, e := range gasp {
		if e.Kind == engine.EventDied {
			diedOrd = e.Ord
		}
	}
	if diedOrd == 0 {
		t.Fatalf("the gasp produced no DIED row: %+v", gasp)
	}
	return game, bear, victim, diedOrd
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestObjectTraceOverHTTP(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game, bear, _, _ := traceGame(t, s, admin)

	rec := hit(t, s, http.MethodGet, "/api/games/"+game+"/objects/"+jsonInt(bear)+"/trace", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("trace: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Trace engine.CharTrace `json:"trace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("trace body: %v (%s)", err, rec.Body)
	}
	trace := body.Trace
	total := trace.PT[len(trace.PT)-1]
	if total.Kind != "total" || total.Power != 7 || total.Toughness != 7 {
		t.Fatalf("total = %+v, want the 7/7", total)
	}
	var sawAnthem, sawCounter, sawGrowth bool
	for _, l := range trace.PT {
		switch l.Label {
		case "Glorious Anthem":
			sawAnthem = true
		case "+1/+1 counter":
			sawCounter = true
		case "Giant Growth":
			sawGrowth = true
		}
	}
	if !sawAnthem || !sawCounter || !sawGrowth {
		t.Fatalf("stack rows missing: anthem %v counter %v growth %v", sawAnthem, sawCounter, sawGrowth)
	}

	// A historical read: the stack before the Giant Growth lands is the
	// 4/4 the table argued about a second ago.
	evs, _ := gameEvents(t, s, admin, game)
	var beforeGrowth int64
	for _, e := range evs {
		if e.Kind == engine.EventModifierAdded && e.Modifier != nil && e.Modifier.SourceCard == "Giant Growth" {
			beforeGrowth = e.Ord - 1
			break
		}
	}
	if beforeGrowth == 0 {
		t.Fatal("no Giant Growth row found")
	}
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/objects/"+jsonInt(bear)+"/trace?at="+jsonInt(beforeGrowth), "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("historical trace: status %d, body %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("historical body: %v", err)
	}
	if total := body.Trace.PT[len(body.Trace.PT)-1]; total.Power != 4 || total.Toughness != 4 {
		t.Fatalf("historical total = %+v, want 4/4", total)
	}

	// An object the game never held answers 404, not an empty fiction.
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/objects/999/trace", "", admin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("absent object: status %d", rec.Code)
	}
	// Another owner's game answers like a missing one.
	friend := gameFriend(t, s, admin)
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/objects/"+jsonInt(bear)+"/trace", "", friend)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign trace: status %d", rec.Code)
	}
}

func TestDeathTraceOverHTTP(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game, _, victim, diedOrd := traceGame(t, s, admin)

	rec := hit(t, s, http.MethodGet, "/api/games/"+game+"/death/"+jsonInt(diedOrd), "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("death: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Death engine.DeathReport `json:"death"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("death body: %v (%s)", err, rec.Body)
	}
	rep := body.Death
	if rep.Object != victim || rep.Cause != "sba_zero_toughness" {
		t.Fatalf("report = %+v", rep)
	}
	if total := rep.PT[len(rep.PT)-1]; total.Toughness != 0 {
		t.Fatalf("death-time toughness = %+v, want 0", total)
	}
	// The walk names the table's act, not the sweep's own rows.
	if rep.TriggeredBy == nil || rep.TriggeredBy.Kind != engine.EventModifierAdded {
		t.Fatalf("triggered_by = %+v", rep.TriggeredBy)
	}
	// Only a DIED row walks: a GAME_STARTED ordinal is a 400.
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/death/1", "", admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-death ordinal: status %d", rec.Code)
	}
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/death/9999", "", admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("absent ordinal: status %d", rec.Code)
	}
}

func TestTurnSliceOverHTTP(t *testing.T) {
	s, _ := newGamesServer(t)
	admin := adminSession(t, s)
	game, _, _, _ := traceGame(t, s, admin)

	rec := hit(t, s, http.MethodGet, "/api/games/"+game+"/turns/1", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn 1: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Turn     int            `json:"turn"`
		TurnSeat int            `json:"turn_seat"`
		Events   []engine.Event `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("turn body: %v (%s)", err, rec.Body)
	}
	if body.Turn != 1 || body.TurnSeat != 1 || len(body.Events) == 0 {
		t.Fatalf("turn slice = turn %d seat %d, %d rows", body.Turn, body.TurnSeat, len(body.Events))
	}
	if body.Events[0].Kind != engine.EventTurnStarted {
		t.Fatalf("slice starts at %s", body.Events[0].Kind)
	}
	for i, e := range body.Events {
		if e.Kind == engine.EventTurnStarted && i > 0 {
			t.Fatalf("turn 1 leaked a second anchor at ord %d", e.Ord)
		}
	}
	// A turn the log never reached is a 404 — never invented rows.
	rec = hit(t, s, http.MethodGet, "/api/games/"+game+"/turns/9", "", admin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("absent turn: status %d", rec.Code)
	}
}
