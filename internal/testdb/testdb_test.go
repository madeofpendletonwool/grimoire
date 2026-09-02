package testdb

import (
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// TestOpenIsFullyMigrated pins the contract every harness leans on: a
// database from Open is stamped at the latest migration, so a subsequent
// migrate.Up is a no-op rather than a replay.
func TestOpenIsFullyMigrated(t *testing.T) {
	db := Open(t)
	v, err := migrate.Version(db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	want, err := migrate.Versions(migrate.FS)
	if err != nil {
		t.Fatalf("known migrations: %v", err)
	}
	if got, latest := v, want[len(want)-1].Version; got != latest {
		t.Fatalf("template stamped at %d, want %d", got, latest)
	}
	if err := migrate.Up(db); err != nil {
		t.Fatalf("up over template: %v", err)
	}
}

// TestCopiesAreIsolated proves the template is copied, not shared: rows
// written through one handle are invisible to the next.
func TestCopiesAreIsolated(t *testing.T) {
	a := Open(t)
	if _, err := a.Exec(
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('x', 'x', 'x', 0, 0)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	b := Open(t)
	var n int
	if err := b.QueryRow(`SELECT COUNT(*) FROM users WHERE id = 'x'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("row leaked between copies")
	}
}
