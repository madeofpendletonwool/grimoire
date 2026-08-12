package index

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestStore_CardNames_RoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	want := []string{"Lightning Bolt", "Prizefight", "Giant Growth"}
	if err := store.IndexCards(context.Background(), want); err != nil {
		t.Fatalf("IndexCards: %v", err)
	}
	got, err := store.LoadCardNames(context.Background())
	if err != nil {
		t.Fatalf("LoadCardNames: %v", err)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip got %v, want %v", got, want)
	}
}

// Reindexing replaces the dictionary rather than appending to it.
func TestStore_IndexCards_Replaces(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.IndexCards(context.Background(), []string{"Lightning Bolt", "Stale Card"}); err != nil {
		t.Fatalf("first IndexCards: %v", err)
	}
	if err := store.IndexCards(context.Background(), []string{"Lightning Bolt", "Fresh Card"}); err != nil {
		t.Fatalf("second IndexCards: %v", err)
	}
	got, _ := store.LoadCardNames(context.Background())
	sort.Strings(got)
	want := []string{"Fresh Card", "Lightning Bolt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (stale card should be gone)", got, want)
	}
}

// Reset clears card_names alongside the docs tables so `grimoire index`
// rebuilds it cleanly.
func TestStore_ResetClearsCardNames(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.IndexCards(context.Background(), []string{"Lightning Bolt"}); err != nil {
		t.Fatalf("IndexCards: %v", err)
	}
	if err := store.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	got, err := store.LoadCardNames(context.Background())
	if err != nil {
		t.Fatalf("LoadCardNames: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("after Reset got %v, want empty", got)
	}
}
