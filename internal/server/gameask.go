package server

// The Magic table's rules judge (MAD-333, stage 5 of MAD-321): a question
// asked mid-game, answered against the live board with citations and no
// board re-entry.
//
//	POST /api/games/{id}/ask    {seat, question} → SSE: meta / delta / done / error
//
// This is substitution of the resolver's input, not a second resolver:
// the folded state — board, stack, priority holder, step — is rendered
// into the resolver's Input (internal/resolver/live.go), and the same
// grounding, prompt assembly and citation behaviour answer it. The reply
// is a judge's walkthrough, not a state write: asking never touches the
// log, and the answer says plainly where the table could not verify
// something (hands, library order).
//
// The SSE framing matches /api/resolve (meta / delta / done / error) so
// the client consumes both the same way; meta carries the citations the
// answer grounded in.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/resolver"
)

// handleGameAsk grounds a mid-game rules question in the live state, real
// card oracle text and the interaction rules, then streams the cited
// answer back. The board, stack, priority holder and step travel with the
// question automatically — nobody types their board in again. The engine
// is format-agnostic and so is the judge: it reads whatever the fold
// holds, honestly, including "the game is still in setup".
func (s *Server) handleGameAsk(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	var req struct {
		Seat     *int   `json:"seat"`
		Question string `json:"question"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	question := strings.TrimSpace(req.Question)
	if question == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a question is required"))
		return
	}
	seat := 0
	if req.Seat != nil {
		seat = *req.Seat
	}
	// The asker asks as a seat they hold (MAD-337): the judge's prompt
	// renders the asker's scoped fold, whose only private zones are the
	// asker's own.
	if err := seatGuard(viewer, seat); err != nil {
		writeGameError(w, err)
		return
	}
	state, err := s.games.StateFor(r.Context(), g.ID, viewer)
	if err != nil {
		writeGameError(w, err)
		return
	}
	in := resolver.FromGame(state, seat, question)

	sse := newSSEWriter(w)

	if !s.llm.Configured() {
		sse.send("error", map[string]any{
			"error": "The rules judge is not configured. Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL / ANTHROPIC_MODEL) to enable it.",
		})
		return
	}

	// A nil *cards.Service must surface as a nil interface so resolver.Ground
	// skips card grounding instead of calling Lookup on a nil pointer.
	var looker cards.Looker
	if s.cards != nil {
		looker = s.cards
	}
	deps := resolver.Deps{Cards: looker, CardDict: s.cardDict, Store: s.store}
	gd := resolver.Ground(r.Context(), deps, in)
	sse.send("meta", map[string]any{
		"sources":          toSources(gd.Sources),
		"cards":            toCardViews(gd.Cards),
		"unresolved_cards": gd.Unresolved,
	})

	prompt := resolver.Prompt(in, gd)
	ctx, cancel := context.WithTimeout(r.Context(), answerTimeout)
	defer cancel()
	answer, streamErr := s.llm.StreamPrompt(ctx, prompt.System, prompt.User, func(text string) error {
		if err := ctx.Err(); err != nil {
			return err // reader is gone or we ran out of time; stop pulling tokens
		}
		return sse.send("delta", map[string]any{"text": text})
	})

	answer = strings.TrimSpace(answer)
	if answer == "" && streamErr != nil {
		sse.send("error", map[string]any{"error": fmt.Sprintf("the judge could not be reached: %v", streamErr)})
		return
	}
	if streamErr != nil {
		// Partial answer: it is on screen, so report the cut-off rather
		// than pretending the answer completed.
		sse.send("error", map[string]any{"error": fmt.Sprintf("the answer was cut short: %v", streamErr)})
		return
	}
	sse.send("done", map[string]any{})
}
