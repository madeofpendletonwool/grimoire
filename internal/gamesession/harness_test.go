package gamesession

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	_ "modernc.org/sqlite" // same pure-Go driver the app opens the real file with
)

// openDB opens a scratch database the way the app does and applies the
// migrations — the session tables exist only through the runner.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := migrate.Up(db); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return db
}

// seeded opens a store over a campaign with an owner, the way every test
// here starts: one user, one campaign, and an empty session layer.
func seeded(t *testing.T) (*Store, string) {
	t.Helper()
	db := openDB(t)
	if _, err := db.Exec(
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('keeper', 'keeper', 'x', 0, 0)`); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	var cid string
	if err := db.QueryRow(
		`INSERT INTO campaigns (id, owner_id, name, created_at, updated_at)
		 VALUES ('camp', 'keeper', 'The Withering Kingdom', 0, 0) RETURNING id`).Scan(&cid); err != nil {
		t.Fatalf("insert campaign: %v", err)
	}
	s, err := New(db)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	return s, cid
}

// addSession is CreateSession with the error handling a test wants.
func addSession(t *testing.T, s *Store, campaignID, name string) *Session {
	t.Helper()
	ses, err := s.CreateSession(context.Background(), campaignID, name)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return ses
}

// addSource is AddSource with the error handling a test wants.
func addSource(t *testing.T, s *Store, sessionID, kind, content string) *Source {
	t.Helper()
	src, err := s.AddSource(context.Background(), sessionID, kind, "DM", "", content, nil)
	if err != nil {
		t.Fatalf("add source: %v", err)
	}
	return src
}

// addEvent is AddEvent with the error handling a test wants.
func addEvent(t *testing.T, s *Store, sessionID, kind, summary, detail string, payload map[string]any) *Event {
	t.Helper()
	ev, err := s.AddEvent(context.Background(), sessionID, kind, summary, detail, payload)
	if err != nil {
		t.Fatalf("add event: %v", err)
	}
	return ev
}
