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

// WithEncounters wires the encounter store and monster search client. It is
// separate from New so the encounter feature is additive for callers that do
// not use it; without it the encounter endpoints report unavailable.
func (s *Server) WithEncounters(store *encounter.Store, bestiary *encounter.Bestiary) *Server {
	s.encounters = store
	s.bestiary = bestiary
	return s
}

// encountersEnabled reports whether the encounter builder is wired, writing
// the error response when it is not.
func (s *Server) encountersEnabled(w http.ResponseWriter) bool {
	if s.encounters == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("encounter builder is not available"))
		return false
	}
	return true
}

// encounterView is the JSON shape of a saved encounter. The verdict is
// recomputed server-side from the stored party and monsters on every read.
type encounterView struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	Party     []int               `json:"party"`
	Monsters  []encounter.Monster `json:"monsters"`
	CreatedAt string              `json:"created_at"`
	UpdatedAt string              `json:"updated_at"`
	Verdict   encounter.Verdict   `json:"verdict"`
}

func toEncounterView(e encounter.Encounter) encounterView {
	return encounterView{
		ID: e.ID, Name: e.Name, Party: e.Party, Monsters: e.Monsters,
		CreatedAt: e.CreatedAt.Format(http.TimeFormat), UpdatedAt: e.UpdatedAt.Format(http.TimeFormat),
		Verdict: encounter.Evaluate(e.Party, e.Monsters),
	}
}

// encounterRequest is the writable half of an encounter. Unknown fields —
// including any attempt to post a difficulty or XP verdict — are ignored;
// XP is always re-derived from each monster's challenge rating.
type encounterRequest struct {
	Name     *string             `json:"name"`
	Party    []int               `json:"party"`
	Monsters []encounter.Monster `json:"monsters"`
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
	if s.bestiary == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("monster search is not available"))
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("q is required"))
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
	e, err := s.encounters.Create(r.Context(), userID(r), *req.Name, req.Party, monsters)
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
	e, err := s.encounters.Update(r.Context(), userID(r), r.PathValue("id"), req.Name, req.Party, req.Monsters, req.Party != nil, req.Monsters != nil)
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
