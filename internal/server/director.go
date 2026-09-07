package server

// The state-aware encounter director's surface (MAD-427, stage 10 of
// MAD-417): advisory monster tactics grounded in the live battle.
//
//	POST /api/campaigns/{id}/combat/director   suggest what the monsters do next (DM)
//
// The route is the DM's, like every combat route: the director reads
// the whole battle — both sides' numbers, the party's balances — and
// that read is the DM-screen perspective. It is invoked explicitly,
// one advisory pass per request, and it writes nothing: the
// suggestions, their citations and the basis ride back in the response
// body and exist nowhere else. The engine (internal/director) holds
// read windows only — there is no dice handle behind it, no write
// path under it, which is what "advisory only" means here: a shape,
// not a promise.
//
// MAD-318 owns the DM-screen surface that will render this; the
// engine and this route are what that surface reads, and the merge
// proposal when it promotes is one director, not two.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/director"
)

// directorEnabled reports the director's availability: the tracker it
// reads and the engine itself must both be wired.
func (s *Server) directorEnabled(w http.ResponseWriter) bool {
	if !s.combatEnabled(w) {
		return false
	}
	if s.director == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the encounter director is not configured on this install"))
		return false
	}
	return true
}

// WithDirector wires the state-aware encounter director (MAD-427).
// Without it the director endpoint answers 503.
func (s *Server) WithDirector(d *director.Service) *Server {
	s.director = d
	return s
}

// directorCitation is one basis line a suggestion rests on, resolved
// and rendered — the state that justifies the suggestion, shown
// beside it.
type directorCitation struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Text   string `json:"text"`
}

// directorSuggestionView is one gated suggestion with its citations
// resolved onto the basis lines they name.
type directorSuggestionView struct {
	Actor     string             `json:"actor"`
	Action    string             `json:"action"`
	Reasoning string             `json:"reasoning"`
	Basis     []directorCitation `json:"basis"`
}

// handleCombatDirector advises on the campaign's active battle: the
// grounding reads the tracker, the ledger and the statblocks the fight
// was built from; the model suggests; the gate decides. A suggestion
// that survives carries its citations; the count of those that did not
// rides beside them so the DM knows what the gate caught.
func (s *Server) handleCombatDirector(w http.ResponseWriter, r *http.Request) {
	if !s.directorEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.llm.Configured() {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf(
			"the encounter director is not configured. Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL / ANTHROPIC_MODEL) to enable it."))
		return
	}
	var req struct {
		Question string `json:"question"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	req.Question = strings.TrimSpace(req.Question)

	g, err := s.director.Ground(r.Context(), userID(r), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	res, err := s.director.Advise(r.Context(), g, req.Question)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("the director could not advise: %v", err))
		return
	}

	byID := make(map[string]director.Basis, len(g.Basis))
	for _, b := range g.Basis {
		byID[b.ID] = b
	}
	suggestions := make([]directorSuggestionView, 0, len(res.Suggestions))
	for _, sg := range res.Suggestions {
		view := directorSuggestionView{Actor: sg.Actor, Action: sg.Action, Reasoning: sg.Reasoning}
		for _, id := range sg.BasisIDs {
			if b, ok := byID[id]; ok {
				view.Basis = append(view.Basis, directorCitation{ID: b.ID, Kind: b.Kind, Source: b.Source, Text: b.Text})
			}
		}
		if view.Basis == nil {
			view.Basis = []directorCitation{}
		}
		suggestions = append(suggestions, view)
	}

	body := map[string]any{
		"combat": map[string]any{
			"id":    g.CombatID,
			"name":  g.Name,
			"round": g.Round,
			"turn":  g.Turn,
		},
		"suggestions": suggestions,
		"basis":       g.Basis,
		"dropped":     res.Dropped,
		"model":       s.director.ModelName(),
	}
	if req.Question != "" {
		body["question"] = req.Question
	}
	if len(g.Caveats) > 0 {
		body["caveats"] = g.Caveats
	}
	writeJSON(w, http.StatusOK, body)
}
