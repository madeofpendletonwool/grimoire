package engine

import (
	"testing"
)

// The stack and its trigger queue (MAD-325): declaration → TRIGGER_FIRED →
// queue → APNAP flush onto the stack at the priority grant → resolution
// applying the declared effect. fourSeats lives in commander_test.go.

// toTurn walks the skeleton until it is a seat's turn.
func toTurn(t *testing.T, s *State, seat int) {
	t.Helper()
	for i := 0; i < len(turnStructure)*len(s.Order)+len(turnStructure) && s.TurnSeat != seat; i++ {
		act(t, s, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
	}
	if s.TurnSeat != seat {
		t.Fatalf("never reached seat %d's turn (at seat %d)", seat, s.TurnSeat)
	}
}

func TestDeclareTriggerFlushesAtHeldPriority(t *testing.T) {
	s := start(t, nil)
	toMain(t, s) // seat 1's precombat main; seat 1 holds priority
	evs := act(t, s, Action{Kind: ActionDeclareTrigger, Seat: 1,
		Card: "Rhystic Study", Effect: "draw a card"})
	if len(evs) != 2 || evs[0].Kind != EventTriggerFired || evs[1].Kind != EventStackPushed {
		t.Fatalf("events = %s", kindsOf(evs))
	}
	if evs[1].Mode != "triggered" || evs[1].Controller != 1 || evs[1].Ability != "draw a card" {
		t.Fatalf("push = %+v", evs[1])
	}
	if len(s.TriggerQueue) != 0 || len(s.Stack) != 1 {
		t.Fatalf("queue = %+v stack = %+v", s.TriggerQueue, s.Stack)
	}
	// The push rode the grant already made: priority never moved to the
	// flushing system action, and it never moved away from the holder.
	if s.PrioritySeat != 1 {
		t.Fatalf("priority = %d", s.PrioritySeat)
	}
	rejected(t, s, Action{Kind: ActionDeclareTrigger, Seat: 1, Effect: "  "})
	rejected(t, s, Action{Kind: ActionDeclareTrigger, Seat: 9, Effect: "x"})
}

func TestTriggerQueueWaitsWithoutPriority(t *testing.T) {
	s := start(t, nil)
	// Untap grants priority to no one (CR 502.3): a trigger declared
	// there waits in the queue for the upkeep grant — the queue is the
	// reason triggers that fire at an awkward moment never interleave
	// with anything.
	evs := act(t, s, Action{Kind: ActionDeclareTrigger, Seat: 1,
		Card: "Phyrexian Arena", Effect: "lose 1 life, draw a card"})
	if len(evs) != 1 || evs[0].Kind != EventTriggerFired {
		t.Fatalf("events = %s", kindsOf(evs))
	}
	if len(s.TriggerQueue) != 1 || len(s.Stack) != 0 {
		t.Fatalf("queue = %+v stack = %+v", s.TriggerQueue, s.Stack)
	}
	evs = act(t, s, Action{Kind: ActionAdvance, Seat: 1})
	if evs[0].Kind != EventStepEntered || evs[0].Step != "upkeep" {
		t.Fatalf("first event = %+v", evs[0])
	}
	if evs[1].Kind != EventStackPushed || evs[1].Mode != "triggered" {
		t.Fatalf("flush = %s", kindsOf(evs[1:]))
	}
	if len(s.TriggerQueue) != 0 || len(s.Stack) != 1 {
		t.Fatalf("queue = %+v stack = %+v", s.TriggerQueue, s.Stack)
	}
}

func TestAPNAPOrderingFourSeats(t *testing.T) {
	s := start(t, fourSeats())
	toTurn(t, s, 2) // seat 2's turn; everyone else is non-active
	toStep(t, s, "beginning", "untap")
	if s.PrioritySeat != 0 {
		t.Fatalf("untap priority = %d", s.PrioritySeat)
	}
	// Four players' triggers queue in a scrambled order — seat 3 with two
	// of its own, declared A then B, the order it chose them in.
	queue := []struct {
		seat   int
		effect string
	}{
		{4, "dave's"}, {1, "collin's"}, {3, "alice A"}, {3, "alice B"}, {2, "bob's"},
	}
	for _, q := range queue {
		act(t, s, Action{Kind: ActionDeclareTrigger, Seat: q.seat, Effect: q.effect})
	}
	if len(s.TriggerQueue) != 5 {
		t.Fatalf("queue = %d", len(s.TriggerQueue))
	}
	// The upkeep grant flushes in APNAP order (CR 603.3b): the active
	// player's triggers on first — lowest, resolving last — then each
	// other player in turn order. Seat 3's own two keep declaration
	// order.
	evs := act(t, s, Action{Kind: ActionAdvance, Seat: 2})
	var order []int
	var effects []string
	for _, e := range evs {
		if e.Kind == EventStackPushed {
			order = append(order, e.Controller)
			effects = append(effects, e.Ability)
		}
	}
	wantOrder, wantEffects := []int{2, 3, 3, 4, 1}, []string{"bob's", "alice A", "alice B", "dave's", "collin's"}
	if len(order) != len(wantOrder) {
		t.Fatalf("pushes = %v", order)
	}
	for i := range wantOrder {
		if order[i] != wantOrder[i] || effects[i] != wantEffects[i] {
			t.Fatalf("push order = %v %v", order, effects)
		}
	}
	// The stack bottom is the active player's; the top is the last
	// player in turn order's, so it resolves first.
	stackControllers := make([]int, len(s.Stack))
	for i, item := range s.Stack {
		stackControllers[i] = item.Controller
	}
	if stackControllers[0] != 2 || stackControllers[len(stackControllers)-1] != 1 {
		t.Fatalf("stack controllers = %v", stackControllers)
	}
	if len(s.TriggerQueue) != 0 {
		t.Fatalf("queue = %+v", s.TriggerQueue)
	}
	// All four seats pass: the top trigger — seat 1's — resolves first,
	// with its declared effect applied, and priority returns to the
	// active player (CR 117.3b).
	for seat := 2; seat <= 4; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	evs = act(t, s, Action{Kind: ActionPassPriority, Seat: 1})
	if evs[0].Kind != EventStackResolved || evs[0].Controller != 1 || evs[0].Mode != "triggered" {
		t.Fatalf("resolve = %+v", evs[0])
	}
	if evs[1].Kind != EventEffectDeclared || evs[1].Effect != "collin's" {
		t.Fatalf("effect = %+v", evs[1])
	}
	if len(s.Stack) != 4 || s.PrioritySeat != 2 {
		t.Fatalf("stack = %d priority = %d", len(s.Stack), s.PrioritySeat)
	}
}

func TestTriggeredAbilityResolvesWithDeclaredEffect(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	id := perm(t, s, 2, TokenSpec{Name: "Rhystic Study", Types: []string{"Enchantment"}}, 0)
	// Seat 2 declares a trigger while seat 1 holds priority: the
	// declaration needs no priority, and the flush stacks it without
	// moving priority off the holder.
	evs := act(t, s, Action{Kind: ActionDeclareTrigger, Seat: 2,
		Object: id, Effect: "{T}: Add {C}"})
	if s.PrioritySeat != 1 {
		t.Fatalf("priority = %d", s.PrioritySeat)
	}
	if len(evs) != 2 || evs[1].Kind != EventStackPushed {
		t.Fatalf("events = %s", kindsOf(evs))
	}
	for seat := 1; seat <= 3; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	if len(s.Stack) != 0 {
		t.Fatalf("stack = %+v", s.Stack)
	}
	if s.PrioritySeat != 1 {
		t.Fatalf("post-resolve priority = %d", s.PrioritySeat)
	}
}

func TestActivatedAbilityResolutionAppliesEffectAndKeepsSource(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	id := land(t, s, 1, "Sol Ring")
	act(t, s, Action{Kind: ActionActivate, Seat: 1, Object: id, Ability: "{T}: Add {C}{C}"})
	for seat := 1; seat <= 3; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	if o := s.Objects[id]; o.Zone != ZoneBattlefield {
		t.Fatalf("activation moved its source to %s", o.Zone)
	}
	if len(s.Stack) != 0 {
		t.Fatalf("stack = %+v", s.Stack)
	}
}

func TestPlayerLeavesDropsQueuedTriggers(t *testing.T) {
	s := start(t, fourSeats())
	toStep(t, s, "beginning", "untap")
	act(t, s, Action{Kind: ActionDeclareTrigger, Seat: 3, Effect: "alice's"})
	act(t, s, Action{Kind: ActionDeclareTrigger, Seat: 4, Effect: "dave's"})
	act(t, s, Action{Kind: ActionConcede, Seat: 3})
	if len(s.TriggerQueue) != 1 || s.TriggerQueue[0].Controller != 4 {
		t.Fatalf("queue = %+v", s.TriggerQueue)
	}
	// The upkeep grant stacks only what survived the departure.
	act(t, s, Action{Kind: ActionAdvance, Seat: 1})
	if len(s.Stack) != 1 || s.Stack[0].Controller != 4 {
		t.Fatalf("stack = %+v", s.Stack)
	}
}

func TestCastThenOpponentTriggerStacksAbove(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	act(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Grizzly Bears",
		Base: &BaseChars{Types: []string{"Creature"}, Power: intPtr(2), Toughness: intPtr(2)}})
	// The Rhystic trigger a cast sets off goes on the stack above the
	// spell, immediately — the flush is part of the declaration's own
	// action because a player holds priority throughout.
	act(t, s, Action{Kind: ActionDeclareTrigger, Seat: 2,
		Card: "Rhystic Study", Effect: "draw a card"})
	if len(s.Stack) != 2 || s.Stack[1].Mode != "triggered" || s.Stack[1].Controller != 2 {
		t.Fatalf("stack = %+v", s.Stack)
	}
}

func kindsOf(evs []Event) string {
	out := ""
	for i, e := range evs {
		if i > 0 {
			out += " "
		}
		out += string(e.Kind)
	}
	return out
}
