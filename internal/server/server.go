// Package server serves the Grimoire web UI and JSON API.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
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
	store            *index.Store
	llm              *llm.Client
	cards            *cards.Service
	cardDict         *cards.Dictionary
	chats            *chat.Store
	users            *auth.Store
	openRegistration bool
	tmpl             *template.Template
	static           fs.FS
}

// New builds a Server from an open index store, an LLM client, a card lookup
// service, an optional card-name dictionary (powers lowercase/unquoted card
// detection in the chat), a chat store, and the authentication wiring. A nil
// card service disables card features gracefully; a nil dictionary leaves
// detection on the text heuristics alone; a nil chat store disables saved
// conversations; a zero Auth leaves the API unauthenticated.
func New(store *index.Store, client *llm.Client, cardSvc *cards.Service, cardDict *cards.Dictionary, chats *chat.Store, ac Auth) (*Server, error) {
	tmpl, err := template.New("").ParseFS(web.Templates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	static, err := fs.Sub(web.Static, "static")
	if err != nil {
		return nil, err
	}
	return &Server{
		store: store, llm: client, cards: cardSvc, cardDict: cardDict, chats: chats,
		users: ac.Users, openRegistration: ac.OpenRegistration,
		tmpl: tmpl, static: static,
	}, nil
}

// Handler returns the HTTP handler tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/auth/state", s.handleAuthState)
	mux.HandleFunc("POST /api/auth/setup", s.handleSetup)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/section", s.handleSection)
	mux.HandleFunc("GET /api/card", s.handleCard)
	mux.HandleFunc("GET /api/card/search", s.handleCardSearch)
	mux.HandleFunc("POST /api/ask", s.handleAsk)
	mux.HandleFunc("GET /api/chats", s.handleListChats)
	mux.HandleFunc("POST /api/chats", s.handleCreateChat)
	mux.HandleFunc("GET /api/chats/{id}", s.handleGetChat)
	mux.HandleFunc("PATCH /api/chats/{id}", s.handleRenameChat)
	mux.HandleFunc("DELETE /api/chats/{id}", s.handleDeleteChat)
	mux.HandleFunc("POST /api/chats/{id}/messages", s.handleChatMessage)
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
		"corpora":         views,
		"chat_configured": s.llm.Configured(),
		"chat_model":      s.llm.Model(),
	})
}

type searchHit struct {
	Number string `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Source string `json:"source"`
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
			Number: res.Number, Title: res.Title, Body: res.Body, Source: res.Source,
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

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	answer, err := s.llm.Answer(ctx, g.request(corpus, req.Question, nil))
	if err != nil {
		answer = fmt.Sprintf("I couldn't reach the model: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":       true,
		"answer":           answer,
		"sources":          g.sources,
		"cards":            g.cards,
		"unresolved_cards": g.unresolved,
	})
}

// grounded is the retrieval result for one question: what the model is shown,
// and what the reader is shown as citations.
type grounded struct {
	docs       []llm.ContextDoc
	cardDocs   []llm.CardDoc
	sources    []searchHit
	cards      []cardView
	unresolved []string
}

func (g grounded) request(corpus data.Corpus, question string, history []llm.Turn) llm.Request {
	return llm.Request{
		CorpusName: corpusDisplayName(corpus),
		Docs:       g.docs,
		Cards:      g.cardDocs,
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
		g.docs = append(g.docs, llm.ContextDoc{Number: res.Number, Title: res.Title, Body: res.Body})
	}
	g.sources = toSources(results)
	g.cardDocs, g.cards, g.unresolved = s.lookupQuestionCards(ctx, corpus, question)
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

func toSources(results []index.Result) []searchHit {
	out := make([]searchHit, 0, len(results))
	for _, r := range results {
		out = append(out, searchHit{Number: r.Number, Title: r.Title, Body: r.Body, Source: r.Source})
	}
	return out
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
