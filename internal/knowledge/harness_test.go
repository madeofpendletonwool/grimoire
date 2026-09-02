package knowledge

import (
	"context"
	"database/sql"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// openDB hands the test a private, fully migrated database — a file copy of
// the template testdb builds once per binary. The knowledge tables exist
// only through the migration runner.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
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
