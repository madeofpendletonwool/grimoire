package server

// The monster designer's handler tests (MAD-382): the shelf's save
// contract through the HTTP surface, the design loop's model gate and
// corrected branch, the placement staged behind the review gate, and the
// homebrew overlay riding the builder's reads.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// scriptedCanonModel replays a script of JSON fills in call order.
type scriptedCanonModel struct {
	responses []string
	calls     []string
}

func (m *scriptedCanonModel) ModelName() string { return "fake-monster-designer" }

func (m *scriptedCanonModel) Complete(ctx context.Context, system, user string) (canon.Completion, error) {
	m.calls = append(m.calls, user)
	i := len(m.calls) - 1
	if i >= len(m.responses) {
		return canon.Completion{}, fmt.Errorf("script exhausted at call %d", i+1)
	}
	return canon.Completion{Text: m.responses[i], InputTokens: 100, OutputTokens: 200}, nil
}

// newMonsterServer builds a server with the encounters, homebrew and canon
// wired over a migrated database, plus a stubbed Open5e behind the search.
func newMonsterServer(t *testing.T, model canon.ModelClient) (*Server, *fixture, *index.Store) {
	t.Helper()
	store, err := index.Open(testdb.Path(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	encounters, err := encounter.New(store.DB())
	if err != nil {
		t.Fatalf("new encounter store: %v", err)
	}
	open5e := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"count":1,"results":[
			{"name":"Goblin","document":{"key":"srd-2024"},"challenge_rating":0.25,"type":{"name":"Humanoid"},"armor_class":15,"hit_points":7}
		]}`)
	}))
	t.Cleanup(open5e.Close)

	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	campaigns, err := campaign.New(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	var engine *canon.Store
	if model == nil {
		engine, err = canon.NewOffline(store.DB())
	} else {
		engine, err = canon.New(store.DB(), model, canon.Config{MaxCandidates: 500, BatchSize: 8})
	}
	if err != nil {
		t.Fatalf("canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)

	key := ""
	if model != nil {
		key = "test"
	}
	s, err := New(store, llm.New(llm.Config{APIKey: key}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	catalog, err := encounter.NewCatalog(store.DB(), open5e.URL)
	if err != nil {
		t.Fatal(err)
	}
	s = s.WithEncounters(encounters, encounter.NewBestiaryWithBase(open5e.URL), catalog)
	s = s.WithHomebrew(encounter.NewHomebrewStore(store.DB()))
	s = s.WithCampaigns(campaigns, knowledgeStore).WithCanon(engine)
	f := buildFixture(t, s)
	return s, &f, store
}

// handSaveBody is a hand-entered CR 7 the calculator agrees with: 168
// effective hit points against the assumed AC 15, 47 damage at +4.
const handSaveBody = `{
	"name": "Vashk, the Grave Marshal",
	"source": "hand",
	"requested_cr": "7",
	"encounter_role": "boss",
	"tactics": "Opens with the blade. Calls the ranks when hurt.",
	"lore": "A battlefield commander who kept commanding after dying. The Dead Bell rings, and the ranks form.",
	"statblock": {
		"name": "Vashk, the Grave Marshal", "size": "Medium", "type": "undead",
		"ac": 15, "hp": 168, "hit_dice": "21d8+74",
		"abilities": {"str": 18, "dex": 14, "con": 18, "int": 12, "wis": 14, "cha": 16},
		"actions": [
			{"name": "Graveblade", "kind": "ACTION",
			 "desc": "Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 47 (6d8 + 20) slashing damage."}
		]
	}
}`

func TestMonsterShelf_SaveComputesAndServes(t *testing.T) {
	s, _, _ := newMonsterServer(t, nil)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/monsters", handSaveBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: status %d, body %s", rec.Code, rec.Body)
	}
	var saved struct {
		Monster struct {
			ID          string `json:"id"`
			ComputedCR  string `json:"computed_cr"`
			RequestedCR string `json:"requested_cr"`
			Homebrew    bool   `json:"homebrew"`
			Detail      struct {
				Label string `json:"label"`
			} `json:"computed_detail"`
		} `json:"monster"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Monster.ComputedCR != "7" || saved.Monster.RequestedCR != "7" ||
		saved.Monster.Detail.Label != "7" || !saved.Monster.Homebrew {
		t.Fatalf("saved monster = %+v — the computed CR and reasoning must travel", saved.Monster)
	}

	// The computed pair is the store's, never the caller's: claim CR 30 in
	// the body and the arithmetic still says 7.
	rec = hit(t, s, http.MethodPost, "/api/monsters", strings.Replace(strings.Replace(handSaveBody,
		`"requested_cr": "7"`, `"requested_cr": "30"`, 1),
		"Vashk, the Grave Marshal", "The Claimed Marshal", -1), dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second save: %d %s", rec.Code, rec.Body)
	}
	var claimed struct {
		Monster struct {
			ComputedCR  string `json:"computed_cr"`
			RequestedCR string `json:"requested_cr"`
		} `json:"monster"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &claimed)
	if claimed.Monster.ComputedCR == "30" {
		t.Fatal("a claimed CR must never become the computed label")
	}

	// List and get carry the whole shape; a shapeless save is refused.
	if rec := hit(t, s, http.MethodGet, "/api/monsters", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	} else {
		var list struct {
			Monsters []map[string]any `json:"monsters"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &list)
		if len(list.Monsters) != 2 {
			t.Fatalf("list = %d monsters, want 2", len(list.Monsters))
		}
	}
	if rec := hit(t, s, http.MethodGet, "/api/monsters/"+saved.Monster.ID, "", dm); rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, "/api/monsters", `{"name":"Empty"}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("shapeless save: %d, want 400", rec.Code)
	}
	if rec := hit(t, s, http.MethodDelete, "/api/monsters/"+saved.Monster.ID, "", dm); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, "/api/monsters/"+saved.Monster.ID, "", dm); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d, want 404", rec.Code)
	}
}

// The design loop over HTTP: the first draft misses, the revision lands,
// and the response shows both the computed CR and the calculator's
// reasoning — plus the envelope the draft was designed inside.
func TestMonsterGenerate_CorrectedOverHTTP(t *testing.T) {
	off := `{
		"name": "Vashk, the Grave Marshal", "size": "Medium", "type": "undead",
		"ac": 15, "hp": 168, "hit_dice": "21d8+74", "speed": "30 ft.",
		"str": 18, "dex": 14, "con": 18, "int": 12, "wis": 14, "cha": 16,
		"trait1_name": "Marshal of the Dead", "trait1_desc": "The ranks hold.",
		"action1_name": "Graveblade",
		"action1_desc": "Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 27 (4d8 + 9) slashing damage.",
		"tactics": "Opens with the blade.", "lore": "The Dead Bell rings.",
		"role": "boss"
	}`
	on := strings.Replace(off, "27 (4d8 + 9)", "47 (6d8 + 20)", 1)
	model := &scriptedCanonModel{responses: []string{off, on}}
	s, _, _ := newMonsterServer(t, model)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/monsters/generate", `{
		"brief": "A CR 7 undead boss that fights like a battlefield commander", "cr": "7", "legendary": false
	}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Design struct {
			Revised   bool     `json:"revised"`
			Shortfall []string `json:"shortfall"`
			Rating    struct {
				Label string `json:"label"`
			} `json:"computed_detail"`
			Envelope struct {
				ProfBonus int `json:"prof_bonus"`
			} `json:"envelope"`
		} `json:"design"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Design.Revised || len(body.Design.Shortfall) != 0 || body.Design.Rating.Label != "7" {
		t.Fatalf("design = %+v, want revised and agreeing at 7", body.Design)
	}
	if body.Design.Envelope.ProfBonus != 3 {
		t.Fatalf("envelope prof bonus = %d", body.Design.Envelope.ProfBonus)
	}
	if !strings.Contains(model.calls[1], "damage per round is 18 short") {
		t.Fatal("the revision prompt did not carry the calculator's wording")
	}

	// An unreadable ask is a 400; a model-less install says so.
	if rec := hit(t, s, http.MethodPost, "/api/monsters/generate", `{"brief":"x","cr":"eleven"}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("unreadable CR: %d, want 400", rec.Code)
	}
}

// Without a model key the design loop is closed, but the shelf still
// serves: designing needs the model, saving needs none.
func TestMonsterGenerate_NeedsTheModel(t *testing.T) {
	s, _, _ := newMonsterServer(t, nil)
	s.llm = llm.New(llm.Config{})
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodPost, "/api/monsters/generate", `{"brief":"x","cr":"7"}`, dm)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("model gate: %d, want 503", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, "/api/monsters", handSaveBody, dm); rec.Code != http.StatusCreated {
		t.Fatalf("the shelf must not need the model: %d", rec.Code)
	}
}

// The placement stages one creature entity behind the review gate and
// writes nothing until the batch is decided.
func TestMonsterPlace_StagesBehindTheGate(t *testing.T) {
	s, f, store := newMonsterServer(t, nil)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/monsters", handSaveBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	var saved struct {
		Monster struct {
			ID string `json:"id"`
		} `json:"monster"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &saved)

	countEntities := func() int {
		var n int
		if err := store.DB().QueryRow(`SELECT COUNT(*) FROM entities WHERE campaign_id = ?`, f.campaignID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countEntities()

	// A player cannot place; the DM's placement is a staged batch.
	body := `{"campaign_id":` + quote(f.campaignID) + `}`
	player := addPlayerMember(t, s, *f, "monster-player", false)
	if rec := hit(t, s, http.MethodPost, "/api/monsters/"+saved.Monster.ID+"/place", body, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player place: %d, want 403", rec.Code)
	}
	rec = hit(t, s, http.MethodPost, "/api/monsters/"+saved.Monster.ID+"/place", body, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("place: %d %s", rec.Code, rec.Body)
	}
	var placed struct {
		Batch struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"batch"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &placed)
	if placed.Batch.Status != "open" {
		t.Fatalf("batch = %+v, want open", placed.Batch)
	}
	if countEntities() != before {
		t.Fatal("the placement wrote to the graph before the decision")
	}
}

// Homebrew rides the builder's reads: the search leads with it, the
// statblock endpoint renders it whole, and it is priced by the existing
// XP arithmetic.
func TestHomebrewRidesTheBuilder(t *testing.T) {
	s, f, _ := newMonsterServer(t, nil)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/monsters", handSaveBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}

	// The picker's search: the DM's own monster leads.
	rec = hit(t, s, http.MethodGet, "/api/encounter/monsters?q=vashk", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d", rec.Code)
	}
	var search struct {
		Monsters []struct {
			Name     string `json:"name"`
			Homebrew bool   `json:"homebrew"`
		} `json:"monsters"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &search)
	if len(search.Monsters) == 0 || !search.Monsters[0].Homebrew || search.Monsters[0].Name != "Vashk, the Grave Marshal" {
		t.Fatalf("search = %+v", search.Monsters)
	}

	// The statblock endpoint renders the homebrew creature, campaign-scoped.
	rec = hit(t, s, http.MethodGet, "/api/encounter/statblock?name=Vashk,+the+Grave+Marshal&campaign="+f.campaignID, "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("statblock: %d %s", rec.Code, rec.Body)
	}
	var creature struct {
		Creature struct {
			Homebrew bool `json:"homebrew"`
			XP       int  `json:"xp"`
			Attacks  []struct {
				ToHit int `json:"to_hit"`
			} `json:"attacks"`
		} `json:"creature"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &creature)
	if !creature.Creature.Homebrew || creature.Creature.XP != 2900 || len(creature.Creature.Attacks) != 1 {
		t.Fatalf("creature = %+v", creature.Creature)
	}
}
