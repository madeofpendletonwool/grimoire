package entities

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/data"
)

// TestScryfall_IsCardProjector confirms the MTG resolver satisfies the marker
// the server routes card corpora on, so Magic keeps its rich card path.
func TestScryfall_IsCardProjector(t *testing.T) {
	var r data.EntityResolver = NewScryfall(nil, nil)
	if _, ok := r.(CardProjector); !ok {
		t.Errorf("*Scryfall should satisfy CardProjector so the server routes MTG through its card path")
	}
}

// TestScryfall_NilServiceResolvesNothing mirrors the card path's graceful
// degradation: a resolver without a card service resolves nothing and reports
// no unresolved names, never errors.
func TestScryfall_NilServiceResolvesNothing(t *testing.T) {
	r := NewScryfall(nil, nil)
	entities, unresolved, err := r.Resolve(context.Background(), `What does "Lightning Bolt" do?`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(entities) != 0 || len(unresolved) != 0 {
		t.Errorf("nil service should resolve nothing, got entities=%v unresolved=%v", entities, unresolved)
	}
}

// TestScryfall_ResolveProjectsCards stubs Scryfall /cards/named and asserts the
// resolver extracts the mention, looks it up, and returns a neutral card entity
// with the oracle text formatted into the body.
func TestScryfall_ResolveProjectsCards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cards/named" || r.URL.Query().Get("fuzzy") != "Lightning Bolt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"name":"Lightning Bolt","mana_cost":"{R}","type_line":"Instant","oracle_text":"Lightning Bolt deals 3 damage to any target."}`))
	}))
	t.Cleanup(srv.Close)

	r := NewScryfall(cards.NewWithBase(srv.URL), nil)
	entities, unresolved, err := r.Resolve(context.Background(), `What does "Lightning Bolt" do?`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
	if len(entities) != 1 {
		t.Fatalf("resolved %d entities, want 1: %+v", len(entities), entities)
	}
	e := entities[0]
	if e.Name != "Lightning Bolt" || e.Kind != "card" {
		t.Errorf("entity = %+v, want Lightning Bolt / card", e)
	}
	for _, want := range []string{"Lightning Bolt deals 3 damage", "Instant"} {
		if !strings.Contains(e.Body, want) {
			t.Errorf("body missing %q: %q", want, e.Body)
		}
	}
}
