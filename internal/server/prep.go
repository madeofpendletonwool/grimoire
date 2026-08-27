package server

// One-click session prep's HTTP surface (MAD-364): two POSTs that answer
// "I have 30 minutes before my players arrive". /prep/directions scores the
// campaign's own state into the five most plausible next-session directions
// with prep-time estimates and the evidence rows behind every score;
// /prep/build turns the chosen one into a session: plan, scenes, planned
// encounters, the read-only prep package, and its Markdown through the
// session's existing export path.
//
// Both are DM-only, like every secret-bearing read on the campaign surface.
// Neither is gated on the model key: the scoring and the offline build are
// deterministic — with no ANTHROPIC_API_KEY the ranked list still comes
// back, only the prose pitches are missing.

import (
	"encoding/json"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
)

// handlePrepDirections ranks the campaign's plausible next-session
// directions. Deterministic scoring, evidence and estimates; a configured
// model adds the prose pitches on top.
func (s *Server) handlePrepDirections(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		Notes string `json:"notes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.canon.Directions(r.Context(), canon.DirectionsInput{
		CampaignID: a.campaign.ID,
		Notes:      req.Notes,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	body := map[string]any{
		"campaign_id":        res.CampaignID,
		"campaign_name":      res.CampaignName,
		"budget":             res.Budget,
		"mix":                res.Mix,
		"party":              res.Party,
		"where_the_party_is": res.WhereThePartyIs,
		"drifted_encounters": res.DriftedEncounter,
		"directions":         res.Directions,
		"offline":            res.Offline,
		"generated_at":       res.GeneratedMS,
	}
	if res.Act != nil {
		body["act"] = toActView(*res.Act)
	}
	writeJSON(w, http.StatusOK, body)
}

// handlePrepBuild turns one chosen direction into a ready-to-run session.
func (s *Server) handlePrepBuild(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		DirectionID string `json:"direction_id"`
		Notes       string `json:"notes"`
		Band        string `json:"band"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.canon.BuildPrep(r.Context(), canon.BuildInput{
		CampaignID:  a.campaign.ID,
		DirectionID: req.DirectionID,
		Notes:       req.Notes,
		Band:        req.Band,
		CreatedBy:   userID(r),
		Catalog:     s.catalog,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	scenes := make([]map[string]any, 0, len(res.Scenes))
	for _, sc := range res.Scenes {
		scenes = append(scenes, toSceneView(sc))
	}
	encounters := make([]map[string]any, 0, len(res.Encounters))
	for _, e := range res.Encounters {
		encounters = append(encounters, map[string]any{
			"scene_id": e.SceneID, "event_id": e.EventID, "name": e.Name,
			"party": e.Party, "monsters": e.Monsters, "band": e.Band,
			"verdict": e.Verdict, "hand_built": e.HandBuilt,
		})
	}
	body := map[string]any{
		"direction":          res.Direction,
		"session_id":         res.SessionID,
		"act":                toActView(res.Act),
		"goal":               res.Goal,
		"scenes":             scenes,
		"encounters":         encounters,
		"package":            res.Package,
		"markdown_source_id": res.MarkdownSourceID,
		"offline":            res.Offline,
		"dropped":            res.Dropped,
		"generated_at":       res.GeneratedMS,
	}
	if res.Batch != nil {
		body["batch"] = toBatchView(*res.Batch, true)
	}
	writeJSON(w, http.StatusCreated, body)
}
