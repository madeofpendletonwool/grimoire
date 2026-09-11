package engine

import "testing"

// CR 800.4: what leaving the game does. Concession needs no priority
// (CR 104.3a); the leaving player's turn ends if it was theirs; their
// objects leave with them; control they held ends; the turn order prunes.
func TestConcedeOnOwnTurnPassesTheTurn(t *testing.T) {
	s := start(t, nil)
	bear := summon(t, s, 1, "Bear", 2, 2) // seat 1's own creature, survives them leaving
	_ = bear
	evs := act(t, s, Action{Kind: ActionConcede, Seat: 1, Source: "manual"})
	sawLeft, sawTurnEnded, sawTurnStarted := false, false, false
	for _, e := range evs {
		switch {
		case e.Kind == EventPlayerLeft && e.TargetSeat == 1 && e.Cause == causeConcession:
			sawLeft = true
		case e.Kind == EventTurnEnded && e.Turn == 1:
			sawTurnEnded = true
		case e.Kind == EventTurnStarted && e.Turn == 2 && e.TurnSeat == 2:
			sawTurnStarted = true
		}
	}
	if !sawLeft || !sawTurnEnded || !sawTurnStarted {
		t.Fatalf("concession chain = %+v", evs)
	}
	if s.Turn != 2 || s.TurnSeat != 2 || s.Phase != "beginning" || s.Step != "untap" {
		t.Fatalf("turn = %d/%d at %s/%s", s.Turn, s.TurnSeat, s.Phase, s.Step)
	}
	if s.PrioritySeat != 0 {
		t.Fatalf("priority during the new untap = %d", s.PrioritySeat)
	}
	// Turn order skips the empty chair for the rest of the game.
	for i := 0; i < 24; i++ {
		act(t, s, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
	}
	if s.TurnSeat == 1 {
		t.Fatalf("seat 1 still taking turns: order = %v", s.Order)
	}
	// Double concession is not a thing.
	rejected(t, s, Action{Kind: ActionConcede, Seat: 1})
}

func TestLeavingTakesObjectsAndControl(t *testing.T) {
	s := start(t, nil)
	// Seat 2's creature, stolen by seat 3.
	mine := summon(t, s, 2, "Goblin", 1, 1)
	act(t, s, Action{Kind: ActionAddModifier, Seat: 3, Object: mine, Modifier: &Modifier{
		Layer: LayerControl, Duration: PermanentDuration, SourceCard: "Act of Treason",
		Delta: Delta{Controller: intPtr(3)}}})
	if c := s.Characteristics(mine); c.Controller != 3 {
		t.Fatal("steal setup failed")
	}
	// Seat 2's aura-equivalent anthem riding on the goblin.
	goblinAnthem := perm(t, s, 2, TokenSpec{Name: "Goblin Anthem", Types: []string{"Artifact"}}, 0)
	act(t, s, Action{Kind: ActionAddModifier, Seat: 2, Object: mine, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: WhileSourcePresent, SourceObj: goblinAnthem,
		Delta: Delta{Power: intPtr(1), Toughness: intPtr(1)}}})
	evs := act(t, s, Action{Kind: ActionConcede, Seat: 3})
	// The control modifier seat 3 granted is asserted gone, so the
	// goblin reverts to its owner.
	sawRevert := false
	for _, e := range evs {
		if e.Kind == EventModifierRemoved && e.Object == mine {
			sawRevert = true
		}
	}
	if !sawRevert {
		t.Fatalf("no control reversion: %+v", evs)
	}
	if c := s.Characteristics(mine); c.Controller != 2 {
		t.Fatalf("controller after seat 3 left = %d", c.Controller)
	}
	if c := s.Characteristics(mine); c.Power == nil || *c.Power != 2 {
		t.Fatalf("goblin = %+v", c)
	}
	// Seat 3's own objects are gone entirely.
	for _, o := range s.Objects {
		if o.Owner == 3 {
			t.Fatalf("seat 3's %d survived them", o.ID)
		}
	}
	// Seat 2's anthem and creature are untouched.
	if _, ok := s.Objects[goblinAnthem]; !ok {
		t.Fatal("seat 2's anthem left with seat 3")
	}
}

func TestLeavingDropsTheirAttachmentsAndStack(t *testing.T) {
	s := start(t, nil)
	// Seat 3 hosts an aura owned by seat 1.
	host := summon(t, s, 3, "Host", 2, 2)
	aura := perm(t, s, 1, TokenSpec{Name: "Gift", Types: []string{"Enchantment", "Aura"}}, host)
	// Seat 3 has a spell on the stack.
	toMain(t, s)
	act(t, s, Action{Kind: ActionPassPriority, Seat: 1})
	act(t, s, Action{Kind: ActionPassPriority, Seat: 2})
	act(t, s, Action{Kind: ActionCast, Seat: 3, Card: "Firebolt",
		Base: &BaseChars{Types: []string{"Instant"}}})
	if len(s.Stack) != 1 {
		t.Fatal("setup: stack empty")
	}
	evs := act(t, s, Action{Kind: ActionConcede, Seat: 3})
	sawUnattach := false
	for _, e := range evs {
		if e.Kind == EventUnattached && e.Object == aura {
			sawUnattach = true
		}
	}
	if !sawUnattach {
		t.Fatalf("aura stayed on the leaver's host: %+v", evs)
	}
	// The aura (owned by seat 1, now unattached) falls in the same sweep.
	fell := false
	for _, e := range evs {
		if e.Kind == EventZoneChanged && e.Object == aura && e.Cause == causeUnattached {
			fell = true
		}
	}
	if !fell {
		t.Fatalf("aura did not fall: %+v", evs)
	}
	// Their spell came off the stack with them.
	if len(s.Stack) != 0 {
		t.Fatalf("stack = %+v", s.Stack)
	}
	// The host is gone with its owner.
	if _, ok := s.Objects[host]; ok {
		t.Fatal("host survived its owner")
	}
}

// The elimination of a priority holder mid-window: the rotation continues
// with the living, and the count of passes stays honest.
func TestEliminationDuringPriorityWindow(t *testing.T) {
	s := start(t, nil)
	act(t, s, Action{Kind: ActionAdvance, Seat: 1}) // upkeep, priority 1
	act(t, s, Action{Kind: ActionPassPriority, Seat: 1})
	act(t, s, Action{Kind: ActionPassPriority, Seat: 2}) // priority now 3
	// Seat 3 dies to damage while holding priority.
	act(t, s, Action{Kind: ActionDealDamage, Seat: 1, Amount: 40, TargetSeat: 3})
	if s.Seats[3].Alive || s.PrioritySeat != 1 {
		t.Fatalf("alive3=%v priority = %d after seat 3 left", s.Seats[3].Alive, s.PrioritySeat)
	}
	if len(s.Order) != 2 {
		t.Fatalf("order = %v", s.Order)
	}
	// The window closes normally over the survivors.
	act(t, s, Action{Kind: ActionPassPriority, Seat: 1})
	if s.Phase != "precombat_main" {
		t.Fatalf("step = %s/%s", s.Phase, s.Step)
	}
}
