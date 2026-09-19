package engine

// Replay's store tests (MAD-339): StateAt is the scrub's whole backend,
// and its contract is the prefix property the fold already promises —
// StateAt(n) equals Fold(log[:n]) for every n, scoped in SQL exactly
// the way StateFor scopes, and clamped past the head the way the
// stream's reconnect forgives a stale cursor.

import (
	"testing"
)

func TestStateAtFoldsThePrefix(t *testing.T) {
	s, _, ctx, game := newGame(t)
	if _, _, err := s.StartGame(ctx, game); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Walk a few turns of play onto the log.
	mustSubmit(t, s, ctx, game, Action{Kind: ActionAdvance, Seat: 1})
	mustSubmit(t, s, ctx, game, Action{Kind: ActionDraw, Seat: 1, Count: 2, Cards: []string{"Forest", "Cultivate"}})
	mustSubmit(t, s, ctx, game, Action{Kind: ActionPlayLand, Seat: 1, Card: "Forest",
		Base: &BaseChars{Name: "Forest", Types: []string{"Land"}}})

	evs, err := s.Events(ctx, game, 0, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	head := int64(len(evs))
	if head < 6 {
		t.Fatalf("fixture too thin: %d rows", head)
	}

	// The prefix property, at every ordinal the log holds.
	for at := int64(0); at <= head; at++ {
		want := Fold(evs[:at])
		got, err := s.StateAt(ctx, game, OwnerViewer(), at)
		if err != nil {
			t.Fatalf("state at %d: %v", at, err)
		}
		if got.LastOrd != at {
			t.Fatalf("state at %d folded through %d", at, got.LastOrd)
		}
		if !statesEqual(got, want) {
			t.Fatalf("state at %d diverged from the prefix fold", at)
		}
	}

	// Past the head reads the present; negative is rejected.
	got, err := s.StateAt(ctx, game, OwnerViewer(), head+50)
	if err != nil {
		t.Fatalf("past head: %v", err)
	}
	if got.LastOrd != head {
		t.Fatalf("past head folded through %d, want %d", got.LastOrd, head)
	}
	if _, err := s.StateAt(ctx, game, OwnerViewer(), -1); err == nil {
		t.Fatal("negative ordinal accepted")
	}
}

func TestStateAtScopesLikeEveryRead(t *testing.T) {
	s, _, ctx, game := newGame(t)
	if _, _, err := s.StartGame(ctx, game); err != nil {
		t.Fatalf("start: %v", err)
	}
	mustSubmit(t, s, ctx, game, Action{Kind: ActionAdvance, Seat: 1})
	// Seat 2 draws two named cards: the identities ride seat-visible
	// CARD_KNOWN rows, invisible to every other seat at every ordinal.
	mustSubmit(t, s, ctx, game, Action{Kind: ActionDraw, Seat: 2, Count: 2, Cards: []string{"Seat Two Secret", "Island"}})

	evs, err := s.Events(ctx, game, 0, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for at := int64(1); at <= int64(len(evs)); at++ {
		seat1, err := s.StateAt(ctx, game, SeatViewer(1), at)
		if err != nil {
			t.Fatalf("seat 1 at %d: %v", at, err)
		}
		for _, name := range seat1.Seats[2].HandKnown {
			if name == "Seat Two Secret" {
				t.Fatalf("seat 1 folded seat 2's identity at ordinal %d", at)
			}
		}
		if seat1.Seats[2].Deck != nil {
			t.Fatalf("seat 1 folded seat 2's deck at ordinal %d", at)
		}
		seat2, err := s.StateAt(ctx, game, SeatViewer(2), at)
		if err != nil {
			t.Fatalf("seat 2 at %d: %v", at, err)
		}
		if seat2.LastOrd > at {
			t.Fatalf("seat 2 folded past %d", at)
		}
	}
	// The seat itself sees its own identity the moment the row lands.
	seat2, err := s.StateAt(ctx, game, SeatViewer(2), int64(len(evs)))
	if err != nil {
		t.Fatalf("seat 2 head: %v", err)
	}
	found := false
	for _, name := range seat2.Seats[2].HandKnown {
		if name == "Seat Two Secret" {
			found = true
		}
	}
	if !found {
		t.Fatal("seat 2 cannot see its own drawn identity")
	}
}

// statesEqual compares two folds through the parts a scrub renders:
// lifecycle, seats, board, stack, position. A byte compare would trip
// on map ordering; this is the board's own notion of same.
func statesEqual(a, b *State) bool {
	if a.Status != b.Status || a.Turn != b.Turn || a.TurnSeat != b.TurnSeat ||
		a.Phase != b.Phase || a.Step != b.Step || a.PrioritySeat != b.PrioritySeat ||
		len(a.Stack) != len(b.Stack) || len(a.Objects) != len(b.Objects) ||
		len(a.Order) != len(b.Order) {
		return false
	}
	for seat, pa := range a.Seats {
		pb, ok := b.Seats[seat]
		if !ok {
			return false
		}
		if pa.Life != pb.Life || pa.Alive != pb.Alive || pa.Hand != pb.Hand ||
			pa.Library != pb.Library || len(pa.HandKnown) != len(pb.HandKnown) ||
			len(pa.Deck) != len(pb.Deck) {
			return false
		}
	}
	for id, oa := range a.Objects {
		ob, ok := b.Objects[id]
		if !ok || oa.Zone != ob.Zone || oa.Controller != ob.Controller || oa.Tapped != ob.Tapped {
			return false
		}
	}
	return true
}
