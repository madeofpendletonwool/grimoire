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

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
)

// designRequest is the body for /api/encounter/design. Everything is
// optional: an empty request still produces an encounter, because "I have no
// idea" is the case the designer exists for. An objective kind outside the
// declared vocabulary is a 400, never a default — an objective is a declared
// kind, not a sentence.
type designRequest struct {
	Idea       string               `json:"idea"`
	Party      []int                `json:"party"`
	Difficulty string               `json:"difficulty"`
	Feedback   string               `json:"feedback"` // revision instruction
	Current    []encounter.Monster  `json:"current"`  // roster the revision applies to
	Notes      string               `json:"notes"`    // the design being revised
	Objective  *encounter.Objective `json:"objective"`
	// CampaignID is optional (MAD-378): with one, an unstated party comes
	// from the campaign's declared party block instead of the default table,
	// and the caller must hold that campaign's DM perspective. Without one
	// this is exactly the request MAD-299 shipped.
	CampaignID string `json:"campaign_id"`
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
	if err := validateObjective(req.Objective); err != nil {
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

	party, partySource, partyLine, partyTable, ok := s.resolveDesignParty(w, r, req.CampaignID, req.Party)
	if !ok {
		return
	}
	// The caller's homebrew rides the whole design: the pool may offer it,
	// the roster parse resolves it, and the tactical read prices it.
	overlay := s.homebrewOverlay(r, req.CampaignID)
	assumedParty := partySource == "default"
	obj := encounter.Objective{}
	if req.Objective != nil {
		obj = *req.Objective
	}
	budget := encounter.Plan(party, req.Difficulty, obj)
	hints := encounter.ReadIdea(req.Idea + " " + req.Feedback)
	pool := encounter.BuildPool(s.catalog, overlay, budget, hints, nil)
	if pool.Len() == 0 {
		// A gate that narrowed to nothing (an idea naming a creature type the
		// budget cannot afford) still deserves an encounter: drop the hints
		// and offer the budget's own shortlist.
		pool = encounter.BuildPool(s.catalog, overlay, budget, encounter.Hints{}, nil)
	}

	sse := newSSEWriter(w)
	sse.send("meta", map[string]any{
		"budget":        budget,
		"hints":         hints,
		"candidates":    pool.Len(),
		"bestiary":      s.catalog.Count(),
		"assumed_party": assumedParty,
		"party":         party,
		"party_source":  partySource,
		"party_label":   partyLine,
	})

	ctx, cancel := context.WithTimeout(r.Context(), answerTimeout)
	defer cancel()
	answer, streamErr := s.llm.StreamPrompt(ctx, encounterDesignSystemPrompt,
		designUserPrompt(req, budget, hints, pool, obj, partyFacts(partyTable)),
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

	design := encounter.ParseDesign(s.catalog, overlay, answer)
	// A survive roster priced across its waves, not as one board; every
	// other fight is priced the way it always was.
	verdict := encounter.Evaluate(party, design.Monsters)
	if obj.Normalized().Kind == encounter.Survive && len(design.Waves) >= 2 {
		verdict = encounter.EvaluateWaves(party, design.Waves)
	}

	// The tactical analysis (MAD-381): the server's read of what this roster
	// will do and to whom, over the statblocks the design chose and the
	// party block, with the terrain when the objective generated one. The
	// model's Tactics prose is gated against it — any figure it asserted
	// that traces to no derivation rejects the prose, never the analysis.
	tacticsParty := partyFacts(partyTable)
	if len(tacticsParty) == 0 {
		tacticsParty = levelFacts(party)
	}
	tacticsRoster, missing := rosterFacts(s.catalog, overlay, design.Monsters)
	tacticsIn := encounter.TacticsInput{
		Party:   tacticsParty,
		Roster:  tacticsRoster,
		Terrain: budget.Terrain,
		Waves:   budget.Waves,
	}
	analysis := encounter.Analyze(tacticsIn)
	appendMissingCaveat(analysis, missing)

	payload := map[string]any{
		"name":       design.Name,
		"monsters":   design.Monsters,
		"unverified": design.Unverified,
		"notes":      design.Prose,
		"verdict":    verdict,
		"budget":     budget,
		"party":      party,
		"tactics":    withProse(tacticsIn, analysis, design.Prose),
	}
	if obj.Normalized().Kind != encounter.Defeat {
		// The fight is about something: the structured objective for the
		// chips and the save, its rendered ending for the "how this ends"
		// block, and the terrain generated with it. All server-owned —
		// the model described them, it did not set them.
		ending := obj.Normalized().Ending()
		payload["objective"] = obj.Normalized()
		payload["ending"] = ending
		if budget.Terrain != nil {
			payload["terrain"] = *budget.Terrain
		}
		payload["waves"] = design.Waves
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
6. The "## Tactics" section may not contain a single digit. Every number in this encounter is the server's; yours is the story. Spell counts as words ("both shamans", "three waves"). The server checks your tactics against its own arithmetic, and a prose with an asserted figure in it is shown to the DM as rejected.

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
Three or four short lines on how the monsters actually fight — opening move, what they do when hurt, whether they flee or fight to the death. Name creatures from the roster and reference their real abilities, and write from the party facts and statblocks you were handed: who the pack's output actually threatens, and what the party can and cannot answer it with. Not a single digit (rule 6) — the server shows the DM the numbers itself.

## Twist
One sentence: something that could turn the fight sideways — a reinforcement, a hostage, a collapse, a reason to talk instead.

## Scaling
One line for a tougher table and one line for a weaker one, adjusting counts or swapping a creature from the shortlist. Never invent a creature here either.

Write like an experienced DM handing a colleague a prepped encounter: concrete, specific, no filler, no preamble, no closing summary.`

// designUserPrompt lays out one design exchange: the brief, the budget as
// fact, and the shortlist tiered by the slot each creature can fill. A
// non-defeat objective adds its own blocks — what the fight is about, the
// terrain generated with it, and the roster format a survive fight needs —
// and a defeat objective adds nothing at all. Party facts ride when the
// party block declared anything: the tactics prose is written against real
// armour classes and weak saves, not invented ones.
func designUserPrompt(req designRequest, b encounter.Budget, h encounter.Hints, pool encounter.Pool, obj encounter.Objective, facts []encounter.PCFact) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "PARTY: %d characters, levels %s (average %.1f)\n",
		b.PartySize, levelList(b.Party), b.AvgLevel)
	fmt.Fprintf(&sb, "TARGET DIFFICULTY: %s\n\n", b.Band)

	writePartyFacts(&sb, facts)
	writeObjectiveBlock(&sb, obj, b)

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
		if s.Waves > 1 {
			fmt.Fprintf(&sb, "- %s: %d monsters per wave, %d waves, ×%g per wave, %d raw XP across all waves (~%d per wave, about CR %s each)\n",
				s.Label, s.Count, s.Waves, s.Multiplier, s.RawXP, s.WaveXP, s.EachCR)
			continue
		}
		fmt.Fprintf(&sb, "- %s: %d monsters, ×%g multiplier, %d raw XP to spend (~%d each, about CR %s)\n",
			s.Label, s.Count, s.Multiplier, s.RawXP, s.EachXP, s.EachCR)
	}
	sb.WriteString("\n")

	writeTerrainBlock(&sb, b)

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

// writePartyFacts hands the model the party block's mechanical sheet: who
// the tactics prose is written against. Only declared fields ride — an
// undeclared armour class stays unknown rather than becoming a number.
func writePartyFacts(sb *strings.Builder, facts []encounter.PCFact) {
	usable := 0
	for _, pc := range facts {
		if pc.AC > 0 || pc.MaxHP > 0 || len(pc.Saves) > 0 || pc.Class != "" {
			usable++
		}
	}
	if usable == 0 {
		return
	}
	sb.WriteString("PARTY FACTS (from the party block — treat as fact; the Tactics section is written against these):\n")
	for _, pc := range facts {
		var bits []string
		if pc.Class != "" && pc.Level > 0 {
			bits = append(bits, fmt.Sprintf("%s %d", pc.Class, pc.Level))
		}
		if pc.AC > 0 {
			bits = append(bits, fmt.Sprintf("AC %d", pc.AC))
		}
		if pc.MaxHP > 0 {
			bits = append(bits, fmt.Sprintf("%d hp", pc.MaxHP))
		}
		if worst, bonus := weakestSave(pc.Saves); worst != "" {
			bits = append(bits, fmt.Sprintf("weakest save %s %+d", worst, bonus))
		}
		if len(bits) > 0 {
			fmt.Fprintf(sb, "- %s: %s\n", pc.Name, strings.Join(bits, ", "))
		}
	}
	sb.WriteString("\n")
}

// weakestSave names the pc's lowest declared save — the one a rider wants.
func weakestSave(saves map[string]int) (string, int) {
	worst, bonus := "", 0
	for _, ab := range []string{"str", "dex", "con", "int", "wis", "cha"} {
		v, ok := saves[ab]
		if !ok {
			continue
		}
		if worst == "" || v < bonus {
			worst, bonus = strings.ToUpper(ab), v
		}
	}
	return worst, bonus
}

// writeObjectiveBlock tells the model what the fight is about and which of
// its numbers are already decided. Defeat writes nothing: the default fight's
// prompt is exactly what it has always been.
func writeObjectiveBlock(sb *strings.Builder, obj encounter.Objective, b encounter.Budget) {
	obj = obj.Normalized()
	if obj.Kind == encounter.Defeat {
		return
	}
	e := obj.Ending()
	fmt.Fprintf(sb, "OBJECTIVE: %s (computed by the server — treat as fact)\n", e.Label)
	fmt.Fprintf(sb, "This fight is about something other than killing everything. Its success and failure are fixed:\n")
	fmt.Fprintf(sb, "- Success: %s\n", e.Success)
	fmt.Fprintf(sb, "- Failure: %s\n", e.Failure)
	if e.Clock != "" {
		fmt.Fprintf(sb, "- Clock: %s\n", e.Clock)
	}
	if len(b.Adjustments) > 0 {
		sb.WriteString("The server has already applied this objective's XP rules, and the budget below includes them:\n")
		for _, a := range b.Adjustments {
			fmt.Fprintf(sb, "- %s\n", a.Detail)
		}
	}
	sb.WriteString("The pitch and tactics must make the objective the reason the fight plays differently — not a footnote on a plain fight.\n\n")
}

// writeTerrainBlock hands the model the terrain generated with the
// objective. The mechanics are server-owned and must survive verbatim; the
// model's job is naming and describing what it was handed.
func writeTerrainBlock(sb *strings.Builder, b encounter.Budget) {
	if b.Terrain == nil || (len(b.Terrain.Features) == 0 && len(b.Terrain.Hazards) == 0) {
		return
	}
	sb.WriteString("TERRAIN the server generated (fact — keep every mechanical effect exactly as given, and name each feature concretely in your Terrain section):\n")
	for _, f := range b.Terrain.Features {
		fmt.Fprintf(sb, "- %s (%s): %s Where: %s.\n", encounter.FeatureLabel(f.Kind), f.Kind, f.Effect, f.Area)
	}
	for _, h := range b.Terrain.Hazards {
		fmt.Fprintf(sb, "- HAZARD %s (%s): %s save DC %d; %s %s damage on a failure. Trigger: %s. Area: %s.\n",
			h.Name, h.Kind, strings.ToUpper(h.SaveAbility), h.DC, h.Damage, h.DamageType, h.Trigger, h.Area)
	}
	if b.Waves > 1 {
		fmt.Fprintf(sb, "ROSTER FORMAT for this objective: the roster is the WHOLE fight across %d waves, one wave per line, exactly \"Wave 1: N × Creature Name\" — this overrides the general roster rule above. Wave 1 is what the party meets first; later waves reinforce it. Spend the full raw XP across every wave: the verdict checks the total against the party, not the first wave alone.\n", b.Waves)
	}
	sb.WriteString("\n")
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
	c, ok := s.catalog.Lookup(name, s.homebrewOverlay(r, strings.TrimSpace(r.URL.Query().Get("campaign"))))
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
		Party      []int                `json:"party"`
		Difficulty string               `json:"difficulty"`
		CampaignID string               `json:"campaign_id"`
		Objective  *encounter.Objective `json:"objective"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if err := validateParty(req.Party); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateObjective(req.Objective); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	obj := encounter.Objective{}
	if req.Objective != nil {
		obj = *req.Objective
	}
	// A campaign_id with no party is what prefills the builder's party boxes;
	// encounter.Plan itself is unchanged and still answers for a bare party,
	// which is what the non-campaign surface sends.
	party := req.Party
	source := ""
	line := ""
	if strings.TrimSpace(req.CampaignID) != "" {
		table := s.campaignParty(w, r, req.CampaignID)
		if table == nil {
			return
		}
		if len(party) == 0 {
			party = table.Levels()
			if len(party) > 0 {
				source, line = "campaign", partyLabel(party)
			}
		}
	}
	budget := encounter.Plan(party, req.Difficulty, obj)
	resp := map[string]any{
		"budget":       budget,
		"bestiary":     s.catalog.Count(),
		"party":        party,
		"party_source": source,
		"party_label":  line,
	}
	// The rendered ending rides along so the builder can show "how this
	// ends" before anything is designed — the server writes it, never the
	// client.
	if obj.Normalized().Kind != encounter.Defeat {
		resp["ending"] = obj.Normalized().Ending()
		if budget.Terrain != nil {
			resp["terrain"] = *budget.Terrain
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveDesignParty settles which party a design request is built against:
// the one it sent, the campaign's declared party block when it named a
// campaign and sent none, or the DMG default table when it has neither. It
// writes the HTTP error itself and reports false when the caller should not
// proceed — which is why it runs before the SSE stream opens, since a 403
// after the first event would arrive as a stream the browser has already
// accepted.
//
// The returned source is "request", "campaign" or "default"; the line is the
// "from your campaign — …" note, empty unless the party came from one. The
// table is the campaign's full party block when one was read — the tactical
// analysis aims at it — and nil otherwise.
func (s *Server) resolveDesignParty(w http.ResponseWriter, r *http.Request, campaignID string, sent []int) (party []int, source, line string, table *campaign.PartyTable, ok bool) {
	if strings.TrimSpace(campaignID) != "" {
		table = s.campaignParty(w, r, campaignID)
		if table == nil {
			return nil, "", "", nil, false
		}
		if len(sent) == 0 {
			if levels := table.Levels(); len(levels) > 0 {
				return levels, "campaign", partyLabel(levels), table, true
			}
		}
	}
	if len(sent) > 0 {
		return sent, "request", "", nil, true
	}
	return append([]int(nil), encounter.DefaultParty...), "default", "", nil, true
}
