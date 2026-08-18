package deck

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "decks.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func TestDeckCRUD(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	d, err := s.Create(ctx, "alice", "Kaalia Angels", "Kaalia of the Vast", []Entry{
		{Name: "Kaalia of the Vast", Count: 1, Board: "commander"},
		{Name: "Sol Ring", Count: 1},
		{Name: " Sol Ring ", Count: 1}, // merges with the above
		{Name: "Plains", Count: 36},
	}, "first draft")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(d.Cards) != 3 {
		t.Fatalf("cards = %+v, want 3 merged rows", d.Cards)
	}
	if d.Cards[1].Count != 2 {
		t.Fatalf("merged sol ring count = %d", d.Cards[1].Count)
	}

	got, err := s.Get(ctx, "alice", d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Kaalia Angels" || got.Commander != "Kaalia of the Vast" {
		t.Fatalf("got = %+v", got)
	}

	// Rename only: cards preserved.
	name := "Kaalia Dragons"
	got, err = s.Update(ctx, "alice", d.ID, &name, nil, nil, nil, false)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Name != "Kaalia Dragons" || len(got.Cards) != 3 {
		t.Fatalf("updated = %+v", got)
	}

	// Replace cards only.
	got, err = s.Update(ctx, "alice", d.ID, nil, nil, nil, []Entry{{Name: "Anger", Count: 1}}, true)
	if err != nil {
		t.Fatalf("update cards: %v", err)
	}
	if len(got.Cards) != 1 || got.Cards[0].Name != "Anger" {
		t.Fatalf("replaced cards = %+v", got.Cards)
	}

	list, err := s.List(ctx, "alice")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	if err := s.Delete(ctx, "alice", d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, "alice", d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
}

func TestDeckOwnerScoping(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	d, err := s.Create(ctx, "alice", "A", "Kaalia of the Vast", nil, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Get(ctx, "bob", d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign get = %v, want ErrNotFound", err)
	}
	if _, err := s.Update(ctx, "bob", d.ID, nil, nil, nil, nil, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign update = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "bob", d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign delete = %v, want ErrNotFound", err)
	}
	// The owner still has it.
	if _, err := s.Get(ctx, "alice", d.ID); err != nil {
		t.Fatalf("owner get after foreign attempts: %v", err)
	}
}
