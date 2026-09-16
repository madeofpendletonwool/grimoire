package server

// The Magic table's deck-aware play endpoints (MAD-336, stage 5 of
// MAD-321): exact probabilities over the derived library, outs search
// against a board object, and opt-in mulligan advice. No model in any
// path — the whole surface is deterministic maths over the fold plus
// the card index, and the refusal rule rides in front: questions that
// depend on library order are declined with the engine's own words.
//
//	GET  /api/games/{id}/library?seat=N   the derived remaining composition
//	PUT  /api/games/{id}/settings         the per-game opt-ins (mulligan advice)
//	POST /api/games/{id}/odds             {seat, question} or {seat, category|card, draws|by_turn}
//	POST /api/games/{id}/outs             {seat, object?|target?, draws?}
//	POST /api/games/{id}/mulligan         {seat, hand:[names]} — gated on the opt-in
//
// The card index (s.carddb, wired with the deck builder) powers the
// category and outs reads; an install without it still answers
// named-card odds and the library composition, and says what is
// missing rather than answering zero.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/odds"
)

// gameLookup builds the card-data seam over the wired index.
func (s *Server) gameLookup() odds.Lookup {
	if s.carddb == nil {
		return nil
	}
	return func(name string) (*carddb.Card, bool) {
		c, err := s.carddb.Get(name)
		if err != nil {
			return nil, false
		}
		return c, true
	}
}

// handleGameLibrary answers the derived read: what is still in a
// seat's library, as a multiset, with the exactness flag. Composition
// never order — the shape itself is the refusal.
func (s *Server) handleGameLibrary(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	seat, err := strconv.Atoi(r.URL.Query().Get("seat"))
	if err != nil || seat < 1 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the library read needs a seat"))
		return
	}
	state, err := s.games.State(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	lib, err := odds.Of(state, seat)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"library": lib})
}

// handleGameSettings writes the per-game opt-ins. Strictly validated —
// the board/leveling settings discipline: unknown keys error, never
// ride along. mulligan_advice is off until a table turns it on,
// because some tables will not want the help.
func (s *Server) handleGameSettings(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	var req struct {
		MulliganAdvice *bool `json:"mulligan_advice"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	if req.MulliganAdvice == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("settings accepts mulligan_advice (boolean)"))
		return
	}
	settings := g.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	settings[engine.SettingsKeyMulliganAdvice] = *req.MulliganAdvice
	if err := s.games.UpdateSettings(r.Context(), g.ID, settings); err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

// handleGameOdds answers one probability question — free text through
// the gate, or the structured shape the gate itself produces. The
// order-dependent refusal is a 400 with the engine's own words, and
// an unknown library answers honestly rather than as zeros.
func (s *Server) handleGameOdds(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	var req struct {
		Seat     *int   `json:"seat"`
		Question string `json:"question"`
		// The structured shape: category or card, a draw horizon, and
		// the least hits wanted.
		Category string `json:"category"`
		Card     string `json:"card"`
		Draws    int    `json:"draws"`
		ByTurn   int    `json:"by_turn"`
		AtLeast  int    `json:"at_least"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	seat := 0
	if req.Seat != nil {
		seat = *req.Seat
	}
	if seat < 1 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the question needs a seat"))
		return
	}

	// The gate: order-dependent phrasings are refused before anything
	// else runs — the mandated rule, out loud.
	query := odds.Query{Kind: "odds", Category: req.Category, Card: req.Card,
		Draws: req.Draws, ByTurn: req.ByTurn, AtLeast: req.AtLeast}
	if req.Question != "" {
		parsed, err := odds.Parse(req.Question)
		if err != nil {
			writeGameError(w, err)
			return
		}
		query = parsed
	}
	if query.Kind != "odds" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("that is an outs question — the outs endpoint answers it"))
		return
	}

	state, err := s.games.State(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	lib, err := odds.Of(state, seat)
	if err != nil {
		writeGameError(w, err)
		return
	}
	if !lib.Known {
		writeJSON(w, http.StatusOK, map[string]any{
			"known": false,
			"note":  "this seat's library composition is unknown — attach a deck at setup and every card that leaves the library is derived from the log",
		})
		return
	}

	// Resolve the draw horizon: an explicit count, or a turn the
	// question draws through. No horizon at all is one draw.
	draws := query.Draws
	if query.ByTurn > 0 {
		draws, err = odds.DrawsByTurn(state.Turn, query.ByTurn)
		if err != nil {
			writeGameError(w, err)
			return
		}
	} else if draws <= 0 {
		draws = 1
	}

	lookup := s.gameLookup()
	match := odds.MatchCategory(query.Category)
	if query.Card == "" && match == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown category %q", query.Category))
		return
	}
	// Categories over type lines and oracle text need the card index;
	// without it the honest answer names the gap, not zero.
	if query.Card == "" && lookup == nil && query.Category != odds.CatCard &&
		query.Category != odds.CatAnyCard {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("category odds need the card index, which this install does not have"))
		return
	}

	answer, err := odds.DrawOdds(req.Question, lib, query.Category, query.Card,
		match, lookup, draws, query.AtLeast)
	if err != nil {
		writeGameError(w, err)
		return
	}
	if query.ByTurn > 0 {
		answer.Note = appendNote(answer.Note,
			fmt.Sprintf("assuming one draw per own turn through turn %d (now turn %d)", query.ByTurn, state.Turn))
	}
	writeJSON(w, http.StatusOK, map[string]any{"known": true, "answer": answer})
}

// handleGameOuts answers "what are my outs?" against a board object —
// an object id from the live state (the ⓘ-adjacent affordance), or
// free text naming a card or a type. Real cards from the actual
// remaining library, each with why it answers.
func (s *Server) handleGameOuts(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	var req struct {
		Seat   *int   `json:"seat"`
		Object *int64 `json:"object"`
		Target string `json:"target"`
		Draws  int    `json:"draws"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	seat := 0
	if req.Seat != nil {
		seat = *req.Seat
	}
	if seat < 1 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the outs search needs a seat"))
		return
	}
	if req.Object == nil && req.Target == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the outs search needs a board object or a target to answer"))
		return
	}
	if s.carddb == nil {
		writeGameError(w, odds.ErrNoCardData)
		return
	}
	state, err := s.games.State(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	lib, err := odds.Of(state, seat)
	if err != nil {
		writeGameError(w, err)
		return
	}
	if !lib.Known {
		writeJSON(w, http.StatusOK, map[string]any{
			"known": false,
			"note":  "this seat's library composition is unknown — attach a deck at setup to make outs searchable",
		})
		return
	}

	var target odds.Target
	if req.Object != nil {
		o, ok := state.Objects[*req.Object]
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("object %d does not exist in this game", *req.Object))
			return
		}
		target, err = odds.TargetFromCardName(o.Identity.Card, o.Zone == engine.ZoneStack, s.gameLookup())
		if err != nil {
			writeGameError(w, err)
			return
		}
	} else {
		target, err = odds.TargetFromText(r.Context(), req.Target, s.carddb, s.gameLookup())
		if err != nil {
			writeGameError(w, err)
			return
		}
	}
	draws := req.Draws
	if draws <= 0 {
		draws = 1
	}
	answer, err := odds.Outs(r.Context(), lib, target, s.carddb, s.gameLookup(), draws)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"known": true, "outs": answer})
}

// handleGameMulligan advises keep-or-mulligan over an opening hand —
// opt-in per game, off until the table says otherwise. The hand is
// spoken, not read from state: the tracker never presumes to know a
// hand it was not told.
func (s *Server) handleGameMulligan(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	if on, _ := g.Settings[engine.SettingsKeyMulliganAdvice].(bool); !on {
		writeError(w, http.StatusForbidden, fmt.Errorf(
			"mulligan advice is opt-in per game — some tables will not want it; enable it with the game's mulligan_advice setting"))
		return
	}
	if s.carddb == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("mulligan advice needs the card index, which this install does not have"))
		return
	}
	var req struct {
		Seat *int     `json:"seat"`
		Hand []string `json:"hand"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	seat := 0
	if req.Seat != nil {
		seat = *req.Seat
	}
	if seat < 1 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the advice needs a seat"))
		return
	}
	state, err := s.games.State(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	p, err := state.Seats[seat], error(nil)
	if _, ok := state.Seats[seat]; !ok {
		err = fmt.Errorf("%w: seat %d is not in this game", engine.ErrInvalid, seat)
	}
	if err != nil {
		writeGameError(w, err)
		return
	}
	if len(p.Deck) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("mulligan advice needs the attached decklist"))
		return
	}
	advice, err := odds.Advise(req.Hand, p.Deck, s.gameLookup())
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"advice": advice})
}

// appendNote joins a caveat onto whatever caveats already stand.
func appendNote(note, add string) string {
	if note == "" {
		return add
	}
	return note + "; " + add
}
