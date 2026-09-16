package server

// The Magic table's ruling log (MAD-338, stage 6 of MAD-321): the
// judge's pen. A ruling is a human record about the game, not an event
// of it — anchored to the ordinal it concerns, carried by the game's
// own history, and never clobbered by the rewind it may itself have
// ordered.
//
//	GET  /api/games/{id}/rulings    the log, oldest first
//	POST /api/games/{id}/rulings    {ord, note} — judge or host only
//
// Every entitled reader — the owner, every seat, every observer — sees
// the whole ruling log: a ruling is public to the room by definition,
// or it settles nothing. The write is the one gate: a judge records, a
// spectator cannot, and a seated player asks the judge rather than
// ruling on their own game. The host may record too — a solo table's
// owner is its judge.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// handleRulingList answers the game's ruling log, oldest first — every
// ruling with its anchor ordinal and the judge who recorded it.
func (s *Server) handleRulingList(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, _ := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	rulings, err := s.games.Rulings(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	if rulings == nil {
		rulings = []engine.Ruling{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rulings": rulings})
}

// handleRulingRecord writes one ruling anchored to an event ordinal.
// The gate is the role: the host and a judge record; a spectator and a
// seated player read. The ordinal must exist — a ruling anchored to a
// row that never happened would be a ruling about nothing.
func (s *Server) handleRulingRecord(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	if !viewer.Owner {
		role, err := s.games.ObserverRole(r.Context(), g.ID, userID(r))
		if err != nil {
			writeGameError(w, err)
			return
		}
		if role != engine.RoleJudge {
			writeError(w, http.StatusForbidden, fmt.Errorf("%w: only the judge records rulings", engine.ErrNotEntitled))
			return
		}
	}
	var req struct {
		Ord  *int64 `json:"ord"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	if req.Ord == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a ruling needs the ordinal it concerns"))
		return
	}
	ruling, err := s.games.RecordRuling(r.Context(), g.ID, *req.Ord, req.Note, userID(r))
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ruling": ruling})
}
