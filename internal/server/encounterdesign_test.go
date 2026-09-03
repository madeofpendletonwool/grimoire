package server

import (
	"encoding/json"
	"fmt"
	"io"
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

// bestiaryFixture is a small SRD stand-in with one creature per encounter
// slot, so a pool built from it exercises every tier.
const bestiaryFixture = `{"next":"","results":[
	{"key":"srd-2024_goblin","name":"Goblin","document":{"key":"srd-2024"},
	 "challenge_rating":0.25,"type":{"name":"Humanoid"},"size":{"name":"Small"},
	 "armor_class":15,"hit_points":7,"speed_all":{"walk":30},
	 "actions":[{"name":"Shortbow","desc":"Ranged Weapon Attack: +4 to hit.","action_type":"ACTION"}]},
	{"key":"srd-2024_goblin-boss","name":"Goblin Boss","document":{"key":"srd-2024"},
	 "challenge_rating":1,"type":{"name":"Humanoid"},"size":{"name":"Small"},
	 "armor_class":17,"hit_points":21,"speed_all":{"walk":30}},
	{"key":"srd-2024_owlbear","name":"Owlbear","document":{"key":"srd-2024"},
	 "challenge_rating":3,"type":{"name":"Monstrosity"},"size":{"name":"Large"},
	 "armor_class":13,"hit_points":59,"speed_all":{"walk":40}}
]}`

// newDesignServer wires the encounter store, a mirrored bestiary built from
// the fixture, and an optional stubbed model.
func newDesignServer(t *testing.T, llmHandler http.HandlerFunc) *Server {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "design.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	open5e := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/creatures/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, bestiaryFixture)
	}))
	t.Cleanup(open5e.Close)

	encounters, err := encounter.New(store.DB())
	if err != nil {
		t.Fatalf("encounter store: %v", err)
	}
	catalog, err := encounter.NewCatalog(store.DB(), open5e.URL)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}

	cfg := llm.Config{APIKey: "", Model: "test"}
	if llmHandler != nil {
		up := httptest.NewServer(llmHandler)
		t.Cleanup(up.Close)
		cfg = llm.Config{BaseURL: up.URL, APIKey: "test-key", Model: "test-model"}
	}
	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("auth store: %v", err)
	}
	s, err := New(store, llm.New(cfg), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s.WithEncounters(encounters, encounter.NewBestiaryWithBase(open5e.URL), catalog)
}

// designFixture is a well-formed design naming two real creatures and one the
// server must refuse to render.
const designFixture = `# The Ledge Above the Trail

## The pitch
Scouts with the high ground and no patience for travellers.

## Roster
4 × Goblin
1 × Goblin Boss
1 × Shadow Wyrm of Nowhere

## Terrain
A twenty-foot ledge over the road.

## Tactics
The boss stays back and shouts.
`

func TestEncounterDesignStreamsAndValidates(t *testing.T) {
	s := newDesignServer(t, sseStub(designFixture))
	admin := adminSession(t, s)

	rec := call(s, http.MethodPost, "/api/encounter/design",
		`{"idea":"goblins on a ledge","party":[3,3,3,3],"difficulty":"Medium"}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("design: %d %s", rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	for _, want := range []string{"event: meta", "event: delta", "event: done"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %q in stream:\n%s", want, raw)
		}
	}

	var meta struct {
		Candidates int  `json:"candidates"`
		Bestiary   int  `json:"bestiary"`
		Assumed    bool `json:"assumed_party"`
		Budget     struct {
			Band     string `json:"band"`
			TargetXP int    `json:"target_xp"`
		} `json:"budget"`
	}
	if err := json.Unmarshal([]byte(sseDataFor(t, raw, "meta")), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta.Bestiary != 3 || meta.Candidates == 0 {
		t.Fatalf("meta = %+v, want a mirrored bestiary and a non-empty shortlist", meta)
	}
	if meta.Budget.Band != "Medium" || meta.Budget.TargetXP != 600 {
		t.Errorf("budget = %+v, want Medium at 600 XP for four 3rd-level PCs", meta.Budget)
	}
	if meta.Assumed {
		t.Error("an explicit party must not be reported as assumed")
	}

	var done struct {
		Name       string              `json:"name"`
		Monsters   []encounter.Monster `json:"monsters"`
		Unverified []string            `json:"unverified"`
		Notes      string              `json:"notes"`
		Verdict    encounter.Verdict   `json:"verdict"`
	}
	if err := json.Unmarshal([]byte(sseDataFor(t, raw, "done")), &done); err != nil {
		t.Fatalf("decode done: %v", err)
	}
	if done.Name != "The Ledge Above the Trail" {
		t.Errorf("name = %q", done.Name)
	}
	if len(done.Monsters) != 2 {
		t.Fatalf("monsters = %+v, want the two real statblocks", done.Monsters)
	}
	for _, m := range done.Monsters {
		if strings.Contains(m.Name, "Nowhere") {
			t.Fatalf("an invented monster reached the roster: %+v", done.Monsters)
		}
		// XP is always re-derived from the table, never taken from the model.
		if want, _ := encounter.XPForCR(m.CR); m.XP != want {
			t.Errorf("%s XP = %d, want %d from the CR table", m.Name, m.XP, want)
		}
	}
	if len(done.Unverified) != 1 || !strings.Contains(done.Unverified[0], "Nowhere") {
		t.Errorf("unverified = %v", done.Unverified)
	}
	if done.Verdict.Difficulty == "" || done.Verdict.AdjustedXP == 0 {
		t.Errorf("verdict not recomputed: %+v", done.Verdict)
	}
	if !strings.Contains(done.Notes, "## Tactics") {
		t.Error("the write-up must survive for the reader")
	}
}

// The whole point of the designer is that a DM with nothing to say still gets
// an encounter, so an empty body must produce a real one against the default
// table rather than a validation error.
func TestEncounterDesignAcceptsAnEmptyBrief(t *testing.T) {
	s := newDesignServer(t, sseStub(designFixture))
	admin := adminSession(t, s)

	rec := call(s, http.MethodPost, "/api/encounter/design", `{}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty brief: %d %s", rec.Code, rec.Body)
	}
	var meta struct {
		Assumed bool `json:"assumed_party"`
		Budget  struct {
			Band  string `json:"band"`
			Party []int  `json:"party"`
		} `json:"budget"`
	}
	if err := json.Unmarshal([]byte(sseDataFor(t, rec.Body.String(), "meta")), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if !meta.Assumed || len(meta.Budget.Party) != 4 || meta.Budget.Band != "Medium" {
		t.Fatalf("empty brief did not fall back to a usable table: %+v", meta)
	}
}

// Validation happens before the stream commits, so a bad party is a 4xx and
// not an error event inside a 200.
func TestEncounterDesignRejectsBadPartyBeforeStreaming(t *testing.T) {
	s := newDesignServer(t, sseStub(designFixture))
	admin := adminSession(t, s)
	if rec := call(s, http.MethodPost, "/api/encounter/design", `{"party":[0]}`, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("level 0: %d %s", rec.Code, rec.Body)
	}
	if rec := call(s, http.MethodPost, "/api/encounter/design", `{"party":[1,1,1,1,1,1,1,1,1,1,1,1,1]}`, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized party: %d", rec.Code)
	}
}

// Without a model there is nothing to design with; the request must say so
// plainly rather than streaming an empty encounter.
func TestEncounterDesignNeedsTheModel(t *testing.T) {
	s := newDesignServer(t, nil)
	admin := adminSession(t, s)
	rec := call(s, http.MethodPost, "/api/encounter/design", `{"idea":"goblins"}`, admin)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "ANTHROPIC_API_KEY") {
		t.Errorf("the notice should name the key to set: %s", rec.Body)
	}
}

func TestEncounterBudgetEndpoint(t *testing.T) {
	s := newDesignServer(t, nil)
	admin := adminSession(t, s)
	rec := call(s, http.MethodPost, "/api/encounter/budget", `{"party":[3,3,3,2],"difficulty":"hard"}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("budget: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Budget encounter.Budget `json:"budget"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Budget.Band != "Hard" || resp.Budget.TargetXP != 825 {
		t.Fatalf("budget = %+v, want Hard at 825 XP for the DMG's sample party", resp.Budget)
	}
	if len(resp.Budget.Shapes) == 0 || resp.Budget.MaxSoloCR == "" {
		t.Errorf("budget carries no build guidance: %+v", resp.Budget)
	}
}

// The statblock endpoint is what lets the roster expand in place; it must
// serve from the mirror and refuse names that are not in it.
func TestEncounterStatblockEndpoint(t *testing.T) {
	s := newDesignServer(t, nil)
	admin := adminSession(t, s)
	// Mirror on demand, the way the designer does.
	if err := s.catalog.Sync(t.Context()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rec := call(s, http.MethodGet, "/api/encounter/statblock?name=owlbear", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("statblock: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Creature encounter.Creature `json:"creature"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Creature.Name != "Owlbear" || resp.Creature.XP != 700 || resp.Creature.HP != 59 {
		t.Fatalf("creature = %+v, want the real Owlbear", resp.Creature)
	}
	if rec := call(s, http.MethodGet, "/api/encounter/statblock?name=Shadow+Wyrm", "", admin); rec.Code != http.StatusNotFound {
		t.Errorf("invented name: %d, want 404", rec.Code)
	}
	if rec := call(s, http.MethodGet, "/api/encounter/statblock", "", admin); rec.Code != http.StatusBadRequest {
		t.Errorf("missing name: %d, want 400", rec.Code)
	}
}

// Once the bestiary is mirrored, the picker must answer from it rather than
// going back out to Open5e.
func TestEncounterMonsterSearchPrefersTheMirror(t *testing.T) {
	s := newDesignServer(t, nil)
	admin := adminSession(t, s)
	if err := s.catalog.Sync(t.Context()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	rec := call(s, http.MethodGet, "/api/encounter/monsters?q=owlbear", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Monsters []encounter.MonsterSummary `json:"monsters"`
		Source   string                     `json:"source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Source != "local" || len(resp.Monsters) == 0 || resp.Monsters[0].Name != "Owlbear" {
		t.Fatalf("search = %+v (source %q), want the mirrored Owlbear", resp.Monsters, resp.Source)
	}
}

/* ---------- objectives, terrain and waves (MAD-380) ---------- */

// capturingStub records the request the server sent the model, then streams
// answer as the reply.
func capturingStub(answer string, captured *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*captured = string(b)
		sseStub(answer)(w, r)
	}
}

// surviveDesignFixture is a well-formed survive design written in the wave
// format the prompt asks for.
const surviveDesignFixture = `# Hold the Crossing

## The pitch
The far bank is safety, and the bridge is not ours.

## Roster
Wave 1: 4 × Goblin
Wave 2: 1 × Goblin Boss

## Terrain
The span groans under anyone who crosses.

## Tactics
The boss arrives with the second wave.
`

// An objective is a declared kind: anything outside the vocabulary is
// rejected at the API boundary — a 400, never a default to defeat.
func TestEncounterDesignRejectsUnknownObjective(t *testing.T) {
	s := newDesignServer(t, nil)
	admin := adminSession(t, s)
	for _, target := range []string{"/api/encounter/design", "/api/encounter/budget"} {
		if rec := call(s, http.MethodPost, target, `{"objective":{"kind":"escort"}}`, admin); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: unknown objective = %d, want 400", target, rec.Code)
		}
	}
	if rec := call(s, http.MethodPost, "/api/encounters", `{"name":"X","objective":{"kind":"escort"}}`, admin); rec.Code != http.StatusBadRequest {
		t.Errorf("create: unknown objective = %d, want 400", rec.Code)
	}
}

// A survive design comes back priced across its waves, with the objective,
// its rendered ending and the generated terrain riding the done frame —
// all of it server-owned.
func TestEncounterDesignSurvive(t *testing.T) {
	var prompt string
	s := newDesignServer(t, capturingStub(surviveDesignFixture, &prompt))
	admin := adminSession(t, s)

	rec := call(s, http.MethodPost, "/api/encounter/design",
		`{"idea":"hold the bridge","party":[3,3,3,3],"objective":{"kind":"survive"}}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("design: %d %s", rec.Code, rec.Body)
	}

	// The model was handed the objective, its XP rules, the terrain with its
	// mechanical effects intact, and the wave roster format.
	for _, want := range []string{
		"OBJECTIVE: Survive", "Failure:", "one wave per line",
		"Difficult ground", "every foot of movement",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}

	var done struct {
		Monsters  []encounter.Monster   `json:"monsters"`
		Waves     [][]encounter.Monster `json:"waves"`
		Verdict   encounter.Verdict     `json:"verdict"`
		Objective encounter.Objective   `json:"objective"`
		Ending    encounter.Ending      `json:"ending"`
		Terrain   encounter.Terrain     `json:"terrain"`
	}
	if err := json.Unmarshal([]byte(sseDataFor(t, rec.Body.String(), "done")), &done); err != nil {
		t.Fatalf("decode done: %v", err)
	}
	if done.Objective.Kind != encounter.Survive || done.Objective.Rounds != 6 {
		t.Errorf("objective = %+v, want survive normalised to 6 rounds", done.Objective)
	}
	if done.Ending.Success == "" || done.Ending.Clock == "" {
		t.Errorf("ending incomplete: %+v", done.Ending)
	}
	// Survive's ground is features only — the waves are the hazard.
	if len(done.Terrain.Features) < 3 || len(done.Terrain.Hazards) != 0 {
		t.Errorf("terrain = %+v, want the holding ground's features and no hazard", done.Terrain)
	}
	for _, f := range done.Terrain.Features {
		if f.Effect == "" || f.Area == "" {
			t.Errorf("feature without its mechanics: %+v", f)
		}
	}
	if len(done.Waves) != 2 {
		t.Fatalf("waves = %+v, want the two parsed waves", done.Waves)
	}
	if len(done.Monsters) != 2 {
		t.Fatalf("roster = %+v, want every wave flattened", done.Monsters)
	}
	// The verdict is the wave arithmetic: 4 goblins ×2 = 400, 1 boss ×1 = 200.
	if len(done.Verdict.Waves) != 2 || done.Verdict.AdjustedXP != 600 {
		t.Fatalf("verdict = %+v, want two waves totalling 600 adjusted", done.Verdict)
	}
	if done.Verdict.Waves[0].AdjustedXP != 400 || done.Verdict.Waves[1].AdjustedXP != 200 {
		t.Errorf("per-wave = %d + %d, want 400 + 200", done.Verdict.Waves[0].AdjustedXP, done.Verdict.Waves[1].AdjustedXP)
	}
}

// A hazardous objective hands the model the hazard with every number it
// prices at — the DMG's tier tables for this party — and none of those
// numbers are the model's to change.
func TestEncounterDesignPromptCarriesTheHazard(t *testing.T) {
	var prompt string
	s := newDesignServer(t, capturingStub(designFixture, &prompt))
	admin := adminSession(t, s)

	rec := call(s, http.MethodPost, "/api/encounter/design",
		`{"party":[3,3,3,3],"objective":{"kind":"reach"}}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("design: %d %s", rec.Code, rec.Body)
	}
	for _, want := range []string{
		"OBJECTIVE: Reach", "HAZARD Rockfall", "DC 15", "2d10 bludgeoning",
		"every foot of movement", "one creature wide",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// A defeat design's stream carries none of the objective layer — the byte
// the MAD-299 surface shipped is the byte it still ships.
func TestEncounterDesignDefeatStaysUntouched(t *testing.T) {
	var prompt string
	s := newDesignServer(t, capturingStub(designFixture, &prompt))
	admin := adminSession(t, s)

	rec := call(s, http.MethodPost, "/api/encounter/design",
		`{"party":[3,3,3,3],"objective":{"kind":"defeat"}}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("design: %d %s", rec.Code, rec.Body)
	}
	done := sseDataFor(t, rec.Body.String(), "done")
	for _, absent := range []string{`"objective"`, `"ending"`, `"terrain"`, `"waves"`} {
		if strings.Contains(done, absent) {
			t.Errorf("defeat done frame carries %q:\n%s", absent, done)
		}
	}
	if strings.Contains(prompt, "OBJECTIVE:") || strings.Contains(prompt, "TERRAIN the server generated") {
		t.Error("a defeat prompt grew objective blocks")
	}
}

// The budget endpoint answers for an objective too: the aim is the
// objective-aware one, the waves are in the readout, and the rendered
// ending lets the builder show "how this ends" before anything is designed.
func TestEncounterBudgetWithObjective(t *testing.T) {
	s := newDesignServer(t, nil)
	admin := adminSession(t, s)

	rec := call(s, http.MethodPost, "/api/encounter/budget",
		`{"party":[3,3,3,3],"difficulty":"medium","objective":{"kind":"survive"}}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("budget: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Budget  encounter.Budget   `json:"budget"`
		Ending  *encounter.Ending  `json:"ending"`
		Terrain *encounter.Terrain `json:"terrain"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Budget.Waves != 2 {
		t.Errorf("waves = %d, want 2", resp.Budget.Waves)
	}
	if len(resp.Budget.Adjustments) == 0 || resp.Budget.Adjustments[0].Rule != "waves" {
		t.Errorf("adjustments = %+v, want the wave rule and its reason", resp.Budget.Adjustments)
	}
	if resp.Ending == nil || resp.Ending.Success == "" {
		t.Errorf("ending missing: %+v", resp.Ending)
	}
	if resp.Terrain == nil || len(resp.Terrain.Features) == 0 {
		t.Errorf("terrain missing: %+v", resp.Terrain)
	}

	// Defeat answers without any of it.
	rec = call(s, http.MethodPost, "/api/encounter/budget", `{"party":[3,3,3,3]}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("defeat budget: %d", rec.Code)
	}
	var plain struct {
		Budget encounter.Budget  `json:"budget"`
		Ending *encounter.Ending `json:"ending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &plain); err != nil {
		t.Fatal(err)
	}
	if plain.Budget.Waves != 0 || plain.Budget.Objective != nil || plain.Ending != nil {
		t.Errorf("defeat budget grew objective fields: %+v", plain.Budget)
	}
}
