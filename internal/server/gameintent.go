package server

// The Magic table's intent surface (MAD-331, stage 4 of MAD-321): table
// talk in, one ladder disposition out — plus the unresolved tray the
// one-tap rule parks questions in.
//
//	POST /api/games/{id}/intent        {seat, text} → the ladder's reply
//	GET  /api/games/{id}/pending        the game's open questions
//	POST /api/games/{id}/pending/{pid}  {answer, seat} | {dismiss: true}
//
// The reply contract is the ladder's, verbatim: an auto/confirm action
// is already applied (events + state ride along, the pane highlights
// what confirm applied optimistically), an ask is NOT applied and
// carries its one-tap question, and a no-parse says so with a note.
// Nothing here can block the log: questions are rows beside it.
//
// Rewind and amend dismiss the open questions whose ordinal was
// truncated away, per docs/table/model.md's mtg_pending contract.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/intent"
)

// intentEnabled reports the pipeline's availability. The games engine
// itself must be wired too — the pipeline submits through it.
func (s *Server) intentEnabled(w http.ResponseWriter) bool {
	if s.intent == nil || s.games == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("table talk is not configured on this install"))
		return false
	}
	return true
}

// WithIntent wires the table-talk pipeline (MAD-331): the grammar with
// the model fallback behind it, the confirmation ladder, and the
// unresolved tray. Without it the intent and pending endpoints answer
// 503 and the game endpoints work on.
func (s *Server) WithIntent(store *intent.Store) *Server {
	s.intent = store
	return s
}

// handleIntent is the talk entry point: one utterance by one seat,
// through the grammar then the gated model fallback, onto the ladder.
// The reply says what happened; applied actions carry their events so
// the client paints without waiting on the stream.
func (s *Server) handleIntent(w http.ResponseWriter, r *http.Request) {
	if !s.intentEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	var req struct {
		Seat   *int   `json:"seat"`
		Text   string `json:"text"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	seat := 0
	if req.Seat != nil {
		seat = *req.Seat
	}
	source := req.Source
	if source != "voice" {
		source = "" // typed talk is the default; the pipeline stamps "grammar"
	}
	reply, err := s.intent.Interpret(r.Context(), g.ID, seat, req.Text, source)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reply": reply})
}

// handlePendingList answers the game's open questions, oldest first —
// asked ones with their tappable options, parked ones without.
func (s *Server) handlePendingList(w http.ResponseWriter, r *http.Request) {
	if !s.intentEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	open, err := s.intent.Open(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	if open == nil {
		open = []intent.PendingRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": open})
}

// handlePendingAnswer closes one question: a tappable answer applies
// the option's action and caches the tapped card as the spoken span's
// resolution; a free-text answer records itself against a parked row;
// dismiss closes it without answering. Never a modal, never a block.
func (s *Server) handlePendingAnswer(w http.ResponseWriter, r *http.Request) {
	if !s.intentEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	pid := r.PathValue("pid")
	var req struct {
		Answer  string `json:"answer"`
		Seat    *int   `json:"seat"`
		Dismiss bool   `json:"dismiss"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	if req.Dismiss {
		if err := s.intent.Dismiss(r.Context(), g.ID, pid); err != nil {
			writeGameError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"dismissed": pid})
		return
	}
	seat := 0
	if req.Seat != nil {
		seat = *req.Seat
	}
	reply, err := s.intent.AnswerPending(r.Context(), g.ID, pid, seat, req.Answer)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reply": reply})
}

// dismissRewound is the rewind contract's tray half: after the log is
// truncated (rewind or amend), open questions about entries that no
// longer exist close themselves. Best effort by contract — the tray is
// bookkeeping beside the log, and a rewind must never fail over it.
func (s *Server) dismissRewound(r *http.Request, gameID string, state *engine.State) {
	if s.intent == nil || state == nil {
		return
	}
	_ = s.intent.DismissRewound(r.Context(), gameID, state.LastOrd)
}
