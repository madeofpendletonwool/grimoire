package share

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/index"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(db.DB())
	if err != nil {
		t.Fatalf("new share store: %v", err)
	}
	return s
}

func sampleSnapshot() Snapshot {
	return Snapshot{
		Question: "Does Deicide exile a god?",
		Answer:   "Yes. Deicide's second ability **exiles** the enchanted creature.",
		Corpus:   "mtg",
		Sources:  json.RawMessage(`[{"number":"702.2a","title":"Deathtouch","body":"Deathtouch is a static ability.","source":"MTG Comp Rules"}]`),
		Cards:    json.RawMessage(`[{"name":"Deicide"}]`),
		Entities: json.RawMessage(`[{"name":"Fireball","kind":"spell"}]`),
		Rulings:  json.RawMessage(`[{"card_name":"Deicide","source":"wotc","published_at":"2014-04-15","comment":"Deicide exiles the card."}]`),
	}
}

func TestCreateAndGet(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	want := sampleSnapshot()

	token, err := s.Create(ctx, "u1", "chat1", 7, want)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(token) < 22 {
		t.Errorf("token %q is shorter than 22 chars", token)
	}

	sh, snap, err := s.Get(ctx, token)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sh.UserID != "u1" || sh.ChatID != "chat1" || sh.MessageID != 7 {
		t.Errorf("share = %+v", sh)
	}
	if !sh.RevokedAt.IsZero() {
		t.Errorf("fresh share must not be revoked: %v", sh.RevokedAt)
	}
	if snap.Question != want.Question || snap.Answer != want.Answer || snap.Corpus != want.Corpus {
		t.Errorf("snapshot = %+v", snap)
	}
	if string(snap.Sources) != string(want.Sources) || string(snap.Cards) != string(want.Cards) || string(snap.Rulings) != string(want.Rulings) {
		t.Errorf("citation payloads changed: %+v", snap)
	}
}

func TestGetUnknownToken(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Get(context.Background(), "nosuchtoken0000000000000"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestTokensAreRandom(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		token, err := s.Create(ctx, "u1", "chat1", 1, sampleSnapshot())
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if seen[token] {
			t.Fatalf("token %q repeated", token)
		}
		seen[token] = true
		if len(token) < 22 {
			t.Errorf("token %q too short", token)
		}
		for _, c := range token {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				t.Errorf("token %q carries non-base64url rune %q", token, c)
			}
		}
	}
}

// Sharing the same message twice must not collide or panic — each share is
// its own revocable artifact.
func TestDuplicateCreateIsFine(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first, err := s.Create(ctx, "u1", "chat1", 3, sampleSnapshot())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := s.Create(ctx, "u1", "chat1", 3, sampleSnapshot())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first == second {
		t.Error("sharing twice must mint two tokens")
	}
}

func TestSnapshotIsImmutable(t *testing.T) {
	// The share must not reference the chat tables: deleting them outright
	// cannot break the link.
	s := newStore(t)
	ctx := context.Background()
	db := s.db

	token, err := s.Create(ctx, "u1", "chat1", 5, sampleSnapshot())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS chat_messages`); err != nil {
		t.Fatalf("drop chat_messages: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS conversations`); err != nil {
		t.Fatalf("drop conversations: %v", err)
	}

	_, snap, err := s.Get(ctx, token)
	if err != nil {
		t.Fatalf("get after chat deletion: %v", err)
	}
	if snap.Answer == "" || snap.Question == "" {
		t.Errorf("snapshot lost its content: %+v", snap)
	}
}

func TestRevokeOwnerOnly(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	token, err := s.Create(ctx, "u1", "chat1", 1, sampleSnapshot())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A different user's revoke is an indistinguishable miss.
	if err := s.Revoke(ctx, "u2", token); err != ErrNotFound {
		t.Errorf("stranger revoke err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Get(ctx, token); err != nil {
		t.Errorf("stranger revoke must not close the link: %v", err)
	}

	if err := s.Revoke(ctx, "u1", token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := s.Get(ctx, token); err != ErrRevoked {
		t.Errorf("get after revoke err = %v, want ErrRevoked", err)
	}

	// Revoking twice is still fine, and an unknown token is a miss.
	if err := s.Revoke(ctx, "u1", token); err != nil {
		t.Errorf("second revoke err = %v", err)
	}
	if err := s.Revoke(ctx, "u1", "nosuchtoken0000000000000"); err != ErrNotFound {
		t.Errorf("unknown revoke err = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.Create(ctx, "u1", "chat1", int64(i+1), sampleSnapshot()); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := s.Create(ctx, "u2", "chat2", 1, sampleSnapshot()); err != nil {
		t.Fatalf("stranger create: %v", err)
	}

	entries, err := s.List(ctx, "u1", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("list = %d entries, want 3 (own only)", len(entries))
	}
	if entries[0].Question != sampleSnapshot().Question {
		t.Errorf("entry carries no question: %+v", entries[0])
	}
	if !entries[0].CreatedAt.After(entries[2].CreatedAt) && !entries[0].CreatedAt.Equal(entries[2].CreatedAt) {
		t.Errorf("list is not newest-first: %v then %v", entries[0].CreatedAt, entries[2].CreatedAt)
	}
}

// The store must sit beside the other packages' tables in one database file
// without colliding with them — the per-package schema pattern.
func TestSharesAlongsideChatTables(t *testing.T) {
	db, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.DB().Exec(`CREATE TABLE IF NOT EXISTS sessions (token TEXT PRIMARY KEY, user_id TEXT NOT NULL)`); err != nil {
		t.Fatalf("seed auth table: %v", err)
	}
	s, err := New(db.DB())
	if err != nil {
		t.Fatalf("new alongside auth tables: %v", err)
	}
	ctx := context.Background()
	token, err := s.Create(ctx, "u1", "chat1", 1, sampleSnapshot())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := s.Get(ctx, token); err != nil {
		t.Fatalf("get: %v", err)
	}

	var n int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if n != 0 {
		t.Error("share store must not touch the sessions table")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for i := 0; i < 2; i++ {
		if _, err := New(db.DB()); err != nil {
			t.Fatalf("new #%d: %v", i, err)
		}
	}
	// A pre-entities snapshot table upgrades in place.
	if _, err := db.DB().Exec(`DROP TABLE share_snapshots`); err != nil {
		t.Fatalf("drop snapshots: %v", err)
	}
	if _, err := db.DB().Exec(`CREATE TABLE share_snapshots (
		token TEXT PRIMARY KEY REFERENCES shares(token) ON DELETE CASCADE,
		question TEXT NOT NULL DEFAULT '', answer TEXT NOT NULL DEFAULT '',
		corpus TEXT NOT NULL DEFAULT '', sources TEXT NOT NULL DEFAULT '',
		cards TEXT NOT NULL DEFAULT '', rulings TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("seed old snapshots: %v", err)
	}
	s, err := New(db.DB())
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	token, err := s.Create(context.Background(), "u1", "chat1", 1, sampleSnapshot())
	if err != nil {
		t.Fatalf("create after upgrade: %v", err)
	}
	_, snap, err := s.Get(context.Background(), token)
	if err != nil {
		t.Fatalf("get after upgrade: %v", err)
	}
	if string(snap.Entities) == "" {
		t.Error("entities column missing after upgrade")
	}
}
