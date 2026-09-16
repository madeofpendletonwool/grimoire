package server

// The Magic table's trigger registry surface (MAD-335, stage 5 of
// MAD-321): the "Magic ADHD assistant" over HTTP.
//
//	GET    /api/games/{id}/triggers          the registry (?card= scopes to one card)
//	POST   /api/games/{id}/triggers          register — a human's declaration or confirmed proposal
//	DELETE /api/games/{id}/triggers          remove one registration (?card=&event_kind=)
//	POST   /api/games/{id}/triggers/propose  the model proposes; the human confirms elsewhere
//	GET    /api/games/{id}/nudges            the don't-forget reminders for the current position
//
// The registry is install-wide card knowledge (like the rules corpora),
// reached through game-scoped paths because the play surface is the
// only writer — auth is the session gate's, scoping the game the way
// every other game endpoint does. The propose endpoint never writes:
// its answer is a candidate, and the confirm tap is what POSTs the
// registration with origin "confirmed".

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/intent"
)

// handleTriggerList answers the registry — one card's registrations
// with ?card=, the whole corpus without. The object context strip and
// the registration form both read from here.
func (s *Server) handleTriggerList(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	rows, err := s.games.TriggerRows(r.Context(), r.URL.Query().Get("card"))
	if err != nil {
		writeGameError(w, err)
		return
	}
	if rows == nil {
		rows = []engine.TriggerRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"triggers": rows})
}

// handleTriggerRegister writes one registration. origin "declared" is
// a human typing it at the strip; origin "confirmed" is the propose →
// confirm tap — either way a human approved the row before it existed.
func (s *Server) handleTriggerRegister(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	var req struct {
		Card      string `json:"card"`
		EventKind string `json:"event_kind"`
		Effect    string `json:"effect"`
		Origin    string `json:"origin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	if req.Origin == "" {
		req.Origin = "declared"
	}
	confirmedBy := ""
	if req.Origin == "confirmed" {
		confirmedBy = userID(r)
	}
	row, err := s.games.RegisterTrigger(r.Context(), req.Card,
		engine.TriggerEvent(req.EventKind), req.Effect, req.Origin, confirmedBy)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"trigger": row})
}

// handleTriggerDelete removes one registration — the correction path
// for a row that over-fires. Query-scoped: ?card=Rhystic+Study&event_kind=OPPONENT_CASTS_SPELL.
func (s *Server) handleTriggerDelete(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	card := r.URL.Query().Get("card")
	kind := engine.TriggerEvent(r.URL.Query().Get("event_kind"))
	if err := s.games.DeleteTrigger(r.Context(), card, kind); err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleTriggerPropose is the model's half of registration: one card
// name in, one gated proposal out, nothing written. A model-less
// install answers 503 — the manual strip still works. The model's
// honest NONE answers 200 with a note, because "no trigger fits" is an
// answer, not an error.
func (s *Server) handleTriggerPropose(w http.ResponseWriter, r *http.Request) {
	if !s.intentEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	var req struct {
		Card string `json:"card"`
		Seat *int   `json:"seat"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	seat := 0
	if req.Seat != nil {
		seat = *req.Seat
	}
	proposal, err := s.intent.ProposeTrigger(r.Context(), g.ID, seat, req.Card)
	switch {
	case errors.Is(err, intent.ErrNone):
		writeJSON(w, http.StatusOK, map[string]any{
			"proposal": nil,
			"note":     "the model found no trigger the registry's vocabulary can name",
		})
		return
	case errors.Is(err, intent.ErrNoModel):
		writeError(w, http.StatusServiceUnavailable, err)
		return
	case err != nil:
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposal": proposal})
}

// handleNudges answers the don't-forget reminders for the game's
// current position: waiting triggers, unresolved triggered abilities on
// the stack, unused attack triggers. A pure read over the fold and the
// registry — the current-action pane re-reads it on every wake.
func (s *Server) handleNudges(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	nudges, err := s.games.NudgesFor(r.Context(), g.ID, viewer)
	if err != nil {
		writeGameError(w, err)
		return
	}
	if nudges == nil {
		nudges = []engine.Nudge{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nudges": nudges})
}
