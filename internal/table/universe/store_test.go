package universe

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// newFixture builds a live game the way the app does: a deck attached to
// seat 1 (composition + commander adoption), a deckless seat 2, started.
func newFixture(t *testing.T) (*engine.Store, *universeFixture) {
	t.Helper()
	db := testdb.Open(t)
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, is_admin, created_at)
		VALUES ('u1', 'collin', 'x', 0, ?)`, time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	cards, err := json.Marshal([]struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
		Board string `json:"board"`
	}{
		{Name: "Rhystic Study", Count: 1},
		{Name: "Island", Count: 30},
		{Name: "Atraxa, Praetors' Voice", Count: 1, Board: "commander"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO decks (id, owner_id, name, commander, cards, notes, created_at, updated_at)
		VALUES (?, 'u1', 'Test Deck', ?, ?, '', ?, ?)`,
		"deck1", "Atraxa, Praetors' Voice", string(cards), time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed deck: %v", err)
	}
	s, err := engine.New(db)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	ctx := context.Background()
	g, err := s.CreateGame(ctx, "u1", "Friday Commander", "commander", 40)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SeatPlayer(ctx, g.ID, 1, "Collin", "u1", "deck1", "", nil); err != nil {
		t.Fatalf("seat 1: %v", err)
	}
	if err := s.SeatPlayer(ctx, g.ID, 2, "Bob", "", "", "Krenko, Mob Boss", nil); err != nil {
		t.Fatalf("seat 2: %v", err)
	}
	if _, _, err := s.StartGame(ctx, g.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	st, err := s.State(ctx, g.ID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	return s, &universeFixture{db: db, ctx: ctx, game: g.ID, state: st}
}

type universeFixture struct {
	db    *sql.DB
	ctx   context.Context
	game  string
	state *engine.State
}

// The same mumble is never re-inferred: the first ask resolves and is
// written; the second is answered by the cache, with the index never
// consulted again.
func TestCacheAnswersBeforeTheTiers(t *testing.T) {
	_, f := newFixture(t)
	global := &fakeGlobal{by: map[string]string{"sol ring": "Sol Ring"}}
	store, err := NewStore(f.db, global)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	r, err := store.ResolveGame(f.ctx, f.state, f.game, 2, "Sol Ring")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Card != "Sol Ring" || r.Method != MethodGlobal || global.calls != 1 {
		t.Fatalf("first resolve → %+v, %d global calls", r, global.calls)
	}
	// The normalized surface form varies; the cache key does not.
	r, err = store.ResolveGame(f.ctx, f.state, f.game, 2, "  sol ring. ")
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if r.Card != "Sol Ring" || r.Scope != ScopeCache || global.calls != 1 {
		t.Fatalf("cached resolve → %+v, %d global calls (want cache, no re-inference)", r, global.calls)
	}
}

// A human correction is the resolution of that mumble from then on, at
// full confidence — and it replaces whatever the tiers previously chose.
func TestManualRecordWins(t *testing.T) {
	_, f := newFixture(t)
	store, err := NewStore(f.db, nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := store.ResolveGame(f.ctx, f.state, f.game, 1, "Rhystic"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := store.Record(f.ctx, f.game, "rhystic", "Smothering Tithe"); err != nil {
		t.Fatalf("record: %v", err)
	}
	r, err := store.ResolveGame(f.ctx, f.state, f.game, 1, "Rhystic")
	if err != nil {
		t.Fatalf("resolve after record: %v", err)
	}
	if r.Card != "Smothering Tithe" || r.Method != MethodManual || r.Confidence != ConfManual {
		t.Fatalf("corrected resolve → %+v", r)
	}
}

// Unresolved names are never cached: the next utterance tries again.
func TestUnresolvedIsNotCached(t *testing.T) {
	_, f := newFixture(t)
	global := &fakeGlobal{}
	store, err := NewStore(f.db, global)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	r, err := store.ResolveGame(f.ctx, f.state, f.game, 1, "Blorple Warp")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Resolved() {
		t.Fatalf("Blorple Warp → %+v", r)
	}
	if _, ok := store.Cached(f.ctx, f.game, "Blorple Warp"); ok {
		t.Fatal("unresolved name was cached")
	}
	if r, err = store.ResolveGame(f.ctx, f.state, f.game, 1, "Blorple Warp"); err != nil || r.Resolved() {
		t.Fatalf("second try → %+v, %v — the retry must happen", r, err)
	}
	if global.calls != 2 {
		t.Fatalf("global calls = %d, want 2 (both tries reached the index)", global.calls)
	}
}

// The cache is game history: a rewind truncates the log, never a
// resolved name.
func TestCacheSurvivesRewind(t *testing.T) {
	s, f := newFixture(t)
	store, err := NewStore(f.db, &fakeGlobal{})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := store.ResolveGame(f.ctx, f.state, f.game, 1, "Rhystic"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := s.RewindTo(f.ctx, f.game, 0); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if r, ok := store.Cached(f.ctx, f.game, "Rhystic"); !ok || r.Card != "Rhystic Study" {
		t.Fatalf("cache after rewind → %+v, %v", r, ok)
	}
}

func TestEmptySpokenRefused(t *testing.T) {
	_, f := newFixture(t)
	store, err := NewStore(f.db, nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := store.ResolveGame(f.ctx, f.state, f.game, 1, "   "); err == nil {
		t.Fatal("empty spoken accepted")
	}
	if err := store.Record(f.ctx, f.game, "", "Sol Ring"); err == nil {
		t.Fatal("empty correction key accepted")
	}
}

// The model fallback's identification (MAD-331): the llm tier joins
// the cache, capped at ConfLLM whatever the model claimed — the cache
// must not launder a model identification into a higher band on replay.
func TestRecordLLMCapsAndCaches(t *testing.T) {
	_, f := newFixture(t)
	store, err := NewStore(f.db, nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := store.RecordLLM(f.ctx, f.game, "the study thing", "Rhystic Study", 1.0); err != nil {
		t.Fatalf("record: %v", err)
	}
	r, ok := store.Cached(f.ctx, f.game, "the study thing")
	if !ok || r.Card != "Rhystic Study" || r.Method != MethodLLM {
		t.Fatalf("cached = %+v ok %v, want the llm-tier row", r, ok)
	}
	if r.Confidence != ConfLLM {
		t.Fatalf("confidence = %v, want the ceiling %v", r.Confidence, ConfLLM)
	}
	// The tiers answer from the cache before they ever run — the same
	// mumble is never re-inferred, never re-billed.
	r, err = store.ResolveGame(f.ctx, f.state, f.game, 1, "The Study Thing")
	if err != nil || r.Scope != ScopeCache || r.Card != "Rhystic Study" {
		t.Fatalf("resolve → %+v err %v, want the cached answer", r, err)
	}
	// A correction still wins over the model's word, being human.
	if err := store.Record(f.ctx, f.game, "the study thing", "Mystic Study"); err != nil {
		t.Fatalf("correct: %v", err)
	}
	if r, _ := store.Cached(f.ctx, f.game, "the study thing"); r.Method != MethodManual || r.Confidence != ConfManual {
		t.Fatalf("after correction = %+v, want manual at full confidence", r)
	}
}
