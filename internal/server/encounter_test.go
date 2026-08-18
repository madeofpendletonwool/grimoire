package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// newEncounterServer builds a server with the encounter store wired and a
// stubbed Open5e behind the monster search.
func newEncounterServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "encounters.db")
	store, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	encounters, err := encounter.New(store.DB())
	if err != nil {
		t.Fatalf("new encounter store: %v", err)
	}
	open5e := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/creatures/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"count":2,"results":[
			{"name":"Goblin","document":{"key":"srd-2024"},"challenge_rating":0.25,"type":{"name":"Humanoid"}},
			{"name":"Goblin Boss","document":{"key":"srd-2024"},"challenge_rating":1,"type":{"name":"Humanoid"}}
		]}`)
	}))
	t.Cleanup(open5e.Close)

	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithEncounters(encounters, encounter.NewBestiaryWithBase(open5e.URL))
	return s, open5e
}

func TestEncounterMonsterSearch(t *testing.T) {
	s, _ := newEncounterServer(t)
	admin := adminSession(t, s)
	rec := call(s, http.MethodGet, "/api/encounter/monsters?q=goblin", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: status %d, body %s", rec.Code, rec.Body)
	}
	var resp struct {
		Monsters []encounter.MonsterSummary `json:"monsters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Monsters) == 0 || resp.Monsters[0].Name != "Goblin" || resp.Monsters[0].XP != 50 {
		t.Fatalf("monsters = %+v, want Goblin XP 50 first", resp.Monsters)
	}
	if rec := call(s, http.MethodGet, "/api/encounter/monsters", "", admin); rec.Code != http.StatusBadRequest {
		t.Errorf("missing q: status %d, want 400", rec.Code)
	}
}

// Monster search must degrade gracefully when Open5e is unreachable: 200 with
// an empty list and a warning, never an error page.
func TestEncounterMonsterSearchDegradesWhenUnreachable(t *testing.T) {
	s, open5e := newEncounterServer(t)
	admin := adminSession(t, s)
	open5e.Close()

	rec := call(s, http.MethodGet, "/api/encounter/monsters?q=goblin", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: status %d, want 200 with a warning", rec.Code)
	}
	var resp struct {
		Monsters []encounter.MonsterSummary `json:"monsters"`
		Warning  string                     `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Monsters) != 0 || resp.Warning == "" {
		t.Fatalf("degraded response = %+v, want empty monsters + warning", resp)
	}
}

func TestEncounterCRUDFlow(t *testing.T) {
	s, _ := newEncounterServer(t)
	admin := adminSession(t, s)

	// Create with the issue's acceptance case: 4 goblins vs five level-1s.
	body := `{"name":"Ambush","party":[1,1,1,1,1],"monsters":[{"name":"Goblin","cr":"1/4","count":4}]}`
	rec := call(s, http.MethodPost, "/api/encounters", body, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body)
	}
	var created struct {
		Encounter encounterView `json:"encounter"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	e := created.Encounter
	if e.ID == "" || len(e.Party) != 5 || len(e.Monsters) != 1 {
		t.Fatalf("created encounter = %+v", e)
	}
	// XP was derived server-side from CR, and the verdict recomputed: the
	// DMG math makes four goblins against five 1st-level characters Hard.
	if e.Monsters[0].XP != 50 {
		t.Errorf("goblin XP = %d, want 50 derived from CR 1/4", e.Monsters[0].XP)
	}
	if e.Verdict.Difficulty != "Hard" || e.Verdict.AdjustedXP != 400 {
		t.Errorf("verdict = %+v, want Hard at 400 adjusted XP", e.Verdict)
	}

	// List, get, update, delete round-trip.
	if rec = call(s, http.MethodGet, "/api/encounters", "", admin); rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec = call(s, http.MethodGet, "/api/encounters/"+e.ID, "", admin); rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	patch := `{"name":"Ambush at the Crag","monsters":[{"name":"Goblin","cr":"1/4","count":6},{"name":"Goblin Boss","cr":"1","count":1}]}`
	if rec = call(s, http.MethodPatch, "/api/encounters/"+e.ID, patch, admin); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	var updated struct {
		Encounter encounterView `json:"encounter"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if updated.Encounter.Name != "Ambush at the Crag" || len(updated.Encounter.Monsters) != 2 {
		t.Fatalf("patched encounter = %+v", updated.Encounter)
	}
	// 6x50 + 200 = 500 raw, x2.5 for seven monsters (7-10 band) = 1250
	// adjusted: Deadly for the party whose Deadly threshold is 500.
	if updated.Encounter.Verdict.AdjustedXP != 1250 || updated.Encounter.Verdict.Difficulty != "Deadly" {
		t.Errorf("patched verdict = %+v, want Deadly at 1000 adjusted", updated.Encounter.Verdict)
	}
	if rec = call(s, http.MethodDelete, "/api/encounters/"+e.ID, "", admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec = call(s, http.MethodGet, "/api/encounters/"+e.ID, "", admin); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d, want 404", rec.Code)
	}
}

// A tampered payload cannot store a bogus verdict: difficulty fields are not
// read, XP is re-derived from CR, and reads always recompute.
func TestEncounterTamperedVerdictIgnored(t *testing.T) {
	s, _ := newEncounterServer(t)
	admin := adminSession(t, s)

	// The payload lies about XP and tries to smuggle a difficulty verdict.
	body := `{"name":"Honest Assessment","party":[1],"monsters":[{"name":"Goblin","cr":"1/4","xp":9999,"count":1}],"verdict":{"difficulty":"Trivial"},"difficulty":"Trivial"}`
	rec := call(s, http.MethodPost, "/api/encounters", body, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body)
	}
	var created struct {
		Encounter encounterView `json:"encounter"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Encounter.Monsters[0].XP != 50 {
		t.Errorf("stored XP = %d, want 50 (client XP ignored)", created.Encounter.Monsters[0].XP)
	}
	// One goblin vs one character: 50 XP x1.5 (lone PC) = 75 adjusted,
	// which exactly meets Hard (75) for a 1st-level character.
	if created.Encounter.Verdict.Difficulty != "Hard" || created.Encounter.Verdict.AdjustedXP != 75 {
		t.Errorf("verdict = %+v, want Easy", created.Encounter.Verdict)
	}
}

// Encounters are invisible across users: a second account cannot read,
// mutate, or delete the first user's encounters.
func TestEncountersOwnerScoped(t *testing.T) {
	s, _ := newEncounterServer(t)
	admin := adminSession(t, s)

	inv := createInvite(t, s, admin, "for a friend")
	rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("riend", "a-fine-passphrase", inv["code"].(string)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	friend := sessionFrom(t, rec)

	body := `{"name":"Keep Secret","party":[5,5,5,5],"monsters":[{"name":"Ogre","cr":"2","count":2}]}`
	rec = call(s, http.MethodPost, "/api/encounters", body, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		Encounter encounterView `json:"encounter"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if rec = call(s, http.MethodGet, "/api/encounters", "", friend); rec.Code != http.StatusOK {
		t.Fatalf("friend list: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Keep Secret") {
		t.Error("friend can see the admin's encounter in their list")
	}
	if rec = call(s, http.MethodGet, "/api/encounters/"+created.Encounter.ID, "", friend); rec.Code != http.StatusNotFound {
		t.Errorf("friend get: %d, want 404", rec.Code)
	}
	if rec = call(s, http.MethodPatch, "/api/encounters/"+created.Encounter.ID, `{"name":"Stolen"}`, friend); rec.Code != http.StatusNotFound {
		t.Errorf("friend patch: %d, want 404", rec.Code)
	}
	if rec = call(s, http.MethodDelete, "/api/encounters/"+created.Encounter.ID, "", friend); rec.Code != http.StatusNotFound {
		t.Errorf("friend delete: %d, want 404", rec.Code)
	}
	if rec = call(s, http.MethodGet, "/api/encounters/"+created.Encounter.ID, "", admin); rec.Code != http.StatusOK {
		t.Errorf("admin's encounter disturbed by foreign access: %d", rec.Code)
	}
}

// The evaluate preview endpoint computes a verdict without persisting.
func TestEncounterEvaluatePreview(t *testing.T) {
	s, _ := newEncounterServer(t)
	admin := adminSession(t, s)

	rec := call(s, http.MethodPost, "/api/encounters/evaluate",
		`{"party":[1,1,1,1,1],"monsters":[{"name":"Goblin","cr":"1/4","count":4}]}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Verdict encounter.Verdict `json:"verdict"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Verdict.Difficulty != "Hard" || resp.Verdict.AdjustedXP != 400 {
		t.Fatalf("verdict = %+v, want Hard at 400", resp.Verdict)
	}

	// Validation still applies to previews.
	if rec = call(s, http.MethodPost, "/api/encounters/evaluate",
		`{"party":[99],"monsters":[]}`, admin); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid party evaluate: %d, want 400", rec.Code)
	}
	if rec = call(s, http.MethodPost, "/api/encounters/evaluate",
		`{"party":[1],"monsters":[{"name":"Weird","cr":"7.5","count":1}]}`, admin); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown CR evaluate: %d, want 400", rec.Code)
	}
}

func TestEncountersUnavailableWithoutStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "none.db")
	store, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if rec := call(s, http.MethodGet, "/api/encounters", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("list without store: %d, want 503", rec.Code)
	}
}
