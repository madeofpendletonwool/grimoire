package server

// The encounter designer. The rest of the encounter surface answers "is this
// fight too hard"; this answers the question a DM actually arrives with —
// "I want something creepy in the swamp for tonight, sort it out." The server
// does the parts that must be right: the DMG budget, the challenge-rating
// windows, and a shortlist of real SRD statblocks. The model does the part
// that must be interesting: choosing among them and writing the encounter
// around them. Every creature it names is resolved back against the local
// bestiary before the DM sees it, so a fabricated monster is reported, never
// rendered as real.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/encounter"
)

// designRequest is the body for /api/encounter/design. Everything is
// optional: an empty request still produces an encounter, because "I have no
// idea" is the case the designer exists for.
type designRequest struct {
	Idea       string              `json:"idea"`
	Party      []int               `json:"party"`
	Difficulty string              `json:"difficulty"`
	Feedback   string              `json:"feedback"` // revision instruction
	Current    []encounter.Monster `json:"current"`  // roster the revision applies to
	Notes      string              `json:"notes"`    // the design being revised
}

// maxFeedbackLen and maxIdeaLen bound the free text a request may carry, so a
// pasted novel cannot blow out the prompt.
const (
	maxIdeaLen     = 2000
	maxFeedbackLen = 2000
	maxNotesLen    = 20000
)

// handleEncounterDesign builds (or revises) an encounter as a stream of SSE
// events:
//
//	meta  — the budget, the shapes it can take, and how big the shortlist is
//	delta — the design as it is written
//	done  — the parsed roster, the recomputed verdict, and anything unverified
func (s *Server) handleEncounterDesign(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	var req designRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	req.Idea = truncateForPrompt(strings.TrimSpace(req.Idea), maxIdeaLen)
	req.Feedback = truncateForPrompt(strings.TrimSpace(req.Feedback), maxFeedbackLen)
	req.Notes = truncateForPrompt(req.Notes, maxNotesLen)

	if err := validateParty(req.Party); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := normalizeMonsters(req.Current); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !s.llm.Configured() {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf(
			"designing an encounter needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}

	// The shortlist comes from the local bestiary; without it there is
	// nothing to choose from. Mirroring it is a one-off cost, so a first
	// request pays it rather than reporting the feature unavailable.
	if s.catalog.Count() == 0 {
		if err := s.catalog.Sync(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, fmt.Errorf(
				"the bestiary is not mirrored yet and could not be fetched: %v", err))
			return
		}
	}

	party := req.Party
	assumedParty := false
	if len(party) == 0 {
		party = append([]int(nil), encounter.DefaultParty...)
		assumedParty = true
	}
	budget := encounter.Plan(party, req.Difficulty)
	hints := encounter.ReadIdea(req.Idea + " " + req.Feedback)
	pool := encounter.BuildPool(s.catalog, budget, hints, nil)
	if pool.Len() == 0 {
		// A gate that narrowed to nothing (an idea naming a creature type the
		// budget cannot afford) still deserves an encounter: drop the hints
		// and offer the budget's own shortlist.
		pool = encounter.BuildPool(s.catalog, budget, encounter.Hints{}, nil)
	}

	sse := newSSEWriter(w)
	sse.send("meta", map[string]any{
		"budget":        budget,
		"hints":         hints,
		"candidates":    pool.Len(),
		"bestiary":      s.catalog.Count(),
		"assumed_party": assumedParty,
	})

	ctx, cancel := context.WithTimeout(r.Context(), answerTimeout)
	defer cancel()
	answer, streamErr := s.llm.StreamPrompt(ctx, encounterDesignSystemPrompt,
		designUserPrompt(req, budget, hints, pool),
		func(text string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return sse.send("delta", map[string]any{"text": text})
		})
	if streamErr != nil && strings.TrimSpace(answer) == "" {
		sse.send("error", map[string]any{"error": fmt.Sprintf("the model could not be reached: %v", streamErr)})
		return
	}

	design := encounter.ParseDesign(s.catalog, answer)
	verdict := encounter.Evaluate(party, design.Monsters)
	payload := map[string]any{
		"name":       design.Name,
		"monsters":   design.Monsters,
		"unverified": design.Unverified,
		"notes":      design.Prose,
		"verdict":    verdict,
		"budget":     budget,
		"party":      party,
	}
	if streamErr != nil {
		// Partial text is already on screen; report the cut alongside
		// whatever roster did parse.
		payload["truncated"] = true
	}
	sse.send("done", payload)
}

const encounterDesignSystemPrompt = `You are Grimoire's encounter designer, building a single D&D 5e combat encounter for a DM who is prepping a session.

You are handed four things, all computed by the server and all authoritative: the party, the XP budget from the 2014 Dungeon Master's Guide tables, the shapes that budget can take, and a shortlist of real SRD creatures with their true challenge ratings, statblock numbers and derived tags. Nothing else is real.

STRICT RULES
1. Use ONLY creatures from the shortlist. Never invent a monster, never rename one, never invent or adjust a statblock number. Spell each name exactly as the shortlist spells it.
2. The "## Roster" section is machine-parsed. Every line in it must be exactly "N × Creature Name" — a count, a multiplication sign, the name. No bullets, no notes, no parentheses, nothing else on the line. Put your commentary in the other sections.
3. Hit the budget. Pick one of the offered shapes (or blend two — a boss plus minions is a blend of solo and horde) and keep the encounter's total raw XP close to that shape's allowance. The server recomputes the difficulty and shows the DM whether you landed it.
4. The DM's idea is the brief. Honour it — the creatures, the mood, the setting. If they gave no idea at all, invent one worth running rather than asking them for more.
5. Keep the whole reply under 450 words. A DM is skimming this at the table.

FORMAT — Markdown, exactly these sections, in this order:

# <the encounter's name — evocative, four words or fewer, no quotation marks>

## The pitch
Two or three sentences: what the party walks into and why it is interesting. Present tense.

## Roster
N × Creature Name
N × Creature Name

## Terrain
Two or three sentences on the battlefield: one feature that changes how the fight plays (cover, height, water, darkness, a hazard), stated concretely with distances.

## Tactics
Three or four short lines on how the monsters actually fight — opening move, what they do when hurt, whether they flee or fight to the death. Name creatures from the roster and reference their real abilities.

## Twist
One sentence: something that could turn the fight sideways — a reinforcement, a hostage, a collapse, a reason to talk instead.

## Scaling
One line for a tougher table and one line for a weaker one, adjusting counts or swapping a creature from the shortlist. Never invent a creature here either.

Write like an experienced DM handing a colleague a prepped encounter: concrete, specific, no filler, no preamble, no closing summary.`

// designUserPrompt lays out one design exchange: the brief, the budget as
// fact, and the shortlist tiered by the slot each creature can fill.
func designUserPrompt(req designRequest, b encounter.Budget, h encounter.Hints, pool encounter.Pool) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "PARTY: %d characters, levels %s (average %.1f)\n",
		b.PartySize, levelList(b.Party), b.AvgLevel)
	fmt.Fprintf(&sb, "TARGET DIFFICULTY: %s\n\n", b.Band)

	sb.WriteString("BUDGET (2014 DMG, computed by the server — treat as fact):\n")
	for _, band := range encounter.Bands {
		marker := ""
		if band == b.Band {
			marker = "  <- target"
		}
		fmt.Fprintf(&sb, "- %s threshold: %d adjusted XP%s\n", band, b.Thresholds[band], marker)
	}
	fmt.Fprintf(&sb, "Aim for about %d adjusted XP; past %d it tips into the next band.\n", b.TargetXP, b.CeilingXP)
	fmt.Fprintf(&sb, "The largest single monster that fits is CR %s.\n\n", b.MaxSoloCR)

	sb.WriteString("SHAPES this budget can take (the multiplier is the DMG's, applied for this party size):\n")
	for _, s := range b.Shapes {
		fmt.Fprintf(&sb, "- %s: %d monsters, ×%g multiplier, %d raw XP to spend (~%d each, about CR %s)\n",
			s.Label, s.Count, s.Multiplier, s.RawXP, s.EachXP, s.EachCR)
	}
	sb.WriteString("\n")

	if req.Idea != "" {
		fmt.Fprintf(&sb, "THE DM'S IDEA: %s\n", req.Idea)
	} else {
		sb.WriteString("THE DM'S IDEA: none given — they want you to come up with something. Invent a concept worth running and commit to it.\n")
	}
	if len(h.Types) > 0 {
		fmt.Fprintf(&sb, "Creature types their idea points at: %s\n", strings.Join(h.Types, ", "))
	}
	if len(h.Tags) > 0 {
		fmt.Fprintf(&sb, "Qualities their idea points at: %s\n", strings.Join(h.Tags, ", "))
	}
	sb.WriteString("\n")

	if req.Feedback != "" {
		fmt.Fprintf(&sb, "REVISION REQUESTED: %s\n", req.Feedback)
		fmt.Fprintf(&sb, "The encounter as it stands: %s\n", encounter.Describe(req.Current))
		if strings.TrimSpace(req.Notes) != "" {
			fmt.Fprintf(&sb, "Its current write-up:\n%s\n", req.Notes)
		}
		sb.WriteString("Rewrite the whole encounter with the revision applied — same format, all sections, still inside the budget.\n\n")
	}

	sb.WriteString("SHORTLIST — every creature below is a real SRD statblock the server verified. You may use only these.\n")
	writeTier(&sb, "BOSS TIER (big enough to anchor the fight)", pool.Boss)
	writeTier(&sb, "STANDARD TIER (the body of a pack)", pool.Standard)
	writeTier(&sb, "MINION TIER (cheap enough to field in numbers)", pool.Minion)
	writeTier(&sb, "MATCHES THE IDEA (any challenge rating — use if the flavour is worth the cost)", pool.Flavour)

	sb.WriteString("\nDesign the encounter now, in the exact format given.")
	return sb.String()
}

// writeTier prints one slot of the shortlist, or nothing when it is empty.
func writeTier(sb *strings.Builder, label string, list []encounter.Creature) {
	if len(list) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n%s:\n", label)
	for _, c := range list {
		fmt.Fprintf(sb, "- %s\n", c.Line())
	}
}

// levelList renders party levels compactly: "3, 3, 3, 2".
func levelList(party []int) string {
	parts := make([]string, 0, len(party))
	for _, lvl := range party {
		parts = append(parts, fmt.Sprint(lvl))
	}
	return strings.Join(parts, ", ")
}

// handleEncounterStatblock serves one creature's full statblock from the
// local bestiary, so a DM can read what a monster actually does without
// leaving the builder.
func (s *Server) handleEncounterStatblock(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name is required"))
		return
	}
	c, ok := s.catalog.Lookup(name)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no SRD statblock for %q", name))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"creature": c})
}

// handleEncounterBudget previews the budget for a party and band without
// designing anything. The builder's "here is what fits" readout calls it, so
// the DMG tables stay on the server the way the verdict does.
func (s *Server) handleEncounterBudget(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	var req struct {
		Party      []int  `json:"party"`
		Difficulty string `json:"difficulty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if err := validateParty(req.Party); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"budget":   encounter.Plan(req.Party, req.Difficulty),
		"bestiary": s.catalog.Count(),
	})
}
