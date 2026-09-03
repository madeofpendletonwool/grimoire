package server

// The monster designer's HTTP surface (MAD-382): the brief-to-statblock
// loop, the homebrew shelf it saves onto, and the placement that stages a
// designed creature into a campaign through the proposal batch. Designing
// needs the model; the shelf and the placement need none.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

/* ---------- views ---------- */

// monsterView is one homebrew monster as the wire carries it: the full
// statblock, the CR the brief asked for, the CR the calculator computed,
// the calculator's whole reasoning, and the designer's prose. The
// requested/computed pair is the labelling — a disagreement is shown, not
// hidden.
func monsterView(m *encounter.HomebrewMonster) map[string]any {
	v := map[string]any{
		"id": m.ID, "name": m.Name, "slug": m.Slug,
		"statblock":       m.Statblock,
		"requested_cr":    m.RequestedCR,
		"computed_cr":     m.ComputedCR,
		"computed_detail": m.Rating,
		"tactics":         m.Tactics,
		"lore":            m.Lore,
		"encounter_role":  m.Role,
		"source":          m.Source,
		"homebrew":        true,
		"created_at":      m.CreatedAt.Format(http.TimeFormat),
		"updated_at":      m.UpdatedAt.Format(http.TimeFormat),
	}
	if m.CampaignID != "" {
		v["campaign_id"] = m.CampaignID
	}
	return v
}

/* ---------- the shelf ---------- */

// homebrewEnabled reports whether the homebrew store is wired, writing the
// error response when it is not.
func (s *Server) homebrewEnabled(w http.ResponseWriter) bool {
	if s.homebrew == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("homebrew monsters are not enabled on this install"))
		return false
	}
	return true
}

// writeHomebrewError maps the store's sentinels, the encounter surface's
// own rule rather than writeStoreError's campaign set.
func writeHomebrewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, encounter.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, encounter.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeStoreError(w, err)
	}
}

func (s *Server) handleListMonsters(w http.ResponseWriter, r *http.Request) {
	if !s.homebrewEnabled(w) {
		return
	}
	list, err := s.homebrew.List(r.Context(), userID(r), strings.TrimSpace(r.URL.Query().Get("campaign")))
	if err != nil {
		writeHomebrewError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, monsterView(&list[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"monsters": out})
}

func (s *Server) handleCreateMonster(w http.ResponseWriter, r *http.Request) {
	if !s.homebrewEnabled(w) {
		return
	}
	var req struct {
		Name        string               `json:"name"`
		CampaignID  string               `json:"campaign_id"`
		Statblock   *statblock.Statblock `json:"statblock"`
		RequestedCR string               `json:"requested_cr"`
		Tactics     string               `json:"tactics"`
		Lore        string               `json:"lore"`
		Role        string               `json:"encounter_role"`
		Source      string               `json:"source"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	// A campaign-scoped save is a write into that campaign's material:
	// the DM gate applies, exactly like every other campaign write.
	if req.CampaignID != "" {
		a := s.resolveCampaignAccess(w, r, req.CampaignID)
		if a == nil || !a.requireDM(w) {
			return
		}
	}
	m, err := s.homebrew.Save(r.Context(), userID(r), "", encounter.HomebrewInput{
		Name: req.Name, CampaignID: req.CampaignID,
		Statblock:   req.Statblock,
		RequestedCR: req.RequestedCR, Tactics: req.Tactics, Lore: req.Lore,
		Role: req.Role, Source: req.Source,
	})
	if err != nil {
		writeHomebrewError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"monster": monsterView(m)})
}

func (s *Server) handleGetMonster(w http.ResponseWriter, r *http.Request) {
	if !s.homebrewEnabled(w) {
		return
	}
	m, err := s.homebrew.Get(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		writeHomebrewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"monster": monsterView(m)})
}

func (s *Server) handleDeleteMonster(w http.ResponseWriter, r *http.Request) {
	if !s.homebrewEnabled(w) {
		return
	}
	if err := s.homebrew.Delete(r.Context(), userID(r), r.PathValue("id")); err != nil {
		writeHomebrewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

/* ---------- the designer loop ---------- */

func (s *Server) handleMonsterGenerate(w http.ResponseWriter, r *http.Request) {
	if !s.canonEnabled(w) {
		return
	}
	if !s.llm.Configured() {
		writeError(w, http.StatusServiceUnavailable, errors.New(
			"designing a monster needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}
	var req struct {
		Brief      string `json:"brief"`
		CR         string `json:"cr"`
		Legendary  bool   `json:"legendary"`
		CampaignID string `json:"campaign_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	if req.CampaignID != "" {
		a := s.resolveCampaignAccess(w, r, req.CampaignID)
		if a == nil || !a.requireDM(w) {
			return
		}
	}
	res, err := s.canon.GenerateMonster(r.Context(), canon.MonsterDesignInput{
		CampaignID: req.CampaignID, Brief: req.Brief, CR: req.CR,
		Legendary: req.Legendary, CreatedBy: userID(r),
	})
	if err != nil {
		if canon.IsOffline(err) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"design": res})
}

/* ---------- the placement ---------- */

func (s *Server) handleMonsterPlace(w http.ResponseWriter, r *http.Request) {
	if !s.canonEnabled(w) {
		return
	}
	if !s.homebrewEnabled(w) {
		return
	}
	var req struct {
		CampaignID string `json:"campaign_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	a := s.resolveCampaignAccess(w, r, req.CampaignID)
	if a == nil || !a.requireDM(w) {
		return
	}
	m, err := s.homebrew.Get(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	batch, err := s.canon.PlaceMonster(r.Context(), canon.MonsterPlaceInput{
		CampaignID: req.CampaignID, HomebrewID: m.ID, Name: m.Name,
		Summary: monsterEntitySummary(m), CRLabel: m.ComputedCR, Lore: m.Lore,
		CreatedBy: userID(r),
	})
	if err != nil {
		if canon.IsOffline(err) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"monster_id": m.ID,
		"batch":      toBatchView(*batch, true),
	})
}

// monsterEntitySummary is the one line the placed creature entity carries:
// the lore's first sentence when there is lore, else the role line.
func monsterEntitySummary(m *encounter.HomebrewMonster) string {
	if lore := strings.TrimSpace(m.Lore); lore != "" {
		if i := strings.IndexAny(lore, ".!?"); i > 0 {
			return lore[:i+1]
		}
		return lore
	}
	if m.Role != "" {
		return "A homebrew " + m.Role + " of computed CR " + m.ComputedCR + "."
	}
	return "A homebrew creature of computed CR " + m.ComputedCR + "."
}
