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

	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/web"
)

// Server holds dependencies for serving the app.
type Server struct {
	store  *index.Store
	llm    *llm.Client
	tmpl   *template.Template
	static fs.FS
}

// New builds a Server from an open index store and an LLM client.
func New(store *index.Store, client *llm.Client) (*Server, error) {
	tmpl, err := template.New("").ParseFS(web.Templates, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	static, err := fs.Sub(web.Static, "static")
	if err != nil {
		return nil, err
	}
	return &Server{store: store, llm: client, tmpl: tmpl, static: static}, nil
}

// Handler returns the HTTP handler tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("POST /api/ask", s.handleAsk)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.static))))
	mux.HandleFunc("GET /", s.handleIndex)
	return s.recoverer(s.logger(mux))
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
	for _, c := range []data.Corpus{data.CorpusMTG, data.CorpusDND} {
		m := meta[c]
		if m.Name == "" {
			continue
		}
		views = append(views, corpusView{
			ID: string(c), Name: m.Name, Version: m.Version, Count: m.RecordCount,
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

	// RAG: retrieve grounding context (lenient OR match), then ask the model.
	results, err := s.store.Retrieve(r.Context(), corpus, req.Question, 6)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	docs := make([]llm.ContextDoc, 0, len(results))
	for _, res := range results {
		docs = append(docs, llm.ContextDoc{Number: res.Number, Title: res.Title, Body: res.Body})
	}
	corpusName := corpusDisplayName(corpus)

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	answer, err := s.llm.Answer(ctx, corpusName, docs, req.Question)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": true,
			"answer":     fmt.Sprintf("I couldn't reach the model: %v", err),
			"sources":    toSources(results),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"answer":     answer,
		"sources":    toSources(results),
	})
}

func toSources(results []index.Result) []searchHit {
	out := make([]searchHit, 0, len(results))
	for _, r := range results {
		out = append(out, searchHit{Number: r.Number, Title: r.Title, Body: r.Body, Source: r.Source})
	}
	return out
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", nil); err != nil {
		log.Printf("render index: %v", err)
	}
}

func parseCorpus(v string) data.Corpus {
	if v == string(data.CorpusDND) {
		return data.CorpusDND
	}
	return data.CorpusMTG
}

func corpusDisplayName(c data.Corpus) string {
	switch c {
	case data.CorpusDND:
		return "D&D 5e SRD"
	default:
		return "Magic: The Gathering"
	}
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
