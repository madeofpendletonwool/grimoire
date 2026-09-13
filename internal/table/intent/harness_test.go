package intent

// The e2e harness: a private migrated database (ADR 8's pattern, the
// testdb template), a live game the way the app builds one, a universe
// resolver over attached decks, and a fake model that replays scripted
// responses in call order and records every prompt it was handed — the
// canon engine's fakeModel shape, once more. Tests assert on the Action
// produced and the disposition assigned, not on the store.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// fixture is one live game with the pipeline wired.
type fixture struct {
	t        *testing.T
	ctx      context.Context
	engine   *engine.Store
	resolve  *universe.Store
	pipeline *Store
	game     string
}

// fakeModel replays scripted responses in call order, recording every
// prompt. An empty script fails the test on any call — a test that
// expects no model call must hear none.
type fakeModel struct {
	t         *testing.T
	responses []string
	calls     []string
}

func (f *fakeModel) ModelName() string { return "fake-intent" }

func (f *fakeModel) Complete(ctx context.Context, system, user string) (Completion, error) {
	i := len(f.calls)
	f.calls = append(f.calls, user)
	if i >= len(f.responses) {
		f.t.Fatalf("unexpected model call %d: %q", i+1, user)
	}
	return Completion{Text: f.responses[i], InputTokens: 100, OutputTokens: 50}, nil
}

// deckCards marshals a deck list the decks table stores.
func deckCards(t *testing.T, names ...string) string {
	t.Helper()
	type entry struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	out := make([]entry, 0, len(names))
	for _, n := range names {
		out = append(out, entry{Name: n, Count: 1})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal deck: %v", err)
	}
	return string(raw)
}

// seedDeck writes one decks row.
func seedDeck(t *testing.T, f *fixture, id, commander string, cards string) {
	t.Helper()
	if _, err := f.engine.DB().Exec(`INSERT INTO decks (id, owner_id, name, commander, cards, notes, created_at, updated_at)
		VALUES (?, 'u1', ?, ?, ?, '', ?, ?)`,
		id, id, commander, cards, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed deck %s: %v", id, err)
	}
}

// newFixture builds a live two-seat game with decks attached: seat 1
// carries the Study pair (the ambiguity fixtures key on it) plus basics;
// seat 2 carries Sol Ring and basics. Started, and walked to the first
// main so seat 1 holds priority.
func newFixture(t *testing.T, model ModelClient) *fixture {
	t.Helper()
	db := testdb.Open(t)
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, is_admin, created_at)
		VALUES ('u1', 'collin', 'x', 0, ?)`, time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	eng, err := engine.New(db)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	resolve, err := universe.NewStore(db, nil) // deck tiers only: hermetic
	if err != nil {
		t.Fatalf("universe: %v", err)
	}
	pipeline, err := New(db, model, eng, resolve)
	if err != nil {
		t.Fatalf("intent: %v", err)
	}
	f := &fixture{t: t, ctx: context.Background(), engine: eng, resolve: resolve, pipeline: pipeline}
	seedDeck(t, f, "deck1", "Atraxa, Praetors' Voice",
		deckCards(t, "Rhystic Study", "Mystic Study", "Sol Ring", "Cultivate"))
	seedDeck(t, f, "deck2", "Krenko, Mob Boss",
		deckCards(t, "Krenko, Mob Boss", "Lightning Bolt", "Mountain"))
	g, err := eng.CreateGame(f.ctx, "u1", "Friday Commander", "commander", 40)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.game = g.ID
	for _, seat := range []struct {
		pos  int
		name string
		deck string
		cmdr string
	}{
		{1, "Collin", "deck1", "Atraxa, Praetors' Voice"},
		{2, "Bob", "deck2", "Krenko, Mob Boss"},
	} {
		if err := eng.SeatPlayer(f.ctx, g.ID, seat.pos, seat.name, "", seat.deck, seat.cmdr, nil); err != nil {
			t.Fatalf("seat %d: %v", seat.pos, err)
		}
	}
	if _, _, err := eng.StartGame(f.ctx, g.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	f.toMain(1)
	return f
}

// toMain advances until seat holds priority in a main phase — the step
// where casts and land drops are legal.
func (f *fixture) toMain(seat int) {
	f.t.Helper()
	for i := 0; i < 12; i++ {
		st, err := f.engine.State(f.ctx, f.game)
		if err != nil {
			f.t.Fatalf("state: %v", err)
		}
		if st.Status == engine.StatusActive && st.PrioritySeat == seat &&
			(st.Phase == "precombat_main" || st.Phase == "postcombat_main") {
			return
		}
		if _, _, err := f.engine.Submit(f.ctx, f.game, engine.Action{Kind: engine.ActionAdvance, Seat: st.TurnSeat}); err != nil {
			f.t.Fatalf("advance: %v", err)
		}
	}
	f.t.Fatalf("never reached a main phase with priority on seat %d", seat)
}

// state folds the game's log.
func (f *fixture) state() *engine.State {
	f.t.Helper()
	st, err := f.engine.State(f.ctx, f.game)
	if err != nil {
		f.t.Fatalf("state: %v", err)
	}
	return st
}

// talk runs one utterance through the pipeline.
func (f *fixture) talk(seat int, utterance string) *Reply {
	f.t.Helper()
	reply, err := f.pipeline.Interpret(f.ctx, f.game, seat, utterance, "")
	if err != nil {
		f.t.Fatalf("interpret %q: %v", utterance, err)
	}
	return reply
}

// latestOrd is the log head.
func (f *fixture) latestOrd() int64 {
	f.t.Helper()
	ord, err := f.engine.LatestOrd(f.ctx, f.game)
	if err != nil {
		f.t.Fatalf("latest ord: %v", err)
	}
	return ord
}

// cachedResolution reads the per-game identity cache.
func (f *fixture) cachedResolution(spoken string) (universe.Resolution, bool) {
	f.t.Helper()
	return f.resolve.Cached(f.ctx, f.game, spoken)
}

// mustReplyAction unwraps a parsed reply's action.
func mustReplyAction(t *testing.T, r *Reply) engine.Action {
	t.Helper()
	if !r.Parsed || r.Action == nil {
		t.Fatalf("reply did not parse: %+v", r)
	}
	return *r.Action
}

// fenced wraps a model payload the way the models actually reply.
func fenced(body string) string {
	return fmt.Sprintf("Here is the action:\n```json\n%s\n```", body)
}
