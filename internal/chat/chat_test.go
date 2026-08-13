package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	s, err := New(db)
	if err != nil {
		t.Fatalf("new chat store: %v", err)
	}
	return s
}

func TestCreateAndGet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	c, err := s.Create(ctx, "", "mtg", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.ID == "" {
		t.Fatal("expected a generated id")
	}
	if c.UserID != AnonymousUser {
		t.Errorf("empty user should fall back to %q, got %q", AnonymousUser, c.UserID)
	}

	got, err := s.Get(ctx, "", c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Corpus != "mtg" {
		t.Errorf("corpus = %q, want mtg", got.Corpus)
	}
}

func TestGetScopedToOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	c, err := s.Create(ctx, "alice", "mtg", "hers")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Get(ctx, "bob", c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user reading a conversation: err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "bob", c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user deleting a conversation: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, "alice", c.ID); err != nil {
		t.Errorf("owner should still read their own conversation: %v", err)
	}
}

func TestListOrdersByRecentActivity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, _ := s.Create(ctx, "u", "mtg", "first")
	second, _ := s.Create(ctx, "u", "mtg", "second")

	// A new message in the older thread should float it to the top.
	if _, err := s.AddMessage(ctx, first.ID, RoleUser, "hello", nil, nil, nil, nil); err != nil {
		t.Fatalf("add message: %v", err)
	}

	list, err := s.List(ctx, "u", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d conversations, want 2", len(list))
	}
	if list[0].ID != first.ID {
		t.Errorf("most recently active should sort first: got %q, want %q", list[0].ID, first.ID)
	}
	if list[1].ID != second.ID {
		t.Errorf("second entry = %q, want %q", list[1].ID, second.ID)
	}
}

func TestMessagesRoundTripCitations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c, _ := s.Create(ctx, "u", "mtg", "t")

	sources := json.RawMessage(`[{"number":"702.2"}]`)
	if _, err := s.AddMessage(ctx, c.ID, RoleUser, "does deathtouch work?", nil, nil, nil, nil); err != nil {
		t.Fatalf("add user: %v", err)
	}
	if _, err := s.AddMessage(ctx, c.ID, RoleAssistant, "yes", sources, nil, nil, nil); err != nil {
		t.Fatalf("add assistant: %v", err)
	}

	msgs, err := s.Messages(ctx, c.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != RoleUser || msgs[1].Role != RoleAssistant {
		t.Errorf("messages out of order: %q then %q", msgs[0].Role, msgs[1].Role)
	}
	if string(msgs[1].Sources) != string(sources) {
		t.Errorf("sources = %s, want %s", msgs[1].Sources, sources)
	}
	if msgs[0].Sources != nil {
		t.Errorf("absent citations should decode to nil, got %s", msgs[0].Sources)
	}
}

func TestMessagesRoundTripRulings(t *testing.T) {
	// Rulings are a third citation payload alongside sources and cards; they
	// must survive the store round-trip so a reopened chat still shows them.
	s := testStore(t)
	ctx := context.Background()
	c, _ := s.Create(ctx, "u", "mtg", "t")

	rulings := json.RawMessage(`[{"card_name":"Derevi, Empyrial Tactician","source":"wotc","published_at":"2020-11-10","comment":"You can activate only in the command zone."}]`)
	if _, err := s.AddMessage(ctx, c.ID, RoleAssistant, "ruling applies", nil, nil, nil, rulings); err != nil {
		t.Fatalf("add: %v", err)
	}

	msgs, err := s.Messages(ctx, c.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if string(msgs[0].Rulings) != string(rulings) {
		t.Errorf("rulings = %s, want %s", msgs[0].Rulings, rulings)
	}
}

func TestMigrateAddsRulingsColumn(t *testing.T) {
	// An install upgraded from before the rulings layer has a chat_messages
	// table without the rulings column. Opening the store on such a database
	// must add the column in place without losing the existing history.
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	// Build the pre-rulings schema by hand, then hand-upgrade via New.
	preRulingsSchema := `
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
CREATE INDEX IF NOT EXISTS chat_messages_thread ON chat_messages(conversation_id, id);`
	if _, err := db.Exec(preRulingsSchema); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO conversations (id, user_id, corpus, title, created_at, updated_at) VALUES ('c1','u','mtg','t',1,1)`); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO chat_messages (conversation_id, role, content, sources, cards, created_at) VALUES ('c1','user','old question','','',1)`); err != nil {
		t.Fatalf("seed old message: %v", err)
	}

	// The upgrade path: New() runs the migration, adding the rulings column.
	store, err := New(db)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}

	// Old history survives, and a new message carrying rulings round-trips.
	msgs, err := store.Messages(context.Background(), "c1")
	if err != nil {
		t.Fatalf("messages after upgrade: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "old question" {
		t.Fatalf("upgrade lost history: %+v", msgs)
	}
	rulings := json.RawMessage(`[{"card_name":"Bolt","source":"wotc","published_at":"2020-01-01","comment":"x"}]`)
	if _, err := store.AddMessage(context.Background(), "c1", RoleAssistant, "with rulings", nil, nil, nil, rulings); err != nil {
		t.Fatalf("add after upgrade: %v", err)
	}
	msgs, _ = store.Messages(context.Background(), "c1")
	if string(msgs[1].Rulings) != string(rulings) {
		t.Errorf("rulings did not round-trip after migration: %s", msgs[1].Rulings)
	}
}

func TestDeleteCascadesMessages(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c, _ := s.Create(ctx, "u", "mtg", "t")
	if _, err := s.AddMessage(ctx, c.ID, RoleUser, "hi", nil, nil, nil, nil); err != nil {
		t.Fatalf("add: %v", err)
	}

	if err := s.Delete(ctx, "u", c.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	msgs, err := s.Messages(ctx, c.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("messages should cascade on delete, got %d", len(msgs))
	}
}

func TestHistoryWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c, _ := s.Create(ctx, "u", "mtg", "t")

	for i := 0; i < 4; i++ {
		if _, err := s.AddMessage(ctx, c.ID, RoleUser, "q", nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddMessage(ctx, c.ID, RoleAssistant, "a", json.RawMessage(`[1]`), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	// A window of 3 would start on an assistant turn, which is not a valid
	// exchange opener; the store trims the orphan.
	got, err := s.History(ctx, c.ID, 3)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2 after trimming the leading assistant turn", len(got))
	}
	if got[0].Role != RoleUser {
		t.Errorf("history must open on a user turn, got %q", got[0].Role)
	}
	for _, m := range got {
		if m.Sources != nil {
			t.Errorf("history should drop citation payloads, got %s", m.Sources)
		}
	}
}

func TestHistoryUnboundedReturnsAll(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c, _ := s.Create(ctx, "u", "mtg", "t")
	s.AddMessage(ctx, c.ID, RoleUser, "q", nil, nil, nil, nil)
	s.AddMessage(ctx, c.ID, RoleAssistant, "a", nil, nil, nil, nil)

	got, err := s.History(ctx, c.ID, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d, want both messages", len(got))
	}
}

func TestRename(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c, _ := s.Create(ctx, "u", "mtg", "")

	if err := s.Rename(ctx, "u", c.ID, "  Deathtouch and trample  "); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, _ := s.Get(ctx, "u", c.ID)
	if got.Title != "Deathtouch and trample" {
		t.Errorf("title = %q, want trimmed", got.Title)
	}
	if err := s.Rename(ctx, "u", "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("renaming a missing conversation: err = %v, want ErrNotFound", err)
	}
}

func TestReassignOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Two threads from the pre-authentication app, and one that already
	// belongs to somebody — the migration must not touch the latter.
	old1, _ := s.Create(ctx, AnonymousUser, "mtg", "before accounts")
	old2, _ := s.Create(ctx, AnonymousUser, "dnd", "also before accounts")
	theirs, _ := s.Create(ctx, "bob", "mtg", "bob's")

	moved, err := s.ReassignOwner(ctx, AnonymousUser, "alice")
	if err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if moved != 2 {
		t.Errorf("moved %d conversations, want 2", moved)
	}
	for _, c := range []*Conversation{old1, old2} {
		if _, err := s.Get(ctx, "alice", c.ID); err != nil {
			t.Errorf("alice should own %q after the migration: %v", c.Title, err)
		}
	}
	if _, err := s.Get(ctx, "bob", theirs.ID); err != nil {
		t.Errorf("bob's conversation was swept up by the migration: %v", err)
	}

	// Idempotent: nothing is left under the anonymous owner to move.
	again, err := s.ReassignOwner(ctx, AnonymousUser, "alice")
	if err != nil {
		t.Fatalf("second reassign: %v", err)
	}
	if again != 0 {
		t.Errorf("second run moved %d conversations, want 0", again)
	}
}

func TestReassignOwnerIgnoresNoOpArguments(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.Create(ctx, AnonymousUser, "mtg", "before accounts")

	tests := []struct {
		name     string
		from, to string
	}{
		{"empty from", "", "alice"},
		{"empty to", AnonymousUser, ""},
		{"same owner", AnonymousUser, AnonymousUser},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := s.ReassignOwner(ctx, tt.from, tt.to)
			if err != nil || n != 0 {
				t.Errorf("ReassignOwner(%q, %q) = %d, %v; want 0, nil", tt.from, tt.to, n, err)
			}
		})
	}
	if list, _ := s.List(ctx, AnonymousUser, 10); len(list) != 1 {
		t.Errorf("the anonymous conversation should be untouched, got %d", len(list))
	}
}

func TestDeriveTitle(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "New chat"},
		{"short kept whole", "How does ward work?", "How does ward work?"},
		{"whitespace collapsed", "  How   does\nward work? ", "How does ward work?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveTitle(tt.in); got != tt.want {
				t.Errorf("DeriveTitle(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	long := "What happens when I attack with a creature that has deathtouch and trample into a blocker with indestructible?"
	got := DeriveTitle(long)
	if len([]rune(got)) > 59 {
		t.Errorf("long title not truncated: %q (%d runes)", got, len([]rune(got)))
	}
	if got[len(got)-len("…"):] != "…" {
		t.Errorf("truncated title should end in an ellipsis: %q", got)
	}
}
