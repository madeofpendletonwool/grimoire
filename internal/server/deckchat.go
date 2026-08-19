package server

// Talking about a deck. The analyzer answers "what is wrong with this list";
// this answers everything after that — "why is my curve bad", "what should I
// cut for Cultivate", "does Shelob actually do anything here". It is the same
// grounded discipline as the sage: the model is handed the real list, the real
// analysis, and the real oracle text of the cards under discussion, and is
// told to say so when it is unsure rather than invent a card.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/deck"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// deckChatRequest is the body for /api/deck/chat: the deck under discussion,
// the question, and the earlier turns so follow-ups resolve against what was
// already said. The deck travels with every request because the surface is
// stateless — an unsaved draft is as discussable as a saved one.
type deckChatRequest struct {
	Commander string       `json:"commander"`
	Cards     []deck.Entry `json:"cards"`
	Question  string       `json:"question"`
	History   []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"history"`
}

// maxChatHistory bounds how much of the conversation is replayed. Deck talk
// wanders, and the grounding block is re-sent every turn anyway.
const maxChatHistory = 12

// handleDeckChat answers a question about the deck as an SSE stream: delta
// events carry the answer as it is written, done closes it.
func (s *Server) handleDeckChat(w http.ResponseWriter, r *http.Request) {
	if !s.deckEnabled(w) {
		return
	}
	var req deckChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("question is required"))
		return
	}
	if !s.llm.Configured() {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("talking about a deck needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}

	ctx := r.Context()
	commander := strings.TrimSpace(req.Commander)
	var commanderCard *carddb.Card
	if commander != "" {
		if c, ok := s.carddb.Resolve(ctx, commander); ok {
			commanderCard, commander = c, c.Name
		}
	}
	entries, _ := s.resolveEntries(ctx, req.Cards)
	analysis := deck.Analyze(commander, entries, s.cardLookup)

	turns := make([]llm.Turn, 0, len(req.History)+1)
	history := req.History
	if len(history) > maxChatHistory {
		history = history[len(history)-maxChatHistory:]
	}
	for _, h := range history {
		turns = append(turns, llm.Turn{Role: h.Role, Content: h.Content})
	}
	turns = append(turns, llm.Turn{
		Role:    "user",
		Content: s.deckChatPrompt(commanderCard, commander, entries, analysis, req.Question),
	})

	sse := newSSEWriter(w)
	ctx, cancel := context.WithTimeout(ctx, answerTimeout)
	defer cancel()
	answer, err := s.llm.StreamChat(ctx, deckChatSystemPrompt, turns, func(text string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return sse.send("delta", map[string]any{"text": text})
	})
	if err != nil && strings.TrimSpace(answer) == "" {
		sse.send("error", map[string]any{"error": fmt.Sprintf("the model could not be reached: %v", err)})
		return
	}
	if err != nil {
		sse.send("error", map[string]any{"error": fmt.Sprintf("the answer was cut short: %v", err)})
		return
	}
	sse.send("done", map[string]any{"answer": answer})
}

const deckChatSystemPrompt = `You are Grimoire's deck coach, talking with a player about their Commander deck.

You are given the real decklist, a deterministic analysis of it computed by the server, and the real oracle text of the commander and of any card the player asked about. Treat all of that as authoritative — it comes from real card data, not from memory.

RULES:
1. Never invent card text. If you are not certain what a card does and its text was not provided, say so and ask, or suggest the player look it up.
2. The analysis numbers (card count, lands, curve, ramp/draw/interaction counts) are computed from the list. Do not contradict them.
3. When you suggest a card that is not in the list, name it plainly and say it is a suggestion the player should verify — you are not being handed its text.
4. Answer the question that was asked. Be concrete and brief: a few short paragraphs or a short list, not an essay. No preamble.
5. Talk like a knowledgeable playgroup regular, not a marketing page.`

// deckChatPrompt lays out one turn's grounding: the commander in full, the
// list in compact form, the analysis, and the oracle text of the cards the
// question actually names — the last is why "what does Arachnogenesis do here"
// gets a real answer rather than a plausible one.
func (s *Server) deckChatPrompt(cmdr *carddb.Card, commanderName string, entries []deck.Entry, a deck.Analysis, question string) string {
	var b strings.Builder
	if cmdr != nil {
		fmt.Fprintf(&b, "Commander: %s\nMana cost: %s\nType: %s\nOracle text: %s\n\n",
			cmdr.Name, cmdr.ManaCost, cmdr.TypeLine, cmdr.OracleText)
	} else if commanderName != "" {
		fmt.Fprintf(&b, "Commander: %s (not found in the card database)\n\n", commanderName)
	} else {
		b.WriteString("Commander: none chosen yet.\n\n")
	}

	if len(entries) == 0 {
		b.WriteString("Decklist: empty — the player has not drafted or pasted a list yet.\n\n")
	} else {
		b.WriteString("Decklist (name | mana cost | type):\n")
		for _, e := range entries {
			c, ok := s.cardLookup(e.Name)
			if !ok {
				fmt.Fprintf(&b, "%d %s\n", e.Count, e.Name)
				continue
			}
			board := ""
			if e.Board == "sideboard" {
				board = " [sideboard]"
			}
			fmt.Fprintf(&b, "%d %s | %s | %s%s\n", e.Count, c.Name, c.ManaCost, c.TypeLine, board)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "Analysis (authoritative, computed from the list):\n")
	fmt.Fprintf(&b, "- %d maindeck cards, %d lands, average mana value %.1f\n", a.TotalMain, a.Lands, a.AvgMV)
	fmt.Fprintf(&b, "- ramp %d, card draw %d, interaction %d\n", a.Ratios.Ramp, a.Ratios.Draw, a.Ratios.Interaction)
	if len(a.IdentityBad) > 0 {
		names := make([]string, 0, len(a.IdentityBad))
		for _, v := range a.IdentityBad {
			names = append(names, v.Name+" ("+v.Identity+")")
		}
		fmt.Fprintf(&b, "- outside the commander's color identity: %s\n", strings.Join(names, ", "))
	}
	if len(a.Warnings) > 0 {
		fmt.Fprintf(&b, "- warnings: %s\n", strings.Join(a.Warnings, "; "))
	}

	if mentioned := s.cardsMentioned(question, entries); len(mentioned) > 0 {
		b.WriteString("\nOracle text of the cards named in the question:\n")
		for _, c := range mentioned {
			fmt.Fprintf(&b, "- %s | %s | %s\n  %s\n", c.Name, c.ManaCost, c.TypeLine, c.OracleText)
		}
	}

	fmt.Fprintf(&b, "\nPlayer's question: %s", question)
	return b.String()
}

// maxMentions bounds how many cards' full text one question can pull in.
const maxMentions = 12

// cardsMentioned finds the cards a question names. The deck's own cards are
// checked first — a player asking "should I cut Nyx Weaver" means the one in
// their list — and any remaining capitalized phrase is resolved against the
// card database so a card they are considering also arrives with real text.
func (s *Server) cardsMentioned(question string, entries []deck.Entry) []*carddb.Card {
	asked := carddb.NormalizeName(question)
	seen := map[string]bool{}
	var out []*carddb.Card
	for _, e := range entries {
		if len(out) >= maxMentions {
			return out
		}
		norm := carddb.NormalizeName(e.Name)
		// Short names ("Forest") match too eagerly to be worth including.
		if len(norm) < 6 || seen[norm] || !strings.Contains(asked, norm) {
			continue
		}
		if c, ok := s.cardLookup(e.Name); ok {
			seen[norm] = true
			out = append(out, c)
		}
	}
	return out
}
