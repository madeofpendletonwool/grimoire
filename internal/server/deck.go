package server

// The deck builder's HTTP surface: commander proposals, the grounded draft
// flow (streamed like the ask flow), decklist analysis, and owner-scoped
// saved decks. Every card the model proposes is validated against the local
// card database before it reaches the reader — the same no-hallucination
// discipline the sage keeps, inverted: the model picks FROM candidates the
// server prepared rather than generating names.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/deck"
	"github.com/madeofpendletonwool/grimoire/internal/edhrec"
)

// WithDeckBuilder wires the deck builder: the card database (grounding for
// every suggestion), the deck store, and an optional EDHREC client that
// enriches candidates when enabled. Separate from New so the feature is
// additive; without it the deck endpoints report unavailable.
func (s *Server) WithDeckBuilder(cards *carddb.Store, decks *deck.Store, edhrecClient *edhrec.Client) *Server {
	s.carddb = cards
	s.decks = decks
	s.edhrec = edhrecClient
	return s
}

// deckEnabled reports whether the deck builder is wired, writing the error
// response when it is not.
func (s *Server) deckEnabled(w http.ResponseWriter) bool {
	if s.carddb == nil || s.decks == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("deck builder is not available"))
		return false
	}
	return true
}

// cardStatView is one card suggestion with its ranking reasons.
type cardStatView struct {
	Name       string   `json:"name"`
	ManaCost   string   `json:"mana_cost,omitempty"`
	ManaValue  float64  `json:"mana_value,omitempty"`
	TypeLine   string   `json:"type_line,omitempty"`
	OracleText string   `json:"oracle_text,omitempty"`
	EDHRECRank int      `json:"edhrec_rank,omitempty"`
	Reasons    []string `json:"reasons,omitempty"`
	Synergy    float64  `json:"synergy,omitempty"`
}

// handleDeckPropose suggests commanders for a free-text idea. Candidates come
// from the local card database (commander-legal, EDHREC-ranked, FTS-narrowed
// by the idea's terms); the model writes the one-line "why" for each from
// their real oracle text. No LLM configured → the raw ranked list still
// works, just without blurbs.
func (s *Server) handleDeckPropose(w http.ResponseWriter, r *http.Request) {
	if !s.deckEnabled(w) {
		return
	}
	var req struct {
		Idea   string `json:"idea"`
		Colors string `json:"colors"` // optional constraint, e.g. "Mardu" or "BRW"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	req.Idea = strings.TrimSpace(req.Idea)
	if req.Idea == "" && strings.TrimSpace(req.Colors) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("idea is required"))
		return
	}

	mask := s.colorMask(req.Colors)
	cmdrs, err := s.carddb.Commanders(r.Context(), mask, deck.ThemeTerms(req.Idea), 24)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Model-written blurbs when configured; deterministic rank order either way.
	type proposal struct {
		cardStatView
		Why string `json:"why"`
	}
	proposals := make([]proposal, 0, len(cmdrs))
	for _, c := range cmdrs {
		p := proposal{cardStatView: toStatView(c, nil)}
		if s.llm.Configured() && len(proposals) < 6 {
			p.Why = s.commanderBlurb(r.Context(), c, req.Idea)
		}
		proposals = append(proposals, p)
	}
	if proposals == nil {
		proposals = []proposal{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"commanders": proposals})
}

// commanderBlurb asks the model for one sentence on why this commander fits
// the idea, grounded in the card's oracle text. Best-effort: empty string on
// any failure (the rank order still stands).
func (s *Server) commanderBlurb(ctx context.Context, c *carddb.Card, idea string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	system := "You recommend Commander generals. Given ONLY the commander's real card text and the player's idea, write ONE sentence (max 30 words) on why this commander fits. Use only what the text says. No preamble."
	user := fmt.Sprintf("Commander: %s\nMana cost: %s\nType: %s\nOracle text: %s\n\nPlayer's idea: %s",
		c.Name, c.ManaCost, c.TypeLine, c.OracleText, idea)
	out, err := s.llm.AnswerPrompt(ctx, system, user)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// colorMask turns a colors string into an allowed mask. Accepts combined
// letters ("BRW", "wubrg"), color names ("mardu", "jeskai", "temur",
// "abzan", "sultai", "grixis", "jund", "naya", "esper", "bant", "golgari",
// "izzet", "boros", "orzhov", "simic", "azorius", "dimir", "rakdos",
// "gruul", "selesnya"), and "colorless". Empty = no constraint.
func (s *Server) colorMask(colors string) int {
	c := strings.ToLower(strings.TrimSpace(colors))
	if c == "" {
		return 0
	}
	if shards, ok := shardNames[c]; ok {
		c = shards
	}
	return carddb.MaskForColors(c)
}

var shardNames = map[string]string{
	"mardu": "brw", "jeskai": "urw", "temur": "gur", "abzan": "wbg", "sultai": "ubg",
	"grixis": "ubr", "jund": "brg", "naya": "wrg", "esper": "wub", "bant": "wug",
	"golgari": "bg", "izzet": "ur", "boros": "rw", "orzhov": "wb", "simic": "ug",
	"azorius": "uw", "dimir": "ub", "rakdos": "br", "gruul": "rg", "selesnya": "wg",
	"colorless": "",
}

// handleDeckBuild drafts (or revises) a deck as a stream of SSE events:
//
//	meta  — the commander, the candidate pool summary, EDHREC enrichment state
//	delta — the drafted decklist (one text event; the UI parses it)
//	done  — the final structured deck + analysis
//
// The flow: the server prepares candidates (identity-legal, theme-narrowed,
// EDHREC-boosted when enabled), the model picks from that list and justifies,
// and the server validates the picks back against the card database before
// the done event. Names the model fabricates are dropped and reported, never
// shown.
// deckBuildRequest is the body for /api/deck/build: the player's idea, the
// locked commander, and — when revising — the instruction plus the current
// list the revision applies to.
type deckBuildRequest struct {
	Idea      string       `json:"idea"`
	Commander string       `json:"commander"`
	Colors    string       `json:"colors"`
	Feedback  string       `json:"feedback"` // revision instruction, if any
	Current   []deck.Entry `json:"current"`  // current list when revising
}

func (s *Server) handleDeckBuild(w http.ResponseWriter, r *http.Request) {
	if !s.deckEnabled(w) {
		return
	}
	var req deckBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	req.Idea = strings.TrimSpace(req.Idea)
	req.Feedback = strings.TrimSpace(req.Feedback)
	req.Commander = strings.TrimSpace(req.Commander)
	if req.Commander == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("commander is required (run propose first)"))
		return
	}

	// Verify the commander before the stream commits: an SSE writer has
	// already written its 200, so a bad commander must reject earlier.
	cmdr, err := s.carddb.Get(req.Commander)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown commander %q — could not verify it", req.Commander))
		return
	}

	sse := newSSEWriter(w)
	if !s.llm.Configured() {
		sse.send("error", map[string]any{
			"error": "The deck builder needs the model. Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL / ANTHROPIC_MODEL) to enable it.",
		})
		return
	}
	allowed := carddb.ColorMask(cmdr.ColorIdentity)

	// Candidate pool: theme + staples, minus what's already in the deck.
	var excluded []string
	for _, e := range req.Current {
		excluded = append(excluded, e.Name)
	}
	engine := deck.NewEngine(s.carddb)
	cands := engine.BuildCandidates(r.Context(), allowed, req.Idea+" "+cmdr.Name, excluded, 60)

	// EDHREC enrichment, when enabled: per-commander synergy re-ranks the
	// pool, and known combos accompany the meta event. Best-effort on every
	// path — the local engine is the floor.
	enriched := false
	if s.edhrec.Enabled() {
		if data, err := s.edhrec.CommanderData(r.Context(), cmdr.Name); err == nil {
			stats := map[string]deck.SynergyStat{}
			for tag, list := range data.Lists {
				_ = tag
				for _, cs := range list {
					if st, ok := stats[strings.ToLower(cs.Name)]; !ok || cs.Synergy > st.Synergy {
						stats[strings.ToLower(cs.Name)] = deck.SynergyStat{Synergy: cs.Synergy, NumDecks: cs.NumDecks}
					}
				}
			}
			deck.BoostByStats(cands, stats)
			enriched = true
		} else if !errors.Is(err, edhrec.ErrNotFound) {
			log.Printf("edhrec commander data for %q: %v", cmdr.Name, err)
		}
	}

	sse.send("meta", map[string]any{
		"commander":  toStatView(cmdr, nil),
		"candidates": len(cands),
		"edhrec":     enriched,
	})

	// Compose the prompt: candidates in (name + cost + text + rank), picks out.
	system := deckBuildSystemPrompt
	user := s.deckBuildUserPrompt(cmdr, req, cands)

	ctx, cancel := context.WithTimeout(r.Context(), answerTimeout)
	defer cancel()
	answer, streamErr := s.llm.StreamPrompt(ctx, system, user, func(text string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return sse.send("delta", map[string]any{"text": text})
	})
	if streamErr != nil {
		if strings.TrimSpace(answer) == "" {
			sse.send("error", map[string]any{"error": fmt.Sprintf("the model could not be reached: %v", streamErr)})
			return
		}
		// Partial text is on screen; report the cut.
		sse.send("error", map[string]any{"error": fmt.Sprintf("the draft was cut short: %v", streamErr)})
		return
	}

	// Parse the model's list, validate every name, compute analysis, done.
	entries := deck.ParseDecklist(answer)
	known, fabricated := s.validateEntries(entries, cmdr)
	analysis := deck.Analyze(cmdr.Name, known, s.cardLookup)
	sse.send("done", map[string]any{
		"deck":       known,
		"analysis":   analysis,
		"unverified": fabricated,
		"commentary": answer,
	})
}

const deckBuildSystemPrompt = `You are Grimoire's Commander deck builder. You draft from a candidate list the server verified against real card data.

STRICT RULES:
1. Propose ONLY cards from the candidate list (plus basic lands when asked for). Never invent a card name, never alter one.
2. The decklist section of your reply is machine-parsed. Format it exactly: one card per line as "N Card Name" (N = count), no bullets, no commentary inside it.
3. Aim for exactly 99 cards maindeck (the commander is not one of them) unless the player asked for a partial list.
4. Include a mana base when drafting from scratch: roughly 34-38 lands (basic lands allowed even though they are not in the candidate list) plus mana rocks.
5. Before the decklist, write a short plan (3-5 sentences). After it, add brief notes on the key synergies you built around, naming only cards in the list.

You reason over the candidate's real oracle text and EDHREC popularity to pick the best fits for the player's idea.`

// deckBuildUserPrompt lays out one draft exchange.
func (s *Server) deckBuildUserPrompt(cmdr *carddb.Card, req deckBuildRequest, cands []deck.Candidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Commander: %s\n", cmdr.Name)
	fmt.Fprintf(&b, "Mana cost: %s\nColor identity: %s\n", cmdr.ManaCost, cmdr.ColorIdentity)
	fmt.Fprintf(&b, "Oracle text: %s\n\n", cmdr.OracleText)
	fmt.Fprintf(&b, "Player's idea: %s\n", req.Idea)
	if req.Feedback != "" {
		fmt.Fprintf(&b, "\nRevision instruction: %s\n", req.Feedback)
		fmt.Fprintf(&b, "Current decklist:\n%s\n", deck.FormatDecklist(req.Current))
	}
	fmt.Fprintf(&b, "\nCandidate cards (verified real; EDHREC rank in parens):\n")
	for i, c := range cands {
		if i >= 90 {
			fmt.Fprintf(&b, "…and %d more\n", len(cands)-i)
			break
		}
		line := fmt.Sprintf("- %s | %s | MV %v | %s", c.Name, c.ManaCost, c.ManaValue, c.TypeLine)
		if c.EDHRECRank > 0 {
			line += fmt.Sprintf(" | EDHREC #%d", c.EDHRECRank)
		}
		if len(c.Reasons) > 0 {
			line += " | " + strings.Join(c.Reasons, "; ")
		}
		if c.OracleText != "" {
			line += "\n  " + truncateForPrompt(c.OracleText, 300)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\nDraft the deck now. Remember: decklist block formatted as \"N Card Name\" lines, 99 cards, basic lands allowed.")
	return b.String()
}

func truncateForPrompt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// validateEntries splits parsed entries into known cards (resolved against
// the card database) and fabricated names (not in the database). Basic lands
// are always known — the database carries them, but a miss here would be a
// data gap, not a hallucination.
func (s *Server) validateEntries(entries []deck.Entry, cmdr *carddb.Card) (known []deck.Entry, fabricated []string) {
	for _, e := range entries {
		if strings.EqualFold(e.Name, cmdr.Name) {
			continue // commander handled separately
		}
		if _, err := s.carddb.Get(e.Name); err == nil {
			known = append(known, e)
			continue
		}
		if isBasicLand(e.Name) {
			known = append(known, e)
			continue
		}
		fabricated = append(fabricated, e.Name)
	}
	return known, fabricated
}

var basicLands = map[string]bool{
	"plains": true, "island": true, "swamp": true, "mountain": true, "forest": true,
	"wastes": true, "snow-covered plains": true, "snow-covered island": true,
	"snow-covered swamp": true, "snow-covered mountain": true, "snow-covered forest": true,
}

func isBasicLand(name string) bool { return basicLands[strings.ToLower(strings.TrimSpace(name))] }

// cardLookup adapts the carddb store to the analyzer's lookup signature.
func (s *Server) cardLookup(name string) (*carddb.Card, bool) {
	c, err := s.carddb.Get(name)
	if err != nil {
		return nil, false
	}
	return c, true
}

// toStatView projects a card (plus optional reasons) into the JSON view.
func toStatView(c *carddb.Card, reasons []string) cardStatView {
	return cardStatView{
		Name: c.Name, ManaCost: c.ManaCost, ManaValue: c.ManaValue, TypeLine: c.TypeLine,
		OracleText: c.OracleText, EDHRECRank: c.EDHRECRank, Reasons: reasons,
	}
}

// handleDeckAnalyze parses a pasted decklist, resolves every name against the
// card database (fuzzy via case-insensitive exact first, then FTS), and
// returns the deterministic analysis plus a model-written critique when the
// LLM is configured. Never mutates anything.
func (s *Server) handleDeckAnalyze(w http.ResponseWriter, r *http.Request) {
	if !s.deckEnabled(w) {
		return
	}
	var req struct {
		Decklist  string `json:"decklist"`
		Commander string `json:"commander"` // optional override; parsed from list otherwise
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if strings.TrimSpace(req.Decklist) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decklist is required"))
		return
	}

	entries := deck.ParseDecklist(req.Decklist)
	commander := strings.TrimSpace(req.Commander)
	for _, e := range entries {
		if e.Board == "commander" {
			commander = e.Name
			break
		}
	}

	resolved, unresolved := s.resolveEntries(r.Context(), entries)
	analysis := deck.Analyze(commander, resolved, s.cardLookup)

	// Model critique, best-effort and grounded in the analysis + card texts.
	critique := ""
	if s.llm.Configured() && len(resolved) > 0 {
		critique = s.deckCritique(r.Context(), commander, resolved, analysis)
	}

	resp := map[string]any{
		"deck":       resolved,
		"analysis":   analysis,
		"unresolved": unresolved,
	}
	if commander != "" {
		if c, err := s.carddb.Get(commander); err == nil {
			resp["commander"] = toStatView(c, nil)
		}
	}
	if critique != "" {
		resp["critique"] = critique
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveEntries resolves parsed names against the database: exact
// (case-insensitive), then an FTS match for typos. Unresolved names are
// reported, never dropped silently.
func (s *Server) resolveEntries(ctx context.Context, entries []deck.Entry) (resolved, unresolved []deck.Entry) {
	for _, e := range entries {
		name := e.Name
		if _, err := s.carddb.Get(name); err == nil {
			resolved = append(resolved, e)
			continue
		}
		// Fuzzy: one FTS search, take the top hit above a sanity bar.
		if hits, err := s.carddb.SearchNames(ctx, name, 1); err == nil && len(hits) > 0 && similarName(name, hits[0].Name) {
			e.Name = hits[0].Name
			e.Note = "matched " + name
			resolved = append(resolved, e)
			continue
		}
		unresolved = append(unresolved, e)
	}
	return resolved, unresolved
}

// similarName guards the fuzzy path: the matched card's name must share the
// first letter or half the words, else "Sol Ring" would "fix" into "Solfatara".
func similarName(want, got string) bool {
	w, g := strings.ToLower(want), strings.ToLower(got)
	if w == g {
		return true
	}
	ww := strings.Fields(w)
	gw := strings.Fields(g)
	shared := 0
	for _, a := range ww {
		for _, b := range gw {
			if a == b {
				shared++
			}
		}
	}
	return shared*2 >= len(ww) || shared > 0 && w[0] == g[0] && shared*3 >= len(ww)
}

// deckCritique asks the model for a grounded read of the analyzed deck.
func (s *Server) deckCritique(ctx context.Context, commander string, entries []deck.Entry, a deck.Analysis) string {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var b strings.Builder
	fmt.Fprintf(&b, "Commander: %s\n\nDecklist:\n%s\n\nAnalysis (authoritative):\n", commander, deck.FormatDecklist(entries))
	fmt.Fprintf(&b, "- %d maindeck cards, %d lands, average MV %.1f\n", a.TotalMain, a.Lands, a.AvgMV)
	fmt.Fprintf(&b, "- ramp %d, draw %d, interaction %d\n", a.Ratios.Ramp, a.Ratios.Draw, a.Ratios.Interaction)
	if len(a.Warnings) > 0 {
		fmt.Fprintf(&b, "- warnings: %s\n", strings.Join(a.Warnings, "; "))
	}

	system := "You are Grimoire's deck analyst. You are given a real decklist and a deterministic analysis. Write a concise critique (max 250 words): what the deck does well, its weak points, and 3-5 specific upgrade or cut suggestions. Suggest only cards that appear in the provided decklist context or are format staples; if unsure of a card's text, say so instead of guessing."
	user := b.String()
	out, err := s.llm.AnswerPrompt(ctx, system, user)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ---- Saved deck CRUD ----

type deckView struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Commander string       `json:"commander"`
	Cards     []deck.Entry `json:"cards"`
	Notes     string       `json:"notes"`
	CreatedAt string       `json:"created_at"`
	UpdatedAt string       `json:"updated_at"`
}

func toDeckView(d deck.Deck) deckView {
	if d.Cards == nil {
		d.Cards = []deck.Entry{}
	}
	return deckView{
		ID: d.ID, Name: d.Name, Commander: d.Commander, Cards: d.Cards, Notes: d.Notes,
		CreatedAt: d.CreatedAt.Format(http.TimeFormat), UpdatedAt: d.UpdatedAt.Format(http.TimeFormat),
	}
}

func (s *Server) handleListDecks(w http.ResponseWriter, r *http.Request) {
	if !s.deckEnabled(w) {
		return
	}
	list, err := s.decks.List(r.Context(), userID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]deckView, 0, len(list))
	for _, d := range list {
		views = append(views, toDeckView(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"decks": views})
}

func (s *Server) handleCreateDeck(w http.ResponseWriter, r *http.Request) {
	if !s.deckEnabled(w) {
		return
	}
	var req struct {
		Name      *string      `json:"name"`
		Commander *string      `json:"commander"`
		Cards     []deck.Entry `json:"cards"`
		Notes     *string      `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name is required"))
		return
	}
	commander := ""
	if req.Commander != nil {
		commander = *req.Commander
	}
	notes := ""
	if req.Notes != nil {
		notes = *req.Notes
	}
	// Validate every card name server-side: a saved deck contains only real
	// cards (plus basics) or is rejected with the offending names.
	known, fabricated := s.validateEntries(req.Cards, &carddb.Card{Name: commander})
	if len(fabricated) > 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("could not verify: %s", strings.Join(fabricated, ", ")))
		return
	}
	d, err := s.decks.Create(r.Context(), userID(r), *req.Name, commander, known, notes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"deck": toDeckView(d)})
}

func (s *Server) handleGetDeck(w http.ResponseWriter, r *http.Request) {
	if !s.deckEnabled(w) {
		return
	}
	d, err := s.decks.Get(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, deck.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deck": toDeckView(d)})
}

func (s *Server) handleUpdateDeck(w http.ResponseWriter, r *http.Request) {
	if !s.deckEnabled(w) {
		return
	}
	var req struct {
		Name      *string      `json:"name"`
		Commander *string      `json:"commander"`
		Cards     []deck.Entry `json:"cards"`
		Notes     *string      `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if req.Cards != nil {
		commander := ""
		if req.Commander != nil {
			commander = *req.Commander
		}
		known, fabricated := s.validateEntries(req.Cards, &carddb.Card{Name: commander})
		if len(fabricated) > 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("could not verify: %s", strings.Join(fabricated, ", ")))
			return
		}
		req.Cards = known
	}
	d, err := s.decks.Update(r.Context(), userID(r), r.PathValue("id"), req.Name, req.Commander, req.Notes, req.Cards, req.Cards != nil)
	if err != nil {
		if errors.Is(err, deck.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deck": toDeckView(d)})
}

func (s *Server) handleDeleteDeck(w http.ResponseWriter, r *http.Request) {
	if !s.deckEnabled(w) {
		return
	}
	if err := s.decks.Delete(r.Context(), userID(r), r.PathValue("id")); err != nil {
		if errors.Is(err, deck.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeckCombos reports known Commander Spellbook combos for the deck's
// commander (and optionally a card), via the EDHREC client. Unavailable →
// empty list with a note, never an error page.
func (s *Server) handleDeckCombos(w http.ResponseWriter, r *http.Request) {
	if !s.deckEnabled(w) {
		return
	}
	card := strings.TrimSpace(r.URL.Query().Get("card"))
	if card == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("card is required"))
		return
	}
	if s.edhrec == nil || !s.edhrec.Enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"combos": []edhrec.Combo{}, "note": "combo data needs EDHREC enabled (GRIMOIRE_EDHREC=1)"})
		return
	}
	combos, err := s.edhrec.Combos(r.Context(), card)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"combos": []edhrec.Combo{}, "note": "combo data unavailable right now"})
		return
	}
	if combos == nil {
		combos = []edhrec.Combo{}
	}
	sort.SliceStable(combos, func(i, j int) bool { return combos[i].Title < combos[j].Title })
	writeJSON(w, http.StatusOK, map[string]any{"combos": combos})
}
