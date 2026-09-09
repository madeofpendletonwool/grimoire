package server

// The player journal surface (MAD-489, stage 3 of MAD-319): the belief loop's
// front door. A seated player authors journal entries against a session —
// each entry a player_journal source whose author is bound server-side to
// the member's character, never client-claimed — reads their own journal,
// and may ask for an AI-assisted draft grounded strictly in what their
// character has learned. The DM reads the campaign's journals, marked by
// author, beside the transcripts and notes the sessions tool already shows.
//
//	POST /api/campaigns/{cid}/sessions/{sid}/journal          write an entry (bound player)
//	GET  /api/campaigns/{cid}/sessions/{sid}/journal          one session's journals (own, or all for the DM)
//	GET  /api/campaigns/{cid}/journal                         the journal across sessions (own, or all for the DM)
//	POST /api/campaigns/{cid}/sessions/{sid}/journal/draft    "help me write today's entry" (bound player)
//
// The write follows the sources rule the whole session layer is built on:
// entries are verbatim and immutable (append + checksum, never mutate), so
// the spans a belief's provenance points at stay valid forever. The belief
// loop itself is the canon engine's — the DM's post-session run extracts
// beliefs out of these entries exactly as it extracts facts out of a
// transcript (internal/canon, MAD-489).
//
// The drafting assist is the campaign chat's discipline applied to a
// different question (MAD-311's pattern, verbatim): perspective is
// authorization, not instruction. The context is assembled only from
// PlayerView reads — the character's discoveries, the quest journal, the
// witnessed timeline — which cannot return a secret or proposed row at all,
// so the assembled prompt provably never contains material outside the
// character's scope. The prompts never say "don't reveal secrets"; they
// don't need to, because there is no secret in them to keep.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

// maxJournalBytes caps one journal entry. A journal entry is an evening's
// recollection, not an archive: a megabyte is a hundred long sessions' worth
// of prose and generous headroom.
const maxJournalBytes = 1 << 20

// journalDraftTimeout bounds one drafting-assist call, mirroring the chat's
// answer budget.
const journalDraftTimeout = 90 * time.Second

/* ---------- views ---------- */

// journalView is one journal entry as the journal surfaces render it: the
// source's metadata, the author marked by name, and the session it was
// written against. Content rides only on the single-entry read.
type journalView struct {
	ID             string `json:"id"`
	SessionID      string `json:"session_id"`
	SessionOrdinal int64  `json:"session_ordinal,omitempty"`
	SessionName    string `json:"session_name,omitempty"`
	Kind           string `json:"kind"`
	Author         string `json:"author"`
	AuthorName     string `json:"author_name,omitempty"`
	Title          string `json:"title"`
	ByteSize       int64  `json:"byte_size"`
	CreatedAt      string `json:"created_at"`
	Content        string `json:"content,omitempty"`
}

func (s *Server) toJournalView(ctx context.Context, campaignID string, e gamesession.JournalEntry, withContent bool) journalView {
	v := journalView{
		ID: e.ID, SessionID: e.SessionID, SessionOrdinal: e.SessionOrdinal, SessionName: e.SessionName,
		Kind: e.Kind, Author: e.Author, AuthorName: s.authorName(ctx, campaignID, e.Author),
		Title: e.Title, ByteSize: e.ByteSize,
		CreatedAt: e.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if withContent {
		v.Content = e.Content
	}
	return v
}

// authorName resolves a journal author id to the character's name; a
// freeform author (a DM-pasted journal) resolves no name and stays as
// written.
func (s *Server) authorName(ctx context.Context, campaignID, author string) string {
	if author == "" {
		return ""
	}
	if e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, author); err == nil {
		return e.Name
	}
	return ""
}

// loadJournalAccess resolves the caller's journal standing: the DM reads the
// campaign's journals; a player bound to a character reads their own; an
// unbound member, an observer and the keeper-without-a-row read nobody's
// (the keeper who is also the DM reads all — the DM path). It writes the
// HTTP error itself and returns ok=false when the request should not
// proceed.
func (s *Server) loadJournalAccess(w http.ResponseWriter, r *http.Request, campaignID string) (a *campAccess, characterID string, ok bool) {
	a = s.resolveCampaignAccess(w, r, campaignID)
	if a == nil {
		return nil, "", false
	}
	if a.isDM() {
		return a, "", true
	}
	if a.view == nil || a.playerScope.Kind() != campaign.ScopeKindCharacter {
		writeError(w, http.StatusForbidden, fmt.Errorf("a journal belongs to a character at the table"))
		return nil, "", false
	}
	return a, a.playerScope.EntityID(), true
}

/* ---------- handlers ---------- */

// handleCreateJournal is the write: a seated player authors one entry against
// a session. The author is the caller's bound character, bound here and never
// read from the body; the kind is player_journal and nothing else — this
// route cannot be aimed at the DM's source kinds, another campaign's
// session, or another member's name.
func (s *Server) handleCreateJournal(w http.ResponseWriter, r *http.Request) {
	a, characterID, ok := s.loadJournalAccess(w, r, r.PathValue("cid"))
	if !ok {
		return
	}
	if a.isDM() {
		writeError(w, http.StatusForbidden, fmt.Errorf("this route writes a player's journal; the DM's own material has the sources route"))
		return
	}
	ses, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid"))
	if !ok {
		return
	}
	var req struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJournalBytes+(1<<20))).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		req.Title = time.Now().UTC().Format("2 January 2006")
	}
	if int64(len(req.Content)) > maxJournalBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("a journal entry exceeds %d MB", maxJournalBytes>>20))
		return
	}
	src, err := s.sessions.AddSource(r.Context(), ses.ID, gamesession.SourcePlayerJournal,
		characterID, req.Title, req.Content, nil)
	if err != nil {
		writeCampaignError(w, err)
		return
	}
	entry := gamesession.JournalEntry{Source: *src, SessionOrdinal: ses.Ordinal, SessionName: ses.Name}
	writeJSON(w, http.StatusCreated, map[string]any{
		"entry": s.toJournalView(r.Context(), r.PathValue("cid"), entry, false),
	})
}

// handleSessionJournal lists one session's journal entries: a player's own,
// or the whole party's for the DM, marked by author.
func (s *Server) handleSessionJournal(w http.ResponseWriter, r *http.Request) {
	_, characterID, ok := s.loadJournalAccess(w, r, r.PathValue("cid"))
	if !ok {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	entries, err := s.sessions.ListJournals(r.Context(), r.PathValue("cid"), r.PathValue("sid"), characterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJournalList(w, r, r.PathValue("cid"), entries)
}

// handleCampaignJournal lists the journal across the campaign's sessions, in
// play order: a player's own entries, or the whole party's for the DM.
func (s *Server) handleCampaignJournal(w http.ResponseWriter, r *http.Request) {
	_, characterID, ok := s.loadJournalAccess(w, r, r.PathValue("cid"))
	if !ok {
		return
	}
	entries, err := s.sessions.ListJournals(r.Context(), r.PathValue("cid"), "", characterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJournalList(w, r, r.PathValue("cid"), entries)
}

func (s *Server) writeJournalList(w http.ResponseWriter, r *http.Request, campaignID string, entries []gamesession.JournalEntry) {
	views := make([]journalView, 0, len(entries))
	for i := range entries {
		views = append(views, s.toJournalView(r.Context(), campaignID, entries[i], false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": views})
}

/* ---------- the drafting assist ---------- */

// journalGrounding is what one drafting assist assembles: the character's
// own discoveries, the quest journal, and the witnessed timeline — PlayerView
// reads only, structurally incapable of carrying a secret or proposed row.
type journalGrounding struct {
	characterName string
	discoveries   []knowledge.Discovery
	quests        []knowledge.QuestJournalEntry
	timeline      []campaign.Event
}

// groundJournal runs the assist's retrieval at the caller's own scope. Every
// read is the player view's — the same surface the portal renders from.
func (s *Server) groundJournal(ctx context.Context, a *campAccess, characterID string) (*journalGrounding, error) {
	g := &journalGrounding{}
	if e, err := a.view.Entity(ctx, a.campaign.ID, characterID); err == nil {
		g.characterName = e.Name
	}
	ds, err := a.view.Discoveries(ctx, a.campaign.ID, "")
	if err != nil {
		return nil, err
	}
	g.discoveries = ds
	qs, err := a.view.QuestJournal(ctx, a.campaign.ID)
	if err != nil {
		return nil, err
	}
	g.quests = qs
	tl, err := a.view.Timeline(ctx, a.campaign.ID)
	if err != nil {
		return nil, err
	}
	g.timeline = tl
	return g, nil
}

// journalDraftSystemPrompt is the assist's standing instruction. Behaviour,
// never secrecy: there is no secret in the assembled context to keep.
func journalDraftSystemPrompt() string {
	return `You are the journaling assistant for one player character in a D&D campaign. You help the player write today's journal entry in their character's voice.

GROUNDING RULES — follow these strictly:
1. Draft ONLY from the character's own record provided: their discoveries, their quest journal, the events they witnessed. The record is the whole of what this character knows.
2. Never assert anything the record does not contain — no invented people, places, motives or secrets. Gaps are the player's to fill, not yours.
3. Write in first person, in the character's voice, as a diary entry: what happened as they experienced it, what they think it means, what they mean to do next. Keep it a starting point the player will edit — a few short paragraphs at most.
4. If the record is empty, say so plainly in one sentence and stop; do not invent a day.`
}

// buildJournalDraftPrompt assembles the assist's user turn: the session, the
// character's grounded record, and the player's own notes. This is the exact
// text the model receives — the function the leak assertions in the tests
// read, so it stays pure and testable.
func buildJournalDraftPrompt(g *journalGrounding, ordinal int64, session, notes string) string {
	var b strings.Builder
	who := g.characterName
	if who == "" {
		who = "the character"
	}
	fmt.Fprintf(&b, "Help %s write today's journal entry.\n\n", who)
	if session != "" {
		fmt.Fprintf(&b, "Session: %d — %s\n\n", ordinal, session)
	}

	b.WriteString("WHAT THIS CHARACTER HAS LEARNED (their discoveries — the whole of their knowledge of the campaign):\n")
	if len(g.discoveries) == 0 {
		b.WriteString("(nothing recorded yet)\n")
	}
	for _, d := range g.discoveries {
		fmt.Fprintf(&b, "- learned %s (how: %s)", d.FactID, d.Method)
		if d.Quote != "" {
			fmt.Fprintf(&b, " — %q", d.Quote)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString("THEIR QUEST JOURNAL (where each quest stands for them):\n")
	if len(g.quests) == 0 {
		b.WriteString("(no quests)\n")
	}
	for _, q := range g.quests {
		fmt.Fprintf(&b, "- %s (%s): %s — currently %q\n", q.Name, q.Status, q.Summary, q.CurrentState.Label)
	}
	b.WriteString("\n")

	b.WriteString("EVENTS THEY WITNESSED (in play order):\n")
	if len(g.timeline) == 0 {
		b.WriteString("(nothing witnessed yet)\n")
	}
	for _, ev := range g.timeline {
		fmt.Fprintf(&b, "- %s\n", ev.Summary)
	}
	b.WriteString("\n")

	if notes = strings.TrimSpace(notes); notes != "" {
		fmt.Fprintf(&b, "THE PLAYER'S OWN NOTES FOR TODAY'S ENTRY (work them in):\n%s\n\n", notes)
	}
	b.WriteString("Write the draft entry now, in their voice, grounded only in the record above.")
	return b.String()
}

// handleJournalDraft is the assist: a bound player asks for today's entry and
// gets a first-person draft to edit and post themselves. The draft is never
// posted for them — the entry a journal stores is always the player's own
// words, and the assist may not assert anything the character has not
// learned.
func (s *Server) handleJournalDraft(w http.ResponseWriter, r *http.Request) {
	a, characterID, ok := s.loadJournalAccess(w, r, r.PathValue("cid"))
	if !ok {
		return
	}
	if a.isDM() {
		writeError(w, http.StatusForbidden, fmt.Errorf("the drafting assist writes a character's journal; the DM's notes have the sources route"))
		return
	}
	ses, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid"))
	if !ok {
		return
	}
	var req struct {
		Notes string `json:"notes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if !s.llm.Configured() {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the journaling assistant is not configured. Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL / ANTHROPIC_MODEL) to enable it."))
		return
	}
	g, err := s.groundJournal(r.Context(), a, characterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	user := buildJournalDraftPrompt(g, ses.Ordinal, ses.Name, req.Notes)
	ctx, cancel := context.WithTimeout(r.Context(), journalDraftTimeout)
	defer cancel()
	draft, err := s.llm.AnswerPrompt(ctx, journalDraftSystemPrompt(), user)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("the journaling assistant could not be reached: %v", err))
		return
	}
	draft = strings.TrimSpace(draft)
	writeJSON(w, http.StatusOK, map[string]any{"draft": draft, "perspective": g.characterName})
}
