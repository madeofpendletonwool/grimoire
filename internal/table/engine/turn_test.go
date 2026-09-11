package engine

import "testing"

// The full step sequence in CR order as a table, walked by ADVANCE alone.
// Turn 1 skips the draw step (CR 103.7a); untap and cleanup hold priority
// for no one (CR 502.3, 514.3a) and every other step grants it to the
// active player as it is entered (CR 117.2a).
func TestAdvanceWalksFullTurns(t *testing.T) {
	s := start(t, nil)
	type restep struct {
		turn, seat  int
		phase, step string
		priority    int
	}
	rows := func(turn, seat int, withDraw bool) []restep {
		one := func(phase, step string, priority int) restep {
			return restep{turn, seat, phase, step, priority}
		}
		rows := []restep{one("beginning", "untap", 0), one("beginning", "upkeep", seat)}
		if withDraw {
			rows = append(rows, one("beginning", "draw", seat))
		}
		return append(rows,
			one("precombat_main", "main", seat),
			one("combat", "beginning_of_combat", seat),
			one("combat", "declare_attackers", seat),
			one("combat", "declare_blockers", seat),
			one("combat", "combat_damage", seat),
			one("combat", "end_of_combat", seat),
			one("postcombat_main", "main", seat),
			one("end", "end", seat),
			one("end", "cleanup", 0),
		)
	}
	want := append(rows(1, 1, false), rows(2, 2, true)...)
	want = append(want, rows(3, 3, true)...)
	for _, w := range want {
		if s.Turn != w.turn || s.TurnSeat != w.seat || s.Phase != w.phase || s.Step != w.step {
			t.Fatalf("at turn %d/%d %s/%s, want turn %d/%d %s/%s",
				s.Turn, s.TurnSeat, s.Phase, s.Step, w.turn, w.seat, w.phase, w.step)
		}
		if s.PrioritySeat != w.priority {
			t.Fatalf("priority in %s/%s = %d, want %d", w.phase, w.step, s.PrioritySeat, w.priority)
		}
		act(t, s, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
	}
	// Leaving seat 3's cleanup starts turn 4 back at seat 1's untap.
	if s.Turn != 4 || s.TurnSeat != 1 || s.Phase != "beginning" || s.Step != "untap" || s.PrioritySeat != 0 {
		t.Fatalf("turn 4 = %d/%d at %s/%s priority %d", s.Turn, s.TurnSeat, s.Phase, s.Step, s.PrioritySeat)
	}
}

func TestAdvanceResetsLandDropPerTurn(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
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
	toMain(t, s)
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
	act(t, s, Action{Kind: ActionAdvance, Seat: 1}) // untap → upkeep, priority 1
	act(t, s, Action{Kind: ActionPassPriority, Seat: 1})
	if s.PrioritySeat != 2 || len(s.Passed) != 1 {
		t.Fatalf("priority = %d passed = %v", s.PrioritySeat, s.Passed)
	}
	act(t, s, Action{Kind: ActionPassPriority, Seat: 2})
	if s.PrioritySeat != 3 {
		t.Fatalf("priority = %d", s.PrioritySeat)
	}
	// Third pass empties the stack-less step: the step ends, exactly as
	// ADVANCE would have walked it, and the active player holds priority
	// in the step that follows. Turn 1 has no draw step.
	act(t, s, Action{Kind: ActionPassPriority, Seat: 3})
	if s.Phase != "precombat_main" || s.Step != "main" {
		t.Fatalf("step = %s/%s", s.Phase, s.Step)
	}
	if s.PrioritySeat != s.TurnSeat || len(s.Passed) != 0 {
		t.Fatalf("priority = %d passed = %v", s.PrioritySeat, s.Passed)
	}
}

func TestPassPriorityResolvesStack(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	act(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Grizzly Bears",
		Base: &BaseChars{Types: []string{"Creature"}, Power: intPtr(2), Toughness: intPtr(2)}})
	// The caster retains priority after the cast (CR 117.2c).
	if s.PrioritySeat != 1 {
		t.Fatalf("priority after cast = %d", s.PrioritySeat)
	}
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
	if o := s.Objects[s.NextObject-1]; o.Zone != ZoneBattlefield || o.Controller != 1 {
		t.Fatalf("resolved bear = %+v", o)
	}
	// Priority returns to the active player after resolution (CR 117.3b)
	// with a fresh pass cycle.
	if s.PrioritySeat != 1 || len(s.Passed) != 0 {
		t.Fatalf("priority = %d passed = %v", s.PrioritySeat, s.Passed)
	}
}

// The priority gates: only the holder casts, activates, plays a land, or
// passes, and nobody does any of that in untap or cleanup.
func TestPriorityGating(t *testing.T) {
	s := start(t, nil)
	// Untap: no one has priority, so the priority actions all reject.
	rejected(t, s, Action{Kind: ActionPassPriority, Seat: 1})
	rejected(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Something"})
	rejected(t, s, Action{Kind: ActionPlayLand, Seat: 1, Card: "Forest"})
	rejected(t, s, Action{Kind: ActionActivate, Seat: 1, Ability: "x"})
	// Upkeep: seat 1 holds it; seat 2 does not.
	act(t, s, Action{Kind: ActionAdvance, Seat: 1})
	rejected(t, s, Action{Kind: ActionPassPriority, Seat: 2})
	rejected(t, s, Action{Kind: ActionCast, Seat: 2, Card: "Something"})
	rejected(t, s, Action{Kind: ActionPassPriority, Seat: 3})
	act(t, s, Action{Kind: ActionPassPriority, Seat: 1}) // now seat 2 holds it
	act(t, s, Action{Kind: ActionPassPriority, Seat: 2})
	// Manual bookkeeping never needed priority — TAP and counters work
	// even in the untap step.
	s2 := start(t, nil)
	bear := summon(t, s2, 1, "Bear", 2, 2)
	act(t, s2, Action{Kind: ActionTap, Seat: 1, Object: bear})
	act(t, s2, Action{Kind: ActionAdjustCounters, Seat: 2, TargetSeat: 1, CounterName: "poison", Delta: 1})
}

// A step does not end over a non-empty stack (CR 500.2): ADVANCE refuses
// until the stack resolves through passes.
func TestAdvanceNeedsEmptyStack(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	act(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Divination",
		Base: &BaseChars{Types: []string{"Sorcery"}}})
	rejected(t, s, Action{Kind: ActionAdvance, Seat: 1})
	for seat := 1; seat <= 3; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	if len(s.Stack) != 0 {
		t.Fatal("stack did not resolve")
	}
	act(t, s, Action{Kind: ActionAdvance, Seat: 1})
	if s.Phase != "combat" || s.Step != "beginning_of_combat" {
		t.Fatalf("step = %s/%s", s.Phase, s.Step)
	}
}

func TestAdvanceBeforeStartRejected(t *testing.T) {
	rejected(t, NewState(), Action{Kind: ActionAdvance, Seat: 1})
	rejected(t, NewState(), Action{Kind: ActionPassPriority, Seat: 1})
}
