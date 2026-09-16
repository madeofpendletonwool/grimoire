package engine

// The multiplayer pod's store tests (MAD-337): the scoped reads are a
// WHERE clause (rows absent, not filtered), the write side never puts
// a hidden identity on a public row (payload or cause), the fold a
// seat builds from its own stream is the public game plus its own
// hidden zones and nothing else, and the join code binds a participant
// to a seat. The leak gate itself lives in leak_test.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// seedMarkerDeck inserts a deck whose maindeck is the given names —
// markers chosen so no marker is a substring of another, which is what
// makes the leak scan's containment match exact.
func seedMarkerDeck(t *testing.T, db *sql.DB, id string, names map[string]int) {
	t.Helper()
	var entries []deckEntry
	for name, n := range names {
		entries = append(entries, deckEntry{Name: name, Count: n})
	}
	cards, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO decks (id, owner_id, name, commander, cards, notes, created_at, updated_at)
		VALUES (?, 'u1', 'Marker Deck', 'Marker Commander', ?, '', ?, ?)`,
		id, string(cards), time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed marker deck: %v", err)
	}
}

// newPodGame builds the four-seat acceptance game: four accounts, four
// decks of marker cards, one started game, seat 1 having drawn two
// marker cards and played one public land.
func newPodGame(t *testing.T) (*Store, *sql.DB, context.Context, string) {
	t.Helper()
	s, db, owner := newStore(t)
	for _, u := range []string{"u2", "u3", "u4"} {
		if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, is_admin, created_at)
			VALUES (?, ?, 'x', 0, ?)`, u, u, time.Now().UnixMilli()); err != nil {
			t.Fatalf("seed user %s: %v", u, err)
		}
	}
	seedMarkerDeck(t, db, "deckA", map[string]int{"Seat One Secret": 4, "Forest": 10})
	seedMarkerDeck(t, db, "deckB", map[string]int{"Seat Two Secret": 4, "Island": 10})
	seedMarkerDeck(t, db, "deckC", map[string]int{"Seat Three Secret": 4, "Swamp": 10})
	seedMarkerDeck(t, db, "deckD", map[string]int{"Seat Four Secret": 4, "Mountain": 10})
	ctx := context.Background()
	g, err := s.CreateGame(ctx, owner, "Pod Night", "commander", 40)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	seats := []struct {
		pos    int
		name   string
		user   string
		deckID string
	}{
		{1, "Collin", "u1", "deckA"},
		{2, "Bob", "u2", "deckB"},
		{3, "Alice", "u3", "deckC"},
		{4, "Dave", "u4", "deckD"},
	}
	for _, st := range seats {
		if err := s.SeatPlayer(ctx, g.ID, st.pos, st.name, st.user, st.deckID, "", nil); err != nil {
			t.Fatalf("seat %d: %v", st.pos, err)
		}
	}
	if _, _, err := s.StartGame(ctx, g.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Seat 2 draws two named cards (seat-visible identities); seat 1
	// advances to its precombat main and plays a public land.
	if _, _, err := s.Submit(ctx, g.ID, Action{Kind: ActionDraw, Seat: 2, Count: 2,
		Cards: []string{"Seat Two Secret", "Island"}}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	for i := 0; i < 2; i++ { // untap → upkeep → draw step (priority along the way)
		if _, _, err := s.Submit(ctx, g.ID, Action{Kind: ActionAdvance, Seat: 1}); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	if _, _, err := s.Submit(ctx, g.ID, Action{Kind: ActionPlayLand, Seat: 1, Card: "Forest"}); err != nil {
		t.Fatalf("land: %v", err)
	}
	return s, db, ctx, g.ID
}

// TestStartGameSplitsDeckVisibility is the write-side half of ADR 13:
// GAME_STARTED is public and deckless, every deck rides its own
// seat-visible DECK_KNOWN row, and the owner's fold is exactly what a
// public echo used to carry.
func TestStartGameSplitsDeckVisibility(t *testing.T) {
	s, _, ctx, game := newPodGame(t)

	evs, err := s.Events(ctx, game, 0, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var started *Event
	deckKnown := map[int]bool{}
	for i := range evs {
		switch evs[i].Kind {
		case EventGameStarted:
			started = &evs[i]
		case EventDeckKnown:
			if evs[i].Visibility != VisibilitySeat {
				t.Fatalf("DECK_KNOWN ord %d is %s, not seat-visible", evs[i].Ord, evs[i].Visibility)
			}
			deckKnown[evs[i].VisibleSeat] = true
		}
	}
	if started == nil {
		t.Fatal("no GAME_STARTED row")
	}
	if started.Visibility != VisibilityPublic {
		t.Fatalf("GAME_STARTED is %s", started.Visibility)
	}
	for _, sc := range started.Seats {
		if len(sc.Deck) > 0 {
			t.Fatalf("public GAME_STARTED echoes seat %d's deck", sc.Seat)
		}
	}
	for seat := 1; seat <= 4; seat++ {
		if !deckKnown[seat] {
			t.Fatalf("seat %d has no DECK_KNOWN row", seat)
		}
	}
	// The owner folds every deck: the split changed who may read a
	// deck, not what the game knows.
	st, err := s.State(ctx, game)
	if err != nil {
		t.Fatalf("owner state: %v", err)
	}
	for seat := 1; seat <= 4; seat++ {
		if len(st.Seats[seat].Deck) == 0 {
			t.Fatalf("owner fold missing seat %d's deck", seat)
		}
		if !st.Seats[seat].Library.Known {
			t.Fatalf("owner fold missing seat %d's library size", seat)
		}
	}
	// The public rows' causes never repeat a deck: the START_GAME
	// action carried all four, the row's cause must not.
	for i := range evs {
		if evs[i].Visibility != VisibilitySeat {
			for _, marker := range []string{"Seat One Secret", "Seat Two Secret", "Seat Three Secret", "Seat Four Secret"} {
				if strings.Contains(evs[i].Cause, marker) {
					t.Fatalf("public %s ord %d cause carries deck marker %s", evs[i].Kind, evs[i].Ord, marker)
				}
			}
		}
	}
}

// TestEventsForIsSQLScoped proves the gate is the query: a seat's
// window contains the public rows and its own seat-visible rows, and
// another seat's CARD_KNOWN and DECK_KNOWN rows are absent as rows.
// The public CARD_DRAWN's cause carries no card names — the identities
// live on the seat-visible CARD_KNOWN row alone.
func TestEventsForIsSQLScoped(t *testing.T) {
	s, _, ctx, game := newPodGame(t)

	bob := SeatViewer(2)
	evs, err := s.EventsFor(ctx, game, bob, 0, 0)
	if err != nil {
		t.Fatalf("events for bob: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("bob's window is empty")
	}
	for i := range evs {
		e := evs[i]
		if e.Visibility == VisibilitySeat && !bob.SeesSeat(e.VisibleSeat) {
			t.Fatalf("bob received seat %d's %s row (ord %d)", e.VisibleSeat, e.Kind, e.Ord)
		}
		switch {
		case e.Kind == EventDeckKnown && e.VisibleSeat != 2:
			t.Fatalf("bob received seat %d's DECK_KNOWN", e.VisibleSeat)
		case e.Kind == EventCardKnown && e.VisibleSeat != 2:
			t.Fatalf("bob received seat %d's CARD_KNOWN", e.VisibleSeat)
		case e.Kind == EventCardDrawn && e.TargetSeat == 2:
			// Bob's own draw's public row: count only, cause redacted.
			if e.Count != 2 {
				t.Fatalf("CARD_DRAWN count = %d", e.Count)
			}
			if strings.Contains(e.Cause, "Seat Two Secret") || strings.Contains(e.Cause, "Island") {
				t.Fatal("public CARD_DRAWN cause carries the drawn identities")
			}
		}
	}
	// Bob's own CARD_KNOWN row is present, full cause and all.
	var known bool
	for i := range evs {
		if evs[i].Kind == EventCardKnown && evs[i].VisibleSeat == 2 {
			known = true
			if len(evs[i].Cards) != 2 {
				t.Fatalf("CARD_KNOWN cards = %v", evs[i].Cards)
			}
		}
	}
	if !known {
		t.Fatal("bob's own CARD_KNOWN row is missing from his window")
	}
	// A viewer holding no seat sees the public stream only.
	stranger := SeatViewer()
	all, err := s.EventsFor(ctx, game, stranger, 0, 0)
	if err != nil {
		t.Fatalf("events for stranger: %v", err)
	}
	for i := range all {
		if all[i].Visibility == VisibilitySeat {
			t.Fatalf("stranger received a seat-visible %s row", all[i].Kind)
		}
	}
	// The owner sees everything, as ever — strictly more than a seated
	// viewer, who sees strictly more than a viewer holding no seat.
	ownerEvs, err := s.EventsFor(ctx, game, OwnerViewer(), 0, 0)
	if err != nil {
		t.Fatalf("events for owner: %v", err)
	}
	if len(ownerEvs) <= len(evs) || len(evs) <= len(all) {
		t.Fatalf("row counts broke the inclusion order: owner %d, stranger %d, bob %d", len(ownerEvs), len(all), len(evs))
	}
}

// TestStateForFoldsTheSeatsOwnGame is the acceptance fold: a seat's
// state is the public game plus its own hidden zones — its deck and
// hand knowledge intact, every other seat's redacted, hand and library
// counts public, and the battlefield identical to the owner's.
func TestStateForFoldsTheSeatsOwnGame(t *testing.T) {
	s, _, ctx, game := newPodGame(t)

	full, err := s.State(ctx, game)
	if err != nil {
		t.Fatalf("owner state: %v", err)
	}
	bob, err := s.StateFor(ctx, game, SeatViewer(2))
	if err != nil {
		t.Fatalf("bob state: %v", err)
	}
	p := bob.Seats[2]
	if len(p.Deck) == 0 || len(p.LibraryComp) == 0 || !p.LibraryExact {
		t.Fatal("bob's own deck is missing from his fold")
	}
	if len(p.HandKnown) != 2 {
		t.Fatalf("bob's hand knowledge = %v", p.HandKnown)
	}
	if !p.Hand.Known || p.Hand.N != 2 {
		t.Fatalf("bob's hand count = %+v", p.Hand)
	}
	for seat := 1; seat <= 4; seat++ {
		if seat == 2 {
			continue
		}
		q := bob.Seats[seat]
		if len(q.HandKnown) != 0 {
			t.Fatalf("seat %d's hand knowledge leaked into bob's fold: %v", seat, q.HandKnown)
		}
		if q.Deck != nil || q.LibraryComp != nil || q.LibraryExact {
			t.Fatalf("seat %d's library composition leaked into bob's fold", seat)
		}
		if q.Hand.Known != full.Seats[seat].Hand.Known || q.Hand.N != full.Seats[seat].Hand.N {
			t.Fatalf("seat %d's hand count should stay public: bob %+v, owner %+v", seat, q.Hand, full.Seats[seat].Hand)
		}
		// Library size is public knowledge (the echo carries it);
		// composition is not.
		if !q.Library.Known || q.Library.N != full.Seats[seat].Library.N {
			t.Fatalf("seat %d's library size should stay public: bob %+v, owner %+v", seat, q.Library, full.Seats[seat].Library)
		}
	}
	// The public game is the same game for everyone: same seats, same
	// life, same battlefield, same turn.
	if bob.Turn != full.Turn || bob.TurnSeat != full.TurnSeat || bob.Phase != full.Phase {
		t.Fatalf("bob's turn state diverged: %+v vs %+v", bob, full)
	}
	if len(bob.Objects) != len(full.Objects) {
		t.Fatalf("bob sees %d objects, owner %d", len(bob.Objects), len(full.Objects))
	}
}

// TestStateForRedactsLegacyRows is the belt over the brass: games
// whose public GAME_STARTED predates the split carried decks on the
// public row, and the redaction pass zeroes them for a scoped reader
// whatever row they rode in on.
func TestStateForRedactsLegacyRows(t *testing.T) {
	s, db, ctx, game := newPodGame(t)
	// Hand-craft one legacy row: a second game whose GAME_STARTED
	// echoes a deck publicly, exactly as rows written before MAD-337.
	legacy := "legacy-game"
	if _, err := db.Exec(`INSERT INTO mtg_games (id, owner_id, name, format, starting_life, status, settings, created_at, updated_at)
		VALUES (?, 'u1', 'Legacy', 'commander', 40, 'active', '{}', ?, ?)`,
		legacy, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed legacy game: %v", err)
	}
	sc := SeatConfig{Seat: 1, Name: "Old", Deck: map[string]int{"Seat One Secret": 4}}
	payload, err := json.Marshal(Event{Kind: EventGameStarted, Visibility: VisibilityPublic,
		Seats: []SeatConfig{sc}, Format: "commander", StartingLife: 40, Ord: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO mtg_events (id, game_id, ord, kind, payload, visibility, created_at)
		VALUES ('le1', ?, 1, 'GAME_STARTED', ?, 'public', ?)`,
		legacy, string(payload), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed legacy event: %v", err)
	}
	st, err := s.StateFor(ctx, legacy, SeatViewer(2))
	if err != nil {
		t.Fatalf("legacy state: %v", err)
	}
	if p := st.Seats[1]; p.Deck != nil || p.LibraryComp != nil || p.LibraryExact {
		t.Fatal("legacy public-echoed deck reached a scoped fold")
	}
	// The owner still folds it — history did not change.
	owner, err := s.State(ctx, legacy)
	if err != nil {
		t.Fatalf("legacy owner state: %v", err)
	}
	if len(owner.Seats[1].Deck) != 1 {
		t.Fatal("owner lost the legacy deck")
	}
	_ = game
}

/* ---------- joining ---------- */

func TestJoinGameBindsASeat(t *testing.T) {
	s, db, ctx, _ := newPodGame(t)
	// A fresh setup table — the pod fixture already started, and seats
	// close when play begins.
	fresh, err := s.CreateGame(ctx, "u1", "Fifth Wheel", "commander", 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.JoinCode) != joinCodeLen {
		t.Fatalf("join code = %q", fresh.JoinCode)
	}
	// A fifth account joins and lands in the next seat.
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, is_admin, created_at)
		VALUES ('u5', 'eve', 'x', 0, ?)`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	jg, seat, err := s.JoinGame(ctx, fresh.JoinCode, "u5", "Eve")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if seat != 1 || jg.ID != fresh.ID {
		t.Fatalf("join → seat %d game %s", seat, jg.ID)
	}
	seats, err := s.SeatsForUser(ctx, fresh.ID, "u5")
	if err != nil || len(seats) != 1 || seats[0] != 1 {
		t.Fatalf("seats for u5 = %v err %v", seats, err)
	}
	// Redeeming again is the same seat, not a new one — typed however
	// the participant typed it.
	_, again, err := s.JoinGame(ctx, strings.ToLower("-"+fresh.JoinCode[:3]+"-"+fresh.JoinCode[3:]), "u5", "Eve")
	if err != nil || again != 1 {
		t.Fatalf("rejoin → seat %d err %v", again, err)
	}
	// The owner joining their own game answers the game, seats nobody.
	_, ownerSeat, err := s.JoinGame(ctx, fresh.JoinCode, "u1", "Collin")
	if err != nil || ownerSeat != 0 {
		t.Fatalf("owner join → seat %d err %v", ownerSeat, err)
	}
	// A wrong code is a missing game.
	if _, _, err := s.JoinGame(ctx, "NOPE01", "u5", "Eve"); err == nil {
		t.Fatal("bad code joined")
	}
}

func TestJoinGameClosesWhenPlayBegins(t *testing.T) {
	s, _, ctx, game := newPodGame(t)
	// The pod game is active already (started in the fixture).
	if _, _, err := s.JoinGame(ctx, mustCode(t, s, ctx, game), "u4", "Dave"); err == nil {
		t.Fatal("joined an active game")
	}
	// A fresh setup game joins fine, then closes after start.
	fresh, err := s.CreateGame(ctx, "u1", "Second Table", "commander", 40)
	if err != nil {
		t.Fatal(err)
	}
	if _, seat, err := s.JoinGame(ctx, fresh.JoinCode, "u2", "Bob"); err != nil || seat != 1 {
		t.Fatalf("setup join → seat %d err %v", seat, err)
	}
	if _, _, err := s.StartGame(ctx, fresh.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, _, err := s.JoinGame(ctx, fresh.JoinCode, "u3", "Alice"); err == nil {
		t.Fatal("joined after start")
	}
}

func TestLegacyGamesHaveNoCode(t *testing.T) {
	s, db, ctx, _ := newPodGame(t)
	if _, err := db.Exec(`INSERT INTO mtg_games (id, owner_id, name, format, starting_life, status, settings, created_at, updated_at)
		VALUES ('oldgame', 'u1', 'Old', 'commander', 40, 'setup', '{}', ?, ?)`,
		time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetGame(ctx, "oldgame")
	if err != nil {
		t.Fatal(err)
	}
	if g.JoinCode != "" {
		t.Fatalf("legacy game carries code %q", g.JoinCode)
	}
}

func mustCode(t *testing.T, s *Store, ctx context.Context, game string) string {
	t.Helper()
	g, err := s.GetGame(ctx, game)
	if err != nil {
		t.Fatal(err)
	}
	return g.JoinCode
}

/* ---------- notes ---------- */

func TestSeatNotesArePerSeat(t *testing.T) {
	s, _, ctx, game := newPodGame(t)
	if err := s.SetSeatNote(ctx, game, 2, "keep the sweeper for Dave"); err != nil {
		t.Fatalf("set note: %v", err)
	}
	body, err := s.SeatNote(ctx, game, 2)
	if err != nil || body != "keep the sweeper for Dave" {
		t.Fatalf("note = %q err %v", body, err)
	}
	// Latest-wins.
	if err := s.SetSeatNote(ctx, game, 2, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if body, _ := s.SeatNote(ctx, game, 2); body != "changed my mind" {
		t.Fatalf("note = %q", body)
	}
	// Another seat's pad is not blank-shared state.
	if body, _ := s.SeatNote(ctx, game, 3); body != "" {
		t.Fatalf("seat 3 note = %q", body)
	}
	// A seat that is not in the game is rejected.
	if err := s.SetSeatNote(ctx, game, 9, "x"); err == nil {
		t.Fatal("note accepted for a missing seat")
	}
	// The cap holds.
	if err := s.SetSeatNote(ctx, game, 2, strings.Repeat("x", noteCap+1)); err == nil {
		t.Fatal("oversized note accepted")
	}
}

/* ---------- small viewer unit tests ---------- */

func TestViewerEntitlement(t *testing.T) {
	owner := OwnerViewer()
	if !owner.SeesSeat(4) || !owner.SeesEvent(Event{Visibility: VisibilitySeat, VisibleSeat: 4}) {
		t.Fatal("owner must see every seat's rows")
	}
	bob := SeatViewer(2)
	if bob.SeesSeat(1) || !bob.SeesSeat(2) {
		t.Fatal("seat viewer entitlement is wrong")
	}
	if bob.SeesEvent(Event{Visibility: VisibilitySeat, VisibleSeat: 1}) {
		t.Fatal("seat viewer must not see another seat's row")
	}
	if !bob.SeesEvent(Event{Kind: EventCardDrawn}) {
		t.Fatal("public rows are everyone's")
	}
	got := FilterEvents(bob, []Event{
		{Kind: EventCardDrawn, Ord: 1},
		{Kind: EventCardKnown, Visibility: VisibilitySeat, VisibleSeat: 1, Ord: 2},
		{Kind: EventCardKnown, Visibility: VisibilitySeat, VisibleSeat: 2, Ord: 3},
	})
	if len(got) != 2 || got[0].Ord != 1 || got[1].Ord != 3 {
		t.Fatalf("FilterEvents = %+v", got)
	}
}

var _ = sql.ErrNoRows
