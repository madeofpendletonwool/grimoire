// Package index wraps a SQLite + FTS5 store for rules documents.
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/data"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (FTS5 enabled)
)

// Store is a searchable FTS5-backed document store.
type Store struct {
	db *sql.DB
}

// Result is a single search hit.
type Result struct {
	Number string
	Title  string
	Body   string
	Source string
	Score  float64
}

// Open opens (or creates) the SQLite index at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite serial writes
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS corpus_meta (
	corpus        TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	version       TEXT NOT NULL,
	source_url    TEXT NOT NULL,
	record_count  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS index_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(
	corpus UNINDEXED,
	number,
	title,
	body,
	source UNINDEXED,
	tokenize = 'porter unicode61 remove_diacritics 2'
);
`

// Reset drops and rebuilds the docs tables (used on reindex).
func (s *Store) Reset() error {
	_, err := s.db.Exec(`DELETE FROM docs; DELETE FROM corpus_meta;`)
	return err
}

// Index loads a full dataset into the store, replacing prior contents.
func (s *Store) Index(ctx context.Context, ds *data.Dataset) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM docs; DELETE FROM corpus_meta;`); err != nil {
		return fmt.Errorf("clear docs: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO docs(corpus, number, title, body, source) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	for _, r := range ds.Records {
		if strings.TrimSpace(r.Body) == "" && strings.TrimSpace(r.Title) == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, string(r.Corpus), r.Number, r.Title, r.Body, r.Source); err != nil {
			return fmt.Errorf("insert doc: %w", err)
		}
	}

	metaStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO corpus_meta(corpus, name, version, source_url, record_count) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer metaStmt.Close()
	for c, m := range ds.Meta {
		if _, err := metaStmt.ExecContext(ctx, string(c), m.Name, m.Version, m.SourceURL, m.RecordCount); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO index_meta(key, value) VALUES('built_at', ?)`, nowRFC3339()); err != nil {
		return err
	}

	return tx.Commit()
}

// ruleNumberRe detects a direct rule-number reference like "205.1a".
var ruleNumberRe = regexp.MustCompile(`^\d{1,3}(?:\.\d+)+[a-z]?$`)

// Search runs a full-text query against a corpus. If the query is a bare rule
// number, it performs an exact lookup first.
func (s *Store) Search(ctx context.Context, corpus data.Corpus, query string, limit int) ([]Result, error) {
	q := strings.TrimSpace(query)
	if limit <= 0 {
		limit = 20
	}

	// Direct rule-number lookup (MTG).
	if corpus == data.CorpusMTG && q != "" && ruleNumberRe.MatchString(q) {
		if hits, err := s.lookupNumber(ctx, corpus, q); err == nil && len(hits) > 0 {
			return hits, nil
		}
	}

	matches, err := toFTSQuery(q)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, ftsSearchSQL, matches, corpus, limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Number, &r.Title, &r.Body, &r.Source, &r.Score); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const ftsSearchSQL = `
SELECT number, title, body, source, rank AS score
FROM docs
WHERE docs MATCH ? AND corpus = ?
ORDER BY rank
LIMIT ?;
`

func (s *Store) lookupNumber(ctx context.Context, corpus data.Corpus, number string) ([]Result, error) {
	// FTS5 supports a column-scoped phrase match on the number column.
	q := fmt.Sprintf("number:%q", number)
	rows, err := s.db.QueryContext(ctx, ftsSearchSQL, q, corpus, 30)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Number, &r.Title, &r.Body, &r.Source, &r.Score); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CorpusMeta returns stored metadata for a corpus.
func (s *Store) CorpusMeta(ctx context.Context) (map[data.Corpus]data.CorpusMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT corpus, name, version, source_url, record_count FROM corpus_meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[data.Corpus]data.CorpusMeta{}
	for rows.Next() {
		var c, name, version, url string
		var count int
		if err := rows.Scan(&c, &name, &version, &url, &count); err != nil {
			return nil, err
		}
		out[data.Corpus(c)] = data.CorpusMeta{
			Name: name, Version: version, SourceURL: url, RecordCount: count,
		}
	}
	return out, rows.Err()
}

// Indexed reports whether the store has any documents.
func (s *Store) Indexed(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM docs`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ErrEmptyQuery is returned when a search query sanitizes to nothing.
var ErrEmptyQuery = errors.New("empty query")

// toFTSQuery converts a free-text query into a safe FTS5 MATCH expression.
// Each whitespace-separated token becomes a prefix phrase; tokens are AND-ed.
func toFTSQuery(q string) (string, error) {
	var parts []string
	for _, tok := range strings.Fields(q) {
		tok = strings.Trim(tok, "\"'*()")
		if tok == "" {
			continue
		}
		// double internal quotes by SQL string literal escaping via %q
		parts = append(parts, fmt.Sprintf("%q*", tok))
	}
	if len(parts) == 0 {
		return "", ErrEmptyQuery
	}
	return strings.Join(parts, " "), nil
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
