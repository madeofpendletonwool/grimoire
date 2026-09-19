package server

// The Magic table's replay and post-game coach (MAD-339, stage 7 of
// MAD-321): the two halves that exist because state is a fold over an
// immutable log. Replay is a viewer over data Stage 2 already writes —
// fold to any ordinal, scrub, watch the game play back — and the coach
// is an LLM read **over a deterministic log**: internal/table/analysis
// derives the facts, the model interprets them, and neither invents
// state.
//
//	GET  /api/games/{id}/replay         the state folded to ?at= (defaults to the head)
//	POST /api/games/{id}/analysis       {seat} → SSE: meta / delta / done / error
//
// Replay scopes exactly like every other read: a seat scrubs its own
// stream (ADR 13 — the SQL is the gate), the owner folds everything,
// an observer scrubs the public game. The coach's privacy is the same
// construction: the analysis folds the requesting viewer's scoped
// stream, so another seat's rows are not filtered out of the prompt —
// they were never read. A seat asks as itself; the owner asks as any
// seat; a judge or spectator asks as the table (seat 0), whose report
// is the public read.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/table/analysis"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// handleGameReplay answers the board as it stood at an ordinal: the
// viewer's stream folded up to and including `at`, clamped to the head
// the way the stream's reconnect forgives a stale cursor. `at` past
// the head is the present; at zero it is the game before anything
// happened. Nothing is materialized — every scrub is a fold, so a
// rewind is reflected the moment it lands and two clients scrubbing
// different positions can never disagree about the same ordinal.
func (s *Server) handleGameReplay(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	at := int64(-1)
	if v := r.URL.Query().Get("at"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("at must be an ordinal"))
			return
		}
		at = parsed
	}
	head, err := s.games.LatestOrd(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	if at < 0 || at > head {
		at = head // no cursor reads the present, a stale one is clamped
	}
	state, err := s.games.StateAt(r.Context(), g.ID, viewer, at)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": state, "at": at, "head": head})
}

// handleGameAnalysis streams a seat's post-game read: the deterministic
// facts arrive whole on `meta` — the resource picture, the damage
// ledger, the missed-trigger diff — and the model's interpretation
// streams after them as `delta`s, exactly the judge's framing so the
// client consumes both panels the same way. The facts are the product:
// with no LLM configured the meta still lands and the error frame says
// what is missing, the same honesty the rules judge keeps.
func (s *Server) handleGameAnalysis(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	var req struct {
		Seat *int `json:"seat"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	seat := 0
	if req.Seat != nil {
		seat = *req.Seat
	}
	// The coach reads one seat's report to that seat alone: a seated
	// player asks as themselves, the owner as any seat (the solo
	// tracker's chair is every chair), an observer as the table —
	// seat 0, the public read, the same ask-side rule the judge keeps.
	if err := askGuard(viewer, seat); err != nil {
		writeGameError(w, err)
		return
	}
	state, err := s.games.StateFor(r.Context(), g.ID, viewer)
	if err != nil {
		writeGameError(w, err)
		return
	}
	if state.Status == engine.StatusSetup {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: there is no game to coach yet — the log begins at start", engine.ErrInvalid))
		return
	}
	if seat != 0 {
		if _, ok := state.Seats[seat]; !ok {
			writeError(w, http.StatusBadRequest, fmt.Errorf("%w: seat %d is not in this game", engine.ErrInvalid, seat))
			return
		}
	}
	// The scoped stream is the report's whole input: the fold a seat
	// holds is the fold the coach reads, private zones and all, and
	// another seat's rows were never selected to leak.
	evs, err := s.games.EventsFor(r.Context(), g.ID, viewer, 0, 0)
	if err != nil {
		writeGameError(w, err)
		return
	}
	reg, err := s.games.TriggerRegistry(r.Context())
	if err != nil {
		writeGameError(w, err)
		return
	}
	sum := analysis.Summarize(evs, reg, seat)
	digest := analysis.Digest(evs, seat)

	sse := newSSEWriter(w)
	sse.send("meta", map[string]any{"summary": sum})

	if !s.llm.Configured() {
		sse.send("error", map[string]any{
			"error": "The coach's facts are derived; its interpretation needs a model. Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL / ANTHROPIC_MODEL) to enable it.",
		})
		return
	}

	system, user := analysis.Prompt(sum, digest)
	ctx, cancel := context.WithTimeout(r.Context(), answerTimeout)
	defer cancel()
	answer, streamErr := s.llm.StreamPrompt(ctx, system, user, func(text string) error {
		if err := ctx.Err(); err != nil {
			return err // reader is gone or we ran out of time; stop pulling tokens
		}
		return sse.send("delta", map[string]any{"text": text})
	})

	answer = strings.TrimSpace(answer)
	if answer == "" && streamErr != nil {
		sse.send("error", map[string]any{"error": fmt.Sprintf("the coach could not be reached: %v", streamErr)})
		return
	}
	if streamErr != nil {
		// Partial answer: it is on screen, so report the cut-off rather
		// than pretending the debrief completed.
		sse.send("error", map[string]any{"error": fmt.Sprintf("the debrief was cut short: %v", streamErr)})
		return
	}
	sse.send("done", map[string]any{})
}
