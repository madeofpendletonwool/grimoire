package gamesession

import (
	"context"
	"database/sql"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// openDB hands the test a private, fully migrated database — a file copy of
// the template testdb builds once per binary. The session tables exist only
// through the migration runner.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
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
