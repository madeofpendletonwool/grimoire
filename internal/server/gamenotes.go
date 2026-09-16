package server

// The pod's private per-seat scratch (MAD-337): mtg_seat_notes, the
// table migration 0014 defined and nothing had yet touched. One pad
// per seat, editable, latest-wins, and — the whole point — never
// another seat's view and never any seat's prompt. There is no read
// path from a note into state, intent, ask or odds by construction;
// the leak gate asserts it with a marker note against every surface
// and every captured prompt.
//
//	GET /api/games/{id}/notes[?seat=N]   the viewer's own scratch
//	PUT /api/games/{id}/notes             {seat?, body}
//
// Whose pad: the account bound to the seat, or — for a local seat no
// account holds — the host whose client runs that seat. Not even the
// game's owner reads a seated player's notes: ADR 13's owner-sees-
// everything is about the game's hidden zones, and a player's pad is
// not a zone of the game.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// noteSeat resolves which seat's pad the request means: the query or
// body seat when given, else the viewer's first held seat. A pad
// belongs to the account bound to its seat — the one place the owner's
// see-everything entitlement stops short (MAD-337). The exception is a
// local seat no account holds: the host's client is the one running
// it, so the host holds its pad.
func (s *Server) noteSeat(w http.ResponseWriter, r *http.Request, g *engine.Game, viewer engine.Viewer, reqSeat *int) (int, bool) {
	seat := 0
	if reqSeat != nil {
		seat = *reqSeat
	}
	if seat == 0 && !viewer.Owner {
		if len(viewer.Seats) == 0 {
			writeError(w, http.StatusForbidden, engine.ErrNotEntitled)
			return 0, false
		}
		seat = viewer.Seats[0]
	}
	if seat == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a local seat's notes need the seat named"))
		return 0, false
	}
	seats, err := s.games.Seats(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return 0, false
	}
	for _, sc := range seats {
		if sc.Seat != seat {
			continue
		}
		if sc.UserID == userID(r) || (sc.UserID == "" && viewer.Owner) {
			return seat, true
		}
		writeError(w, http.StatusForbidden, engine.ErrNotEntitled)
		return 0, false
	}
	writeError(w, http.StatusBadRequest, fmt.Errorf("seat %d is not in this game", seat))
	return 0, false
}

// handleSeatNoteGet answers the viewer's own scratch.
func (s *Server) handleSeatNoteGet(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	var seat *int
	if v := r.URL.Query().Get("seat"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("seat must be a number"))
			return
		}
		seat = &n
	}
	got, ok := s.noteSeat(w, r, g, viewer, seat)
	if !ok {
		return
	}
	body, err := s.games.SeatNote(r.Context(), g.ID, got)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seat": got, "body": body})
}

// handleSeatNotePut writes the viewer's own scratch, latest-wins.
func (s *Server) handleSeatNotePut(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	var req struct {
		Seat *int   `json:"seat"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	seat, ok := s.noteSeat(w, r, g, viewer, req.Seat)
	if !ok {
		return
	}
	if err := s.games.SetSeatNote(r.Context(), g.ID, seat, req.Body); err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seat": seat, "saved": true})
}
