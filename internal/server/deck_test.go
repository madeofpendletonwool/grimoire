package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/deck"
	"github.com/madeofpendletonwool/grimoire/internal/edhrec"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// deckFixtureGzip builds a small gzipped AtomicCards payload.
func deckFixtureGzip(t *testing.T) []byte {
	t.Helper()
	payload := map[string]any{"data": map[string]any{
		"Kaalia of the Vast": []map[string]any{{
			"manaCost": "{1}{R}{W}{B}", "manaValue": 4,
			"type":             "Legendary Creature — Angel Cleric",
			"text":             "Flying. Whenever Kaalia of the Vast attacks, put an Angel, Demon, or Dragon creature card from your hand onto the battlefield tapped and attacking.",
			"colorIdentity":    []string{"R", "W", "B"},
			"edhrecRank":       122,
			"leadershipSkills": map[string]any{"Commander": true},
			"legalities":       map[string]string{"commander": "Legal"},
		}},
		"Sol Ring": []map[string]any{{
			"manaCost": "{1}", "manaValue": 1, "type": "Artifact", "text": "{T}: Add {C}{C}.",
			"colorIdentity": []string{}, "edhrecRank": 1,
			"legalities": map[string]string{"commander": "Legal"},
		}},
		"Angel of Serenity": []map[string]any{{
			"manaCost": "{5}{W}{W}{W}", "manaValue": 7, "type": "Creature — Angel",
			"text":          "Flying. When Angel of Serenity enters, exile up to three target creatures.",
			"colorIdentity": []string{"W"}, "edhrecRank": 800,
			"legalities": map[string]string{"commander": "Legal"},
		}},
		"Counterspell": []map[string]any{{
			"manaCost": "{U}{U}", "manaValue": 2, "type": "Instant", "text": "Counter target spell.",
			"colorIdentity": []string{"U"}, "edhrecRank": 15,
			"legalities": map[string]string{"commander": "Legal"},
		}},
		"Mountain": []map[string]any{{
			"type": "Basic Land — Mountain", "text": "{T}: Add {R}.",
			"colorIdentity": []string{"R"}, "edhrecRank": 40,
			"legalities": map[string]string{"commander": "Legal"},
		}},
	}}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

// newDeckServer builds a server with the card database seeded, the deck store
// wired, and an optional stubbed LLM. With llmHandler nil the server runs
// unconfigured (no API key).
func newDeckServer(t *testing.T, llmHandler http.HandlerFunc) *Server {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "deck.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	mtgjson := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/gzip")
		_, _ = w.Write(deckFixtureGzip(t))
	}))
	t.Cleanup(mtgjson.Close)
	if _, err := carddb.Populate(context.Background(), store.DB(), mtgjson.URL); err != nil {
		t.Fatalf("seed cards: %v", err)
	}
	cdb, err := carddb.New(store.DB())
	if err != nil {
		t.Fatalf("carddb: %v", err)
	}

	decks, err := deck.New(store.DB())
	if err != nil {
		t.Fatalf("deck store: %v", err)
	}

	cfg := llm.Config{APIKey: "", Model: "test"}
	if llmHandler != nil {
		up := httptest.NewServer(llmHandler)
		t.Cleanup(up.Close)
		cfg = llm.Config{BaseURL: up.URL, APIKey: "test-key", Model: "test-model"}
	}
	s, err := New(store, llm.New(cfg), nil, nil, nil, nil, nil, nil, Auth{}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s.WithDeckBuilder(cdb, decks, edhrec.New(edhrec.Options{Enabled: false}))
}

// sseStub streams answer as two SSE deltas — the shape StreamPrompt reads.
func sseStub(answer string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mid := len(answer) / 2
		w.Header().Set("content-type", "text/event-stream")
		for _, part := range []string{answer[:mid], answer[mid:]} {
			payload, _ := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"delta": map[string]any{"type": "text_delta", "text": part},
			})
			fmt.Fprintf(w, "data: %s\n\n", payload)
		}
	}
}

func postJSON(t *testing.T, s *Server, target, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func TestDeckPropose(t *testing.T) {
	s := newDeckServer(t, nil)
	rec, body := postJSON(t, s, "/api/deck/propose", `{"idea":"aggressive angels and dragons"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	cmdrs, _ := body["commanders"].([]any)
	if len(cmdrs) == 0 {
		t.Fatal("no commanders proposed")
	}
	first := cmdrs[0].(map[string]any)
	if first["name"] != "Kaalia of the Vast" {
		t.Fatalf("first = %v", first)
	}

	// Empty idea rejected.
	if rec, _ := postJSON(t, s, "/api/deck/propose", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty idea: %d", rec.Code)
	}
}

func TestDeckBuildStreamsAndValidates(t *testing.T) {
	// The stub model drafts from the candidates it was offered, plus one
	// fabricated name the server must strip, plus basic lands.
	answer := "Plan: swarm angels.\n\n1 Sol Ring\n2 Angel of Serenity\n1 Totally Fake Card\n36 Mountain\n\nNotes: big angels."
	s := newDeckServer(t, sseStub(answer))

	req := httptest.NewRequest(http.MethodPost, "/api/deck/build",
		strings.NewReader(`{"idea":"angels","commander":"Kaalia of the Vast"}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("build: %d %s", rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	for _, want := range []string{"event: meta", "event: delta", "event: done"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %q in stream:\n%s", want, raw)
		}
	}
	if !strings.Contains(raw, "Totally Fake Card") || !strings.Contains(raw, "unverified") {
		t.Fatalf("fabricated card not flagged:\n%s", raw)
	}
	if strings.Contains(raw, `"deck":"`) {
		// deck must be a parsed array; the fabricated name must not appear inside the deck entries
	}

	// The done event's deck carries only verified cards: parse its JSON and
	// assert the fabricated name is absent from deck, present in unverified.
	doneData := sseDataFor(t, raw, "done")
	var done struct {
		Deck       []map[string]any `json:"deck"`
		Unverified []string         `json:"unverified"`
		Analysis   map[string]any   `json:"analysis"`
	}
	if err := json.Unmarshal([]byte(doneData), &done); err != nil {
		t.Fatalf("decode done: %v (%s)", err, doneData)
	}
	for _, e := range done.Deck {
		if e["name"] == "Totally Fake Card" {
			t.Fatalf("fabricated card in deck: %v", done.Deck)
		}
	}
	if len(done.Unverified) != 1 || done.Unverified[0] != "Totally Fake Card" {
		t.Fatalf("unverified = %v", done.Unverified)
	}
	if len(done.Deck) != 3 {
		t.Fatalf("deck entries = %v", done.Deck)
	}
	if done.Analysis["identity"] != "BRW" {
		t.Fatalf("analysis = %v", done.Analysis)
	}
}

// sseDataFor extracts the data JSON of the named event from an SSE body.
func sseDataFor(t *testing.T, body, event string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	var capturing bool
	var data strings.Builder
	for _, l := range lines {
		if l == "event: "+event {
			capturing = true
			continue
		}
		if capturing && strings.HasPrefix(l, "data: ") {
			return strings.TrimPrefix(l, "data: ")
		}
	}
	if !capturing {
		t.Fatalf("event %q not found", event)
	}
	return data.String()
}

func TestDeckBuildRequiresCommander(t *testing.T) {
	// The no-commander rejection is data validation: it fires before the SSE
	// stream starts, so even an unconfigured server returns 400.
	s := newDeckServer(t, nil)
	if rec, _ := postJSON(t, s, "/api/deck/build", `{"idea":"angels"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("no commander: %d", rec.Code)
	}
	// The unknown-commander rejection sits behind the configured check (an
	// unconfigured server streams its 200/error event first), so wire a stub.
	s = newDeckServer(t, sseStub("1 Sol Ring"))
	if rec, _ := postJSON(t, s, "/api/deck/build", `{"commander":"Not A Card"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown commander: %d", rec.Code)
	}
}

func TestDeckAnalyze(t *testing.T) {
	s := newDeckServer(t, nil)
	list := "Commander: Kaalia of the Vast\n1 Sol Ring\n1 Counterspell\n36 Mountain\n1 Typo Cardname"
	rec, body := postJSON(t, s, "/api/deck/analyze", fmt.Sprintf(`{"decklist":%q}`, list))
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze: %d %s", rec.Code, rec.Body)
	}
	analysis, _ := body["analysis"].(map[string]any)
	if analysis == nil {
		t.Fatal("no analysis")
	}
	if analysis["identity"] != "BRW" {
		t.Fatalf("identity = %v", analysis["identity"])
	}
	bad, _ := analysis["identity_violations"].([]any)
	if len(bad) != 1 || bad[0].(map[string]any)["name"] != "Counterspell" {
		t.Fatalf("violations = %v", bad)
	}
	// Typo flagged, not dropped.
	unres, _ := body["unresolved"].([]any)
	if len(unres) != 1 {
		t.Fatalf("unresolved = %v", unres)
	}
	// Fuzzy: "Sol Rng" should resolve to Sol Ring.
	rec, body = postJSON(t, s, "/api/deck/analyze", `{"decklist":"1 Sol Rng\n36 Mountain"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("fuzzy analyze: %d", rec.Code)
	}
	deckArr, _ := body["deck"].([]any)
	found := false
	for _, e := range deckArr {
		if strings.EqualFold(e.(map[string]any)["name"].(string), "Sol Ring") {
			found = true
		}
	}
	if !found {
		t.Fatalf("fuzzy did not resolve Sol Rng: %v", deckArr)
	}
}

func TestDeckCRUDAndValidation(t *testing.T) {
	s := newDeckServer(t, nil)

	// A fabricated card is rejected with its name.
	rec, _ := postJSON(t, s, "/api/decks", `{"name":"Test","commander":"Kaalia of the Vast","cards":[{"name":"Fake Card","count":1}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Fake Card") {
		t.Fatalf("fabricated accepted: %d %s", rec.Code, rec.Body)
	}

	// Real cards save.
	rec, body := postJSON(t, s, "/api/decks", `{"name":"Mardu Angels","commander":"Kaalia of the Vast","cards":[{"name":"Sol Ring","count":1},{"name":"Angel of Serenity","count":2},{"name":"Mountain","count":36}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	id := body["deck"].(map[string]any)["id"].(string)

	// Get + list.
	rec, body = postJSON(t, s, "/api/decks/"+id, ``)
	_ = rec
	req := httptest.NewRequest(http.MethodGet, "/api/decks/"+id, nil)
	r2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(r2, req)
	if r2.Code != http.StatusOK {
		t.Fatalf("get: %d", r2.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/decks", nil)
	r2 = httptest.NewRecorder()
	s.Handler().ServeHTTP(r2, req)
	if !strings.Contains(r2.Body.String(), "Mardu Angels") {
		t.Fatalf("list missing deck: %s", r2.Body)
	}

	// Patch: rename.
	req = httptest.NewRequest(http.MethodPatch, "/api/decks/"+id, strings.NewReader(`{"name":"Renamed"}`))
	req.Header.Set("content-type", "application/json")
	r2 = httptest.NewRecorder()
	s.Handler().ServeHTTP(r2, req)
	if r2.Code != http.StatusOK || !strings.Contains(r2.Body.String(), "Renamed") {
		t.Fatalf("patch: %d %s", r2.Code, r2.Body)
	}

	// Delete.
	req = httptest.NewRequest(http.MethodDelete, "/api/decks/"+id, nil)
	r2 = httptest.NewRecorder()
	s.Handler().ServeHTTP(r2, req)
	if r2.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", r2.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/decks/"+id, nil)
	r2 = httptest.NewRecorder()
	s.Handler().ServeHTTP(r2, req)
	if r2.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d", r2.Code)
	}
}

func TestDeckCombosUnavailable(t *testing.T) {
	s := newDeckServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/deck/combos?card=Sol+Ring", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("combos: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "GRIMOIRE_EDHREC") {
		t.Fatalf("note missing: %s", rec.Body)
	}
}

// The disabled-EDHREC client must never break the build flow: enrichment is
// skipped, candidates still flow from the local engine.
func TestDeckBuildWithoutEDHREC(t *testing.T) {
	s := newDeckServer(t, sseStub("1 Sol Ring\n36 Mountain"))
	req := httptest.NewRequest(http.MethodPost, "/api/deck/build",
		strings.NewReader(`{"idea":"angels","commander":"Kaalia of the Vast"}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: done") {
		t.Fatalf("build without edhrec: %d\n%s", rec.Code, rec.Body)
	}
	// Body drained.
	_, _ = io.ReadAll(req.Body)
}
