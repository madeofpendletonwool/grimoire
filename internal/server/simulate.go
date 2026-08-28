package server

// The simulation tick surface (MAD-367): advance the world by N days. The
// pure function is internal/sim; this file is the HTTP shape. DM-only, like
// every write and every secret-bearing read on the campaign surface.
//
// POST .../simulate is the preview — a question; it writes nothing but its
// own sim_ticks row. POST .../simulate/{tid}/stage turns a preview into a
// proposal batch behind the same review gate everything else uses, and the
// decision happens on the ordinary batch view: accepting moves the clock by
// exactly the window, exactly once.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/sim"
)

// WithSim wires the simulation tick store (MAD-367). Without it the
// simulate endpoints answer 503.
func (s *Server) WithSim(store *sim.Store) *Server {
	s.sims = store
	return s
}

// simEnabled reports the tick surface's availability: the store is wired
// only when the canon engine and the faction plans both are.
func (s *Server) simEnabled(w http.ResponseWriter) bool {
	if s.sims == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the simulation tick is not configured on this install"))
		return false
	}
	return true
}

// tickView is one sim_ticks row as the API renders it.
type tickView struct {
	ID             string `json:"id"`
	FromDay        int64  `json:"from_day"`
	ToDay          int64  `json:"to_day"`
	Seed           int64  `json:"seed"`
	SnapshotDigest string `json:"snapshot_digest"`
	BatchID        string `json:"batch_id,omitempty"`
	Status         string `json:"status"`
	CreatedBy      string `json:"created_by,omitempty"`
	CreatedAt      int64  `json:"created_at"`
}

func toTickView(t *sim.TickRow) tickView {
	return tickView{
		ID: t.ID, FromDay: t.FromDay, ToDay: t.ToDay, Seed: t.Seed,
		SnapshotDigest: t.SnapshotDigest, BatchID: t.BatchID, Status: t.Status,
		CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt.UnixMilli(),
	}
}

// handleCampaignSimulate previews one window: {"days": 14, "seed": 7}. The
// seed is optional — a deterministic default is derived when absent. The
// response carries the tick's row, the deterministic outcome set, and the
// optional flavour pass (absent when no model is configured or it failed
// validation — the tick degrades to the deterministic summary, never fails).
func (s *Server) handleCampaignSimulate(w http.ResponseWriter, r *http.Request) {
	if !s.simEnabled(w) {
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
		Days int    `json:"days"`
		Seed *int64 `json:"seed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	pv, err := s.sims.Preview(r.Context(), a.campaign.ID, req.Days, req.Seed, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := map[string]any{
		"tick":    toTickView(pv.Tick),
		"result":  pv.Result,
		"offline": pv.Offline,
	}
	if len(pv.Flavour) > 0 {
		out["flavour"] = pv.Flavour
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCampaignSimulateStage turns one preview into a proposal batch. The
// batch renders and decides on the ordinary review surface — no new review
// screen; this response hands the DM its id.
func (s *Server) handleCampaignSimulateStage(w http.ResponseWriter, r *http.Request) {
	if !s.simEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	batch, _, err := s.sims.StageTick(r.Context(), a.campaign.ID, r.PathValue("tid"), userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"batch": toBatchView(*batch, true),
		"tick": tickView{
			ID: r.PathValue("tid"), Status: sim.TickStaged, BatchID: batch.ID,
		},
	})
}
