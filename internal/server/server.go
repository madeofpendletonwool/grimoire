// Package server serves the Grimoire web UI and JSON API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/cache"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/deck"
	"github.com/madeofpendletonwool/grimoire/internal/edhrec"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/entities"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/rulings"
	"github.com/madeofpendletonwool/grimoire/internal/share"
	"github.com/madeofpendletonwool/grimoire/internal/study"
	"github.com/madeofpendletonwool/grimoire/internal/transcribe"
	"github.com/madeofpendletonwool/grimoire/web"
)

// Auth wires the authentication layer. A zero Auth leaves the API open, which
// is only appropriate for tests and for callers that gate the app themselves.
type Auth struct {
	Users *auth.Store
	// OpenRegistration keeps the account-creation endpoint available after the
	// first keeper exists. Off by default: a self-hosted install facing the
	// internet should not hand out accounts.
	OpenRegistration bool
}

// Server holds dependencies for serving the app.
type Server struct {
	store      *index.Store
	llm        *llm.Client
	cards      *cards.Service
	rulings    *rulings.Service
	cardDict   *cards.Dictionary
	chats      *chat.Store
	answers    *cache.Store
	study      *study.Store
	encounters *encounter.Store
	bestiary   *encounter.Bestiary
	catalog    *encounter.Catalog
	carddb     *carddb.Store
	decks      *deck.Store
	edhrec     *edhrec.Client
	shares     *share.Store
	users      *auth.Store
	// The campaign graph with its knowledge layer and its session layer
	// (MAD-303/305/306). Wired with WithCampaign/WithCampaigns; nil
	// disables the campaign endpoints.
	campaigns *campaign.Store
	sessions  *gamesession.Store
	knowledge *knowledge.Store
	// The canon engine's deterministic surface (MAD-309): the offline
	// consistency checks and the flag ledger. Wired with WithCanon.
	canon *canon.Store
	// The optional audio→transcript hook (MAD-320): an OpenAI-compatible
	// transcription client plus its job worker. Wired with WithTranscriber;
	// nil (or unconfigured) means the affordance is not there.
	transcribe       *transcribe.Client
	transcribeOpts   TranscribeOptions
	transcribeWork   transcribeWorkState
	openRegistration bool
	tmpl             *template.Template
	static           fs.FS
	// rebuild, when wired, rebuilds the rules index from its sources. It backs
	// the admin's reindex control in Settings; nil disables those endpoints.
	rebuild func(ctx context.Context) error
	reindex reindexState
}

// New builds a Server from an open index store, an LLM client, a card lookup
// service, a rulings lookup service, an optional card-name dictionary (powers
// lowercase/unquoted card detection in the chat), a chat store, an answer cache,
// a study store, the authentication wiring, and an optional index-rebuild
// function (powers the admin's Settings → Rebuild index control; nil disables
// it). A nil card service disables card features gracefully; a nil rulings
// service disables the rulings layer the same way; a nil dictionary leaves
// detection on the text heuristics alone; a nil chat store disables saved
// conversations; a nil answer cache disables response caching; a nil study
// store disables the study mode; a zero Auth leaves the API unauthenticated.
func New(store *index.Store, client *llm.Client, cardSvc *cards.Service, rulingsSvc *rulings.Service, cardDict *cards.Dictionary, chats *chat.Store, answers *cache.Store, studyStore *study.Store, ac Auth, rebuild func(ctx context.Context) error) (*Server, error) {
	tmpl, err := template.New("").ParseFS(web.Templates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	static, err := fs.Sub(web.Static, "static")
	if err != nil {
		return nil, err
	}
	return &Server{
		store: store, llm: client, cards: cardSvc, rulings: rulingsSvc, cardDict: cardDict, chats: chats,
		answers: answers,
		study:   studyStore,
		users:   ac.Users, openRegistration: ac.OpenRegistration,
		tmpl: tmpl, static: static, rebuild: rebuild,
	}, nil
}

// Handler returns the HTTP handler tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/auth/state", s.handleAuthState)
	mux.HandleFunc("POST /api/auth/setup", s.handleSetup)
	mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /api/invites", s.handleCreateInvite)
	mux.HandleFunc("GET /api/invites", s.handleListInvites)
	mux.HandleFunc("DELETE /api/invites/{id}", s.handleRevokeInvite)
	mux.HandleFunc("POST /api/admin/reindex", s.handleReindexStart)
	mux.HandleFunc("GET /api/admin/reindex", s.handleReindexStatus)
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/section", s.handleSection)
	mux.HandleFunc("GET /api/card", s.handleCard)
	mux.HandleFunc("GET /api/card/search", s.handleCardSearch)
	mux.HandleFunc("POST /api/ask", s.handleAsk)
	mux.HandleFunc("POST /api/resolve", s.handleResolve)
	mux.HandleFunc("GET /api/chats", s.handleListChats)
	mux.HandleFunc("POST /api/chats", s.handleCreateChat)
	mux.HandleFunc("GET /api/chats/{id}", s.handleGetChat)
	mux.HandleFunc("PATCH /api/chats/{id}", s.handleRenameChat)
	mux.HandleFunc("DELETE /api/chats/{id}", s.handleDeleteChat)
	mux.HandleFunc("POST /api/chats/{id}/messages", s.handleChatMessage)
	mux.HandleFunc("GET /api/study/queue", s.handleStudyQueue)
	mux.HandleFunc("POST /api/study/grade", s.handleStudyGrade)
	mux.HandleFunc("GET /api/reader/guides", s.handleReaderGuides)
	mux.HandleFunc("GET /api/reader/toc", s.handleReaderTOC)
	mux.HandleFunc("GET /api/reader/page", s.handleReaderPage)
	mux.HandleFunc("GET /api/encounter/monsters", s.handleEncounterMonsters)
	mux.HandleFunc("GET /api/encounter/statblock", s.handleEncounterStatblock)
	mux.HandleFunc("POST /api/encounter/budget", s.handleEncounterBudget)
	mux.HandleFunc("POST /api/encounter/design", s.handleEncounterDesign)
	mux.HandleFunc("POST /api/encounters/evaluate", s.handleEvaluate)
	mux.HandleFunc("POST /api/deck/propose", s.handleDeckPropose)
	mux.HandleFunc("POST /api/deck/build", s.handleDeckBuild)
	mux.HandleFunc("POST /api/deck/analyze", s.handleDeckAnalyze)
	mux.HandleFunc("POST /api/deck/chat", s.handleDeckChat)
	mux.HandleFunc("GET /api/deck/combos", s.handleDeckCombos)
	mux.HandleFunc("GET /api/decks", s.handleListDecks)
	mux.HandleFunc("POST /api/decks", s.handleCreateDeck)
	mux.HandleFunc("GET /api/decks/{id}", s.handleGetDeck)
	mux.HandleFunc("PATCH /api/decks/{id}", s.handleUpdateDeck)
	mux.HandleFunc("DELETE /api/decks/{id}", s.handleDeleteDeck)
	mux.HandleFunc("GET /api/encounters", s.handleListEncounters)
	mux.HandleFunc("POST /api/encounters", s.handleCreateEncounter)
	mux.HandleFunc("GET /api/encounters/{id}", s.handleGetEncounter)
	mux.HandleFunc("PATCH /api/encounters/{id}", s.handleUpdateEncounter)
	mux.HandleFunc("DELETE /api/encounters/{id}", s.handleDeleteEncounter)
	mux.HandleFunc("POST /api/chats/{id}/messages/{messageID}/share", s.handleShareMessage)
	mux.HandleFunc("GET /api/shares", s.handleListShares)
	mux.HandleFunc("DELETE /api/shares/{token}", s.handleRevokeShare)
	mux.HandleFunc("GET /s/{token}", s.handleSharePage)
	// The campaign surface: every route below resolves a knowledge.Scope from
	// the caller's campaign_members row before touching the store.
	mux.HandleFunc("GET /api/campaigns", s.handleListCampaigns)
	mux.HandleFunc("POST /api/campaigns", s.handleCreateCampaign)
	mux.HandleFunc("POST /api/campaigns/join", s.handleJoinCampaign)
	mux.HandleFunc("GET /api/campaigns/{id}", s.handleGetCampaign)
	mux.HandleFunc("PATCH /api/campaigns/{id}", s.handleUpdateCampaign)
	mux.HandleFunc("DELETE /api/campaigns/{id}", s.handleDeleteCampaign)
	mux.HandleFunc("GET /api/campaigns/{id}/members", s.handleCampaignMembers)
	mux.HandleFunc("PATCH /api/campaigns/{id}/members/{uid}", s.handleUpdateCampaignMember)
	mux.HandleFunc("DELETE /api/campaigns/{id}/members/{uid}", s.handleRemoveCampaignMember)
	mux.HandleFunc("GET /api/campaigns/{id}/invites", s.handleListCampaignInvites)
	mux.HandleFunc("POST /api/campaigns/{id}/invites", s.handleCreateCampaignInvite)
	mux.HandleFunc("DELETE /api/campaigns/{id}/invites/{iid}", s.handleRevokeCampaignInvite)
	mux.HandleFunc("GET /api/campaigns/{id}/entities", s.handleCampaignEntities)
	mux.HandleFunc("POST /api/campaigns/{id}/entities", s.handleCreateCampaignEntity)
	mux.HandleFunc("GET /api/campaigns/{id}/entities/{eid}", s.handleCampaignEntity)
	mux.HandleFunc("PATCH /api/campaigns/{id}/entities/{eid}", s.handleUpdateCampaignEntity)
	mux.HandleFunc("DELETE /api/campaigns/{id}/entities/{eid}", s.handleDeleteCampaignEntity)
	mux.HandleFunc("POST /api/campaigns/{id}/entities/{eid}/names", s.handleAddEntityName)
	mux.HandleFunc("GET /api/campaigns/{id}/facts", s.handleCampaignFacts)
	mux.HandleFunc("POST /api/campaigns/{id}/facts", s.handleCreateCampaignFact)
	mux.HandleFunc("GET /api/campaigns/{id}/facts/{fid}", s.handleCampaignFact)
	mux.HandleFunc("POST /api/campaigns/{id}/facts/{fid}/supersede", s.handleSupersedeFact)
	mux.HandleFunc("GET /api/campaigns/{id}/timeline", s.handleCampaignTimeline)
	mux.HandleFunc("POST /api/campaigns/{id}/events", s.handleCreateCampaignEvent)
	mux.HandleFunc("POST /api/campaigns/{id}/events/{eid}/participants", s.handleAddEventParticipant)
	mux.HandleFunc("POST /api/campaigns/{id}/events/{eid}/links", s.handleLinkCampaignEvents)
	mux.HandleFunc("GET /api/campaigns/{id}/relationships", s.handleCampaignRelationships)
	mux.HandleFunc("POST /api/campaigns/{id}/relationships", s.handleCreateCampaignRelationship)
	mux.HandleFunc("DELETE /api/campaigns/{id}/relationships/{rid}", s.handleDeleteCampaignRelationship)
	mux.HandleFunc("GET /api/campaigns/{id}/awareness", s.handleCampaignAwareness)
	mux.HandleFunc("PUT /api/campaigns/{id}/awareness", s.handleSetCampaignAwareness)
	mux.HandleFunc("GET /api/campaigns/{id}/quests", s.handleCampaignQuests)
	mux.HandleFunc("POST /api/campaigns/{id}/quests", s.handleCreateCampaignQuest)
	mux.HandleFunc("GET /api/campaigns/{id}/quests/{qid}", s.handleCampaignQuest)
	mux.HandleFunc("POST /api/campaigns/{id}/quests/{qid}/transition", s.handleQuestTransition)
	mux.HandleFunc("GET /api/campaigns/{id}/graph", s.handleCampaignGraph)
	mux.HandleFunc("GET /api/campaigns/{id}/search", s.handleCampaignSearch)
	mux.HandleFunc("GET /api/campaigns/{cid}/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/campaigns/{cid}/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/campaigns/{cid}/sessions/{sid}", s.handleGetSession)
	mux.HandleFunc("PATCH /api/campaigns/{cid}/sessions/{sid}", s.handleUpdateSession)
	mux.HandleFunc("GET /api/campaigns/{cid}/sessions/{sid}/sources", s.handleListSources)
	mux.HandleFunc("POST /api/campaigns/{cid}/sessions/{sid}/sources", s.handleAddSource)
	mux.HandleFunc("GET /api/campaigns/{cid}/sessions/{sid}/sources/{srcid}", s.handleGetSource)
	mux.HandleFunc("GET /api/campaigns/{cid}/sessions/{sid}/span", s.handleResolveSpan)
	mux.HandleFunc("GET /api/campaigns/{cid}/sessions/{sid}/events", s.handleListEvents)
	mux.HandleFunc("POST /api/campaigns/{cid}/sessions/{sid}/events", s.handleAddEvent)
	mux.HandleFunc("GET /api/campaigns/{cid}/sessions/{sid}/export", s.handleExportSession)
	// The optional audio→transcript hook (MAD-320): upload a recording, poll
	// the job, land a transcript source. DM only; 503 when unconfigured.
	mux.HandleFunc("POST /api/campaigns/{cid}/sessions/{sid}/transcriptions", s.handleStartTranscription)
	mux.HandleFunc("GET /api/campaigns/{cid}/sessions/{sid}/transcriptions", s.handleListTranscriptions)
	mux.HandleFunc("GET /api/campaigns/{cid}/sessions/{sid}/transcriptions/{tid}", s.handleGetTranscription)
	mux.HandleFunc("DELETE /api/campaigns/{cid}/sessions/{sid}/transcriptions/{tid}", s.handleDeleteTranscription)
	mux.HandleFunc("POST /api/campaigns/{cid}/sessions/{sid}/transcriptions/{tid}/retry", s.handleRetryTranscription)
	// The canon engine's deterministic surface: run the checks (offline, no
	// model), read the flag ledger, decide a flag. DM-only.
	mux.HandleFunc("POST /api/campaigns/{id}/canon/check", s.handleCanonCheck)
	mux.HandleFunc("GET /api/campaigns/{id}/canon/flags", s.handleCanonFlags)
	mux.HandleFunc("POST /api/campaigns/{id}/canon/flags/decision", s.handleCanonFlagDecision)
	mux.HandleFunc("POST /api/campaigns/{id}/canon/reviews/build", s.handleCanonReviewsBuild)
	mux.HandleFunc("GET /api/campaigns/{id}/canon/reviews", s.handleCanonReviews)
	mux.HandleFunc("POST /api/campaigns/{id}/canon/reviews/accept-agree", s.handleCanonReviewsAcceptAgree)
	mux.HandleFunc("POST /api/campaigns/{id}/canon/reviews/{rid}/decision", s.handleCanonReviewDecision)
	mux.HandleFunc("GET /api/campaigns/{id}/canon/reviews/export", s.handleCanonReviewsExport)
	// The canon engine's continuity, entailment and health surfaces
	// (MAD-312): deterministic cores with optional model passes. DM-only.
	mux.HandleFunc("POST /api/campaigns/{id}/canon/continuity", s.handleCanonContinuity)
	mux.HandleFunc("POST /api/campaigns/{id}/canon/entail", s.handleCanonEntail)
	mux.HandleFunc("POST /api/campaigns/{id}/canon/health", s.handleCanonHealth)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.static))))
	mux.HandleFunc("GET /", s.handleIndex)
	return s.recoverer(s.logger(s.requireSession(mux)))
}

// userID identifies the caller who owns a conversation. requireSession puts the
// authenticated user on the request context; the anonymous fallback only
// applies when no auth store is wired, where every caller is the same person.
func userID(r *http.Request) string {
	if u, ok := userFrom(r.Context()); ok {
		return u.ID
	}
	return chat.AnonymousUser
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	ok, err := s.store.Indexed(ctx)
	status := "ok"
	if err != nil || !ok {
		status = "indexing"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "indexed": ok})
}

type corpusView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Count   int    `json:"count"`
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, err := s.store.CorpusMeta(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var views []corpusView
	for _, d := range data.Registered() {
		m := meta[d.Corpus]
		if m.Name == "" {
			continue
		}
		views = append(views, corpusView{
			ID: string(d.Corpus), Name: m.Name, Version: m.Version, Count: m.RecordCount,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"corpora":               views,
		"chat_configured":       s.llm.Configured(),
		"chat_model":            s.llm.Model(),
		"chat_fallbacks":        s.llm.FallbackModels(),
		"transcribe_configured": s.transcribe.Configured(),
		"transcribe_model":      s.transcribe.Model(),
	})
}

type searchHit struct {
	Number string `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Source string `json:"source"`
}

// displayRuleNumRe matches an MTG-style rule number ("205.1a").
var displayRuleNumRe = regexp.MustCompile(`^\d{1,3}(?:\.\d+)+[a-z]?$`)

// displayNumber blanks numbers that are not reader-facing citations. MTG rule
// numbers ("702.2a") are; D&D path-style record ids ("spells/0003/0042.1")
// are internal anchors for grounding, dedup, and cache keys — the UI's chips
// and section drawer show the title for those instead.
func displayNumber(n string) string {
	if displayRuleNumRe.MatchString(n) {
		return n
	}
	return ""
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	corpus := parseCorpus(r.URL.Query().Get("corpus"))
	limit := parseLimit(r.URL.Query().Get("limit"), 20)

	results, err := s.store.Search(r.Context(), corpus, q, limit)
	if err != nil {
		if err == index.ErrEmptyQuery {
			writeJSON(w, http.StatusOK, map[string]any{"results": []searchHit{}})
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	hits := make([]searchHit, 0, len(results))
	for _, res := range results {
		hits = append(hits, searchHit{
			Number: displayNumber(res.Number), Title: res.Title, Body: res.Body, Source: res.Source,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": hits})
}

// sectionResponse is the JSON shape for /api/section: the parent rule (the
// bare numbered rule, e.g. "702.2") when it exists, and its lettered
// sub-rules in order. Together they describe a full mechanic.
type sectionResponse struct {
	Parent   *searchHit  `json:"parent,omitempty"`
	Children []searchHit `json:"children"`
}

func (s *Server) handleSection(w http.ResponseWriter, r *http.Request) {
	corpus := parseCorpus(r.URL.Query().Get("corpus"))
	number := strings.TrimSpace(r.URL.Query().Get("number"))
	if number == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("number is required"))
		return
	}

	entries, err := s.store.Section(r.Context(), corpus, number)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resp := sectionResponse{Children: []searchHit{}}
	for _, e := range entries {
		hit := searchHit{Number: e.Number, Title: e.Title, Body: e.Body, Source: e.Source}
		// The parent is the entry whose number carries no trailing letter.
		if resp.Parent == nil && e.Number != "" && sectionRoot(e.Number) == e.Number {
			parent := hit
			resp.Parent = &parent
			continue
		}
		resp.Children = append(resp.Children, hit)
	}
	writeJSON(w, http.StatusOK, resp)
}

// sectionRoot strips a trailing letter run from a numbered rule, returning its
// section root: "702.2a" -> "702.2". A number without a trailing letter is
// returned unchanged.
func sectionRoot(number string) string {
	for i := len(number) - 1; i >= 0; i-- {
		c := number[i]
		if c >= 'a' && c <= 'z' {
			continue
		}
		return number[:i+1]
	}
	return number
}

type askRequest struct {
	Corpus   string `json:"corpus"`
	Question string `json:"question"`
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req askRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("question is required"))
		return
	}
	corpus := parseCorpus(req.Corpus)

	if !s.llm.Configured() {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"answer":     "The Q&A chat is not configured. Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL / ANTHROPIC_MODEL) to enable it.",
		})
		return
	}

	g, err := s.ground(r.Context(), corpus, req.Question)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// The grounding is part of the cache key, so retrieval always runs (it is
	// local and fast); a hit skips only the expensive LLM call. ?nocache forces
	// a fresh answer and refreshes the entry.
	if s.answers != nil && !r.URL.Query().Has("nocache") {
		key := cache.Key(string(corpus), req.Question, sourceIDs(g.sources))
		if hit, err := s.answers.Get(r.Context(), key); err != nil {
			log.Printf("answer cache get: %v", err)
		} else if hit != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"configured":       true,
				"cached":           true,
				"answer":           hit.Answer,
				"sources":          decodeSources(hit.Sources),
				"cards":            decodeCards(hit.Cards),
				"entities":         decodeEntities(hit.Entities),
				"rulings":          decodeRulings(hit.Rulings),
				"unresolved_cards": g.unresolved,
			})
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	answer, llmErr := s.llm.Answer(ctx, g.request(corpus, req.Question, nil))
	if llmErr != nil {
		answer = fmt.Sprintf("I couldn't reach the model: %v", llmErr)
	}
	// Only a real answer is cached — never the error placeholder.
	if s.answers != nil && llmErr == nil {
		sources, cardsJSON, entitiesJSON, rulingsJSON := marshalCitations(g)
		key := cache.Key(string(corpus), req.Question, sourceIDs(g.sources))
		if err := s.answers.Put(r.Context(), key, string(corpus), answer, sources, cardsJSON, entitiesJSON, rulingsJSON); err != nil {
			log.Printf("answer cache put: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":       true,
		"cached":           false,
		"answer":           answer,
		"sources":          g.sources,
		"cards":            g.cards,
		"entities":         g.entities,
		"rulings":          g.rulings,
		"unresolved_cards": g.unresolved,
	})
}

// grounded is the retrieval result for one question: what the model is shown,
// and what the reader is shown as citations.
type grounded struct {
	docs       []llm.ContextDoc
	cardDocs   []llm.CardDoc
	entityDocs []llm.EntityDoc
	rulingDocs []llm.RulingDoc
	sources    []searchHit
	cards      []cardView
	entities   []entityView
	rulings    []rulingView
	unresolved []string
}

func (g grounded) request(corpus data.Corpus, question string, history []llm.Turn) llm.Request {
	return llm.Request{
		CorpusName: corpusDisplayName(corpus),
		Docs:       g.docs,
		Cards:      g.cardDocs,
		Entities:   g.entityDocs,
		Rulings:    g.rulingDocs,
		Unresolved: g.unresolved,
		History:    history,
		Question:   question,
	}
}

// ground runs retrieval for a question: a lenient OR match seeds the citation
// list, those seeds are grown out to whole rule sections for the model (so a
// mechanic arrives complete rather than as the one sub-rule that ranked), and
// any card mentions are resolved to real oracle text.
func (s *Server) ground(ctx context.Context, corpus data.Corpus, question string) (grounded, error) {
	var g grounded
	results, err := s.store.Retrieve(ctx, corpus, question, retrieveSeeds)
	if err != nil {
		return g, err
	}
	expanded, err := s.store.Expand(ctx, corpus, results)
	if err != nil {
		return g, err
	}
	g.docs = make([]llm.ContextDoc, 0, len(expanded))
	for _, res := range expanded {
		g.docs = append(g.docs, llm.ContextDoc{Number: res.Number, Title: res.Title, Body: res.Body, Source: res.Source})
	}
	g.sources = toSources(results)
	g.cardDocs, g.cards, g.unresolved = s.lookupQuestionCards(ctx, corpus, question)
	g.rulingDocs, g.rulings = s.lookupCardRulings(ctx, corpus, g.cards)
	// Neutral entity grounding (D&D/Open5e and future corpora). MTG's Scryfall
	// resolver projects cards rather than neutral entities, so it is routed
	// through lookupQuestionCards above and skipped here — the card path is the
	// richer projection (images, faces, rulings) the MTG UI depends on.
	if def, ok := data.Lookup(corpus); ok && def.EntityResolver != nil {
		if _, isCards := def.EntityResolver.(entities.CardProjector); !isCards {
			g.entityDocs, g.entities, g.unresolved = s.lookupQuestionEntities(ctx, def.EntityResolver, question)
		}
	}
	return g, nil
}

// retrieveSeeds is how many search hits seed a Q&A answer. They are the
// sources shown to the reader, and the anchors Expand grows the model's
// grounding context from.
const retrieveSeeds = 12

// lookupQuestionCards extracts candidate card names from the question and
// resolves each to real oracle text via Scryfall. It returns the cards for the
// model, the cards for the UI, and the names it could not resolve — a miss is
// reported rather than dropped, so "we failed to look it up" never reads as
// "no card was mentioned". MTG-only.
func (s *Server) lookupQuestionCards(ctx context.Context, corpus data.Corpus, question string) ([]llm.CardDoc, []cardView, []string) {
	if s.cards == nil || corpus != data.CorpusMTG {
		return nil, nil, nil
	}
	res := cards.Resolve(ctx, s.cards, cards.ExtractCandidatesWithDict(question, s.cardDict))
	var cardDocs []llm.CardDoc
	var hits []cardView
	for _, c := range res.Cards {
		cardDocs = append(cardDocs, llm.CardDoc{
			Name: c.Name, ManaCost: c.ManaCost, TypeLine: c.TypeLine, OracleText: c.OracleText,
		})
		hits = append(hits, toCardView(c))
	}
	if len(res.Unresolved) > 0 {
		log.Printf("card lookup: no match for %q", strings.Join(res.Unresolved, ", "))
	}
	return cardDocs, hits, res.Unresolved
}

// lookupQuestionEntities resolves named entities for corpora without a
// card-shaped resolver (D&D/Open5e). It feeds the resolved reference text to the
// model as grounding and to the UI as citations, and surfaces unresolved names
// the way the MTG card path does — a miss is reported, not dropped.
func (s *Server) lookupQuestionEntities(ctx context.Context, resolver data.EntityResolver, question string) ([]llm.EntityDoc, []entityView, []string) {
	if resolver == nil {
		return nil, nil, nil
	}
	resolved, unresolved, err := resolver.Resolve(ctx, question)
	if err != nil {
		// A resolver error is best-effort: never break the answer over a lookup
		// failure, just as the card path tolerates Scryfall outages.
		log.Printf("entity lookup: %v", err)
		return nil, nil, nil
	}
	if len(resolved) == 0 && len(unresolved) == 0 {
		return nil, nil, nil
	}
	docs := make([]llm.EntityDoc, 0, len(resolved))
	views := make([]entityView, 0, len(resolved))
	for _, e := range resolved {
		docs = append(docs, llm.EntityDoc{Name: e.Name, Kind: e.Kind, Body: e.Body})
		views = append(views, entityView{Name: e.Name, Kind: e.Kind, Body: e.Body})
	}
	if len(unresolved) > 0 {
		log.Printf("entity lookup: no match for %q", strings.Join(unresolved, ", "))
	}
	return docs, views, unresolved
}

// lookupCardRulings fetches the official rulings for each resolved card. It
// feeds the rulings to the model as precedent (so answers cite Gatherer/Oracle
// rulings, not just rule text) and to the UI as citations. MTG-only, and a no-op
// when no rulings service is wired or no cards resolved.
func (s *Server) lookupCardRulings(ctx context.Context, corpus data.Corpus, hits []cardView) ([]llm.RulingDoc, []rulingView) {
	if s.rulings == nil || corpus != data.CorpusMTG || len(hits) == 0 {
		return nil, nil
	}
	var docs []llm.RulingDoc
	var views []rulingView
	for _, c := range hits {
		if c.Name == "" {
			continue
		}
		rs, err := s.rulings.Fetch(ctx, c.Name)
		if err != nil {
			// ErrNotFound covers both "no card" and "card with no rulings" —
			// neither is a hard failure, so they are not logged. Other errors
			// (timeout, upstream 5xx) are logged but never break the answer.
			if !errors.Is(err, rulings.ErrNotFound) {
				log.Printf("rulings lookup for %q: %v", c.Name, err)
			}
			continue
		}
		for _, r := range rs {
			docs = append(docs, llm.RulingDoc{
				CardName: c.Name, Source: r.Source, PublishedAt: r.PublishedAt, Comment: r.Comment,
			})
			views = append(views, rulingView{
				CardName: c.Name, Source: r.Source, PublishedAt: r.PublishedAt, Comment: r.Comment,
			})
		}
	}
	return docs, views
}

func toSources(results []index.Result) []searchHit {
	out := make([]searchHit, 0, len(results))
	for _, r := range results {
		out = append(out, searchHit{Number: displayNumber(r.Number), Title: r.Title, Body: r.Body, Source: r.Source})
	}
	return out
}

// sourceIDs extracts the stable identifier of each grounding source — a rule
// number when present, otherwise the title (D&D and other unnumbered corpora).
// It mirrors the dedup key index.Store.Expand uses, so the cache key tracks the
// same notion of "same grounding" the retrieval pipeline does.
func sourceIDs(hits []searchHit) []string {
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		switch {
		case h.Number != "":
			ids = append(ids, h.Number)
		case h.Title != "":
			ids = append(ids, h.Title)
		}
	}
	return ids
}

// decodeSources unpacks a cached citation payload back into the JSON shape the
// API returns, falling back to an empty slice so an absent payload renders as
// [] rather than null — matching a freshly grounded response.
func decodeSources(raw json.RawMessage) []searchHit {
	hits := []searchHit{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &hits)
	}
	return hits
}

// decodeCards is the card-citation counterpart to decodeSources.
func decodeCards(raw json.RawMessage) []cardView {
	cards := []cardView{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cards)
	}
	return cards
}

// decodeEntities is the entity-citation counterpart to decodeCards, used to
// replay cached D&D entity citations.
func decodeEntities(raw json.RawMessage) []entityView {
	entities := []entityView{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &entities)
	}
	return entities
}

// decodeRulings is the rulings-citation counterpart to decodeSources.
func decodeRulings(raw json.RawMessage) []rulingView {
	rulings := []rulingView{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &rulings)
	}
	return rulings
}

// cardView is the JSON shape for a card returned to the UI. It carries the
// fields needed to render a card and to attribute its source (Scryfall).
type cardView struct {
	Name        string     `json:"name"`
	ManaCost    string     `json:"mana_cost,omitempty"`
	TypeLine    string     `json:"type_line,omitempty"`
	OracleText  string     `json:"oracle_text,omitempty"`
	Power       string     `json:"power,omitempty"`
	Toughness   string     `json:"toughness,omitempty"`
	Loyalty     string     `json:"loyalty,omitempty"`
	Set         string     `json:"set,omitempty"`
	SetName     string     `json:"set_name,omitempty"`
	ImageURL    string     `json:"image_url,omitempty"`
	ScryfallURI string     `json:"scryfall_uri,omitempty"`
	Faces       []cardFace `json:"faces,omitempty"`
}

type cardFace struct {
	Name       string `json:"name"`
	ManaCost   string `json:"mana_cost,omitempty"`
	TypeLine   string `json:"type_line,omitempty"`
	OracleText string `json:"oracle_text,omitempty"`
	Power      string `json:"power,omitempty"`
	Toughness  string `json:"toughness,omitempty"`
	Loyalty    string `json:"loyalty,omitempty"`
	ImageURL   string `json:"image_url,omitempty"`
}

// rulingView is the JSON shape for an official ruling returned to the UI. It is
// attributed by the card it applies to, its source (wotc/scryfall), and its
// publish date so the UI can render it as a citation.
type rulingView struct {
	CardName    string `json:"card_name"`
	Source      string `json:"source"`
	PublishedAt string `json:"published_at"`
	Comment     string `json:"comment"`
}

// entityView is the JSON shape for a resolved reference entity returned to the
// UI (a D&D spell, creature, item, or feat). It carries the kind and the
// formatted reference body so the UI can render it as a citation, the way card
// citations are rendered for MTG.
type entityView struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
	Body string `json:"body,omitempty"`
}

func toCardView(c *cards.Card) cardView {
	if c == nil {
		return cardView{}
	}
	v := cardView{
		Name: c.Name, ManaCost: c.ManaCost, TypeLine: c.TypeLine, OracleText: c.OracleText,
		Power: c.Power, Toughness: c.Toughness, Loyalty: c.Loyalty, Set: c.Set,
		SetName: c.SetName, ImageURL: c.ImageURL, ScryfallURI: c.ScryfallURI,
	}
	for i := range c.Faces {
		f := c.Faces[i]
		v.Faces = append(v.Faces, cardFace{
			Name: f.Name, ManaCost: f.ManaCost, TypeLine: f.TypeLine, OracleText: f.OracleText,
			Power: f.Power, Toughness: f.Toughness, Loyalty: f.Loyalty, ImageURL: f.ImageURL,
		})
	}
	return v
}

// handleCard looks up a single MTG card by fuzzy name for the UI. On an
// unambiguous match it returns the card; otherwise it falls back to a search
// so the caller can offer alternatives.
func (s *Server) handleCard(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("q (card name) is required"))
		return
	}
	if s.cards == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("card lookup is not configured"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	if c, err := s.cards.Lookup(ctx, q); err == nil && c != nil {
		writeJSON(w, http.StatusOK, map[string]any{"card": toCardView(c)})
		return
	} else if err != nil && err != cards.ErrNotFound {
		// Upstream Scryfall error (timeout, 5xx) — surface it.
		writeJSON(w, http.StatusOK, map[string]any{
			"card":  nil,
			"error": fmt.Sprintf("card lookup failed: %v", err),
		})
		return
	}

	// No single match — offer alternatives from a name search.
	matches, err := s.cards.Search(ctx, q, 6)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"card": nil, "matches": []cardView{}})
		return
	}
	views := make([]cardView, 0, len(matches))
	for _, m := range matches {
		views = append(views, toCardView(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"card": nil, "matches": views})
}

// handleCardSearch returns name suggestions for the card lookup autocomplete.
// It calls Scryfall's name-order search directly so it doesn't burn the
// /cards/named round-trip that handleCard does on every keystroke. Misses
// return an empty matches slice rather than a 404 so the dropdown just
// collapses instead of erroring out.
func (s *Server) handleCardSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("q (card name) is required"))
		return
	}
	if s.cards == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("card lookup is not configured"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	matches, err := s.cards.Search(ctx, q, 8)
	if err != nil {
		// ErrNotFound is a normal "no matches" outcome — return an empty
		// list so the client can collapse the dropdown cleanly. Any other
		// error (timeout, upstream 5xx) surfaces as a non-fatal error field
		// alongside the empty list.
		if err == cards.ErrNotFound {
			writeJSON(w, http.StatusOK, map[string]any{"matches": []cardView{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"matches": []cardView{},
			"error":   fmt.Sprintf("card search failed: %v", err),
		})
		return
	}
	views := make([]cardView, 0, len(matches))
	for _, m := range matches {
		views = append(views, toCardView(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"matches": views})
}

// handleIndex serves the app to a signed-in reader and the gate to everyone
// else. Both are served at / rather than redirecting to a /login path, so a
// bookmark of the app still lands somewhere useful when a session has lapsed.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page := "index.html"
	if s.users != nil && s.currentUser(r) == nil {
		page = "auth.html"
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, page, nil); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

// parseCorpus resolves a corpus slug from the request. An unknown or empty
// value falls back to the default registered corpus (MTG), preserving the
// prior behavior. The set of valid corpora is whatever is registered, so a new
// corpus is valid the moment it is registered — no edit here.
func parseCorpus(v string) data.Corpus {
	if d, ok := data.Lookup(data.Corpus(v)); ok {
		return d.Corpus
	}
	return data.Default().Corpus
}

// corpusDisplayName returns a corpus's registered display name, falling back to
// the default corpus's name for an unknown value.
func corpusDisplayName(c data.Corpus) string {
	if d, ok := data.Lookup(c); ok {
		return d.Name
	}
	return data.Default().Name
}

func parseLimit(v string, def int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 100 {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %v", r.Method, r.URL.Path, time.Since(start))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				writeError(w, http.StatusInternalServerError, fmt.Errorf("internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
