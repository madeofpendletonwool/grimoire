package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/encounter"
)

// The encounter builder's HTTP surface: SRD monster search plus owner-scoped
// saved encounters. Difficulty verdicts are computed here on every request —
// clients send party and monsters only, never a difficulty.

// WithEncounters wires the encounter store, the local SRD bestiary, and the
// remote monster search that stands in when the bestiary has not mirrored
// yet. It is separate from New so the encounter feature is additive for
// callers that do not use it; without it the encounter endpoints report
// unavailable.
func (s *Server) WithEncounters(store *encounter.Store, bestiary *encounter.Bestiary, catalog *encounter.Catalog) *Server {
	s.encounters = store
	s.bestiary = bestiary
	s.catalog = catalog
	return s
}

// WithHomebrew wires the homebrew monster store (MAD-382): the overlay the
// builder's reads and the monster designer save through. nil disables both.
func (s *Server) WithHomebrew(store *encounter.HomebrewStore) *Server {
	s.homebrew = store
	return s
}

// homebrewOverlay loads the caller's homebrew as a catalog overlay. With a
// campaign named, that campaign's designs ride along. Every failure
// degrades to no overlay — the builder must never stop because the
// homebrew shelf is unreadable — which is safe because nil is the plain
// catalog everywhere the overlay is taken.
func (s *Server) homebrewOverlay(r *http.Request, campaignID string) *encounter.Overlay {
	if s.homebrew == nil {
		return nil
	}
	list, err := s.homebrew.Overlay(r.Context(), userID(r), campaignID)
	if err != nil || len(list) == 0 {
		return nil
	}
	return encounter.NewOverlay(list)
}

// encountersEnabled reports whether the encounter builder is wired, writing
// the error response when it is not.
func (s *Server) encountersEnabled(w http.ResponseWriter) bool {
	if s.encounters == nil || s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("encounter builder is not available"))
		return false
	}
	return true
}

// encounterView is the JSON shape of a saved encounter. The verdict is
// recomputed server-side from the stored party and monsters on every read.
//
// The campaign fields (MAD-378) are omitted when empty, so an owner-scoped
// encounter serialises to exactly the object the builder has always received.
// The objective and its rendered ending (MAD-380) ride along the same way:
// absent unless the fight declares one.
type encounterView struct {
	ID             string               `json:"id"`
	Name           string               `json:"name"`
	Party          []int                `json:"party"`
	Monsters       []encounter.Monster  `json:"monsters"`
	Notes          string               `json:"notes"`
	CampaignID     string               `json:"campaign_id,omitempty"`
	SessionEventID string               `json:"session_event_id,omitempty"`
	SceneID        string               `json:"scene_id,omitempty"`
	Objective      *encounter.Objective `json:"objective,omitempty"`
	Ending         *encounter.Ending    `json:"ending,omitempty"`
	Terrain        *encounter.Terrain   `json:"terrain,omitempty"`
	Status         string               `json:"status,omitempty"`
	CreatedAt      string               `json:"created_at"`
	UpdatedAt      string               `json:"updated_at"`
	Verdict        encounter.Verdict    `json:"verdict"`
}

func toEncounterView(e encounter.Encounter) encounterView {
	v := encounterView{
		ID: e.ID, Name: e.Name, Party: e.Party, Monsters: e.Monsters, Notes: e.Notes,
		CampaignID: e.CampaignID, SessionEventID: e.SessionEventID, SceneID: e.SceneID,
		Objective: e.Objective, Terrain: e.Terrain, Status: e.Status,
		CreatedAt: e.CreatedAt.Format(http.TimeFormat), UpdatedAt: e.UpdatedAt.Format(http.TimeFormat),
		Verdict: encounter.Evaluate(e.Party, e.Monsters),
	}
	if e.Objective != nil {
		ending := e.Objective.Ending()
		v.Ending = &ending
	}
	return v
}

// encounterRequest is the writable half of an encounter. Unknown fields —
// including any attempt to post a difficulty or XP verdict — are ignored;
// XP is always re-derived from each monster's challenge rating. The objective
// and terrain pointers distinguish "not sent" from "sent": a PATCH without
// them leaves them alone, one with them validates against the declared
// vocabularies first — an unknown objective kind is a 400, never a default.
type encounterRequest struct {
	Name      *string              `json:"name"`
	Party     []int                `json:"party"`
	Monsters  []encounter.Monster  `json:"monsters"`
	Notes     *string              `json:"notes"`
	Objective *encounter.Objective `json:"objective"`
	Terrain   *encounter.Terrain   `json:"terrain"`
}

// validateObjective checks an optional objective at the API boundary. A nil
// pointer is "not sent"; an empty kind is "no objective"; anything else
// outside the vocabulary is refused.
func validateObjective(o *encounter.Objective) error {
	if o == nil {
		return nil
	}
	return o.Validate()
}

// validateTerrain checks an optional terrain block at the API boundary: a
// nil pointer is "not sent", anything sent must speak the declared
// vocabulary.
func validateTerrain(t *encounter.Terrain) error {
	if t == nil {
		return nil
	}
	return t.Validate()
}

// contentOptions turns the request's objective and terrain into store
// options. It assumes validateObjective and terrain validation already ran.
func contentOptions(req encounterRequest) []encounter.Option {
	var opts []encounter.Option
	if req.Objective != nil {
		opts = append(opts, encounter.WithObjective(*req.Objective))
	}
	if req.Terrain != nil {
		opts = append(opts, encounter.WithTerrain(*req.Terrain))
	}
	return opts
}

// maxEncounterSize bounds a stored encounter: enough for any sane table,
// small enough that nothing can wedge the server evaluating it.
const (
	maxPartySize     = 12
	maxMonsterRows   = 50
	maxMonstersTotal = 500
)

// validateParty checks levels and length. An empty party is allowed — a
// builder can save a monster roster before filling the party in; the verdict
// simply carries no difficulty until one exists.
func validateParty(party []int) error {
	if len(party) > maxPartySize {
		return fmt.Errorf("party is limited to %d characters", maxPartySize)
	}
	for _, lvl := range party {
		if lvl < 1 || lvl > 20 {
			return fmt.Errorf("party levels must be between 1 and 20")
		}
	}
	return nil
}

// normalizeMonsters re-derives every monster's XP from its challenge rating
// via the Monster Manual table and checks counts. A tampered payload cannot
// smuggle in its own XP values.
func normalizeMonsters(monsters []encounter.Monster) ([]encounter.Monster, error) {
	if len(monsters) > maxMonsterRows {
		return nil, fmt.Errorf("encounters are limited to %d monster kinds", maxMonsterRows)
	}
	total := 0
	for i := range monsters {
		m := &monsters[i]
		m.Name = strings.TrimSpace(m.Name)
		if m.Name == "" {
			return nil, fmt.Errorf("monster %d has no name", i+1)
		}
		xp, err := encounter.XPForCR(m.CR)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}
		m.XP = xp
		if m.Count < 1 || m.Count > 200 {
			return nil, fmt.Errorf("%s: count must be between 1 and 200", m.Name)
		}
		total += m.Count
	}
	if total > maxMonstersTotal {
		return nil, fmt.Errorf("encounters are limited to %d monsters", maxMonstersTotal)
	}
	return monsters, nil
}

// handleEncounterMonsters serves SRD monster search for the builder's picker.
// An unreachable Open5e degrades to a friendly notice rather than an error
// page: the response stays 200 with an empty list and a warning string.
func (s *Server) handleEncounterMonsters(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	if s.bestiary == nil && s.catalog.Count() == 0 {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("monster search is not available"))
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("q is required"))
		return
	}
	// The caller's homebrew leads the search when they have any; an optional
	// campaign scopes which designs ride along.
	overlay := s.homebrewOverlay(r, strings.TrimSpace(r.URL.Query().Get("campaign")))
	// The mirrored bestiary answers instantly and works offline; the remote
	// search is the fallback for an install that has not mirrored yet.
	if hits := s.catalog.Search(q, 0, overlay); len(hits) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"monsters": hits, "source": "local"})
		return
	}
	if s.bestiary == nil {
		writeJSON(w, http.StatusOK, map[string]any{"monsters": []encounter.MonsterSummary{}})
		return
	}
	hits, err := s.bestiary.Search(r.Context(), q)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"monsters": []encounter.MonsterSummary{},
			"warning":  "the bestiary could not be reached — try again in a moment",
		})
		return
	}
	if hits == nil {
		hits = []encounter.MonsterSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"monsters": hits})
}

// handleEvaluate previews an encounter's difficulty without saving anything:
// the live meter in the UI hits this, keeping the XP math in one place —
// the server — instead of duplicating the tables in JavaScript.
func (s *Server) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	var req encounterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	monsters, err := normalizeMonsters(req.Monsters)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateParty(req.Party); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"verdict": encounter.Evaluate(req.Party, monsters)})
}

func (s *Server) handleListEncounters(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	list, err := s.encounters.List(r.Context(), userID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]encounterView, 0, len(list))
	for _, e := range list {
		views = append(views, toEncounterView(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"encounters": views})
}

func (s *Server) handleCreateEncounter(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	var req encounterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name is required"))
		return
	}
	if err := validateParty(req.Party); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	monsters, err := normalizeMonsters(req.Monsters)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateObjective(req.Objective); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateTerrain(req.Terrain); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	notes := ""
	if req.Notes != nil {
		notes = *req.Notes
	}
	e, err := s.encounters.Create(r.Context(), userID(r), *req.Name, req.Party, monsters, notes,
		contentOptions(req)...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"encounter": toEncounterView(e)})
}

func (s *Server) handleGetEncounter(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	e, err := s.encounters.Get(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, encounter.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"encounter": toEncounterView(e)})
}

func (s *Server) handleUpdateEncounter(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	var req encounterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if req.Party != nil {
		if err := validateParty(req.Party); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if req.Monsters != nil {
		normalized, err := normalizeMonsters(req.Monsters)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.Monsters = normalized
	}
	if err := validateObjective(req.Objective); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateTerrain(req.Terrain); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	e, err := s.encounters.Update(r.Context(), userID(r), r.PathValue("id"), req.Name, req.Party, req.Monsters, req.Notes, req.Party != nil, req.Monsters != nil,
		contentOptions(req)...)
	if err != nil {
		if errors.Is(err, encounter.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"encounter": toEncounterView(e)})
}

func (s *Server) handleDeleteEncounter(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	if err := s.encounters.Delete(r.Context(), userID(r), r.PathValue("id")); err != nil {
		if errors.Is(err, encounter.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
