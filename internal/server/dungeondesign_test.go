package server

// The dungeon designer's handler tests (MAD-373): creation needs no
// model, scope enforcement, room and edge edits through the HTTP
// surface, the dress pass's model gate, and the placing pass staged and
// decided through the review gate.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/dungeon"
)

const dungeonBody = `{
	"name": "The Sunken Reliquary",
	"theme": "a drowned temple beneath the marsh",
	"size": "delve", "level": 4, "expected_sessions": 1,
	"combat_density": 1, "puzzle_density": 1, "explore_density": 1,
	"branchiness": 1, "seed": 4242
}`

// dungeonFill scripts a full dress fill for the dungeon the create
// handler made (same seed and params).
func dungeonFill(t *testing.T) string {
	t.Helper()
	graph, err := dungeon.Layout(dungeon.Params{
		Theme: "a drowned temple beneath the marsh", Size: dungeon.SizeDelve,
		Level: 4, ExpectedSessions: 1,
		CombatDensity: 1, PuzzleDensity: 1, ExploreDensity: 1, Branchiness: 1,
	}, 4242)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{
		"boss_name":        "Vesh of the Drowned Bell",
		"boss_motive":      "Vesh wants the bell rung again, whatever wakes.",
		"secret_statement": "The temple sank on purpose.",
		"key_item_name":    "the Pale Key",
	}
	for _, r := range graph.Rooms {
		m["room_"+r.Key+"_name"] = fmt.Sprintf("The %s of %s", r.Purpose, r.Key)
		m["room_"+r.Key+"_detail"] = "Water on the steps, and something counting them."
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// createDungeon creates one dungeon through the API and returns its id.
func createDungeon(t *testing.T, s *Server, f *fixture, dm *http.Cookie) string {
	t.Helper()
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/dungeons", dungeonBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create dungeon: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Dungeon struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Rooms  []struct {
				ID  string `json:"id"`
				Key string `json:"key"`
				X   int    `json:"x"`
				Y   int    `json:"y"`
			} `json:"rooms"`
			Edges []struct {
				ID   string `json:"id"`
				From string `json:"from_room"`
				To   string `json:"to_room"`
			} `json:"edges"`
		} `json:"dungeon"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if body.Dungeon.ID == "" || body.Dungeon.Status != "draft" || len(body.Dungeon.Rooms) == 0 {
		t.Fatalf("bad dungeon view: %+v", body.Dungeon)
	}
	return body.Dungeon.ID
}

func TestDungeon_CreateNeedsNoModel(t *testing.T) {
	s, f, _, store := newSkeletonServer(t)
	// An offline canon engine: creation must still work — the layout is
	// pure arithmetic, the promise of the separation.
	offline, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("offline engine: %v", err)
	}
	s.canon = offline.WithGraphStores(s.campaigns, s.knowledge)
	dm := dmSession(t, s)
	createDungeon(t, s, f, dm)
}

func TestDungeon_ScopeAndValidation(t *testing.T) {
	s, f, _, _ := newSkeletonServer(t)
	player := addPlayerMember(t, s, *f, "mira", true)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/dungeons", dungeonBody, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player create status = %d, want 403", rec.Code)
	}
	dm := dmSession(t, s)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/dungeons", `{"name":"x","size":"castle"}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad size status = %d, want 400", rec.Code)
	}
	did := createDungeon(t, s, f, dm)
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/dungeons", "", player); rec.Code != http.StatusForbidden {
		t.Fatalf("player list status = %d, want 403", rec.Code)
	}
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/dungeons", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("dm list status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "The Sunken Reliquary") {
		t.Fatalf("listing missing the dungeon: %s", rec.Body)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/dungeons/"+did, "", player); rec.Code != http.StatusForbidden {
		t.Fatalf("player get status = %d, want 403", rec.Code)
	}
}

func TestDungeon_RoomAndEdgeEdits(t *testing.T) {
	s, f, _, _ := newSkeletonServer(t)
	dm := dmSession(t, s)
	did := createDungeon(t, s, f, dm)

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/dungeons/"+did, "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	var view struct {
		Dungeon struct {
			Rooms []struct {
				ID  string `json:"id"`
				Key string `json:"key"`
				X   int    `json:"x"`
				Y   int    `json:"y"`
			} `json:"rooms"`
			Edges []struct {
				ID   string `json:"id"`
				From string `json:"from_room"`
				To   string `json:"to_room"`
			} `json:"edges"`
		} `json:"dungeon"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	rooms := view.Dungeon.Rooms
	nEdges := len(view.Dungeon.Edges)

	// Drag the last room to a free cell and name it.
	victim := rooms[len(rooms)-1]
	x, y := victim.X+3, victim.Y+2
	rec = hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/dungeons/"+did+"/rooms/"+victim.ID,
		fmt.Sprintf(`{"x":%d,"y":%d,"name":"The Bone Choir"}`, x, y), dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch room: %d %s", rec.Code, rec.Body)
	}
	// Onto an occupied cell is refused.
	other := rooms[0]
	rec = hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/dungeons/"+did+"/rooms/"+other.ID,
		fmt.Sprintf(`{"x":%d,"y":%d}`, x, y), dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("occupied cell accepted: %d", rec.Code)
	}

	// Add an edge between two unconnected rooms, then cut it.
	var from, to string
	for _, a := range rooms {
		if a.Key == victim.Key {
			continue
		}
		connected := false
		for _, e := range view.Dungeon.Edges {
			if (e.From == a.Key && e.To == victim.Key) || (e.From == victim.Key && e.To == a.Key) {
				connected = true
			}
		}
		if !connected {
			from, to = a.Key, victim.Key
			break
		}
	}
	if from == "" {
		t.Fatal("no unconnected pair")
	}
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/dungeons/"+did+"/edges",
		fmt.Sprintf(`{"from":%q,"to":%q,"kind":"passage"}`, from, to), dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add edge: %d %s", rec.Code, rec.Body)
	}
	var added struct {
		Dungeon struct {
			Edges []struct {
				ID string `json:"id"`
			} `json:"edges"`
		} `json:"dungeon"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if len(added.Dungeon.Edges) != nEdges+1 {
		t.Fatalf("edges %d after add, want %d", len(added.Dungeon.Edges), nEdges+1)
	}
	newEdge := added.Dungeon.Edges[len(added.Dungeon.Edges)-1]
	rec = hit(t, s, http.MethodDelete, "/api/campaigns/"+f.campaignID+"/dungeons/"+did+"/edges/"+newEdge.ID, "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("cut edge: %d", rec.Code)
	}

	// Deleting the design works while it is unplaced.
	rec = hit(t, s, http.MethodDelete, "/api/campaigns/"+f.campaignID+"/dungeons/"+did, "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
}

func TestDungeon_DressAndPlaceThroughTheGate(t *testing.T) {
	s, f, model, _ := newSkeletonServer(t)
	dm := dmSession(t, s)
	did := createDungeon(t, s, f, dm)

	// Dress with the fake model's scripted fill.
	model.response = dungeonFill(t)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/dungeons/"+did+"/dress", `{}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("dress: %d %s", rec.Code, rec.Body)
	}
	var dressed struct {
		Dungeon struct {
			Status   string `json:"status"`
			KeyItem  string `json:"key_item"`
			BossName string `json:"boss_name"`
			Rooms    []struct {
				Name string `json:"name"`
			} `json:"rooms"`
		} `json:"dungeon"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dressed); err != nil {
		t.Fatal(err)
	}
	if dressed.Dungeon.Status != "dressed" || dressed.Dungeon.KeyItem != "the Pale Key" {
		t.Fatalf("dress did not land: %+v", dressed.Dungeon)
	}
	for _, r := range dressed.Dungeon.Rooms {
		if r.Name == "" {
			t.Fatal("a room came back unnamed")
		}
	}

	// Place: a proposal batch, nothing written yet.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/dungeons/"+did+"/place", `{}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("place: %d %s", rec.Code, rec.Body)
	}
	var placed struct {
		Batch struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		} `json:"batch"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &placed); err != nil {
		t.Fatal(err)
	}
	if placed.Batch.ID == "" || placed.Batch.Source != "dungeon" {
		t.Fatalf("no batch staged: %+v", placed.Batch)
	}

	// Decide the batch through the proposals surface; the dungeon is
	// marked placed with every room's entity recorded.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals/"+placed.Batch.ID+"/decision",
		`{"decision":"accept"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/dungeons/"+did, "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-read: %d", rec.Code)
	}
	var after struct {
		Dungeon struct {
			Status         string `json:"status"`
			LocationEntity string `json:"location_entity"`
			Rooms          []struct {
				EntityID string `json:"entity_id"`
			} `json:"rooms"`
		} `json:"dungeon"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if after.Dungeon.Status != "placed" || after.Dungeon.LocationEntity == "" {
		t.Fatalf("not placed after decision: %+v", after.Dungeon)
	}
	for _, r := range after.Dungeon.Rooms {
		if r.EntityID == "" {
			t.Fatal("a room has no entity after placement")
		}
	}

	// A placed dungeon refuses deletion.
	if rec := hit(t, s, http.MethodDelete, "/api/campaigns/"+f.campaignID+"/dungeons/"+did, "", dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("placed delete status = %d, want 400", rec.Code)
	}
}
