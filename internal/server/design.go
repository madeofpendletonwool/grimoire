package server

// The story planner and scene designer's HTTP surface (MAD-362): two POSTs
// that plan forward from the campaign as it stands — the next session or a
// whole act's worth through /design/plan, one scene through /design/scene.
// DM-only, like every canon write and every secret-bearing read; the model
// key gates the whole surface. Scenes, cast, secrets and outcomes land in
// the spine directly (plan, not canon); the awareness changes the outcomes
// promise come back as a proposal batch behind the review gate.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
)

// handleCampaignPlan runs one planning exchange: the mode (next session, or
// a whole act's worth), an optional act id, and the DM's notes.
func (s *Server) handleCampaignPlan(w http.ResponseWriter, r *http.Request) {
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
			"planning a session needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}
	var req struct {
		Mode  string `json:"mode"`
		ActID string `json:"act_id"`
		Notes string `json:"notes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.canon.GeneratePlan(r.Context(), canon.PlanInput{
		CampaignID: a.campaign.ID,
		Mode:       req.Mode, ActID: req.ActID, Notes: req.Notes,
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
	body := map[string]any{
		"act":    toActView(res.Act),
		"budget": res.Budget,
		"mix":    res.Mix,
	}
	sessions := make([]map[string]any, 0, len(res.Sessions))
	for _, ps := range res.Sessions {
		scenes := make([]map[string]any, 0, len(ps.Scenes))
		for _, sc := range ps.Scenes {
			scenes = append(scenes, toSceneView(sc))
		}
		sessions = append(sessions, map[string]any{
			"session_id": ps.SessionID, "act_id": ps.ActID, "goal": ps.Goal,
			"new_plan": ps.NewPlan, "scenes": scenes, "dropped": ps.Dropped,
		})
	}
	body["sessions"] = sessions
	if res.Batch != nil {
		body["batch"] = toBatchView(*res.Batch, true)
	}
	writeJSON(w, http.StatusCreated, body)
}

// handleCampaignScene runs one scene-design exchange: the act (and
// optionally the session) it belongs to, an optional setting and kind, and
// the DM's notes.
func (s *Server) handleCampaignScene(w http.ResponseWriter, r *http.Request) {
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
			"designing a scene needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}
	var req struct {
		ActID     string `json:"act_id"`
		SessionID string `json:"session_id"`
		Setting   string `json:"setting"`
		Kind      string `json:"kind"`
		Notes     string `json:"notes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.canon.GenerateScene(r.Context(), canon.SceneInput{
		CampaignID: a.campaign.ID,
		ActID:      req.ActID, SessionID: req.SessionID,
		Setting: req.Setting, Kind: req.Kind, Notes: req.Notes,
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
	body := map[string]any{
		"scene":   toSceneView(res.Scene),
		"dropped": res.Dropped,
	}
	if res.Batch != nil {
		body["batch"] = toBatchView(*res.Batch, true)
	}
	writeJSON(w, http.StatusCreated, body)
}
