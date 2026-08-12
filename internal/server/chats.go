package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// historyTurns caps how many prior messages are replayed to the model. Enough
// for a follow-up thread to stay coherent, bounded so a long conversation
// can't crowd the retrieved rules out of the context window.
const historyTurns = 12

// answerTimeout bounds one streamed answer end to end.
const answerTimeout = 3 * time.Minute

// chatView is the JSON shape of a conversation in list and detail responses.
type chatView struct {
	ID        string `json:"id"`
	Corpus    string `json:"corpus"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toChatView(c *chat.Conversation) chatView {
	return chatView{
		ID:        c.ID,
		Corpus:    c.Corpus,
		Title:     c.Title,
		CreatedAt: c.CreatedAt.Format(time.RFC3339),
		UpdatedAt: c.UpdatedAt.Format(time.RFC3339),
	}
}

// chatsEnabled reports whether saved conversations are available, writing the
// error response when they are not.
func (s *Server) chatsEnabled(w http.ResponseWriter) bool {
	if s.chats == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("chat history is not available"))
		return false
	}
	return true
}

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	if !s.chatsEnabled(w) {
		return
	}
	convs, err := s.chats.List(r.Context(), userID(r), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]chatView, 0, len(convs))
	for i := range convs {
		views = append(views, toChatView(&convs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": views})
}

func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	if !s.chatsEnabled(w) {
		return
	}
	var req struct {
		Corpus string `json:"corpus"`
		Title  string `json:"title"`
	}
	// An empty body is fine — it means "a new chat with the defaults".
	_ = json.NewDecoder(r.Body).Decode(&req)

	c, err := s.chats.Create(r.Context(), userID(r), string(parseCorpus(req.Corpus)), req.Title)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"chat": toChatView(c)})
}

func (s *Server) handleGetChat(w http.ResponseWriter, r *http.Request) {
	if !s.chatsEnabled(w) {
		return
	}
	c, err := s.chats.Get(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		writeChatError(w, err)
		return
	}
	msgs, err := s.chats.Messages(r.Context(), c.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": toChatView(c), "messages": msgs})
}

func (s *Server) handleRenameChat(w http.ResponseWriter, r *http.Request) {
	if !s.chatsEnabled(w) {
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("title is required"))
		return
	}
	id := r.PathValue("id")
	if err := s.chats.Rename(r.Context(), userID(r), id, req.Title); err != nil {
		writeChatError(w, err)
		return
	}
	c, err := s.chats.Get(r.Context(), userID(r), id)
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": toChatView(c)})
}

func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	if !s.chatsEnabled(w) {
		return
	}
	if err := s.chats.Delete(r.Context(), userID(r), r.PathValue("id")); err != nil {
		writeChatError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleChatMessage appends a question to a conversation and streams the
// sage's answer back as server-sent events:
//
//	meta   citations and the (possibly new) conversation title, sent as soon
//	       as retrieval finishes so the UI can render sources before the model
//	       has produced a single token
//	delta  a chunk of answer text
//	done   the stored assistant message id
//	error  a failure; any text already streamed is still saved
func (s *Server) handleChatMessage(w http.ResponseWriter, r *http.Request) {
	if !s.chatsEnabled(w) {
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

	conv, err := s.chats.Get(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		writeChatError(w, err)
		return
	}
	corpus := parseCorpus(conv.Corpus)

	// Persistence must outlive the request context: when the reader navigates
	// away mid-answer we still want the turn saved, so the conversation they
	// come back to matches what they saw.
	saveCtx := context.WithoutCancel(r.Context())

	// Read history before the new question lands so it isn't replayed twice.
	prior, err := s.chats.History(r.Context(), conv.ID, historyTurns)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	history := make([]llm.Turn, 0, len(prior))
	for _, m := range prior {
		history = append(history, llm.Turn{Role: m.Role, Content: m.Content})
	}

	if _, err := s.chats.AddMessage(saveCtx, conv.ID, chat.RoleUser, req.Question, nil, nil); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// The first question names the thread.
	title := conv.Title
	if strings.TrimSpace(title) == "" {
		title = chat.DeriveTitle(req.Question)
		if err := s.chats.Rename(saveCtx, userID(r), conv.ID, title); err != nil {
			log.Printf("chat: title %s: %v", conv.ID, err)
		}
	}

	sse := newSSEWriter(w)
	if !s.llm.Configured() {
		sse.send("error", map[string]any{
			"error": "The Q&A chat is not configured. Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL / ANTHROPIC_MODEL) to enable it.",
			"title": title,
		})
		return
	}

	g, err := s.ground(r.Context(), corpus, req.Question)
	if err != nil {
		sse.send("error", map[string]any{"error": fmt.Sprintf("retrieval failed: %v", err), "title": title})
		return
	}
	sse.send("meta", map[string]any{
		"sources":          g.sources,
		"cards":            g.cards,
		"unresolved_cards": g.unresolved,
		"title":            title,
	})

	ctx, cancel := context.WithTimeout(r.Context(), answerTimeout)
	defer cancel()

	answer, streamErr := s.llm.Stream(ctx, g.request(corpus, req.Question, history), func(text string) error {
		if err := ctx.Err(); err != nil {
			return err // reader is gone or we ran out of time; stop pulling tokens
		}
		return sse.send("delta", map[string]any{"text": text})
	})

	answer = strings.TrimSpace(answer)
	if answer == "" && streamErr != nil {
		sse.send("error", map[string]any{"error": fmt.Sprintf("the sage could not be reached: %v", streamErr)})
		return
	}

	sources, cardsJSON := marshalCitations(g)
	saved, err := s.chats.AddMessage(saveCtx, conv.ID, chat.RoleAssistant, answer, sources, cardsJSON)
	if err != nil {
		log.Printf("chat: save answer %s: %v", conv.ID, err)
	}
	if streamErr != nil {
		// Partial answer: it is stored and already on screen, so report the
		// interruption rather than pretending the turn completed.
		sse.send("error", map[string]any{"error": fmt.Sprintf("the answer was cut short: %v", streamErr)})
		return
	}
	var id int64
	if saved != nil {
		id = saved.ID
	}
	sse.send("done", map[string]any{"message_id": id, "title": title})
}

// marshalCitations encodes the citation payloads stored alongside an answer.
func marshalCitations(g grounded) (sources, cardsJSON json.RawMessage) {
	if len(g.sources) > 0 {
		if b, err := json.Marshal(g.sources); err == nil {
			sources = b
		}
	}
	if len(g.cards) > 0 {
		if b, err := json.Marshal(g.cards); err == nil {
			cardsJSON = b
		}
	}
	return sources, cardsJSON
}

func writeChatError(w http.ResponseWriter, err error) {
	if errors.Is(err, chat.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

// sseWriter frames server-sent events and flushes each one, so the browser
// paints tokens as they arrive instead of at the end of the response.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("content-type", "text/event-stream")
	h.Set("cache-control", "no-cache")
	h.Set("connection", "keep-alive")
	// Streaming is pointless behind a buffering reverse proxy.
	h.Set("x-accel-buffering", "no")
	w.WriteHeader(http.StatusOK)
	s := &sseWriter{w: w, rc: http.NewResponseController(w)}
	_ = s.rc.Flush()
	return s
}

func (s *sseWriter) send(event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, body); err != nil {
		return err
	}
	return s.rc.Flush()
}
