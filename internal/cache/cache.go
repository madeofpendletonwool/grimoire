// Package cache stores grounded Q&A answers so an identical (or
// grounding-equivalent) question returns instantly without a second LLM call.
//
// The cache key is derived from the corpus, the normalized question, and the
// sorted set of retrieved grounding source identifiers. Because the source set
// is part of the key, a rules reindex (which changes what retrieval returns)
// changes the key naturally — a stale answer cannot survive a reindex.
//
// The answer_cache table is a sibling of the rules docs and chat history in the
// same SQLite file. index.Store.Reset only clears the docs tables, so rebuilding
// the rules index never drops cached answers (they simply stop hitting once the
// grounding shifts).
package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultTTL is how long a cached answer is considered fresh when no explicit
// TTL is configured. Roughly a rules-set release cadence.
const DefaultTTL = 7 * 24 * time.Hour

// Entry is a cached answer plus the citations shown alongside it.
type Entry struct {
	Answer    string
	Sources   json.RawMessage
	Cards     json.RawMessage
	Entities  json.RawMessage
	Rulings   json.RawMessage
	CreatedAt time.Time
}

// Store persists cached answers in the shared SQLite database.
type Store struct {
	db  *sql.DB
	ttl time.Duration
}

// New builds a cache store on an existing database handle and ensures its
// schema exists. ttl bounds how long an entry is fresh; ttl <= 0 falls back to
// DefaultTTL. The store is safe to share across the index and chat stores
// because answer_cache is a sibling table Reset leaves alone.
func New(db *sql.DB, ttl time.Duration) (*Store, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	s := &Store{db: db, ttl: ttl}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("cache migrate: %w", err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("cache migrate: %w", err)
	}
	return s, nil
}

// TTL reports the configured freshness window.
func (s *Store) TTL() time.Duration { return s.ttl }

const schema = `
CREATE TABLE IF NOT EXISTS answer_cache (
	key        TEXT PRIMARY KEY,
	corpus     TEXT NOT NULL,
	answer     TEXT NOT NULL,
	sources    TEXT NOT NULL DEFAULT '',
	cards      TEXT NOT NULL DEFAULT '',
	entities   TEXT NOT NULL DEFAULT '',
	rulings    TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS answer_cache_corpus ON answer_cache(corpus, created_at);
`

// migrate applies additive column additions for installs whose answer_cache
// table predates a later feature. Each step is idempotent: a fresh database
// already has the column from schema, and an upgraded one gets it added
// in-place without losing cached answers.
func migrate(db *sql.DB) error {
	for _, col := range []string{"rulings", "entities"} {
		if err := ensureColumn(db, "answer_cache", col); err != nil {
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

// Put stores an answer under key, refreshing any existing entry. corpus is
// recorded for observability and per-corpus sweeps; it is already part of the
// key, so it carries no correctness load here. sources, cards, entities, and
// rulings may be nil.
func (s *Store) Put(ctx context.Context, key, corpus, answer string, sources, cards, entities, rulings json.RawMessage) error {
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO answer_cache (key, corpus, answer, sources, cards, entities, rulings, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		key, corpus, answer, string(sources), string(cards), string(entities), string(rulings), now)
	if err != nil {
		return fmt.Errorf("cache put: %w", err)
	}
	return nil
}

// Get returns a fresh entry for key, or (nil, nil) on a miss or expiry. An
// expired entry is treated as a miss rather than deleted: the table is keyed by
// a grounding hash, so its cardinality is bounded by the distinct questions a
// corpus can produce, and a lazy sweep adds write load for no real gain.
func (s *Store) Get(ctx context.Context, key string) (*Entry, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT answer, sources, cards, entities, rulings, created_at FROM answer_cache WHERE key = ?`, key)
	var (
		e        Entry
		sources  string
		cards    string
		entities string
		rulings  string
		created  int64
	)
	if err := row.Scan(&e.Answer, &sources, &cards, &entities, &rulings, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cache get: %w", err)
	}
	e.Sources = rawOrNil(sources)
	e.Cards = rawOrNil(cards)
	e.Entities = rawOrNil(entities)
	e.Rulings = rawOrNil(rulings)
	e.CreatedAt = time.UnixMilli(created).UTC()
	if time.Since(e.CreatedAt) > s.ttl {
		return nil, nil
	}
	return &e, nil
}

// Key derives the cache key for a grounded question. It folds together the
// corpus, the normalized question, and the sorted set of retrieved grounding
// source identifiers — so an answer is reused only when a later question lands
// on the same rules, and a reindex that moves the grounding invalidates it.
// sourceIDs may arrive in any order; sorting makes the key order-independent.
func Key(corpus, question string, sourceIDs []string) string {
	ids := make([]string, 0, len(sourceIDs))
	for _, id := range sourceIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	h := sha256.New()
	fmt.Fprintln(h, corpus)
	fmt.Fprintln(h, normalizeQuestion(question))
	fmt.Fprintln(h, strings.Join(ids, "\n"))
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeQuestion collapses a question to a canonical form for keying:
// lowercased with runs of whitespace squeezed to single spaces. Two questions
// that differ only in case or spacing share an entry; near-duplicate phrasings
// (semantic dedup) are deliberately out of scope.
func normalizeQuestion(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), " "))
}

func rawOrNil(s string) json.RawMessage {
	if s == "" || s == "null" {
		return nil
	}
	return json.RawMessage(s)
}
