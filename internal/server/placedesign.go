package server

// The location designer's HTTP surface (MAD-372): one POST that turns a
// premise into a staged settlement behind the review gate, and one POST
// that fleshes an existing location out around what is already there. Both
// DM-only, like every canon write; the model key gates both.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
)

// handleCampaignLocationDesign runs one design exchange: the premise, the
// settlement kind, the scale band, an optional parent location and an
// optional near anchor. Returns the staged batch — nothing is written to
// the graph until it is decided — plus the scale band that fixed the
// budget and the entities reused from the campaign.
func (s *Server) handleCampaignLocationDesign(w http.ResponseWriter, r *http.Request) {
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
			"designing a place needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}
	var req struct {
		Premise string   `json:"premise"`
		Kind    string   `json:"kind"`
		Scale   string   `json:"scale"`
		Parent  string   `json:"parent"`
		Near    string   `json:"near"`
		Parts   []string `json:"parts"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.canon.GenerateLocation(r.Context(), canon.LocationDesignInput{
		CampaignID: a.campaign.ID,
		Premise:    req.Premise, Kind: req.Kind, Scale: req.Scale,
		Parent: req.Parent, Near: req.Near, Parts: req.Parts,
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
		"shape":  res.Shape,
		"reused": res.Reused,
	}
	if res.Batch != nil {
		body["batch"] = toBatchView(*res.Batch, true)
	}
	writeJSON(w, http.StatusCreated, body)
}

// handleLocationFleshOut runs the flesh-out exchange: a location that
// already exists but is a name and one line gets its block, its people and
// its secrets proposed around what is already there — never replacing it.
// Optional parts regenerate one piece of the shape ("re-roll the people,
// keep the geography"); the default proposes everything the scale band
// still has room for.
func (s *Server) handleLocationFleshOut(w http.ResponseWriter, r *http.Request) {
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
			"fleshing a place out needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}
	var req struct {
		Premise string   `json:"premise"`
		Kind    string   `json:"kind"`
		Scale   string   `json:"scale"`
		Near    string   `json:"near"`
		Parts   []string `json:"parts"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Premise == "" {
		// A flesh-out premise may simply be the place itself: its name
		// and one line carry enough signal to propose around.
		req.Premise = a.campaign.Premise
	}
	res, err := s.canon.GenerateLocation(r.Context(), canon.LocationDesignInput{
		CampaignID: a.campaign.ID,
		Premise:    req.Premise, Kind: req.Kind, Scale: req.Scale,
		Near: req.Near, Location: r.PathValue("eid"), Parts: req.Parts,
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
		"shape":  res.Shape,
		"reused": res.Reused,
	}
	if res.Batch != nil {
		body["batch"] = toBatchView(*res.Batch, true)
	}
	writeJSON(w, http.StatusCreated, body)
}
