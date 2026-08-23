package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	_ "modernc.org/sqlite" // same pure-Go driver the app opens the real file with
)

// openDB opens a scratch database the way the app does and applies the
// migrations — the knowledge tables exist only through the runner.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "knowledge.db")
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

// seeded builds the full fixture stack: campaign graph plus knowledge rows.
func seeded(t *testing.T) (*Store, *campaign.Fixture, *KnowledgeFixture) {
	t.Helper()
	db := openDB(t)
	if _, err := db.Exec(
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('keeper', 'keeper', 'x', 0, 0)`); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	fx, err := campaign.Seed(context.Background(), db, "keeper", "")
	if err != nil {
		t.Fatalf("campaign seed: %v", err)
	}
	kx, err := Seed(context.Background(), db, fx, "keeper")
	if err != nil {
		t.Fatalf("knowledge seed: %v", err)
	}
	s, err := New(db)
	if err != nil {
		t.Fatalf("knowledge store: %v", err)
	}
	return s, fx, kx
}

// userIDs mints user rows the membership tables foreign-key.
func userIDs(t *testing.T, db *sql.DB, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := db.Exec(
			`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES (?, ?, 'x', 0, 0)`,
			id, "user-"+id); err != nil {
			t.Fatalf("insert user %s: %v", id, err)
		}
	}
}
