package engine

import "testing"

// The full step sequence in CR order, walked by ADVANCE alone. The first
// turn skips the draw step (CR 103.7a); every turn after walks all twelve.
func TestAdvanceWalksFullTurns(t *testing.T) {
	s := start(t, nil)
	type restep struct{ phase, step string }
	want := []restep{
		{"beginning", "untap"}, {"beginning", "upkeep"}, // turn 1: no draw
		{"precombat_main", "main"},
		{"combat", "beginning_of_combat"}, {"combat", "declare_attackers"},
		{"combat", "declare_blockers"}, {"combat", "combat_damage"},
		{"combat", "end_of_combat"},
		{"postcombat_main", "main"},
		{"end", "end"}, {"end", "cleanup"},
	}
	for _, w := range want {
		if s.Phase != w.phase || s.Step != w.step {
			t.Fatalf("turn 1 at %s/%s, want %s/%s", s.Phase, s.Step, w.phase, w.step)
		}
		act(t, s, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
	}
	// Leaving cleanup ends the turn and starts the next seat's.
	if s.Turn != 2 || s.TurnSeat != 2 || s.Phase != "beginning" || s.Step != "untap" {
		t.Fatalf("turn 2 = %d/%d at %s/%s", s.Turn, s.TurnSeat, s.Phase, s.Step)
	}
	if s.PrioritySeat != 2 {
		t.Fatalf("priority = %d", s.PrioritySeat)
	}
	full := []restep{
		{"beginning", "untap"}, {"beginning", "upkeep"}, {"beginning", "draw"},
		{"precombat_main", "main"},
		{"combat", "beginning_of_combat"}, {"combat", "declare_attackers"},
		{"combat", "declare_blockers"}, {"combat", "combat_damage"},
		{"combat", "end_of_combat"},
		{"postcombat_main", "main"},
		{"end", "end"}, {"end", "cleanup"},
	}
	for _, w := range full {
		if s.Phase != w.phase || s.Step != w.step {
			t.Fatalf("turn 2 at %s/%s, want %s/%s", s.Phase, s.Step, w.phase, w.step)
		}
		act(t, s, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
	}
	if s.Turn != 3 || s.TurnSeat != 3 {
		t.Fatalf("turn 3 = %d/%d", s.Turn, s.TurnSeat)
	}
	// Wrap around the table.
	for i := 0; i < 24; i++ {
		act(t, s, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
	}
	if s.Turn != 5 || s.TurnSeat != 2 {
		t.Fatalf("after 2 more turns: %d/%d", s.Turn, s.TurnSeat)
	}
}

func TestAdvanceResetsLandDropPerTurn(t *testing.T) {
	s := start(t, nil)
	land(t, s, 1, "Forest")
	rejected(t, s, Action{Kind: ActionPlayLand, Seat: 1, Card: "Forest"})
	// Walk around the table back to seat 1's next turn: the drop resets.
	for i := 0; i < 36; i++ {
		act(t, s, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
	}
	if s.Turn != 4 || s.TurnSeat != 1 {
		t.Fatalf("turn = %d/%d", s.Turn, s.TurnSeat)
	}
	if s.Seats[1].LandsThisTurn != 0 {
		t.Fatalf("land drop did not reset: %d", s.Seats[1].LandsThisTurn)
	}
	land(t, s, 1, "Forest")
}

func TestUntilEndOfTurnExpiresAtCleanup(t *testing.T) {
	s := start(t, nil)
	bear := summon(t, s, 1, "Bear", 2, 2)
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: UntilEOT, SourceCard: "Giant Growth",
		Delta: Delta{Power: intPtr(3), Toughness: intPtr(3)}}})
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: PermanentDuration, SourceCard: "Hardened Scales-ish",
		Delta: Delta{Power: intPtr(1), Toughness: intPtr(1)}}})
	act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 2, TargetObject: bear})
	if c := s.Characteristics(bear); *c.Power != 6 || *c.Toughness != 6 {
		t.Fatalf("grown bear = %d/%d", *c.Power, *c.Toughness)
	}
	// ADVANCE through to the next turn: TURN_ENDED expires the growth,
	// clears marked damage, keeps the permanent modifier.
	for i := 0; i < 12; i++ {
		act(t, s, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
	}
	o := s.Objects[bear]
	if o.Damage != 0 {
		t.Fatalf("damage survived cleanup: %d", o.Damage)
	}
	if len(o.Modifiers) != 1 || o.Modifiers[0].Duration != PermanentDuration {
		t.Fatalf("modifiers = %+v", o.Modifiers)
	}
	if c := s.Characteristics(bear); *c.Power != 3 || *c.Toughness != 3 {
		t.Fatalf("post-cleanup bear = %d/%d", *c.Power, *c.Toughness)
	}
}

func TestPassPriorityRotation(t *testing.T) {
	s := start(t, nil)
	act(t, s, Action{Kind: ActionPassPriority, Seat: 1})
	if s.PrioritySeat != 2 || len(s.Passed) != 1 {
		t.Fatalf("priority = %d passed = %v", s.PrioritySeat, s.Passed)
	}
	act(t, s, Action{Kind: ActionPassPriority, Seat: 2})
	if s.PrioritySeat != 3 {
		t.Fatalf("priority = %d", s.PrioritySeat)
	}
	// Third pass empties the stack-less step: the step ends, exactly as
	// ADVANCE would have walked it, and the active player holds priority.
	act(t, s, Action{Kind: ActionPassPriority, Seat: 3})
	if s.Step != "upkeep" {
		t.Fatalf("step = %s", s.Step)
	}
	if s.PrioritySeat != s.TurnSeat {
		t.Fatalf("priority = %d", s.PrioritySeat)
	}
}

func TestPassPriorityResolvesStack(t *testing.T) {
	s := start(t, nil)
	act(t, s, Action{Kind: ActionCast, Seat: 2, Card: "Grizzly Bears",
		Base: &BaseChars{Types: []string{"Creature"}, Power: intPtr(2), Toughness: intPtr(2)}})
	// Any push clears the pass cycle.
	if len(s.Passed) != 0 {
		t.Fatalf("passed = %v after a cast", s.Passed)
	}
	act(t, s, Action{Kind: ActionPassPriority, Seat: 1})
	act(t, s, Action{Kind: ActionPassPriority, Seat: 2})
	if len(s.Stack) != 1 {
		t.Fatal("stack resolved early")
	}
	act(t, s, Action{Kind: ActionPassPriority, Seat: 3})
	if len(s.Stack) != 0 {
		t.Fatal("all-pass did not resolve the stack")
	}
	if o := s.Objects[s.NextObject-1]; o.Zone != ZoneBattlefield || o.Controller != 2 {
		t.Fatalf("resolved bear = %+v", o)
	}
	// Priority returns to the active player after resolution.
	if s.PrioritySeat != 1 {
		t.Fatalf("priority = %d", s.PrioritySeat)
	}
}

func TestAdvanceBeforeStartRejected(t *testing.T) {
	rejected(t, NewState(), Action{Kind: ActionAdvance, Seat: 1})
	rejected(t, NewState(), Action{Kind: ActionPassPriority, Seat: 1})
}
