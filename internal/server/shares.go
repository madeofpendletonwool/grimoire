package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/share"
)

// WithShares wires the shared-link store. It is separate from New so shared
// links are additive for callers that do not use them; without it the share
// endpoints report unavailable.
func (s *Server) WithShares(store *share.Store) *Server {
	s.shares = store
	return s
}

// sharesEnabled reports whether shared links are available, writing the error
// response when they are not.
func (s *Server) sharesEnabled(w http.ResponseWriter) bool {
	if s.shares == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("shared links are not available"))
		return false
	}
	return true
}

// shareView is the JSON shape of a share in the owner's list. URL is absolute
// so it can be copied straight from Settings.
type shareView struct {
	Token     string `json:"token"`
	URL       string `json:"url"`
	Question  string `json:"question"`
	Corpus    string `json:"corpus"`
	CreatedAt string `json:"created_at"`
	RevokedAt string `json:"revoked_at,omitempty"`
}

func (s *Server) handleListShares(w http.ResponseWriter, r *http.Request) {
	if !s.sharesEnabled(w) {
		return
	}
	entries, err := s.shares.List(r.Context(), userID(r), 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]shareView, 0, len(entries))
	for _, e := range entries {
		views = append(views, shareView{
			Token: e.Token, URL: shareURL(r, e.Token), Question: e.Question, Corpus: e.Corpus,
			CreatedAt: e.CreatedAt.Format(time.RFC3339),
			RevokedAt: timeFmt(e.RevokedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": views})
}

func timeFmt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// shareURL builds the public link against the request's host, so it works
// behind whatever proxy fronts the app — the same reasoning as inviteURL.
func shareURL(r *http.Request, token string) string {
	scheme := "http"
	if secureCookies(r) {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/s/%s", scheme, r.Host, token)
}

// handleShareMessage snapshots a stored Q&A under a fresh token. Ownership
// rides on the chat lookup: a stranger's chat id is an indistinguishable 404,
// exactly like every other conversation endpoint.
func (s *Server) handleShareMessage(w http.ResponseWriter, r *http.Request) {
	if !s.sharesEnabled(w) {
		return
	}
	if !s.chatsEnabled(w) {
		return
	}
	msgID, err := strconv.ParseInt(r.PathValue("messageID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("message id is required"))
		return
	}
	conv, err := s.chats.Get(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		writeChatError(w, err)
		return
	}
	msgs, err := s.chats.Messages(r.Context(), conv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Find the answer, and walk back to the question it answered: the nearest
	// preceding user turn. Shares are single Q&A pairs.
	var answer *chat.Message
	question := ""
	for i := range msgs {
		if msgs[i].ID == msgID {
			if msgs[i].Role != chat.RoleAssistant {
				writeError(w, http.StatusNotFound, fmt.Errorf("only answers can be shared"))
				return
			}
			answer = &msgs[i]
			for j := i - 1; j >= 0; j-- {
				if msgs[j].Role == chat.RoleUser {
					question = msgs[j].Content
					break
				}
			}
			break
		}
	}
	if answer == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("message not found"))
		return
	}

	token, err := s.shares.Create(r.Context(), userID(r), conv.ID, msgID, share.Snapshot{
		Question: question,
		Answer:   answer.Content,
		Corpus:   conv.Corpus,
		Sources:  answer.Sources,
		Cards:    answer.Cards,
		Entities: answer.Entities,
		Rulings:  answer.Rulings,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"url": "/s/" + token})
}

// handleRevokeShare closes a link. Owner only; an unknown token and someone
// else's token are the same 404.
func (s *Server) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	if !s.sharesEnabled(w) {
		return
	}
	if err := s.shares.Revoke(r.Context(), userID(r), r.PathValue("token")); err != nil {
		if errors.Is(err, share.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sharePageView is everything the public page renders. The answer arrives as
// raw Markdown and is hydrated by the same client renderer the app uses, so
// a shared page and the chat it came from can never disagree on formatting.
type sharePageView struct {
	State      string // "ok" | "gone" | "missing"
	Corpus     string
	CorpusName string
	Question   string
	Title      string
	Answer     string
	Sources    []searchHit
	Cards      []cardView
	Entities   []entityView
	Rulings    []rulingView
	CreatedAt  string
}

// validTokenRe bounds what is even looked up: base64url, the charset Create
// mints. Anything else is an unknown token by construction.
var validTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{22,128}$`)

// handleSharePage serves the public, read-only page. No session is required
// and none is consulted — the token is the whole access model.
func (s *Server) handleSharePage(w http.ResponseWriter, r *http.Request) {
	v := sharePageView{State: "missing", Title: "Grimoire"}
	if s.shares == nil {
		s.renderSharePage(w, http.StatusNotFound, v)
		return
	}
	token := r.PathValue("token")
	if !validTokenRe.MatchString(token) {
		s.renderSharePage(w, http.StatusNotFound, v)
		return
	}
	sh, snap, err := s.shares.Get(r.Context(), token)
	switch {
	case errors.Is(err, share.ErrRevoked):
		v.State = "gone"
		s.renderSharePage(w, http.StatusGone, v)
		return
	case errors.Is(err, share.ErrNotFound):
		s.renderSharePage(w, http.StatusNotFound, v)
		return
	case err != nil:
		log.Printf("share page %s: %v", token, err)
		s.renderSharePage(w, http.StatusInternalServerError, v)
		return
	}

	corpus := parseCorpus(snap.Corpus)
	v = sharePageView{
		State:      "ok",
		Corpus:     string(corpus),
		CorpusName: corpusDisplayName(corpus),
		Question:   snap.Question,
		Title:      deriveShareTitle(snap.Question),
		Answer:     snap.Answer,
		Sources:    decodeSources(snap.Sources),
		Cards:      decodeCards(snap.Cards),
		Entities:   decodeEntities(snap.Entities),
		Rulings:    decodeRulings(snap.Rulings),
		CreatedAt:  sh.CreatedAt.Format("2 January 2006"),
	}
	s.renderSharePage(w, http.StatusOK, v)
}

func (s *Server) renderSharePage(w http.ResponseWriter, status int, v sharePageView) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, "share.html", v); err != nil {
		log.Printf("render share.html: %v", err)
	}
}

// deriveShareTitle names the page after its question, trimmed like the
// sidebar trims a conversation title.
func deriveShareTitle(question string) string {
	t := chat.DeriveTitle(question)
	if t == "New chat" {
		return "Grimoire"
	}
	return t + " — Grimoire"
}
