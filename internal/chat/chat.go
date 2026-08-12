// Package chat persists Q&A conversations so a session survives a reload and
// the sage can answer follow-up questions with the earlier turns in hand.
//
// The tables live in the same SQLite file as the search index. That is safe:
// index.Store.Reset only clears the docs tables, so rebuilding the rules index
// never drops chat history.
//
// Every conversation carries a user_id. Until authentication is wired up all
// rows share the AnonymousUser sentinel, which keeps the queries and the store
// API identical once real user IDs start arriving.
package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AnonymousUser is the owner recorded for conversations created before (or
// without) authentication. It is a real value rather than an empty string so
// "not logged in" is queryable rather than indistinguishable from unset.
const AnonymousUser = "anonymous"

// Roles for a stored message.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// ErrNotFound is returned when a conversation does not exist, or belongs to a
// different user. The two cases are deliberately indistinguishable so the API
// cannot be used to probe for other users' conversation IDs.
var ErrNotFound = errors.New("conversation not found")

// Conversation is a single saved chat thread. Corpus is fixed when the thread
// is created: the grounding differs per rule set, so a thread that switched
// corpora mid-way would carry incoherent history.
type Conversation struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	Corpus    string    `json:"corpus"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Message is one turn in a conversation. Sources and Cards hold the citation
// payloads shown under an assistant answer, stored as raw JSON so the chat
// store stays independent of the server's view types.
type Message struct {
	ID        int64           `json:"id"`
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	Sources   json.RawMessage `json:"sources,omitempty"`
	Cards     json.RawMessage `json:"cards,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// Store persists conversations and their messages.
type Store struct {
	db *sql.DB
}

// New builds a chat store on an existing database handle and ensures its
// schema exists.
func New(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("chat migrate: %w", err)
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS conversations (
	id         TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL DEFAULT 'anonymous',
	corpus     TEXT NOT NULL,
	title      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS conversations_owner ON conversations(user_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS chat_messages (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
	role            TEXT NOT NULL,
	content         TEXT NOT NULL,
	sources         TEXT NOT NULL DEFAULT '',
	cards           TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS chat_messages_thread ON chat_messages(conversation_id, id);
`

// Create opens a new conversation for a user against a fixed corpus.
func (s *Store) Create(ctx context.Context, userID, corpus, title string) (*Conversation, error) {
	now := time.Now().UTC()
	c := &Conversation{
		ID:        uuid.NewString(),
		UserID:    defaultUser(userID),
		Corpus:    corpus,
		Title:     strings.TrimSpace(title),
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations (id, user_id, corpus, title, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.UserID, c.Corpus, c.Title, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("create conversation: %w", err)
	}
	return c, nil
}

// List returns a user's conversations, most recently active first.
func (s *Store) List(ctx context.Context, userID string, limit int) ([]Conversation, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, corpus, title, created_at, updated_at
		   FROM conversations WHERE user_id = ?
		  ORDER BY updated_at DESC LIMIT ?`, defaultUser(userID), limit)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	out := []Conversation{}
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// Get returns a single conversation owned by userID.
func (s *Store) Get(ctx context.Context, userID, id string) (*Conversation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, corpus, title, created_at, updated_at
		   FROM conversations WHERE id = ? AND user_id = ?`, id, defaultUser(userID))
	c, err := scanConversation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// Rename sets a conversation's title.
func (s *Store) Rename(ctx context.Context, userID, id, title string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET title = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		strings.TrimSpace(title), time.Now().UTC().UnixMilli(), id, defaultUser(userID))
	return affected(res, err, "rename conversation")
}

// Delete removes a conversation; its messages cascade.
func (s *Store) Delete(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE id = ? AND user_id = ?`, id, defaultUser(userID))
	return affected(res, err, "delete conversation")
}

// AddMessage appends a turn and bumps the conversation's updated_at so the
// sidebar orders by real activity. Sources and cards may be nil.
func (s *Store) AddMessage(ctx context.Context, convID, role, content string, sources, cards json.RawMessage) (*Message, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_messages (conversation_id, role, content, sources, cards, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		convID, role, content, string(sources), string(cards), now.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("add message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET updated_at = ? WHERE id = ?`, now.UnixMilli(), convID); err != nil {
		return nil, fmt.Errorf("touch conversation: %w", err)
	}
	return &Message{ID: id, Role: role, Content: content, Sources: sources, Cards: cards, CreatedAt: now}, nil
}

// Messages returns a conversation's turns in order.
func (s *Store) Messages(ctx context.Context, convID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, role, content, sources, cards, created_at
		   FROM chat_messages WHERE conversation_id = ? ORDER BY id ASC`, convID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	out := []Message{}
	for rows.Next() {
		var (
			m             Message
			sources, card string
			created       int64
		)
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &sources, &card, &created); err != nil {
			return nil, err
		}
		m.Sources = rawOrNil(sources)
		m.Cards = rawOrNil(card)
		m.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// History returns the most recent maxTurns messages, oldest first, for use as
// LLM conversation context. Citation payloads are dropped — the model only
// needs the prose, and re-sending every prior answer's sources would crowd out
// the grounding for the current question.
func (s *Store) History(ctx context.Context, convID string, maxTurns int) ([]Message, error) {
	all, err := s.Messages(ctx, convID)
	if err != nil {
		return nil, err
	}
	if maxTurns > 0 && len(all) > maxTurns {
		all = all[len(all)-maxTurns:]
	}
	// A history that opens on an assistant turn is not a valid Messages API
	// exchange; drop the leading orphan the window may have created.
	if len(all) > 0 && all[0].Role != RoleUser {
		all = all[1:]
	}
	for i := range all {
		all[i].Sources = nil
		all[i].Cards = nil
	}
	return all, nil
}

// DeriveTitle turns the opening question into a sidebar label, trimmed at a
// word boundary so titles don't end mid-word.
func DeriveTitle(question string) string {
	t := strings.Join(strings.Fields(question), " ")
	const max = 58
	if len([]rune(t)) <= max {
		if t == "" {
			return "New chat"
		}
		return t
	}
	r := []rune(t)[:max]
	if i := strings.LastIndex(string(r), " "); i > max/2 {
		return string(r)[:i] + "…"
	}
	return string(r) + "…"
}

// rowScanner covers both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanConversation(r rowScanner) (*Conversation, error) {
	var (
		c                Conversation
		created, updated int64
	)
	if err := r.Scan(&c.ID, &c.UserID, &c.Corpus, &c.Title, &created, &updated); err != nil {
		return nil, err
	}
	c.CreatedAt = time.UnixMilli(created).UTC()
	c.UpdatedAt = time.UnixMilli(updated).UTC()
	return &c, nil
}

func affected(res sql.Result, err error, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func defaultUser(userID string) string {
	if strings.TrimSpace(userID) == "" {
		return AnonymousUser
	}
	return userID
}

func rawOrNil(s string) json.RawMessage {
	if s == "" || s == "null" {
		return nil
	}
	return json.RawMessage(s)
}
