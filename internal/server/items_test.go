package server

// The item designer's handler tests (MAD-383): the shelf's save contract
// through the HTTP surface, the design comparison that rejects the broken
// and compares the sound — never computing a rarity — the homebrew
// overlay riding the picker's search, and the placement staged behind the
// review gate.

import (
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
	"github.com/madeofpendletonwool/grimoire/internal/items"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// newItemServer builds a server with the item catalog, the homebrew item
// shelf, the campaign graph and the canon engine wired over a migrated
// database, plus a stubbed Open5e behind the mirror.
func newItemServer(t *testing.T) (*Server, *fixture, *index.Store) {
	t.Helper()
	store, err := index.Open(testdb.Path(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	open5e := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"next":"","results":[
			{"key":"flame-tongue","name":"Flame Tongue","document":{"key":"srd-2024"},
			 "category":{"name":"Weapon","key":"weapon"},"rarity":{"name":"Rare","rank":2},
			 "requires_attunement":true,
			 "desc":"You gain a +1 bonus to attack and damage rolls made with this magic weapon. When you hit with it, the target takes an extra 2d6 fire damage."}
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
	engine, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)

	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	catalog, err := items.NewCatalog(store.DB(), open5e.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Sync(t.Context()); err != nil {
		t.Fatalf("sync item catalog: %v", err)
	}
	encounters, err := encounter.New(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	s = s.WithItems(catalog, items.NewHomebrewStore(store.DB()))
	s = s.WithEncounters(encounters, nil, nil)
	s = s.WithCampaigns(campaigns, knowledgeStore).WithCanon(engine)
	f := buildFixture(t, s)
	return s, &f, store
}

// emberbrandBody is a sound weapon design: base item, bonus inside the
// ceiling, attunement, and a damage rider in the game's own vocabulary.
const emberbrandBody = `{
	"name": "Emberbrand",
	"notes": "Forged for the siege of Blackwater.",
	"design": {
		"name": "Emberbrand", "type": "weapon", "base": "longsword", "bonus": 1,
		"attunement": {"required": true},
		"effects": [{"text": "When you hit with it, the target takes an extra 1d6 fire damage.", "damage": "1d6 fire"}]
	}
}`

func TestItemShelf_SaveRejectsAndServes(t *testing.T) {
	s, _, _ := newItemServer(t)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/items/homebrew", emberbrandBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: status %d, body %s", rec.Code, rec.Body)
	}
	var saved struct {
		Item struct {
			ID              string   `json:"id"`
			RequestedRarity string   `json:"requested_rarity"`
			Tags            []string `json:"tags"`
			Homebrew        bool     `json:"homebrew"`
			Design          struct {
				Rarity string `json:"rarity"`
			} `json:"design"`
		} `json:"item"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Item.Homebrew || saved.Item.RequestedRarity != "" {
		t.Fatalf("saved item = %+v — the homebrew marking must travel", saved.Item)
	}
	for _, want := range []string{"homebrew", "weapon", "damage-rider"} {
		found := false
		for _, tag := range saved.Item.Tags {
			if tag == want {
				found = true
			}
		}
		if !found {
			t.Errorf("tags %v, want %q", saved.Item.Tags, want)
		}
	}

	// The rarity is the DM's label, echoed: claim one and it rides
	// verbatim. There is no computed half to contradict it.
	claimed := strings.Replace(emberbrandBody, `"bonus": 1`, `"bonus": 1, "rarity": "Rare"`, 1)
	claimed = strings.Replace(claimed, "Emberbrand", "Emberbrand II", -1)
	rec = hit(t, s, http.MethodPost, "/api/items/homebrew", claimed, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("claimed save: %d %s", rec.Code, rec.Body)
	}
	var claimedBody struct {
		Item struct {
			RequestedRarity string `json:"requested_rarity"`
		} `json:"item"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &claimedBody)
	if claimedBody.Item.RequestedRarity != "Rare" {
		t.Fatalf("requested rarity = %q, want the DM's own label echoed", claimedBody.Item.RequestedRarity)
	}

	// A malformed design is refused with the rule it broke named.
	broken := strings.Replace(emberbrandBody,
		`"effects": [{"text": "When you hit with it, the target takes an extra 1d6 fire damage.", "damage": "1d6 fire"}]`,
		`"effects": [{"text": "The target is weakened."}]`, 1)
	if rec := hit(t, s, http.MethodPost, "/api/items/homebrew", broken, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("broken save: %d, want 400", rec.Code)
	} else if !strings.Contains(rec.Body.String(), "no game vocabulary") {
		t.Fatalf("broken save body = %s, want the specific rule", rec.Body)
	}

	// List, get, delete, and the 404 after.
	if rec := hit(t, s, http.MethodGet, "/api/items/homebrew", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, "/api/items/homebrew/"+saved.Item.ID, "", dm); rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodDelete, "/api/items/homebrew/"+saved.Item.ID, "", dm); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, "/api/items/homebrew/"+saved.Item.ID, "", dm); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d, want 404", rec.Code)
	}
}

// The design endpoint rejects the structurally broken and compares the
// sound. The comparison is asserted in the response shape itself: bands,
// notes and neighbours — and no field anywhere that computes, implies or
// labels a rarity.
func TestItemDesign_RejectsAndCompares(t *testing.T) {
	s, _, _ := newItemServer(t)
	dm := dmSession(t, s)

	broken := `{"name": "Emberbrand", "type": "weapon", "base": "longsword",
		"effects": [{"text": "The target is weakened."}]}`
	rec := hit(t, s, http.MethodPost, "/api/items/design", broken, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("broken design: %d, want 400", rec.Code)
	}
	var rejected struct {
		Report struct {
			Problems []string `json:"problems"`
		} `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rejected); err != nil {
		t.Fatal(err)
	}
	if len(rejected.Report.Problems) == 0 {
		t.Fatal("the rejection carries no problems")
	}

	sound := `{"name": "Emberbrand", "type": "weapon", "base": "longsword", "bonus": 3, "rarity": "Uncommon",
		"attunement": {"required": true},
		"effects": [{"text": "When you hit with it, the target takes an extra 1d6 fire damage.", "damage": "1d6 fire"}]}`
	rec = hit(t, s, http.MethodPost, "/api/items/design", sound, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("sound design: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Report struct {
			Metrics struct {
				Bonus int `json:"bonus"`
			} `json:"metrics"`
			Rarity string `json:"rarity"`
			Bands  []struct {
				Rarity string `json:"rarity"`
				Count  int    `json:"count"`
			} `json:"bands"`
			Notes      []string `json:"notes"`
			Neighbours []struct {
				Name     string `json:"name"`
				Homebrew bool   `json:"homebrew"`
			} `json:"neighbours"`
		} `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Report.Metrics.Bonus != 3 || body.Report.Rarity != "Uncommon" {
		t.Fatalf("report = %+v, want the design's own numbers echoed", body.Report)
	}
	if len(body.Report.Bands) == 0 {
		t.Fatal("the comparison carries no bands")
	}
	// "+3 at Uncommon" against a shelf that carries no +3 at all: the
	// note is the checkable claim, not a verdict.
	claimed := false
	for _, n := range body.Report.Notes {
		if strings.Contains(n, "No SRD item in the mirror carries a +3 bonus") {
			claimed = true
		}
	}
	if !claimed {
		t.Errorf("notes %v, want the checkable claim about +3s", body.Report.Notes)
	}
	if len(body.Report.Neighbours) == 0 || body.Report.Neighbours[0].Name != "Flame Tongue" {
		t.Errorf("neighbours = %+v, want Flame Tongue as the yardstick", body.Report.Neighbours)
	}
	// The shape itself is the honesty: no computed rarity, no verdict.
	raw := rec.Body.String()
	for _, forbidden := range []string{"computed_rarity", "computed", "verdict", "rarity_score"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("response contains %q — the designer must not compute", forbidden)
		}
	}
}

// The picker's search leads with the DM's own items, flagged.
func TestItems_SearchLeadsWithHomebrew(t *testing.T) {
	s, _, _ := newItemServer(t)
	dm := dmSession(t, s)

	if rec := hit(t, s, http.MethodPost, "/api/items/homebrew", emberbrandBody, dm); rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}

	rec := hit(t, s, http.MethodGet, "/api/items?q=ember", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d", rec.Code)
	}
	var search struct {
		Items []struct {
			Name     string `json:"name"`
			Homebrew bool   `json:"homebrew"`
		} `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &search)
	if len(search.Items) == 0 || !search.Items[0].Homebrew || search.Items[0].Name != "Emberbrand" {
		t.Fatalf("search = %+v, want the flagged homebrew hit first", search.Items)
	}

	// A filter browse works without a query, and the overlay competes
	// under the same gates.
	rec = hit(t, s, http.MethodGet, "/api/items?type=weapon", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("browse: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &search); err != nil {
		t.Fatal(err)
	}
	for _, it := range search.Items {
		if !it.Homebrew {
			t.Fatalf("browse = %+v, want the weapon gate to hold", search.Items)
		}
	}
}

// The placement stages one item entity behind the review gate and writes
// nothing until the batch is decided. A player cannot place.
func TestItemPlace_StagesBehindTheGate(t *testing.T) {
	s, f, store := newItemServer(t)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/items/homebrew", emberbrandBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	var saved struct {
		Item struct {
			ID string `json:"id"`
		} `json:"item"`
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

	body := `{"campaign_id":` + quote(f.campaignID) + `}`
	player := addPlayerMember(t, s, *f, "item-player", false)
	if rec := hit(t, s, http.MethodPost, "/api/items/homebrew/"+saved.Item.ID+"/place", body, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player place: %d, want 403", rec.Code)
	}
	rec = hit(t, s, http.MethodPost, "/api/items/homebrew/"+saved.Item.ID+"/place", body, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("place: %d %s", rec.Code, rec.Body)
	}
	var placed struct {
		Batch struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Source string `json:"source"`
		} `json:"batch"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &placed)
	if placed.Batch.Status != "open" || placed.Batch.Source != "item" {
		t.Fatalf("batch = %+v, want an open item batch", placed.Batch)
	}
	if countEntities() != before {
		t.Fatal("the placement wrote to the graph before the decision")
	}
}

// Without the catalog wired, the item endpoints say so rather than
// pretending.
func TestItems_UnavailableWithoutWiring(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	adminSession(t, s)
	dm := dmSession(t, s)
	for _, path := range []string{"/api/items", "/api/items/homebrew"} {
		if rec := hit(t, s, http.MethodGet, path, "", dm); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s unwired: %d, want 503", path, rec.Code)
		}
	}
	if rec := hit(t, s, http.MethodPost, "/api/items/design", `{}`, dm); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /api/items/design unwired: %d, want 503", rec.Code)
	}
}
