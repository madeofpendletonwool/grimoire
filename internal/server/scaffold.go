package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// The resolver walks a stated board and sequence step by step — priority,
// triggers, layers, what resolves when — and it is the right tool for exactly
// the questions people type into the chat instead: "can I equip in response to
// their instant?" is a board and a sequence written as a sentence. Nobody
// switches modes to restate it in the resolver's notation, so the resolver
// goes unused while the chat answers stack questions in prose.
//
// This closes the gap from the other side. A question that describes a board
// is converted into the resolver's own grammar and handed straight to it, so
// the walkthrough is one click from the answer rather than a mode the reader
// has to know exists and a form they have to fill in.

// scaffoldSystem instructs the conversion. It is deliberately mechanical: the
// model is transcribing a scenario into a notation, not reasoning about it —
// the resolver does the reasoning, with its own grounding, immediately after.
const scaffoldSystem = `You convert a Magic: The Gathering question written in prose into the interaction resolver's notation. You do not answer the question.

Output JSON only, with exactly these keys:
{"board": "...", "sequence": "...", "note": "..."}

board: one permanent per line, as [controller:] card name [# state].
  Controllers are "You" and "Opp". State after "#" carries annotations like "tapped".
  Include only permanents the question actually establishes.
sequence: one numbered step per line, in the order they are played:
  "1. Opp casts Lightning Bolt targeting Grizzly Bears"
  Each step names who acts and what they do. Put the earliest action first.
note: any clarification the question gives that is neither a permanent nor a step
  (an ability's text, an assumption stated by the asker). Empty string if none.

Use the exact card names given. If the question names no specific cards, describe the
objects as plainly as the question does ("an Equipment that grants hexproof"). Never
invent permanents, steps, or cards the question does not mention.`

// handleResolveScaffold turns a prose question into resolver input. It is a
// conversion, not an answer: the client takes what comes back, shows it in the
// resolver's editable form, and runs the walkthrough from there — so a bad
// transcription is visible and fixable rather than silently decisive.
func (s *Server) handleResolveScaffold(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Corpus   string     `json:"corpus"`
		Question string     `json:"question"`
		History  []llm.Turn `json:"history"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("question is required"))
		return
	}
	if !resolveScaffoldSupported(parseCorpus(req.Corpus)) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the interaction resolver is Magic-only"))
		return
	}
	if !s.llm.Configured() {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the resolver is not configured"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), scaffoldTimeout)
	defer cancel()
	out, err := s.llm.AnswerPrompt(ctx, scaffoldSystem, scaffoldUser(req.Question, req.History))
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("the scenario could not be transcribed: %w", err))
		return
	}

	var scaffold struct {
		Board    string `json:"board"`
		Sequence string `json:"sequence"`
		Note     string `json:"note"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &scaffold); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("the scenario could not be transcribed"))
		return
	}
	if strings.TrimSpace(scaffold.Board) == "" && strings.TrimSpace(scaffold.Sequence) == "" {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Errorf("this question doesn't describe a board and a sequence to walk"))
		return
	}
	writeJSON(w, http.StatusOK, scaffold)
}

// scaffoldTimeout is short by design: this is one small structured conversion
// standing between a click and the walkthrough the reader actually wants.
const scaffoldTimeout = 45 * time.Second

// scaffoldHistoryTurns bounds how much of the conversation the conversion
// reads. A board-state question is usually built over two or three turns —
// the scenario, then the detail that changes it — and older turns describe a
// different board.
const scaffoldHistoryTurns = 4

func scaffoldUser(question string, history []llm.Turn) string {
	var b strings.Builder
	if n := len(history); n > 0 {
		if n > scaffoldHistoryTurns {
			history = history[n-scaffoldHistoryTurns:]
		}
		b.WriteString("Earlier in the conversation:\n\n")
		for _, t := range history {
			fmt.Fprintf(&b, "%s: %s\n\n", t.Role, truncateText(t.Content, 1200))
		}
	}
	b.WriteString("Convert this question into resolver notation:\n\n")
	b.WriteString(question)
	return b.String()
}

// extractJSON pulls the JSON object out of a reply that may have arrived
// wrapped in a code fence or trailing prose.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}

// resolveScaffoldSupported reports whether a corpus has an interaction
// resolver at all. Magic's does; the SRD has no stack to walk.
func resolveScaffoldSupported(corpus data.Corpus) bool { return corpus == data.CorpusMTG }
