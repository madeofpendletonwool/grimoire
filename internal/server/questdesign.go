package server

// The quest designer's HTTP surface (MAD-371): one POST that turns a hook
// into a staged branching quest behind the review gate, and one POST that
// branches an existing quest at a state the DM picks. DM-only, like every
// canon write and every secret-bearing read; the model key gates both.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
)

// handleCampaignQuestDesign runs one design exchange: hook text, an
// optional shape kind, branch count, depth and an optional anchor entity.
// Returns the staged batch — nothing is written to the graph until it is
// decided — plus the computed shape, the assembled machine (a preview) and
// the entities reused from the campaign.
func (s *Server) handleCampaignQuestDesign(w http.ResponseWriter, r *http.Request) {
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
			"designing a quest needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}
	var req struct {
		Hook         string `json:"hook"`
		Kind         string `json:"kind"`
		BranchPoints int    `json:"branch_points"`
		Depth        int    `json:"depth"`
		Anchor       string `json:"anchor"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.canon.GenerateQuest(r.Context(), canon.QuestDesignInput{
		CampaignID: a.campaign.ID,
		Hook:       req.Hook, Kind: req.Kind,
		BranchPoints: req.BranchPoints, Depth: req.Depth,
		Anchor: req.Anchor, CreatedBy: userID(r),
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
		"shape":    res.Shape,
		"topology": res.Topology,
		"machine":  toMachineView(res.Machine),
		"reused":   res.Reused,
	}
	if res.Batch != nil {
		body["batch"] = toBatchView(*res.Batch, true)
	}
	writeJSON(w, http.StatusCreated, body)
}

// handleQuestBranch runs the branch exchange: an existing quest, a state
// the DM picks, and two exclusive outcomes proposed off it. Returns the
// staged batch; the quest's machine only changes when it is decided.
func (s *Server) handleQuestBranch(w http.ResponseWriter, r *http.Request) {
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
			"branching a quest needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}
	var req struct {
		State string `json:"state"`
		Notes string `json:"notes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.canon.GenerateQuestBranch(r.Context(), canon.QuestBranchInput{
		CampaignID: a.campaign.ID,
		QuestID:    r.PathValue("qid"),
		StateKey:   req.State, Notes: req.Notes,
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
		"machine":   toMachineView(res.Machine),
		"state_key": res.StateKey,
	}
	if res.Batch != nil {
		body["batch"] = toBatchView(*res.Batch, true)
	}
	writeJSON(w, http.StatusCreated, body)
}

// toMachineView renders a proposed state machine for the wire: the initial
// state, the states with their labels, detail and terminal markers, and the
// named edges — the keyed form MAD-369's machine always writes.
func toMachineView(m campaign.StateMachine) map[string]any {
	states := make([]map[string]any, 0, len(m.States))
	for _, st := range m.States {
		states = append(states, map[string]any{
			"key": st.Key, "label": st.Label, "detail": st.Detail, "terminal": st.Terminal,
		})
	}
	edges := make([]map[string]any, 0, len(m.Edges))
	for _, e := range m.Edges {
		edges = append(edges, map[string]any{
			"from": e.From, "to": e.To, "label": e.Label, "detail": e.Detail,
		})
	}
	return map[string]any{"initial": m.Initial, "states": states, "edges": edges}
}
