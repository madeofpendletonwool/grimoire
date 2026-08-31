package server

// The dungeon designer's HTTP surface (MAD-373): the seeded room graph a
// DM designs, edits and dresses, staged into the world only on request.
// DM-only, like every planning surface — but creating, editing and
// mapping a dungeon needs no model key at all: the layout is pure
// arithmetic (internal/dungeon); only the dressing pass asks the model.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/dungeon"
)

/* ---------- views ---------- */

type dungeonRoomView struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Name        string `json:"name,omitempty"`
	Purpose     string `json:"purpose"`
	Detail      string `json:"detail,omitempty"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	Depth       int    `json:"depth"`
	EntityID    string `json:"entity_id,omitempty"`
	EncounterID string `json:"encounter_id,omitempty"`
}

type dungeonEdgeView struct {
	ID            string `json:"id"`
	FromRoom      string `json:"from_room"`
	ToRoom        string `json:"to_room"`
	Kind          string `json:"kind"`
	KeyItemEntity string `json:"key_item_entity,omitempty"`
	OneWay        bool   `json:"one_way,omitempty"`
}

type dungeonView struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Theme            string            `json:"theme,omitempty"`
	Size             string            `json:"size"`
	Level            int               `json:"level"`
	ExpectedSessions int               `json:"expected_sessions"`
	Seed             int64             `json:"seed"`
	Params           dungeon.Params    `json:"params"`
	KeyItem          string            `json:"key_item,omitempty"`
	Secret           string            `json:"secret,omitempty"`
	BossName         string            `json:"boss_name,omitempty"`
	LocationEntity   string            `json:"location_entity,omitempty"`
	Status           string            `json:"status"`
	Rooms            []dungeonRoomView `json:"rooms"`
	Edges            []dungeonEdgeView `json:"edges"`
	CreatedAt        string            `json:"created_at"`
	UpdatedAt        string            `json:"updated_at"`
}

func toDungeonView(d *campaign.Dungeon) dungeonView {
	rooms := make([]dungeonRoomView, 0, len(d.Rooms))
	for _, r := range d.Rooms {
		rooms = append(rooms, dungeonRoomView{
			ID: r.ID, Key: r.Key, Name: r.Name, Purpose: r.Purpose, Detail: r.Detail,
			X: r.X, Y: r.Y, Depth: r.Depth, EntityID: r.EntityID, EncounterID: r.EncounterID,
		})
	}
	edges := make([]dungeonEdgeView, 0, len(d.Edges))
	for _, e := range d.Edges {
		edges = append(edges, dungeonEdgeView{
			ID: e.ID, FromRoom: e.FromRoom, ToRoom: e.ToRoom, Kind: e.Kind,
			KeyItemEntity: e.KeyItemEntity, OneWay: e.OneWay,
		})
	}
	return dungeonView{
		ID: d.ID, Name: d.Name, Theme: d.Theme, Size: d.Size, Level: d.Level,
		ExpectedSessions: d.ExpectedSessions, Seed: d.Seed, Params: d.Params,
		KeyItem: d.KeyItem, Secret: d.Secret, BossName: d.BossName,
		LocationEntity: d.LocationEntity, Status: d.Status,
		Rooms: rooms, Edges: edges,
		CreatedAt: d.CreatedAt.Format(http.TimeFormat),
		UpdatedAt: d.UpdatedAt.Format(http.TimeFormat),
	}
}

/* ---------- the dungeon rows ---------- */

func (s *Server) handleCampaignDungeons(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	dungeons, err := s.campaigns.ListDungeons(r.Context(), campaign.ScopeDM, a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]dungeonView, 0, len(dungeons))
	for i := range dungeons {
		v := toDungeonView(&dungeons[i])
		v.Rooms = nil // the listing carries headlines; rooms load per dungeon
		v.Edges = nil
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"dungeons": views})
}

func (s *Server) handleCreateCampaignDungeon(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Name             string `json:"name"`
		Theme            string `json:"theme"`
		Size             string `json:"size"`
		Level            int    `json:"level"`
		ExpectedSessions int    `json:"expected_sessions"`
		CombatDensity    int    `json:"combat_density"`
		PuzzleDensity    int    `json:"puzzle_density"`
		ExploreDensity   int    `json:"explore_density"`
		Branchiness      int    `json:"branchiness"`
		Seed             int64  `json:"seed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	d, err := s.campaigns.CreateDungeon(r.Context(), a.campaign.ID, campaign.DungeonInput{
		Name:  req.Name,
		Theme: req.Theme,
		Params: dungeon.Params{
			Theme: req.Theme, Size: req.Size, Level: req.Level,
			ExpectedSessions: req.ExpectedSessions,
			CombatDensity:    req.CombatDensity, PuzzleDensity: req.PuzzleDensity,
			ExploreDensity: req.ExploreDensity, Branchiness: req.Branchiness,
		},
		Seed: req.Seed,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"dungeon": toDungeonView(d)})
}

func (s *Server) handleCampaignDungeon(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	d, err := s.campaigns.GetDungeon(r.Context(), campaign.ScopeDM, a.campaign.ID, r.PathValue("did"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dungeon": toDungeonView(d)})
}

func (s *Server) handleUpdateCampaignDungeon(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Name  *string `json:"name"`
		Theme *string `json:"theme"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	d, err := s.campaigns.UpdateDungeon(r.Context(), a.campaign.ID, r.PathValue("did"),
		campaign.DungeonUpdate{Name: req.Name, Theme: req.Theme})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dungeon": toDungeonView(d)})
}

func (s *Server) handleDeleteCampaignDungeon(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if err := s.campaigns.DeleteDungeon(r.Context(), a.campaign.ID, r.PathValue("did")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

/* ---------- room edits: dragging a room writes the cell back ---------- */

func (s *Server) handleUpdateDungeonRoom(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Name        *string `json:"name"`
		Detail      *string `json:"detail"`
		X           *int    `json:"x"`
		Y           *int    `json:"y"`
		EncounterID *string `json:"encounter_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	d, err := s.campaigns.UpdateDungeonRoom(r.Context(), a.campaign.ID, r.PathValue("did"), r.PathValue("rid"),
		campaign.DungeonRoomUpdate{
			Name: req.Name, Detail: req.Detail, X: req.X, Y: req.Y, EncounterID: req.EncounterID,
		})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dungeon": toDungeonView(d)})
}

/* ---------- edge edits: adding and cutting connections ---------- */

func (s *Server) handleAddDungeonEdge(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		FromRoom      string `json:"from"`
		ToRoom        string `json:"to"`
		Kind          string `json:"kind"`
		OneWay        bool   `json:"one_way"`
		KeyItemEntity string `json:"key_item_entity"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	d, err := s.campaigns.AddDungeonEdge(r.Context(), a.campaign.ID, r.PathValue("did"),
		campaign.DungeonEdgeInput{
			FromRoom: req.FromRoom, ToRoom: req.ToRoom, Kind: req.Kind,
			OneWay: req.OneWay, KeyItemEntity: req.KeyItemEntity,
		})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"dungeon": toDungeonView(d)})
}

func (s *Server) handleDeleteDungeonEdge(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	d, err := s.campaigns.RemoveDungeonEdge(r.Context(), a.campaign.ID, r.PathValue("did"), r.PathValue("eid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dungeon": toDungeonView(d)})
}

/* ---------- the model pass and the placement ---------- */

func (s *Server) handleDungeonDress(w http.ResponseWriter, r *http.Request) {
	if !s.canonEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.llm.Configured() {
		writeError(w, http.StatusServiceUnavailable, errors.New(
			"dressing a dungeon needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}
	res, err := s.canon.DressDungeon(r.Context(), canon.DungeonDressInput{
		CampaignID: a.campaign.ID, DungeonID: r.PathValue("did"), CreatedBy: userID(r),
	})
	if err != nil {
		if canon.IsOffline(err) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dungeon": toDungeonView(res.Dungeon)})
}

func (s *Server) handleDungeonPlace(w http.ResponseWriter, r *http.Request) {
	if !s.canonEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	res, err := s.canon.PlaceDungeon(r.Context(), canon.DungeonPlaceInput{
		CampaignID: a.campaign.ID, DungeonID: r.PathValue("did"), CreatedBy: userID(r),
	})
	if err != nil {
		if canon.IsOffline(err) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	body := map[string]any{"dungeon_id": r.PathValue("did")}
	if res.Batch != nil {
		body["batch"] = toBatchView(*res.Batch, true)
	}
	writeJSON(w, http.StatusCreated, body)
}
