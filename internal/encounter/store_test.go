package encounter

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/index"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "encounters.db")
	store, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store.DB())
	if err != nil {
		t.Fatalf("new encounter store: %v", err)
	}
	return s
}

func TestStoreCRUD(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Create(ctx, "alice", "Ambush on the Triboar Trail", []int{1, 1, 2}, []Monster{
		{Name: "Goblin", CR: "1/4", XP: 50, Count: 4},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("create did not mint an id")
	}

	got, err := s.Get(ctx, "alice", created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Ambush on the Triboar Trail" || !reflect.DeepEqual(got.Party, []int{1, 1, 2}) || len(got.Monsters) != 1 || got.Monsters[0].Count != 4 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	name := "Renamed"
	newParty := []int{3, 3, 3, 2}
	updated, err := s.Update(ctx, "alice", created.ID, &name, newParty, []Monster{
		{Name: "Bugbear", CR: "1", XP: 200, Count: 1},
		{Name: "Hobgoblin", CR: "1/2", XP: 100, Count: 3},
	}, true, true)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Renamed" || len(updated.Party) != 4 || len(updated.Monsters) != 2 {
		t.Fatalf("update mismatch: %+v", updated)
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) && !updated.UpdatedAt.Equal(updated.CreatedAt) {
		t.Fatalf("updated_at not moved: %+v", updated)
	}

	list, err := s.List(ctx, "alice")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v (err %v), want 1", list, err)
	}

	if err := s.Delete(ctx, "alice", created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, "alice", created.ID); err != ErrNotFound {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "alice", created.ID); err != ErrNotFound {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
}

// Another user's encounters are invisible: foreign reads, updates, and
// deletes all report not-found and leave the row untouched.
func TestStoreOwnerScoping(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	created, err := s.Create(ctx, "alice", "Keep", []int{5}, []Monster{{Name: "Ogre", CR: "2", XP: 450, Count: 2}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.Get(ctx, "bob", created.ID); err != ErrNotFound {
		t.Fatalf("foreign get = %v, want ErrNotFound", err)
	}
	if _, err := s.Update(ctx, "bob", created.ID, nil, nil, nil, false, false); err != ErrNotFound {
		t.Fatalf("foreign update = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "bob", created.ID); err != ErrNotFound {
		t.Fatalf("foreign delete = %v, want ErrNotFound", err)
	}
	if list, err := s.List(ctx, "bob"); err != nil || len(list) != 0 {
		t.Fatalf("bob sees %v (err %v), want nothing", list, err)
	}
	got, err := s.Get(ctx, "alice", created.ID)
	if err != nil || got.Name != "Keep" {
		t.Fatalf("alice's encounter disturbed by foreign access: %+v (%v)", got, err)
	}
}

func TestStoreUpdateKeepsUnsentFields(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	created, err := s.Create(ctx, "alice", "Waves", []int{2, 2, 3}, []Monster{{Name: "Hobgoblin", CR: "1/2", XP: 100, Count: 6}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	name := "Waves, revised"
	updated, err := s.Update(ctx, "alice", created.ID, &name, nil, nil, false, false)
	if err != nil {
		t.Fatalf("rename-only update: %v", err)
	}
	if updated.Name != "Waves, revised" || len(updated.Party) != 3 || len(updated.Monsters) != 1 || updated.Monsters[0].Count != 6 {
		t.Fatalf("rename-only update lost fields: %+v", updated)
	}
}
