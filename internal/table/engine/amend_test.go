package engine

// Amend's tests (MAD-328): the batch stamp makes one action's rows
// exactly addressable, AmendAt replaces a batch atomically — contiguous
// ordinals, honest lifecycle, the correction folded in — and a rejected
// correction leaves the log byte-identical.

import (
	"reflect"
	"testing"
)

func TestBatchBoundsFindsOneSubmitsRows(t *testing.T) {
	cause := `{"kind":"ADVANCE","seat":1}`
	evs := []Event{
		{Ord: 1, Batch: "b1", Cause: cause},
		{Ord: 2, Batch: "b1", Cause: cause},
		{Ord: 3, Batch: "b2", Cause: cause}, // identical action, new Submit
		{Ord: 4, Batch: "b3", Cause: `{"kind":"DRAW","seat":1,"count":1}`},
		{Ord: 5, Batch: "b3", Cause: `{"kind":"DRAW","seat":1,"count":1}`},
	}
	for _, tc := range []struct {
		ord    int64
		lo, hi int
	}{
		{1, 0, 2},
		{2, 0, 2},
		{3, 2, 3}, // the identical repeat is its own batch — undo removes one, not both
		{4, 3, 5},
		{5, 3, 5},
	} {
		lo, hi, ok := batchBounds(evs, tc.ord)
		if !ok || lo != tc.lo || hi != tc.hi {
			t.Fatalf("bounds(%d) = [%d,%d) ok=%v, want [%d,%d)", tc.ord, lo, hi, ok, tc.lo, tc.hi)
		}
	}
	if _, _, ok := batchBounds(evs, 9); ok {
		t.Fatal("an ordinal the log does not hold has no batch")
	}

	// Rows from before the stamp fall back to cause equality: the best a
	// repeating cause allows, and the boundary the stamp draws is honest.
	legacy := []Event{
		{Ord: 1, Cause: cause},
		{Ord: 2, Cause: cause},
		{Ord: 3, Batch: "b1", Cause: cause},
	}
	lo, hi, ok := batchBounds(legacy, 2)
	if !ok || lo != 0 || hi != 2 {
		t.Fatalf("legacy bounds = [%d,%d) ok=%v, want [0,2)", lo, hi, ok)
	}
	lo, hi, ok = batchBounds(legacy, 3)
	if !ok || lo != 2 || hi != 3 {
		t.Fatalf("stamped-after-legacy bounds = [%d,%d) ok=%v, want [2,3)", lo, hi, ok)
	}
}

func TestStoreSubmitsStampDistinctBatches(t *testing.T) {
	s, _, ctx, gameID := newGame(t)
	if _, _, err := s.StartGame(ctx, gameID); err != nil {
		t.Fatal(err)
	}
	first := mustSubmit(t, s, ctx, gameID, Action{Kind: ActionChangeLife, Seat: 1, TargetSeat: 1, Delta: -1})
	second := mustSubmit(t, s, ctx, gameID, Action{Kind: ActionChangeLife, Seat: 1, TargetSeat: 1, Delta: -1})
	if first[0].Batch == "" || first[0].Batch != first[len(first)-1].Batch {
		t.Fatalf("one submit's rows must share one stamp: %+v", first)
	}
	if first[0].Batch == second[0].Batch {
		t.Fatal("identical consecutive actions share a stamp — batches would merge")
	}
}

func TestStoreAmendReplacesTheBatch(t *testing.T) {
	s, _, ctx, gameID := newGame(t)
	if _, _, err := s.StartGame(ctx, gameID); err != nil {
		t.Fatal(err)
	}
	mustSubmit(t, s, ctx, gameID, Action{Kind: ActionAdvance, Seat: 1})
	mustSubmit(t, s, ctx, gameID, Action{Kind: ActionAdvance, Seat: 1})
	mustSubmit(t, s, ctx, gameID, Action{Kind: ActionDraw, Seat: 1, Count: 2})
	// The misidentified card: cast Cultivate, meant something else.
	castEvs := mustSubmit(t, s, ctx, gameID, Action{Kind: ActionCast, Seat: 1, Card: "Cultivate", FromZone: ZoneHand})
	mustSubmit(t, s, ctx, gameID, Action{Kind: ActionChangeLife, Seat: 2, TargetSeat: 2, Delta: -3})
	if _, err := s.Events(ctx, gameID, 0, 0); err != nil {
		t.Fatal(err)
	}

	amended, state, err := s.AmendAt(ctx, gameID, castEvs[0].Ord, Action{
		Kind: ActionCast, Seat: 1, Card: "Smothering Tithe", FromZone: ZoneHand})
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	after, err := s.Events(ctx, gameID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(after)) != castEvs[0].Ord-1+int64(len(amended)) {
		t.Fatalf("log = %d rows, want the %d before the batch + the %d amended",
			len(after), castEvs[0].Ord-1, len(amended))
	}
	// The correction lands exactly where the amended batch began.
	if amended[0].Ord != castEvs[0].Ord {
		t.Fatalf("amended rows start at %d, want %d", amended[0].Ord, castEvs[0].Ord)
	}
	for i, e := range after {
		if e.Ord != int64(i+1) {
			t.Fatalf("ord %d at position %d — amend must keep ordinals contiguous", e.Ord, i)
		}
	}
	// Everything after the amended batch is gone, exactly as a rewind to
	// the same point would take it.
	if state.Seats[2].Life != 40 {
		t.Fatalf("seat 2 life = %d, want 40 — the later change must not survive", state.Seats[2].Life)
	}
	if !reflect.DeepEqual(state, Fold(after)) {
		t.Fatal("amended state is not the refolded log")
	}
	// The stack carries the corrected card.
	onStack := false
	for _, it := range state.Stack {
		if it.Card == "Smothering Tithe" {
			onStack = true
		}
	}
	if !onStack {
		t.Fatalf("stack = %+v, want Smothering Tithe on it", state.Stack)
	}
}

func TestStoreAmendRejectedWritesNothing(t *testing.T) {
	s, _, ctx, gameID := newGame(t)
	if _, _, err := s.StartGame(ctx, gameID); err != nil {
		t.Fatal(err)
	}
	life := mustSubmit(t, s, ctx, gameID, Action{Kind: ActionChangeLife, Seat: 1, TargetSeat: 1, Delta: -5})
	before, err := s.Events(ctx, gameID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// A correction the prefix cannot validate — seat 9 does not exist.
	if _, _, err := s.AmendAt(ctx, gameID, life[0].Ord, Action{Kind: ActionChangeLife, Seat: 9, TargetSeat: 9, Delta: -1}); err == nil {
		t.Fatal("an invalid correction was accepted")
	}
	after, err := s.Events(ctx, gameID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a rejected amend changed the log: %d → %d rows", len(before), len(after))
	}
	st, err := s.State(ctx, gameID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Seats[1].Life != 35 {
		t.Fatalf("life = %d, want 35 — the amended change must still stand", st.Seats[1].Life)
	}

	// Ordinals the log does not hold are rejected, not clamped.
	if _, _, err := s.AmendAt(ctx, gameID, 999, Action{Kind: ActionAdvance, Seat: 1}); err == nil {
		t.Fatal("amend past the head accepted")
	}
	if _, _, err := s.AmendAt(ctx, gameID, 0, Action{Kind: ActionAdvance, Seat: 1}); err == nil {
		t.Fatal("amend at zero accepted")
	}
}

func TestStoreAmendPastStartRestatesLifecycle(t *testing.T) {
	s, _, ctx, gameID := newGame(t)
	startEvs, _, err := s.StartGame(ctx, gameID)
	if err != nil {
		t.Fatal(err)
	}
	mustSubmit(t, s, ctx, gameID, Action{Kind: ActionChangeLife, Seat: 1, TargetSeat: 1, Delta: -5})

	// Amending the GAME_STARTED batch with a non-start correction hands
	// the game back to setup, the posture a rewind to zero would hold —
	// and the rejected shape (anything but a start) leaves it there
	// without touching the log.
	if _, _, err := s.AmendAt(ctx, gameID, startEvs[0].Ord, Action{Kind: ActionDraw, Seat: 1, Count: 1}); err == nil {
		t.Fatal("a non-start correction past GAME_STARTED was accepted")
	}
	g, _ := s.GetGame(ctx, gameID)
	if g.Status != StatusActive {
		t.Fatalf("rejected amend changed the lifecycle: %s", g.Status)
	}

	// The honest shape: re-submitting the start amends the whole log
	// away and replays it, active again, ordinals from 1.
	seats, err := s.Seats(ctx, gameID)
	if err != nil {
		t.Fatal(err)
	}
	restart := Action{Kind: ActionStartGame, Source: "system", Seats: seats, Format: "commander", StartingLife: 40}
	evs, st, err := s.AmendAt(ctx, gameID, startEvs[0].Ord, restart)
	if err != nil {
		t.Fatalf("amend the start: %v", err)
	}
	if evs[0].Ord != 1 || evs[0].Kind != EventGameStarted {
		t.Fatalf("amended start = ord %d %s, want ord 1 GAME_STARTED", evs[0].Ord, evs[0].Kind)
	}
	g, _ = s.GetGame(ctx, gameID)
	if g.Status != StatusActive {
		t.Fatalf("status = %s, want active", g.Status)
	}
	if st.Seats[1].Life != 40 {
		t.Fatalf("life = %d, want the restarted 40", st.Seats[1].Life)
	}
}
