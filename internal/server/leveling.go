package server

// The leveling surface (MAD-424, stage 7 of MAD-417): XP awards from the
// encounter builder's own budgets, level-ups through the review gate, and
// the post-session reconciliation pass.
//
//	GET  /api/campaigns/{id}/leveling                     mode + party progress (DM)
//	PUT  /api/campaigns/{id}/leveling/settings            xp | milestone (DM, owner-shaped)
//	POST /api/campaigns/{id}/encounters/{eid}/award       award encounter XP (DM)
//	GET  /api/campaigns/{id}/awards                       the award log (DM)
//	POST /api/campaigns/{id}/level-ups/propose            stage a level-up (DM)
//	GET  /api/campaigns/{id}/level-ups                    the level-up log (DM)
//	POST /api/campaigns/{id}/reconcile                    run the truth pass (DM)
//
// The permission line is the campaign layer's, unchanged: everything here
// is DM material — the awards and diffs price the party's mechanical
// state, and a player's read of their own sheet already exists. A
// level-up is confirmed at the batch surface (/proposals/{bid}/decision)
// like every other proposal; the acceptance is the DM's, standing in for
// the table's. Player-confirmed level-ups are the portal stage's product
// question, not a default this issue sets.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/leveling"
)

// levelingEnabled reports the surface's availability.
func (s *Server) levelingEnabled(w http.ResponseWriter) bool {
	if s.leveling == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("leveling is not configured on this install"))
		return false
	}
	return true
}

// WithLeveling wires the leveling store (MAD-424). Without it the
// leveling endpoints answer 503.
func (s *Server) WithLeveling(store *leveling.Store) *Server {
	s.leveling = store
	return s
}

/* ---------- the mode and the party's progress ---------- */

func (s *Server) handleLevelingRead(w http.ResponseWriter, r *http.Request) {
	if !s.levelingEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	progress, err := s.leveling.PartyProgress(r.Context(), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"leveling": map[string]any{
			"mode":     leveling.ConfigOf(a.campaign.Settings[leveling.SettingsKey]).Mode,
			"progress": progress,
		},
	})
}

// handleLevelingSettings writes the mode: data on the campaign under the
// leveling settings key, validated strictly, owner-shaped like every
// settings write — the board-settings pattern verbatim.
func (s *Server) handleLevelingSettings(w http.ResponseWriter, r *http.Request) {
	if !s.levelingEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.isDM() || (a.campaign.OwnerID != userID(r) && !a.keeper) {
		writeError(w, http.StatusForbidden, fmt.Errorf("only the campaign's owner may set the leveling mode"))
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	if req.Mode == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("mode is required: xp | milestone"))
		return
	}
	cfg, err := leveling.Parse(map[string]any{"mode": req.Mode})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	settings := a.campaign.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	settings[leveling.SettingsKey] = cfg.SettingsValue()
	c, err := s.campaigns.UpdateCampaign(r.Context(), a.campaign.OwnerID, a.campaign.ID,
		nil, nil, nil, nil, settings)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"leveling": map[string]any{"mode": leveling.ConfigOf(c.Settings[leveling.SettingsKey]).Mode},
	})
}

/* ---------- the XP award ---------- */

func (s *Server) handleEncounterAward(w http.ResponseWriter, r *http.Request) {
	if !s.levelingEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Characters []string `json:"characters"`
		SessionID  string   `json:"session_id"`
		Note       string   `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	result, err := s.leveling.Award(r.Context(), a.campaign.ID, r.PathValue("eid"),
		req.Characters, req.SessionID, req.Note, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"award": result})
}

func (s *Server) handleAwards(w http.ResponseWriter, r *http.Request) {
	if !s.levelingEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	awards, err := s.leveling.Awards(r.Context(), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"awards": awards})
}

/* ---------- the staged level-up ---------- */

func (s *Server) handleLevelUpPropose(w http.ResponseWriter, r *http.Request) {
	if !s.levelingEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Character string `json:"character"`
		Class     string `json:"class"`
		SessionID string `json:"session_id"`
		Note      string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	if req.Character == "" || req.Class == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("character and class are required"))
		return
	}
	row, batch, err := s.leveling.StageLevelUp(r.Context(), a.campaign.ID,
		req.Character, req.Class, req.SessionID, req.Note, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"level_up": row,
		"batch":    toBatchView(*batch, true),
	})
}

func (s *Server) handleLevelUps(w http.ResponseWriter, r *http.Request) {
	if !s.levelingEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	rows, err := s.leveling.LevelUps(r.Context(), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"level_ups": rows})
}

/* ---------- the reconciliation pass ---------- */

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if !s.levelingEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
			return
		}
	}
	result, err := s.leveling.Reconcile(r.Context(), a.campaign.ID, userID(r), req.Note)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	body := map[string]any{"findings": result.Findings}
	if result.Batch != nil {
		body["batch"] = toBatchView(*result.Batch, true)
	}
	writeJSON(w, http.StatusOK, body)
}
