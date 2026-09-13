package server

// The Magic table's provenance endpoints (MAD-334, stage 5 of
// MAD-321): the deterministic half of the reasoning layer — no model
// calls, no tokens, milliseconds. Every computed characteristic traces
// to the rows that produced it, and the three questions a table argues
// with — "why is this 7/7?", "why did it die?", "what happened on turn
// 5?" — are reads over the log the panes already render.
//
//	GET /api/games/{id}/objects/{oid}/trace    the characteristic stack (?at=ord for a moment)
//	GET /api/games/{id}/death/{ord}            the walk back from one DIED row
//	GET /api/games/{id}/turns/{n}              one turn's slice of the log
//
// All three fold the same mtg_events rows the board does; nothing is
// materialized, so a rewind is immediately reflected and no answer can
// drift from the log.

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// handleObjectTrace answers "why is this creature 7/7?" for one object:
// the P/T stack (base, modifiers in CR 613 order, counters, the switch,
// the total) plus every other layer's changes, each row naming its
// source, layer and duration. `at` renders the stack as of an ordinal —
// the moment before a death, say — by folding only the rows up to it.
func (s *Server) handleObjectTrace(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	oid, err := strconv.ParseInt(r.PathValue("oid"), 10, 64)
	if err != nil || oid <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the trace needs an object id"))
		return
	}
	evs, err := s.games.Events(r.Context(), g.ID, 0, 0)
	if err != nil {
		writeGameError(w, err)
		return
	}
	head := int64(0)
	if len(evs) > 0 {
		head = evs[len(evs)-1].Ord
	}
	at := head
	if v := r.URL.Query().Get("at"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("at must be an ordinal"))
			return
		}
		// Clamp to the head: a stale cursor re-reads the present
		// instead of erroring — the same forgiveness the stream's
		// reconnect gives.
		at = min(parsed, head)
	}
	upto := evs[:0]
	for _, e := range evs {
		if e.Ord > at {
			break
		}
		upto = append(upto, e)
	}
	trace := engine.Fold(upto).CharTrace(oid)
	if trace == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("object %d has no trace in this game", oid))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trace": trace, "at": at})
}

// handleDeathTrace answers "why did my creature die?" for the DIED row
// at an ordinal: the state-based action with its CR citation, the
// characteristic stack in force the instant before, the marked damage
// with its sources, and the table action that set the sweep off. The
// client reaches it from the log entry itself — one ⓘ on the death.
func (s *Server) handleDeathTrace(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	ord, err := strconv.ParseInt(r.PathValue("ord"), 10, 64)
	if err != nil || ord <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the death trace needs the DIED row's ordinal"))
		return
	}
	evs, err := s.games.Events(r.Context(), g.ID, 0, 0)
	if err != nil {
		writeGameError(w, err)
		return
	}
	rep, err := engine.DeathTrace(evs, ord)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"death": rep})
}

// handleTurnSlice answers "what happened on turn 5?": the log's rows of
// one turn, from its TURN_STARTED anchor to the next one's, rendered by
// the same describeEvent the log pane uses. An absent turn answers 404
// — a provenance layer must never invent rows.
func (s *Server) handleTurnSlice(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	turn, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || turn < 1 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the turn slice needs a turn number"))
		return
	}
	evs, err := s.games.Events(r.Context(), g.ID, 0, 0)
	if err != nil {
		writeGameError(w, err)
		return
	}
	slice := engine.TurnSlice(evs, turn)
	if len(slice) == 0 {
		writeError(w, http.StatusNotFound, fmt.Errorf("turn %d is not in this game's log", turn))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"turn":      turn,
		"turn_seat": slice[0].TurnSeat,
		"events":    slice,
	})
}
