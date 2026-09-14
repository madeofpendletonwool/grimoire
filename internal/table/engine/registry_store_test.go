package engine

// The trigger registry's store half (MAD-335): the registration surface
// over the real table, and Submit firing registered triggers end to end
// — the acceptance shape: a registered trigger fires on the right
// structural event, queues, and reaches the stack, over a live log.

import (
	"context"
	"testing"
)

func TestRegisterTriggerUpsertsAndLists(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	row, err := s.RegisterTrigger(ctx, "Rhystic Study", TriggerOppCasts, "may draw a card", "declared", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if row.Card != "Rhystic Study" || row.Event != TriggerOppCasts || row.Origin != "declared" {
		t.Fatalf("row = %+v", row)
	}
	// The upsert: the same (card, kind) replaces, keeping one row.
	if _, err := s.RegisterTrigger(ctx, "Rhystic Study", TriggerOppCasts,
		"opponent may pay {1}, else draw", "confirmed", "u1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rows, err := s.TriggerRows(ctx, "Rhystic Study")
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(rows) != 1 || rows[0].Effect != "opponent may pay {1}, else draw" || rows[0].Origin != "confirmed" {
		t.Fatalf("rows = %+v", rows)
	}
	// Validation: unknown kind, empty card, empty effect, bad origin.
	if _, err := s.RegisterTrigger(ctx, "X", TriggerEvent("NOPE"), "e", "declared", ""); err == nil {
		t.Error("unknown kind accepted")
	}
	if _, err := s.RegisterTrigger(ctx, "  ", TriggerUpkeep, "e", "declared", ""); err == nil {
		t.Error("empty card accepted")
	}
	if _, err := s.RegisterTrigger(ctx, "X", TriggerUpkeep, "  ", "declared", ""); err == nil {
		t.Error("empty effect accepted")
	}
	if _, err := s.RegisterTrigger(ctx, "X", TriggerUpkeep, "e", "guessed", ""); err == nil {
		t.Error("bad origin accepted")
	}
	// Delete, and the missing-row answer.
	if err := s.DeleteTrigger(ctx, "Rhystic Study", TriggerOppCasts); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteTrigger(ctx, "Rhystic Study", TriggerOppCasts); err == nil {
		t.Error("deleting a missing row succeeded")
	}
}

func TestSubmitFiresRegisteredTriggerEndToEnd(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.RegisterTrigger(ctx, "Rhystic Study", TriggerOppCasts, "may draw a card", "declared", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	game, err := s.CreateGame(ctx, "u1", "Friday Commander", "commander", 40)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SeatPlayer(ctx, game.ID, 1, "Collin", "u1", "", "Atraxa, Praetors' Voice", nil); err != nil {
		t.Fatalf("seat 1: %v", err)
	}
	if err := s.SeatPlayer(ctx, game.ID, 2, "Bob", "", "", "Krenko, Mob Boss", nil); err != nil {
		t.Fatalf("seat 2: %v", err)
	}
	if _, _, err := s.StartGame(ctx, game.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Walk untap → upkeep → draw into seat 1's precombat main, where
	// casting is legal.
	for i := 0; i < 3; i++ {
		mustSubmit(t, s, ctx, game.ID, Action{Kind: ActionAdvance, Seat: 1})
	}
	// Seat 1's main: Rhystic lands (cast + resolve through the store).
	mustSubmit(t, s, ctx, game.ID, Action{Kind: ActionCast, Seat: 1, Card: "Rhystic Study",
		Base: &BaseChars{Name: "Rhystic Study", Types: []string{"Enchantment"}}})
	for {
		st, err := s.State(ctx, game.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Stack) == 0 {
			break
		}
		mustSubmit(t, s, ctx, game.ID, Action{Kind: ActionPassPriority, Seat: st.PrioritySeat})
	}
	// Pass to seat 2; their cast fires the Rhystic under seat 1.
	for {
		st, err := s.State(ctx, game.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st.PrioritySeat == 2 {
			break
		}
		mustSubmit(t, s, ctx, game.ID, Action{Kind: ActionPassPriority, Seat: st.PrioritySeat})
	}
	evs, st, err := s.Submit(ctx, game.ID, Action{Kind: ActionCast, Seat: 2, Card: "Sol Ring",
		Base: &BaseChars{Name: "Sol Ring", Types: []string{"Artifact"}}})
	if err != nil {
		t.Fatalf("opponent cast: %v", err)
	}
	fired := 0
	for _, e := range evs {
		if e.Kind == EventTriggerFired && e.Card == "Rhystic Study" {
			fired++
		}
	}
	if fired != 1 {
		t.Fatalf("fired %d Rhystic rows, want 1 (%s)", fired, kindsOf(evs))
	}
	if len(st.TriggerQueue) != 0 {
		t.Fatalf("queue = %+v, want flushed", st.TriggerQueue)
	}
	onStack := false
	for _, it := range st.Stack {
		if it.Mode == "triggered" && it.Card == "Rhystic Study" && it.Controller == 1 {
			onStack = true
		}
	}
	if !onStack {
		t.Fatalf("the fired trigger never reached the stack: %+v", st.Stack)
	}
	// The fold of the stored log reproduces the same position — the
	// fired row is log truth, registry or no registry.
	all, err := s.Events(ctx, game.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	refolded := Fold(all)
	if len(refolded.TriggerQueue) != len(st.TriggerQueue) || len(refolded.Stack) != len(st.Stack) {
		t.Fatalf("refold queue/stack = %d/%d, want %d/%d",
			len(refolded.TriggerQueue), len(refolded.Stack), len(st.TriggerQueue), len(st.Stack))
	}
	// Nudges: the unresolved Rhystic is the don't-forget the pane shows.
	nudges, err := s.Nudges(ctx, game.ID)
	if err != nil {
		t.Fatalf("nudges: %v", err)
	}
	saw := false
	for _, n := range nudges {
		if n.Kind == "unresolved" && n.Card == "Rhystic Study" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("nudges = %+v, want the unresolved Rhystic", nudges)
	}
}

func TestOrderTriggersThroughTheStore(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	game, err := s.CreateGame(ctx, "u1", "Friday Commander", "commander", 40)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SeatPlayer(ctx, game.ID, 1, "Collin", "u1", "", "A", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SeatPlayer(ctx, game.ID, 2, "Bob", "", "", "B", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.StartGame(ctx, game.ID); err != nil {
		t.Fatal(err)
	}
	// Untap: no priority, so declarations wait in the queue.
	mustSubmit(t, s, ctx, game.ID, Action{Kind: ActionDeclareTrigger, Seat: 1, Card: "A", Effect: "first"})
	mustSubmit(t, s, ctx, game.ID, Action{Kind: ActionDeclareTrigger, Seat: 1, Card: "C", Effect: "second"})
	st, err := s.State(ctx, game.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.TriggerQueue) != 2 {
		t.Fatalf("queue = %d", len(st.TriggerQueue))
	}
	// The fired ords are real ords now; seat 1 flips their order.
	c, a := st.TriggerQueue[1].FiredOrd, st.TriggerQueue[0].FiredOrd
	_, st, err = s.Submit(ctx, game.ID, Action{Kind: ActionOrderTriggers, Seat: 1, Order: []int64{c, a}})
	if err != nil {
		t.Fatalf("order: %v", err)
	}
	if st.TriggerQueue[0].Card != "C" || st.TriggerQueue[1].Card != "A" {
		t.Fatalf("queue = %+v", st.TriggerQueue)
	}
}
