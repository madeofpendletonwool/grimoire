package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

func newStore(t *testing.T) (*Store, *sql.DB, string) {
	t.Helper()
	db := testdb.Open(t)
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, is_admin, created_at)
		VALUES ('u1', 'collin', 'x', 0, ?)`, time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	s, err := New(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s, db, "u1"
}

// seedDeck inserts a decks row: 30 Forest, 4 Cultivate maindeck, a
// sideboard Bolt that must stay out of the library, and the commander on
// the commander board.
func seedDeck(t *testing.T, db *sql.DB, id, commander string) {
	t.Helper()
	cards, err := json.Marshal([]deckEntry{
		{Name: "Forest", Count: 30},
		{Name: "Cultivate", Count: 4},
		{Name: "Lightning Bolt", Count: 3, Board: "sideboard"},
		{Name: commander, Count: 1, Board: "commander"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO decks (id, owner_id, name, commander, cards, notes, created_at, updated_at)
		VALUES (?, 'u1', 'Test Deck', ?, ?, '', ?, ?)`,
		id, commander, string(cards), time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed deck: %v", err)
	}
}

func mustSubmit(t *testing.T, s *Store, ctx context.Context, game string, a Action) []Event {
	t.Helper()
	evs, _, err := s.Submit(ctx, game, a)
	if err != nil {
		t.Fatalf("submit %s: %v", a.Kind, err)
	}
	return evs
}

// A full Commander set-up through the store: seats, a deck whose
// composition becomes the library, a commander adopted from the deck.
func newGame(t *testing.T) (*Store, *sql.DB, context.Context, string) {
	t.Helper()
	s, db, owner := newStore(t)
	seedDeck(t, db, "deck1", "Atraxa, Praetors' Voice")
	ctx := context.Background()
	g, err := s.CreateGame(ctx, owner, "Friday Commander", "commander", 40)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if g.Status != StatusSetup {
		t.Fatalf("status = %s", g.Status)
	}
	if err := s.SeatPlayer(ctx, g.ID, 1, "Collin", "u1", "deck1", "", nil); err != nil {
		t.Fatalf("seat 1: %v", err)
	}
	if err := s.SeatPlayer(ctx, g.ID, 2, "Bob", "", "", "Krenko, Mob Boss", nil); err != nil {
		t.Fatalf("seat 2: %v", err)
	}
	if err := s.SeatPlayer(ctx, g.ID, 2, "Dup", "", "", "", nil); err == nil {
		t.Fatal("duplicate seat position accepted")
	}
	return s, db, ctx, g.ID
}

func TestStoreStartGameEchoesConfig(t *testing.T) {
	s, db, ctx, gameID := newGame(t)
	evs, st, err := s.StartGame(ctx, gameID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if evs[0].Kind != EventGameStarted {
		t.Fatalf("first event = %s", evs[0].Kind)
	}
	g, _ := s.GetGame(ctx, gameID)
	if g.Status != StatusActive || g.StartedAt == nil {
		t.Fatalf("game row = %+v", g)
	}
	p := st.Seats[1]
	if !p.Library.Known || p.Library.N != 34 {
		t.Fatalf("library = %+v (sideboard and commander must stay out)", p.Library)
	}
	if p.Commander != "Atraxa, Praetors' Voice" {
		t.Fatalf("commander adopted = %q", p.Commander)
	}
	if len(st.ZoneObjects(1, ZoneCommand)) != 1 {
		t.Fatal("commander object missing from the command zone")
	}
	// Setup writes are closed once the log begins.
	if err := s.SeatPlayer(ctx, gameID, 3, "Late", "", "", "", nil); err == nil {
		t.Fatal("seating after start accepted")
	}
	if err := s.AttachDeck(ctx, gameID, 2, "deck1"); err == nil {
		t.Fatal("deck attach after start accepted")
	}
	_ = db
}

func TestStoreSubmitPersistsContiguousOrdinals(t *testing.T) {
	s, _, ctx, gameID := newGame(t)
	if _, _, err := s.StartGame(ctx, gameID); err != nil {
		t.Fatal(err)
	}
	start, err := s.Events(ctx, gameID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(start) != 3 {
		t.Fatalf("start events = %d", len(start))
	}
	var all []Event
	// The turn starts in untap, where no one holds priority: walk to the
	// main phase before the play actions.
	all = append(all, mustSubmit(t, s, ctx, gameID, Action{Kind: ActionAdvance, Seat: 1})...)
	all = append(all, mustSubmit(t, s, ctx, gameID, Action{Kind: ActionAdvance, Seat: 1})...)
	all = append(all, mustSubmit(t, s, ctx, gameID, Action{Kind: ActionDraw, Seat: 1, Count: 2, Cards: []string{"Forest", "Cultivate"}})...)
	all = append(all, mustSubmit(t, s, ctx, gameID, Action{Kind: ActionPlayLand, Seat: 1, Card: "Forest"})...)
	all = append(all, mustSubmit(t, s, ctx, gameID, Action{Kind: ActionCast, Seat: 1, Card: "Cultivate"})...)
	all = append(all, mustSubmit(t, s, ctx, gameID, Action{Kind: ActionChangeLife, Seat: 2, TargetSeat: 2, Delta: -3, SourceCard: "Lightning Bolt"})...)

	whole := append(append([]Event{}, start...), all...)
	for i, e := range whole {
		if e.Ord != int64(i+1) {
			t.Fatalf("ord %d at position %d", e.Ord, i)
		}
		if e.ID == "" || e.CreatedAt == 0 {
			t.Fatalf("event %d unassigned: %+v", e.Ord, e)
		}
	}
	// Pagination reads a window past an ordinal.
	mid, err := s.Events(ctx, gameID, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(mid) != 2 || mid[0].Ord != 5 || mid[1].Ord != 6 {
		t.Fatalf("page = %v", ords(mid))
	}
	// The cause column is the Action JSON, self-contained per row.
	for _, e := range whole {
		if e.Cause == "" {
			t.Fatalf("event %d has no cause", e.Ord)
		}
		var back Action
		if err := json.Unmarshal([]byte(e.Cause), &back); err != nil {
			t.Fatalf("cause parse: %v", err)
		}
	}
}

func ords(evs []Event) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.Ord
	}
	return out
}

func TestStoreRefoldFromDatabaseIsIdentical(t *testing.T) {
	s, _, ctx, gameID := newGame(t)
	if _, _, err := s.StartGame(ctx, gameID); err != nil {
		t.Fatal(err)
	}
	// Drive a representative slice of the engine through the writer.
	script := []Action{
		// Untap grants no priority: reach the main phase first.
		{Kind: ActionAdvance, Seat: 1},
		{Kind: ActionAdvance, Seat: 1},
		{Kind: ActionDraw, Seat: 1, Count: 3, Cards: []string{"Forest", "Forest", "Cultivate"}},
		{Kind: ActionPlayLand, Seat: 1, Card: "Forest"},
		{Kind: ActionCast, Seat: 1, Card: "Cultivate"},
		{Kind: ActionPassPriority, Seat: 1},
		{Kind: ActionPassPriority, Seat: 2},
		{Kind: ActionDraw, Seat: 2, Count: 1},
		{Kind: ActionCreateToken, Seat: 2, Count: 2, Token: &TokenSpec{Name: "Goblin", Types: []string{"Creature"}, Power: intPtr(1), Toughness: intPtr(1)}},
		{Kind: ActionAdjustCounters, Seat: 1, CounterName: "energy", Delta: 3},
		{Kind: ActionSetFlag, Seat: 1, Flag: "monarch", Value: "true"},
		{Kind: ActionDealDamage, Seat: 2, Amount: 4, SourceObj: 1, TargetSeat: 2, CombatDmg: true},
		{Kind: ActionAdvance, Seat: 1},
		{Kind: ActionAddModifier, Seat: 1, Object: 1, Modifier: &Modifier{
			Layer: LayerPTModify, Duration: UntilEOT, SourceCard: "Giant Growth",
			Delta: Delta{Power: intPtr(3), Toughness: intPtr(3)}}},
	}
	var live *State
	for _, a := range script {
		_, live, _ = mustSubmit2(t, s, ctx, gameID, a)
	}
	// The acceptance line: the log persisted, then re-folded from the
	// database, is the identical state the writer handed back.
	persisted, err := s.Events(ctx, gameID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	refolded := Fold(persisted)
	if !reflect.DeepEqual(refolded, Fold(persisted)) {
		t.Fatal("refold unstable")
	}
	if !reflect.DeepEqual(refolded, live) {
		t.Fatalf("refolded state diverges from the live fold")
	}
	fromDB, err := s.State(ctx, gameID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromDB, live) {
		t.Fatal("store State() diverges from the live fold")
	}
	// Spot-check the game the log rebuilds.
	if fromDB.Seats[2].CommanderDamage["Atraxa, Praetors' Voice"] != 4 {
		t.Fatalf("commander damage = %v", fromDB.Seats[2].CommanderDamage)
	}
	if fromDB.Seats[1].Counters["energy"] != 3 || fromDB.Seats[1].Flags["monarch"] != "true" {
		t.Fatalf("counters %v flags %v", fromDB.Seats[1].Counters, fromDB.Seats[1].Flags)
	}
}

func mustSubmit2(t *testing.T, s *Store, ctx context.Context, gameID string, a Action) ([]Event, *State, error) {
	t.Helper()
	evs, st, err := s.Submit(ctx, gameID, a)
	if err != nil {
		t.Fatalf("submit %s: %v", a.Kind, err)
	}
	return evs, st, err
}

func TestStoreHiddenZoneRows(t *testing.T) {
	s, _, ctx, gameID := newGame(t)
	if _, _, err := s.StartGame(ctx, gameID); err != nil {
		t.Fatal(err)
	}
	evs := mustSubmit(t, s, ctx, gameID, Action{Kind: ActionDraw, Seat: 1, Count: 2, Cards: []string{"Forest", "Cultivate"}})
	var drawn, known *Event
	for i := range evs {
		switch evs[i].Kind {
		case EventCardDrawn:
			drawn = &evs[i]
		case EventCardKnown:
			known = &evs[i]
		}
	}
	if drawn == nil || known == nil {
		t.Fatalf("draw rows = %+v", evs)
	}
	if drawn.Visibility != VisibilityPublic || drawn.VisibleSeat != 0 {
		t.Fatalf("public draw row = %+v", drawn)
	}
	if known.Visibility != VisibilitySeat || known.VisibleSeat != 1 {
		t.Fatalf("identity row = %+v", known)
	}
	// The seats the rows are gated for in SQL, exactly as the CHECK on
	// mtg_events demands: a public row has no seat, a seat row names it.
	var vis string
	var seat sql.NullInt64
	if err := s.DB().QueryRow(`SELECT visibility, visible_seat FROM mtg_events WHERE id = ?`, known.ID).
		Scan(&vis, &seat); err != nil {
		t.Fatal(err)
	}
	if vis != "seat" || !seat.Valid || seat.Int64 != 1 {
		t.Fatalf("identity row in SQL: %s %v", vis, seat)
	}
}

func TestStoreRewindTruncatesAndRefolds(t *testing.T) {
	s, _, ctx, gameID := newGame(t)
	if _, _, err := s.StartGame(ctx, gameID); err != nil {
		t.Fatal(err)
	}
	mustSubmit(t, s, ctx, gameID, Action{Kind: ActionChangeLife, Seat: 1, TargetSeat: 1, Delta: -5})
	mustSubmit(t, s, ctx, gameID, Action{Kind: ActionChangeLife, Seat: 2, TargetSeat: 2, Delta: -7})
	all, err := s.Events(ctx, gameID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Rewind to the ordinal before the second life change: the refolded
	// state must equal folding exactly the surviving prefix.
	cut := int64(len(all) - 1)
	st, err := s.RewindTo(ctx, gameID, cut)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.Events(ctx, gameID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(after)) != cut {
		t.Fatalf("log = %d rows after rewind to %d", len(after), cut)
	}
	if !reflect.DeepEqual(st, Fold(after)) {
		t.Fatal("rewound state is not the prefix fold")
	}
	if st.Seats[2].Life != 40 {
		t.Fatalf("seat 2 life = %d", st.Seats[2].Life)
	}
	if st.Seats[1].Life != 35 {
		t.Fatalf("seat 1 life = %d", st.Seats[1].Life)
	}
	// Ordinals continue contiguously after the rewind.
	mustSubmit(t, s, ctx, gameID, Action{Kind: ActionChangeLife, Seat: 1, TargetSeat: 1, Delta: -1})
	tail, _ := s.Events(ctx, gameID, cut, 0)
	if len(tail) != 1 || tail[0].Ord != cut+1 {
		t.Fatalf("tail = %v", ords(tail))
	}
	// Rewinding before GAME_STARTED hands the game back to setup.
	if _, err := s.RewindTo(ctx, gameID, 0); err != nil {
		t.Fatal(err)
	}
	g, _ := s.GetGame(ctx, gameID)
	if g.Status != StatusSetup || g.StartedAt != nil {
		t.Fatalf("game = %+v", g)
	}
	evs, _ := s.Events(ctx, gameID, 0, 0)
	if len(evs) != 0 {
		t.Fatalf("log = %d rows after rewind to 0", len(evs))
	}
	// The game can start again from the same seat rows.
	if _, _, err := s.StartGame(ctx, gameID); err != nil {
		t.Fatalf("restart: %v", err)
	}
	// Rewinding past the head is an error, not a clamp.
	if _, err := s.RewindTo(ctx, gameID, 99); err == nil {
		t.Fatal("rewind past head accepted")
	}
	if _, err := s.RewindTo(ctx, gameID, -1); err == nil {
		t.Fatal("negative rewind accepted")
	}
}

func TestStoreEndGameAndResubmit(t *testing.T) {
	s, _, ctx, gameID := newGame(t)
	if _, _, err := s.StartGame(ctx, gameID); err != nil {
		t.Fatal(err)
	}
	mustSubmit(t, s, ctx, gameID, Action{Kind: ActionEndGame, Seat: 1, Reason: "gg"})
	g, _ := s.GetGame(ctx, gameID)
	if g.Status != StatusFinished || g.EndedAt == nil {
		t.Fatalf("game = %+v", g)
	}
	_, _, err := s.Submit(ctx, gameID, Action{Kind: ActionDraw, Seat: 1})
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("post-end submit err = %v", err)
	}
	// A finished game cannot start again.
	if _, _, err := s.StartGame(ctx, gameID); err == nil {
		t.Fatal("restart of a finished game accepted")
	}
}

func TestStoreErrors(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.GetGame(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("get err = %v", err)
	}
	if _, _, err := s.Submit(ctx, "nope", Action{Kind: ActionAdvance}); err == nil {
		t.Fatal("submit to a missing game accepted")
	}
	g, err := s.CreateGame(ctx, "u1", "Defaults", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if g.Format != "commander" || g.StartingLife != 40 {
		t.Fatalf("defaults = %s/%d", g.Format, g.StartingLife)
	}
	// A game with no seats cannot start.
	if _, _, err := s.StartGame(ctx, g.ID); err == nil {
		t.Fatal("empty start accepted")
	}
	// A missing deck id fails the seat write at the FK.
	if err := s.SeatPlayer(ctx, g.ID, 1, "Collin", "", "ghost-deck", "", nil); err == nil {
		t.Fatal("missing deck accepted")
	}
}
