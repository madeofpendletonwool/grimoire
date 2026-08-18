package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/data"
)

// The reader surface: book-shaped browsing of the corpora. Three endpoints:
//
//	GET /api/reader/guides?corpus=mtg        the readable books
//	GET /api/reader/toc?corpus=mtg&guide=…   one book's table of contents
//	GET /api/reader/page?corpus=mtg&guide=…&number=…   one stop, full text
//
// plus an internal resolve used by the drawer to deep-link from a citation
// ("rule 205.1a" → its page) via ?number= on /api/reader/toc when guide is
// omitted: the response then carries the resolved guide.

// readerGuideView is the JSON shape of a readable book.
type readerGuideView struct {
	Corpus string `json:"corpus"`
	Guide  string `json:"guide"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Nodes  int    `json:"nodes"`
}

func (s *Server) handleReaderGuides(w http.ResponseWriter, r *http.Request) {
	corpus := parseCorpus(r.URL.Query().Get("corpus"))
	guides, err := s.store.ReaderGuides(r.Context(), corpus)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]readerGuideView, 0, len(guides))
	for _, g := range guides {
		views = append(views, readerGuideView{Corpus: g.Corpus, Guide: g.Guide, Title: g.Title, Kind: g.Kind, Nodes: g.Nodes})
	}
	writeJSON(w, http.StatusOK, map[string]any{"guides": views})
}

// readerTOCView is one stop in a table of contents. Leaf bodies are omitted —
// the TOC is for navigation; the page endpoint carries the text.
type readerTOCView struct {
	Number  string `json:"number"`
	Title   string `json:"title"`
	Level   int    `json:"level"`
	HasBody bool   `json:"has_body"`
}

func (s *Server) handleReaderTOC(w http.ResponseWriter, r *http.Request) {
	corpus := parseCorpus(r.URL.Query().Get("corpus"))
	q := r.URL.Query()
	guide := q.Get("guide")

	// A bare number deep-links: resolve it to its guide and stop, then serve
	// that guide's TOC with the stop flagged for the client to open.
	if guide == "" {
		number := q.Get("number")
		if number == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("guide (or number) is required"))
			return
		}
		g, n, err := s.store.ReaderResolve(r.Context(), corpus, number)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if g == "" {
			writeJSON(w, http.StatusOK, map[string]any{"guide": nil, "toc": []readerTOCView{}})
			return
		}
		guide = g
		toc := s.tocViews(w, r, corpus, guide)
		if toc == nil {
			return // response already written
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"guide":  g,
			"number": n,
			"toc":    toc,
		})
		return
	}

	toc := s.tocViews(w, r, corpus, guide)
	if toc == nil {
		return // response already written
	}
	writeJSON(w, http.StatusOK, map[string]any{"guide": guide, "toc": toc})
}

func (s *Server) tocViews(w http.ResponseWriter, r *http.Request, corpus data.Corpus, guide string) []readerTOCView {
	toc, err := s.store.ReaderTOC(r.Context(), corpus, guide)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return nil
	}
	views := make([]readerTOCView, 0, len(toc))
	for _, t := range toc {
		views = append(views, readerTOCView{Number: t.Number, Title: t.Title, Level: t.Level, HasBody: t.HasBody})
	}
	return views
}

// readerPageView is the JSON shape of a reading page. Body stays raw (markdown
// for D&D sources, rules text for MTG) — the client renders it.
type readerPageView struct {
	Corpus     string          `json:"corpus"`
	Guide      string          `json:"guide"`
	GuideTitle string          `json:"guide_title"`
	GuideKind  string          `json:"guide_kind,omitempty"`
	Number     string          `json:"number"`
	Title      string          `json:"title"`
	Body       string          `json:"body"`
	Source     string          `json:"source,omitempty"`
	Crumbs     []readerTOCView `json:"crumbs"`
	Prev       *readerTOCView  `json:"prev,omitempty"`
	Next       *readerTOCView  `json:"next,omitempty"`
}

func (s *Server) handleReaderPage(w http.ResponseWriter, r *http.Request) {
	corpus := parseCorpus(r.URL.Query().Get("corpus"))
	q := r.URL.Query()
	guide, number := q.Get("guide"), q.Get("number")
	if number == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("number is required"))
		return
	}
	if guide == "" {
		g, n, err := s.store.ReaderResolve(r.Context(), corpus, number)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if g == "" {
			writeError(w, http.StatusNotFound, fmt.Errorf("no reading page for %q", number))
			return
		}
		guide, number = g, n
	}

	p, err := s.store.ReaderPage(r.Context(), corpus, guide, number)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("no such reading page"))
		return
	}
	view := readerPageView{
		Corpus: p.Corpus, Guide: p.Guide, GuideTitle: p.GuideTitle, GuideKind: p.GuideKind,
		Number: p.Number, Title: p.Title, Body: p.Body, Source: p.Source,
		Crumbs: make([]readerTOCView, 0, len(p.Crumbs)),
	}
	for _, c := range p.Crumbs {
		view.Crumbs = append(view.Crumbs, readerTOCView{Number: c.Number, Title: c.Title, Level: c.Level, HasBody: c.HasBody})
	}
	if p.Prev != nil {
		view.Prev = &readerTOCView{Number: p.Prev.Number, Title: p.Prev.Title, Level: p.Prev.Level, HasBody: p.Prev.HasBody}
	}
	if p.Next != nil {
		view.Next = &readerTOCView{Number: p.Next.Number, Title: p.Next.Title, Level: p.Next.Level, HasBody: p.Next.HasBody}
	}
	writeJSON(w, http.StatusOK, view)
}
