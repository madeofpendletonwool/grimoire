package server

// The campaign chat (MAD-311): the DM Grimoire and the Player Grimoire — the
// same campaign, the same model, a different epistemic reality depending on
// who is asking. A player asking "where is the headquarters?" gets "you don't
// know that"; the DM gets the monastery beneath Greyfall.
//
// The one rule this file exists to enforce, inherited from the knowledge
// layer (ADR 2): perspective is authorization, not instruction. Every
// campaign row that reaches a prompt arrives through a scope-filtered SQL
// query — the DM path through the wide store at the DM scope, every non-DM
// caller through knowledge.PlayerView, which cannot return a secret or
// proposed row at all. The prompts below never say "don't tell the players";
// they don't need to, because the player's context provably never contained
// the secret. What the prompts DO carry is behaviour guidance: answer from
// the record, cite facts, and say "you don't know that" when the record at
// this perspective is empty.
//
// Conversations reuse internal/chat, pinned to a campaign and a scope at
// creation (chat.CreateInCampaign). The pinned scope is re-checked on every
// later turn, so history recorded at one epistemic scope — the party's, one
// character's — can never be replayed or extended at a wider one: promote a
// player to DM, bind a new character, and their old threads refuse to
// continue rather than answer above their station.
//
// Retrieval is dynamic, not a context dump: scoped FTS over campaign prose
// (strict AND, then a ranked OR fallback) picks the handful of entities,
// facts and events the question actually touches, and the rules corpora ride
// in unscoped beside them — D&D rules are not a secret. A 300-session
// campaign needs no 300 sessions in the prompt; that is the whole reason the
// graph exists.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/entities"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

const (
	// campaignProseHits bounds the scoped campaign retrieval feeding one
	// answer. Small on purpose: the graph is consulted, not poured.
	campaignProseHits = 8
	// campaignHistoryTurns caps replayed prior turns, mirroring the rules
	// chat's window.
	campaignHistoryTurns = 12
)

/* ---------- citations ---------- */

// campaignFactCitation is one campaign fact an answer rests on. Provenance is
// attached at the DM scope only — the "why does Grimoire think this?" trail
// is a DM surface.
type campaignFactCitation struct {
	ID         string           `json:"id"`
	Statement  string           `json:"statement"`
	Visibility string           `json:"visibility,omitempty"`
	Confidence string           `json:"confidence,omitempty"`
	Provenance []provenanceView `json:"provenance,omitempty"`
}

type campaignEntityCitation struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
}

type campaignEventCitation struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

// campaignCitations is the citation payload stored on an assistant message
// and replayed in the meta SSE frame.
type campaignCitations struct {
	Facts    []campaignFactCitation   `json:"facts,omitempty"`
	Entities []campaignEntityCitation `json:"entities,omitempty"`
	Events   []campaignEventCitation  `json:"events,omitempty"`
}

/* ---------- grounding ---------- */

// campaignGrounding is one question's retrieval at one perspective: the
// scoped campaign rows the question touches, plus the unscoped rules
// grounding the sage already uses. It is the whole campaign context — nothing
// else about the campaign is read for the answer.
type campaignGrounding struct {
	facts      []campaign.Fact
	entities   []campaign.Entity
	events     []knowledge.ProseHit
	docs       []llm.ContextDoc // rules corpora, unscoped
	entityDocs []llm.EntityDoc  // SRD reference entries, unscoped
	unresolved []string
	// empty reports that no campaign row surfaced at this scope.
	empty bool
}

// campaignCorpus maps a campaign's system onto the rules corpus that grounds
// its answers. Only 5e is wired (the campaign core is deliberately
// system-agnostic but not speculative); anything else falls back to the
// default corpus.
func campaignCorpus(system string) data.Corpus {
	s := strings.ToLower(system)
	if strings.Contains(s, "dnd") || strings.Contains(s, "d&d") || strings.Contains(s, "5e") {
		if d, ok := data.Lookup(data.CorpusDND); ok {
			return d.Corpus
		}
	}
	return data.Default().Corpus
}

// chatScope is the perspective this caller chats at: the DM scope for the dm
// role and the keeper, the resolved player scope (party or character) for
// everyone else — the same scope their campaign reads run at.
func (a *campAccess) chatScope() campaign.Scope {
	if a.isDM() {
		return campaign.ScopeDM
	}
	return a.playerScope
}

// perspectiveLabel names the perspective for prompts and the UI badge.
func (a *campAccess) perspectiveLabel(ctx context.Context) string {
	if a.isDM() {
		return "dm"
	}
	switch a.playerScope.Kind() {
	case campaign.ScopeKindCharacter:
		if e, err := a.view.Entity(ctx, a.campaign.ID, a.playerScope.EntityID()); err == nil {
			return e.Name
		}
		return "character"
	default:
		return "party"
	}
}

// groundCampaign runs one question's retrieval at the caller's perspective.
// The campaign side is scoped in SQL; the rules side is the same unscoped
// grounding the rules sage uses.
func (s *Server) groundCampaign(ctx context.Context, a *campAccess, question string) (*campaignGrounding, error) {
	g := &campaignGrounding{}
	cid := a.campaign.ID

	// 1. Scoped campaign prose search: strict AND first, ranked OR fallback.
	hits, err := s.campaignProseSearch(ctx, a, question, campaignProseHits)
	if err != nil {
		return nil, err
	}

	// 2. Grow each hit into a full citation through the same scoped read
	// path — a hit the scope cannot refetch is dropped, defense in depth.
	seen := map[string]bool{}
	for _, h := range hits {
		key := h.Kind + ":" + h.RefID
		if seen[key] {
			continue
		}
		seen[key] = true
		switch h.Kind {
		case "fact":
			f, err := s.campaignFact(ctx, a, cid, h.RefID)
			if err != nil {
				continue
			}
			g.facts = append(g.facts, *f)
		case "entity":
			e, err := s.campaignEntity(ctx, a, cid, h.RefID)
			if err != nil {
				continue
			}
			g.entities = append(g.entities, *e)
		case "event":
			g.events = append(g.events, h)
		}
	}
	g.empty = len(g.facts) == 0 && len(g.entities) == 0 && len(g.events) == 0

	// 3. Rules corpora and reference entries, unscoped — D&D rules are not a
	// secret. Mirrors ground() in the rules chat, minus the MTG card path.
	corpus := campaignCorpus(a.campaign.System)
	results, err := s.store.Retrieve(ctx, corpus, question, retrieveSeeds)
	if err != nil {
		return nil, err
	}
	expanded, err := s.store.Expand(ctx, corpus, results)
	if err != nil {
		return nil, err
	}
	g.docs = make([]llm.ContextDoc, 0, len(expanded))
	for _, res := range expanded {
		g.docs = append(g.docs, llm.ContextDoc{Number: res.Number, Title: res.Title, Body: res.Body, Source: res.Source})
	}
	if def, ok := data.Lookup(corpus); ok && def.EntityResolver != nil {
		if _, isCards := def.EntityResolver.(entities.CardProjector); !isCards {
			g.entityDocs, _, g.unresolved = s.lookupQuestionEntities(ctx, def.EntityResolver, question)
		}
	}
	return g, nil
}

// campaignProseSearch runs the scoped prose search at the caller's
// perspective: the strict AND query, then the ranked OR fallback when it
// finds nothing.
func (s *Server) campaignProseSearch(ctx context.Context, a *campAccess, question string, limit int) ([]knowledge.ProseHit, error) {
	search := func(relaxed bool) ([]knowledge.ProseHit, error) {
		if a.isDM() {
			if relaxed {
				return s.knowledge.SearchProseRelaxed(ctx, campaign.ScopeDM, a.campaign.ID, question, limit)
			}
			return s.knowledge.SearchProse(ctx, campaign.ScopeDM, a.campaign.ID, question, limit)
		}
		if relaxed {
			return a.view.SearchProseRelaxed(ctx, a.campaign.ID, question, limit)
		}
		return a.view.SearchProse(ctx, a.campaign.ID, question, limit)
	}
	hits, err := search(false)
	if err != nil && !errors.Is(err, knowledge.ErrEmptyQuery) {
		return nil, err
	}
	if len(hits) > 0 {
		return hits, nil
	}
	hits, err = search(true)
	if err != nil && !errors.Is(err, knowledge.ErrEmptyQuery) {
		return nil, err
	}
	return hits, nil
}

func (s *Server) campaignFact(ctx context.Context, a *campAccess, campaignID, factID string) (*campaign.Fact, error) {
	if a.isDM() {
		return s.knowledge.Fact(ctx, campaign.ScopeDM, campaignID, factID)
	}
	return a.view.Fact(ctx, campaignID, factID)
}

func (s *Server) campaignEntity(ctx context.Context, a *campAccess, campaignID, entityID string) (*campaign.Entity, error) {
	if a.isDM() {
		return s.knowledge.Entity(ctx, campaign.ScopeDM, campaignID, entityID)
	}
	return a.view.Entity(ctx, campaignID, entityID)
}

// citations builds the reader-facing citation payload from the grounding,
// provenance attached at the DM scope only.
func (s *Server) citations(ctx context.Context, a *campAccess, g *campaignGrounding) campaignCitations {
	out := campaignCitations{}
	for i := range g.facts {
		f := g.facts[i]
		c := campaignFactCitation{ID: f.ID, Statement: f.Statement, Visibility: f.Visibility, Confidence: f.Confidence}
		if a.isDM() {
			if prov, err := s.campaigns.FactProvenance(ctx, campaign.ScopeDM, a.campaign.ID, f.ID); err == nil {
				for i := range prov {
					c.Provenance = append(c.Provenance, toProvenanceView(&prov[i]))
				}
			}
		}
		out.Facts = append(out.Facts, c)
	}
	for i := range g.entities {
		e := g.entities[i]
		out.Entities = append(out.Entities, campaignEntityCitation{ID: e.ID, Kind: e.Kind, Name: e.Name, Summary: e.Summary})
	}
	for _, ev := range g.events {
		out.Events = append(out.Events, campaignEventCitation{ID: ev.RefID, Summary: ev.Snippet})
	}
	return out
}

/* ---------- the prompt ---------- */

// campaignSystemPrompt is the standing instruction for one perspective. It
// governs behaviour — answer from the record, cite it, admit ignorance —
// never secrecy: secrecy is already enforced in the SQL that assembled the
// record, so this prompt has no secret to keep and nothing to leak if the
// model ignores it.
func campaignSystemPrompt(perspective string, corpusName string) string {
	var b strings.Builder
	if perspective == "dm" {
		fmt.Fprintf(&b, `You are the campaign Grimoire for this campaign, answering the Dungeon Master — the one soul who sees the whole record, secrets included. Secrets are marked (secret); use them freely, that is what they are there for.`)
	} else if perspective == "party" {
		fmt.Fprintf(&b, `You are the campaign Grimoire for this campaign, answering the adventuring party. The campaign record below is the whole of what the party has discovered — nothing outside it about this campaign's people, places, plots, items or secrets may be asserted from imagination.`)
	} else {
		fmt.Fprintf(&b, `You are the campaign Grimoire for this campaign, answering %s, a player character. The campaign record below is the whole of what %s has learned — nothing outside it about this campaign's people, places, plots, items or secrets may be asserted from imagination.`, perspective, perspective)
	}

	b.WriteString("\n\nGROUNDING RULES — follow these strictly:")
	b.WriteString(`
1. Answer campaign questions ONLY from the campaign record provided. Never invent people, places, events, factions, or secrets it does not contain.
2. Cite campaign facts by their marker (e.g. [F2]), rules by rule number or section title, and reference entries by name.`)
	if perspective != "dm" {
		b.WriteString(`
3. If the question is about the campaign's world and the record does not answer it, reply that they do not know that yet — plainly, without guessing, hinting, or reasoning from genre conventions. Do not soften it into speculation.
4. If the question is about the game's rules rather than the campaign's world, answer it from the rules excerpts normally.`)
	} else {
		b.WriteString(`
3. The record is retrieval, not the whole campaign: when it does not cover the question, say what the record holds and what it does not rather than filling the gap.
4. Answer rules questions from the rules excerpts and name the book behind a ruling.`)
	}
	b.WriteString(`
5. If a rules excerpt or reference entry decides a question, cite it the way the rules sage does.
6. Keep answers concise and practical for a table mid-session.`)
	_ = corpusName
	return b.String()
}

// buildCampaignUserMessage assembles the final user turn: the scoped
// campaign record, the unscoped rules grounding, and the question. This is
// the exact text the model receives — the function the leak assertions in
// the tests read, so it stays pure and testable.
func buildCampaignUserMessage(g *campaignGrounding, corpusName string, question string) string {
	var b strings.Builder
	b.WriteString("Campaign record — everything this perspective may see of the campaign that bears on the question:\n\n")
	if g.empty {
		b.WriteString("(nothing — no campaign entity, fact or event known at this perspective matches this question)\n\n")
	}
	for i := range g.entities {
		e := g.entities[i]
		header := fmt.Sprintf("[E%d] %s (%s)", i+1, e.Name, e.Kind)
		if e.Summary != "" {
			header += ": " + e.Summary
		}
		fmt.Fprintf(&b, "%s\n", header)
	}
	if len(g.entities) > 0 {
		b.WriteString("\n")
	}
	for i := range g.facts {
		f := g.facts[i]
		mark := fmt.Sprintf("(secret) [F%d] %s", i+1, f.Statement)
		if f.Visibility != campaign.VisibilitySecret {
			mark = fmt.Sprintf("[F%d] %s", i+1, f.Statement)
		}
		fmt.Fprintf(&b, "%s\n", mark)
	}
	if len(g.facts) > 0 {
		b.WriteString("\n")
	}
	for i, ev := range g.events {
		fmt.Fprintf(&b, "[V%d] %s\n", i+1, ev.Snippet)
	}
	if len(g.events) > 0 {
		b.WriteString("\n")
	}

	b.WriteString("Relevant " + corpusName + " rules:\n\n")
	if len(g.docs) == 0 {
		b.WriteString("(no directly matching rules found)\n\n")
	}
	for _, d := range g.docs {
		header := d.Title
		if displayRuleNumRe.MatchString(d.Number) {
			if header != "" {
				header = d.Number + " — " + header
			} else {
				header = d.Number
			}
		}
		if d.Source != "" {
			if header != "" {
				header += " [" + d.Source + "]"
			} else {
				header = "[" + d.Source + "]"
			}
		}
		fmt.Fprintf(&b, "### %s\n%s\n\n", header, truncateRunes(d.Body, 1500))
	}

	if len(g.entityDocs) > 0 {
		b.WriteString("Reference entries (authoritative):\n\n")
		for _, e := range g.entityDocs {
			header := e.Name
			if e.Kind != "" {
				header = e.Name + " (" + e.Kind + ")"
			}
			fmt.Fprintf(&b, "### %s\n%s\n\n", header, truncateRunes(e.Body, 1500))
		}
	}
	if len(g.unresolved) > 0 {
		fmt.Fprintf(&b, "Names in the question that could not be looked up: %s\n"+
			"Do not describe these from memory — say the lookup failed.\n\n",
			strings.Join(g.unresolved, ", "))
	}

	b.WriteString("Question: " + question)
	return b.String()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

/* ---------- handlers ---------- */

// handleListCampaignChats lists the caller's conversations in one campaign.
func (s *Server) handleListCampaignChats(w http.ResponseWriter, r *http.Request) {
	if !s.chatsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	convs, err := s.chats.ListInCampaign(r.Context(), userID(r), a.campaign.ID, 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]chatView, 0, len(convs))
	for i := range convs {
		views = append(views, toChatView(&convs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": views, "perspective": a.perspectiveLabel(r.Context())})
}

// handleCreateCampaignChat opens a conversation pinned to the campaign and to
// the caller's resolved scope. The scope is derived from the membership row —
// a client cannot ask for a wider one.
func (s *Server) handleCreateCampaignChat(w http.ResponseWriter, r *http.Request) {
	if !s.chatsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	c, err := s.chats.CreateInCampaign(r.Context(), userID(r),
		string(campaignCorpus(a.campaign.System)), a.campaign.ID, a.chatScope().String(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"chat": toChatView(c), "perspective": a.perspectiveLabel(r.Context())})
}

// loadCampaignChat fetches a conversation and enforces the two invariants:
// the thread belongs to this campaign, and the caller's current scope is the
// scope the thread was opened at. It writes the error itself and returns nil
// when the caller should not proceed.
func (s *Server) loadCampaignChat(w http.ResponseWriter, r *http.Request, a *campAccess) *chat.Conversation {
	conv, err := s.chats.Get(r.Context(), userID(r), r.PathValue("chatID"))
	if err != nil {
		writeChatError(w, err)
		return nil
	}
	// A wrong-campaign thread is indistinguishable from a missing one.
	if conv.CampaignID != a.campaign.ID {
		writeError(w, http.StatusNotFound, fmt.Errorf("conversation not found"))
		return nil
	}
	// The pinned scope. A caller whose perspective changed — promoted to DM,
	// rebound to another character — cannot grow an old thread at their new,
	// wider view of the world.
	if conv.Scope != a.chatScope().String() {
		writeError(w, http.StatusForbidden, fmt.Errorf("this thread was opened at a different perspective (%s) than yours (%s) — start a new thread", conv.Scope, a.chatScope()))
		return nil
	}
	return conv
}

func (s *Server) handleGetCampaignChat(w http.ResponseWriter, r *http.Request) {
	if !s.chatsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	conv := s.loadCampaignChat(w, r, a)
	if conv == nil {
		return
	}
	msgs, err := s.chats.Messages(r.Context(), conv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": toChatView(conv), "messages": msgs, "perspective": a.perspectiveLabel(r.Context())})
}

func (s *Server) handleDeleteCampaignChat(w http.ResponseWriter, r *http.Request) {
	if !s.chatsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	conv := s.loadCampaignChat(w, r, a)
	if conv == nil {
		return
	}
	if err := s.chats.Delete(r.Context(), userID(r), conv.ID); err != nil {
		writeChatError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCampaignChatMessage appends a question to a campaign conversation and
// streams the answer as SSE: meta (citations, perspective, title), delta,
// done, error — the same frame contract as the rules chat, with campaign
// citations riding in the meta payload and persisted on the message.
//
// There is deliberately no answer cache on this path: the rules cache keys on
// corpus + question + sources, which carries no perspective, and a cache hit
// is exactly the place a DM-shaped answer could slide into a player's hands.
// Campaign answers are per-perspective by construction; they are regenerated.
func (s *Server) handleCampaignChatMessage(w http.ResponseWriter, r *http.Request) {
	if !s.chatsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	var req struct {
		Question string `json:"question"`
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
	conv := s.loadCampaignChat(w, r, a)
	if conv == nil {
		return
	}

	// Persistence must outlive the request context, exactly like the rules
	// chat: navigating away mid-answer still saves the turn.
	saveCtx := context.WithoutCancel(r.Context())

	// MAD-363: the DM Grimoire routes recognized commands to the command
	// engine instead of answering them in prose. A slash prefix is the
	// deterministic signal — every DM message is otherwise a question, and
	// guessing which questions are commands is the failure this surface
	// exists to avoid. Players' slashes stay questions: they cannot
	// command the database.
	if isChatCommand(a, req.Question) {
		s.handleCampaignChatCommand(w, r, a, conv, saveCtx, req.Question)
		return
	}

	prior, err := s.chats.History(r.Context(), conv.ID, campaignHistoryTurns)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	turns := make([]llm.Turn, 0, len(prior)+1)
	for _, m := range prior {
		turns = append(turns, llm.Turn{Role: m.Role, Content: m.Content})
	}

	if _, err := s.chats.AddMessage(saveCtx, conv.ID, chat.RoleUser, req.Question, nil, nil, nil, nil, nil); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	title := conv.Title
	if strings.TrimSpace(title) == "" {
		title = chat.DeriveTitle(req.Question)
		if err := s.chats.Rename(saveCtx, userID(r), conv.ID, title); err != nil {
			log.Printf("campaign chat: title %s: %v", conv.ID, err)
		}
	}

	sse := newSSEWriter(w)
	if !s.llm.Configured() {
		sse.send("error", map[string]any{
			"error": "The campaign chat is not configured. Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL / ANTHROPIC_MODEL) to enable it.",
			"title": title,
		})
		return
	}

	g, err := s.groundCampaign(r.Context(), a, req.Question)
	if err != nil {
		sse.send("error", map[string]any{"error": fmt.Sprintf("retrieval failed: %v", err), "title": title})
		return
	}
	cites := s.citations(r.Context(), a, g)
	perspective := a.perspectiveLabel(r.Context())
	corpus := campaignCorpus(a.campaign.System)

	// The assembled prompt: system at this perspective, prior turns verbatim,
	// and the final user turn carrying the scoped record + unscoped rules.
	// This is the object the leak tests assert on.
	system := campaignSystemPrompt(perspective, corpusDisplayName(corpus))
	turns = append(turns, llm.Turn{Role: "user", Content: buildCampaignUserMessage(g, corpusDisplayName(corpus), req.Question)})

	sse.send("meta", map[string]any{
		"title":       title,
		"perspective": perspective,
		"campaign":    cites,
		"sources":     rulesResults(g.docs),
		"entities":    entityDocViews(g.entityDocs),
		"unresolved":  g.unresolved,
	})

	ctx, cancel := context.WithTimeout(r.Context(), answerTimeout)
	defer cancel()
	answer, streamErr := s.llm.StreamChat(ctx, system, turns, func(text string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return sse.send("delta", map[string]any{"text": text})
	})
	answer = strings.TrimSpace(answer)
	if answer == "" && streamErr != nil {
		sse.send("error", map[string]any{"error": fmt.Sprintf("the Grimoire could not be reached: %v", streamErr)})
		return
	}

	var sourcesJSON, entitiesJSON json.RawMessage
	if len(g.docs) > 0 {
		srcs := rulesResults(g.docs)
		if b, err := json.Marshal(srcs); err == nil {
			sourcesJSON = b
		}
	}
	if views := entityDocViews(g.entityDocs); len(views) > 0 {
		if b, err := json.Marshal(views); err == nil {
			entitiesJSON = b
		}
	}
	var campaignJSON json.RawMessage
	if b, err := json.Marshal(cites); err == nil {
		campaignJSON = b
	}
	saved, saveErr := s.chats.AddMessage(saveCtx, conv.ID, chat.RoleAssistant, answer, sourcesJSON, nil, entitiesJSON, nil, campaignJSON)
	if saveErr != nil {
		log.Printf("campaign chat: save answer %s: %v", conv.ID, saveErr)
	}
	if streamErr != nil {
		sse.send("error", map[string]any{"error": fmt.Sprintf("the answer was cut short: %v", streamErr)})
		return
	}
	var id int64
	if saved != nil {
		id = saved.ID
	}
	sse.send("done", map[string]any{"message_id": id, "title": title, "perspective": perspective})
}

// handleCampaignChatCommand runs one slash-prefixed DM message through the
// command engine (MAD-363) and renders the result as the same SSE frame
// contract a streamed answer uses: meta (title, perspective, and the
// command payload), one delta carrying the whole message, done. The
// exchange persists like any other turn — the user message with its slash,
// the assistant message carrying the command result — so a reloaded
// thread replays the command history verbatim. There is no model answer
// to stream: the engine's result IS the turn.
func (s *Server) handleCampaignChatCommand(w http.ResponseWriter, r *http.Request, a *campAccess, conv *chat.Conversation, saveCtx context.Context, question string) {
	if _, err := s.chats.AddMessage(saveCtx, conv.ID, chat.RoleUser, question, nil, nil, nil, nil, nil); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	title := conv.Title
	if strings.TrimSpace(title) == "" {
		title = chat.DeriveTitle(question)
		if err := s.chats.Rename(saveCtx, userID(r), conv.ID, title); err != nil {
			log.Printf("campaign chat: title %s: %v", conv.ID, err)
		}
	}
	res := s.runCampaignCommand(r, a, strings.TrimPrefix(strings.TrimSpace(question), "/"))

	var commandJSON json.RawMessage
	if b, err := json.Marshal(res); err == nil {
		commandJSON = b
	}
	perspective := a.perspectiveLabel(r.Context())
	saved, saveErr := s.chats.AddMessage(saveCtx, conv.ID, chat.RoleAssistant, res.Message, nil, nil, nil, nil, commandJSON)
	if saveErr != nil {
		log.Printf("campaign chat: save command %s: %v", conv.ID, saveErr)
	}

	sse := newSSEWriter(w)
	sse.send("meta", map[string]any{
		"title":       title,
		"perspective": perspective,
		"command":     res,
	})
	sse.send("delta", map[string]any{"text": res.Message})
	var id int64
	if saved != nil {
		id = saved.ID
	}
	sse.send("done", map[string]any{"message_id": id, "title": title, "perspective": perspective})
}

// rulesResults re-wraps rules docs into the index.Result-free shape
// toSources expects: the campaign path never ran the seed query the rules
// chat bases its citation list on, so the expanded docs stand in directly.
func rulesResults(docs []llm.ContextDoc) []searchHit {
	out := make([]searchHit, 0, len(docs))
	for _, d := range docs {
		out = append(out, searchHit{Number: displayNumber(d.Number), Title: d.Title, Body: d.Body, Source: d.Source})
	}
	return out
}

// entityDocViews projects SRD reference docs into the UI citation shape the
// rules chat already renders.
func entityDocViews(docs []llm.EntityDoc) []entityView {
	if len(docs) == 0 {
		return nil
	}
	views := make([]entityView, 0, len(docs))
	for _, e := range docs {
		views = append(views, entityView{Name: e.Name, Kind: e.Kind, Body: e.Body})
	}
	return views
}
