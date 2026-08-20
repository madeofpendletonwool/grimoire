// Package share turns a grounded Q&A into a public, read-only page.
//
// A share is a snapshot, not a reference: the question, the answer, and the
// citation payloads are copied at share time, so a chat that is later edited
// or deleted leaves already-shared pages standing exactly as they were
// shared. The token is the whole access model — 128 bits from crypto/rand in
// base64url, unguessable and carrying no sequential ids — and the public page
// asks for nothing but it.
//
// The tables live in the same SQLite file as the search index and the chat
// history. That is safe: index.Store.Reset only clears the docs tables, and
// nothing here references the chat tables, so deleting a conversation never
// touches a share.
package share

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors callers are expected to branch on. Get returns ErrNotFound for an
// unknown token and ErrRevoked for a revoked one, so the public page can
// answer 404 and 410 respectively.
var (
	ErrNotFound = errors.New("share not found")
	ErrRevoked  = errors.New("share has been revoked")
)

// Share is one shared link: the token that names it, who minted it, the
// message it was minted from, and whether it still stands.
type Share struct {
	Token     string
	UserID    string
	ChatID    string
	MessageID int64
	CreatedAt time.Time
	RevokedAt time.Time // zero while the link stands
}

// Snapshot is the copied Q&A a public page renders. The citation payloads are
// stored as raw JSON in the same shapes the API serves, so the store stays
// independent of the server's view types.
type Snapshot struct {
	Question string
	Answer   string
	Corpus   string
	Sources  json.RawMessage
	Cards    json.RawMessage
	Entities json.RawMessage
	Rulings  json.RawMessage
}

// ListEntry is a share joined with enough of its snapshot for the owner's
// Settings list: the question names it, the dates tell its story.
type ListEntry struct {
	Token     string
	Question  string
	Corpus    string
	CreatedAt time.Time
	RevokedAt time.Time
}

// Store persists shares and their snapshots.
type Store struct {
	db *sql.DB
}

// New builds a share store on an existing database handle and ensures its
// schema exists. A sibling of the chat and auth tables; an index rebuild
// never touches them.
func New(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("share migrate: %w", err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("share migrate: %w", err)
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS shares (
	token      TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL,
	chat_id    TEXT NOT NULL,
	message_id INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	revoked_at INTEGER
);
CREATE INDEX IF NOT EXISTS shares_owner ON shares(user_id, created_at DESC);
CREATE TABLE IF NOT EXISTS share_snapshots (
	token      TEXT PRIMARY KEY REFERENCES shares(token) ON DELETE CASCADE,
	question   TEXT NOT NULL DEFAULT '',
	answer     TEXT NOT NULL DEFAULT '',
	corpus     TEXT NOT NULL DEFAULT '',
	sources    TEXT NOT NULL DEFAULT '',
	cards      TEXT NOT NULL DEFAULT '',
	entities   TEXT NOT NULL DEFAULT '',
	rulings    TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
`

// migrate applies additive column additions for installs whose tables predate
// a later feature (an expiry column is the obvious candidate). Each step is
// idempotent: a fresh database already has the column from schema, and an
// upgraded one gets it added in-place without losing shares.
func migrate(db *sql.DB) error {
	for _, col := range []string{"entities"} {
		if err := ensureColumn(db, "share_snapshots", col); err != nil {
			return err
		}
	}
	return nil
}

// ensureColumn adds a nullable TEXT column to a table when it is not already
// present. SQLite cannot express add-column-if-not-exists, so the presence is
// checked against PRAGMA table_info first.
func ensureColumn(db *sql.DB, table, column string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			return rows.Close()
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s TEXT NOT NULL DEFAULT ''", table, column))
	if err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

// newToken mints a share token: 16 bytes from crypto/rand, base64url without
// padding — 22 characters, URL-safe, no sequential ids for anyone to walk.
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Create snapshots a Q&A under a fresh token and returns the token. Sharing
// the same message twice mints two links on purpose — each share is its own
// revocable artifact. The token's uniqueness constraint is the only one on
// these tables; the astronomically unlikely collision just retries.
func (s *Store) Create(ctx context.Context, userID, chatID string, messageID int64, snap Snapshot) (string, error) {
	now := time.Now().UTC().UnixMilli()
	var token string
	for attempt := 0; ; attempt++ {
		t, err := newToken()
		if err != nil {
			return "", err
		}
		token = t
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO shares (token, user_id, chat_id, message_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			token, userID, chatID, messageID, now)
		if err == nil {
			break
		}
		if attempt < 2 && strings.Contains(strings.ToLower(err.Error()), "unique") {
			continue
		}
		return "", fmt.Errorf("create share: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO share_snapshots (token, question, answer, corpus, sources, cards, entities, rulings, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		token, snap.Question, snap.Answer, snap.Corpus,
		string(snap.Sources), string(snap.Cards), string(snap.Entities), string(snap.Rulings), now); err != nil {
		return "", fmt.Errorf("create share snapshot: %w", err)
	}
	return token, nil
}

// Get returns the share and its snapshot for a token. A revoked share is
// ErrRevoked rather than a snapshot, so the public page can say "gone"
// instead of pretending the link never existed.
func (s *Store) Get(ctx context.Context, token string) (*Share, *Snapshot, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT token, user_id, chat_id, message_id, created_at, revoked_at
		   FROM shares WHERE token = ?`, token)
	sh, err := scanShare(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get share: %w", err)
	}
	if !sh.RevokedAt.IsZero() {
		return nil, nil, ErrRevoked
	}

	row = s.db.QueryRowContext(ctx,
		`SELECT question, answer, corpus, sources, cards, entities, rulings FROM share_snapshots WHERE token = ?`, token)
	var (
		snap              Snapshot
		sources, cards    string
		entities, rulings string
	)
	if err := row.Scan(&snap.Question, &snap.Answer, &snap.Corpus, &sources, &cards, &entities, &rulings); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("get share snapshot: %w", err)
	}
	snap.Sources = rawOrNil(sources)
	snap.Cards = rawOrNil(cards)
	snap.Entities = rawOrNil(entities)
	snap.Rulings = rawOrNil(rulings)
	return sh, &snap, nil
}

// Revoke closes a link. Only the owner can: an unknown token and someone
// else's token are the same ErrNotFound, so the API cannot be used to probe
// for other users' shares. Revoking twice succeeds — the caller wanted it
// gone, and it is.
func (s *Store) Revoke(ctx context.Context, userID, token string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shares SET revoked_at = ? WHERE token = ? AND user_id = ? AND revoked_at IS NULL`,
		time.Now().UTC().UnixMilli(), token, userID)
	if err != nil {
		return fmt.Errorf("revoke share: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		// Already-revoked-and-owned is a success, not a miss.
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM shares WHERE token = ? AND user_id = ? AND revoked_at IS NOT NULL`, token, userID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// List returns a user's shares, newest first, with each snapshot's question
// so the Settings list can name the link.
func (s *Store) List(ctx context.Context, userID string, limit int) ([]ListEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.token, COALESCE(sn.question, ''), COALESCE(sn.corpus, ''), s.created_at, s.revoked_at
		   FROM shares s LEFT JOIN share_snapshots sn ON sn.token = s.token
		  WHERE s.user_id = ?
		  ORDER BY s.created_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list shares: %w", err)
	}
	defer rows.Close()

	out := []ListEntry{}
	for rows.Next() {
		var (
			e       ListEntry
			created int64
			revoked sql.NullInt64
		)
		if err := rows.Scan(&e.Token, &e.Question, &e.Corpus, &created, &revoked); err != nil {
			return nil, err
		}
		e.CreatedAt = time.UnixMilli(created).UTC()
		if revoked.Valid {
			e.RevokedAt = time.UnixMilli(revoked.Int64).UTC()
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// rowScanner covers both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanShare(r rowScanner) (*Share, error) {
	var (
		s       Share
		created int64
		revoked sql.NullInt64
	)
	if err := r.Scan(&s.Token, &s.UserID, &s.ChatID, &s.MessageID, &created, &revoked); err != nil {
		return nil, err
	}
	s.CreatedAt = time.UnixMilli(created).UTC()
	if revoked.Valid {
		s.RevokedAt = time.UnixMilli(revoked.Int64).UTC()
	}
	return &s, nil
}

func rawOrNil(s string) json.RawMessage {
	if s == "" || s == "null" {
		return nil
	}
	return json.RawMessage(s)
}
