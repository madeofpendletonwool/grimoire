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

	"github.com/madeofpendletonwool/grimoire/internal/cache"
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

	if _, err := s.chats.AddMessage(saveCtx, conv.ID, chat.RoleUser, req.Question, nil, nil, nil, nil); err != nil {
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

	// A cache hit skips the LLM call but still streams through the same meta →
	// delta → done frames the UI expects, so a cached answer feels identical to
	// a freshly generated one (only faster). The user's question was already
	// appended above; the cached answer is appended here so the saved thread
	// matches what the reader saw. ?nocache forces a fresh generation.
	if s.answers != nil && !r.URL.Query().Has("nocache") {
		key := cache.Key(string(corpus), req.Question, sourceIDs(g.sources))
		if hit, err := s.answers.Get(r.Context(), key); err != nil {
			log.Printf("answer cache get: %v", err)
		} else if hit != nil {
			sse.send("meta", map[string]any{
				"sources":          decodeSources(hit.Sources),
				"cards":            decodeCards(hit.Cards),
				"entities":         decodeEntities(hit.Entities),
				"rulings":          decodeRulings(hit.Rulings),
				"unresolved_cards": g.unresolved,
				"title":            title,
				"cached":           true,
			})
			sse.send("delta", map[string]any{"text": hit.Answer})
			saved, saveErr := s.chats.AddMessage(saveCtx, conv.ID, chat.RoleAssistant, hit.Answer, hit.Sources, hit.Cards, hit.Entities, hit.Rulings)
			if saveErr != nil {
				log.Printf("chat: save cached answer %s: %v", conv.ID, saveErr)
			}
			var id int64
			if saved != nil {
				id = saved.ID
			}
			sse.send("done", map[string]any{"message_id": id, "title": title, "cached": true})
			return
		}
	}

	sse.send("meta", map[string]any{
		"sources":          g.sources,
		"cards":            g.cards,
		"entities":         g.entities,
		"rulings":          g.rulings,
		"unresolved_cards": g.unresolved,
		"title":            title,
		"cached":           false,
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

	sources, cardsJSON, entitiesJSON, rulingsJSON := marshalCitations(g)
	// A complete answer is cached for grounding-equivalent repeats; a partial /
	// errored one is not, so a truncated response never comes back "instant"
	// for the next person — they can regenerate it with ?nocache instead.
	if s.answers != nil && streamErr == nil && answer != "" {
		key := cache.Key(string(corpus), req.Question, sourceIDs(g.sources))
		if err := s.answers.Put(saveCtx, key, string(corpus), answer, sources, cardsJSON, entitiesJSON, rulingsJSON); err != nil {
			log.Printf("answer cache put: %v", err)
		}
	}
	saved, err := s.chats.AddMessage(saveCtx, conv.ID, chat.RoleAssistant, answer, sources, cardsJSON, entitiesJSON, rulingsJSON)
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
	sse.send("done", map[string]any{"message_id": id, "title": title, "cached": false})
}

// marshalCitations encodes the citation payloads stored alongside an answer.
func marshalCitations(g grounded) (sources, cardsJSON, entitiesJSON, rulingsJSON json.RawMessage) {
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
	if len(g.entities) > 0 {
		if b, err := json.Marshal(g.entities); err == nil {
			entitiesJSON = b
		}
	}
	if len(g.rulings) > 0 {
		if b, err := json.Marshal(g.rulings); err == nil {
			rulingsJSON = b
		}
	}
	return sources, cardsJSON, entitiesJSON, rulingsJSON
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
