package server

// The campaign skeleton generator's HTTP surface (MAD-361): one POST that
// turns a premise into a proposal batch plus the spine's acts and session
// plans. DM-only, like every canon write and every secret-bearing read; the
// model key gates the whole surface.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
)

// handleCampaignSkeleton runs one design exchange: premise, level range,
// optional act count, tone knobs and the parts to regenerate. Returns the
// staged batch (the canon objects behind the review gate) and the acts and
// session plans written to the spine when the acts part ran.
func (s *Server) handleCampaignSkeleton(w http.ResponseWriter, r *http.Request) {
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
			"designing a campaign needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}
	var req struct {
		Premise    string   `json:"premise"`
		Tone       string   `json:"tone"`
		LevelStart int      `json:"level_start"`
		LevelEnd   int      `json:"level_end"`
		ActCount   int      `json:"act_count"`
		Parts      []string `json:"parts"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.canon.GenerateSkeleton(r.Context(), canon.SkeletonInput{
		CampaignID: a.campaign.ID,
		Premise:    req.Premise, Tone: req.Tone,
		LevelStart: req.LevelStart, LevelEnd: req.LevelEnd,
		ActCount: req.ActCount, Parts: req.Parts,
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
	acts := make([]map[string]any, 0, len(res.Acts))
	for _, act := range res.Acts {
		acts = append(acts, toActView(act))
	}
	plans := make([]map[string]any, 0, len(res.Plans))
	for _, p := range res.Plans {
		plans = append(plans, toPlanView(p))
	}
	body := map[string]any{
		"pacing": res.Pacing,
		"bands":  res.Bands,
		"acts":   acts,
		"plans":  plans,
		"reused": res.Reused,
	}
	if res.Batch != nil {
		body["batch"] = toBatchView(*res.Batch, true)
	}
	writeJSON(w, http.StatusCreated, body)
}
