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
	if _, err := s.AddMessage(ctx, first.ID, RoleUser, "hello", nil, nil); err != nil {
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
	if _, err := s.AddMessage(ctx, c.ID, RoleUser, "does deathtouch work?", nil, nil); err != nil {
		t.Fatalf("add user: %v", err)
	}
	if _, err := s.AddMessage(ctx, c.ID, RoleAssistant, "yes", sources, nil); err != nil {
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

func TestDeleteCascadesMessages(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c, _ := s.Create(ctx, "u", "mtg", "t")
	if _, err := s.AddMessage(ctx, c.ID, RoleUser, "hi", nil, nil); err != nil {
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
		if _, err := s.AddMessage(ctx, c.ID, RoleUser, "q", nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddMessage(ctx, c.ID, RoleAssistant, "a", json.RawMessage(`[1]`), nil); err != nil {
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
	s.AddMessage(ctx, c.ID, RoleUser, "q", nil, nil)
	s.AddMessage(ctx, c.ID, RoleAssistant, "a", nil, nil)

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
