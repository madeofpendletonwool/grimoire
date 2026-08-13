package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/resolver"
)

// resolveRequest is the JSON body for /api/resolve. Board and Sequence are the
// line-oriented text blocks the UI sends (see resolver.ParseInput for the
// grammar); Note carries any free-text clarifications.
type resolveRequest struct {
	Board    string `json:"board"`
	Sequence string `json:"sequence"`
	Note     string `json:"note"`
}

// handleResolve grounds a stated board + spell/ability sequence in real card
// text and the interaction rules, then streams a step-by-step trace back as
// server-sent events. It reuses the chat SSE framing (meta / delta / done /
// error) so the client consumes it the same way. MTG-only: the interaction
// chapters it grounds in (stack, triggers, layers, replacement effects) are
// Magic's. A missing card service or an empty index is graceful — the resolver
// runs with less grounding and tells the model to report the gap.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	var req resolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if strings.TrimSpace(req.Board) == "" && strings.TrimSpace(req.Sequence) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("board or sequence is required"))
		return
	}
	in := resolver.ParseInput(req.Board, req.Sequence, req.Note)

	sse := newSSEWriter(w)

	if !s.llm.Configured() {
		sse.send("error", map[string]any{
			"error": "The resolver is not configured. Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL / ANTHROPIC_MODEL) to enable it.",
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
	g := resolver.Ground(r.Context(), deps, in)
	sse.send("meta", map[string]any{
		"sources":          toSources(g.Sources),
		"cards":            toCardViews(g.Cards),
		"unresolved_cards": g.Unresolved,
	})

	prompt := resolver.Prompt(in, g)
	ctx, cancel := context.WithTimeout(r.Context(), answerTimeout)
	defer cancel()
	trace, streamErr := s.llm.StreamPrompt(ctx, prompt.System, prompt.User, func(text string) error {
		if err := ctx.Err(); err != nil {
			return err // reader is gone or we ran out of time; stop pulling tokens
		}
		return sse.send("delta", map[string]any{"text": text})
	})

	trace = strings.TrimSpace(trace)
	if trace == "" && streamErr != nil {
		sse.send("error", map[string]any{"error": fmt.Sprintf("the resolver could not be reached: %v", streamErr)})
		return
	}
	if streamErr != nil {
		// Partial trace: it is on screen, so report the cut-off rather than
		// pretending the turn completed.
		sse.send("error", map[string]any{"error": fmt.Sprintf("the trace was cut short: %v", streamErr)})
		return
	}
	sse.send("done", map[string]any{})
}

// toCardViews maps resolved cards into the JSON view the UI renders.
func toCardViews(cs []*cards.Card) []cardView {
	out := make([]cardView, 0, len(cs))
	for _, c := range cs {
		out = append(out, toCardView(c))
	}
	return out
}
