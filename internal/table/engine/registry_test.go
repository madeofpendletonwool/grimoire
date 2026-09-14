package engine

// The trigger registry's engine half (MAD-335): firing on the
// structural kinds with the scope semantics each kind fixes, the
// source-present rule (and the dying source's exception), ORDER_TRIGGERS
// over the waiting queue, and the don't-forget nudges — all pure, all
// over the same act/fold harness the rest of the engine tests use.

import (
	"testing"
)

// ract applies an action through the registry-bearing Apply, stamping
// contiguous ordinals the way the store would before folding — fired
// triggers are addressed by their TRIGGER_FIRED ordinal, so the tests
// need real ones. reg may be nil: the registry-less engine, unchanged.
func ract(t *testing.T, s *State, reg TriggerRegistry, a Action) []Event {
	t.Helper()
	evs, err := ApplyWithTriggers(s, a, reg)
	if err != nil {
		t.Fatalf("apply %s: %v", a.Kind, err)
	}
	next := s.LastOrd + 1
	for i := range evs {
		evs[i].Ord = next + int64(i)
	}
	s.FoldInto(evs)
	return cloneEvents(evs)
}

// firedIn counts the TRIGGER_FIRED rows a batch produced.
func firedIn(evs []Event) int {
	n := 0
	for _, e := range evs {
		if e.Kind == EventTriggerFired {
			n++
		}
	}
	return n
}

// firedRow returns the batch's one TRIGGER_FIRED row.
func firedRow(t *testing.T, evs []Event) Event {
	t.Helper()
	for _, e := range evs {
		if e.Kind == EventTriggerFired {
			return e
		}
	}
	t.Fatalf("no TRIGGER_FIRED row in %s", kindsOf(evs))
	return Event{}
}

// rhystic is the standing registration of these tests: one card, one
// kind, the table's most famous trigger.
func rhystic(kind TriggerEvent, effect string) TriggerRegistry {
	return NewTriggerRegistry([]TriggerSpec{{Card: "Rhystic Study", Event: kind, Effect: effect}})
}

// playCard casts a card from the seat's hand and resolves it onto the
// battlefield, returning the object id — the honest way a card-named
// permanent arrives, which is what the registry keys on (tokens carry
// no card name to register against). reg may fire on the way; the
// caller that wants a quiet arrival registers nothing.
func playCard(t *testing.T, s *State, reg TriggerRegistry, seat int, name string, types ...string) int64 {
	t.Helper()
	ract(t, s, reg, Action{Kind: ActionCast, Seat: seat, Card: name,
		Base: &BaseChars{Name: name, Types: types}})
	resolveStack(t, s, reg)
	var id int64
	for i, o := range s.Objects {
		if o.Identity.Card == name && o.Zone == ZoneBattlefield {
			id = i
		}
	}
	if id == 0 {
		t.Fatalf("%s never reached the battlefield", name)
	}
	return id
}

func TestRegistryFiresLandPlayedForController(t *testing.T) {
	reg := NewTriggerRegistry([]TriggerSpec{
		{Card: "Avenger of Zendikar", Event: TriggerLandPlayed, Effect: "plant tokens"}})
	s := start(t, nil)
	toMain(t, s)
	playCard(t, s, nil, 1, "Avenger of Zendikar", "Creature")
	evs := ract(t, s, reg, Action{Kind: ActionPlayLand, Seat: 1, Card: "Forest",
		Base: &BaseChars{Name: "Forest", Types: []string{"Land"}}})
	if firedIn(evs) != 1 {
		t.Fatalf("own land fired %d triggers, want 1 (%s)", firedIn(evs), kindsOf(evs))
	}
	if evs[len(evs)-1].Kind != EventStackPushed || evs[len(evs)-1].Mode != "triggered" {
		t.Fatalf("the fired trigger did not flush: %s", kindsOf(evs))
	}
	fired := firedRow(t, evs)
	if fired.Card != "Avenger of Zendikar" || fired.Controller != 1 ||
		fired.Effect != "plant tokens" || fired.Object == 0 {
		t.Fatalf("fired row = %+v", fired)
	}
}

func TestRegistryLandPlayedIgnoresOtherControllers(t *testing.T) {
	reg := NewTriggerRegistry([]TriggerSpec{
		{Card: "Avenger of Zendikar", Event: TriggerLandPlayed, Effect: "plant tokens"}})
	s := start(t, nil)
	toMain(t, s)
	playCard(t, s, nil, 1, "Avenger of Zendikar", "Creature")
	toTurn(t, s, 2)
	toMain(t, s)
	evs := ract(t, s, reg, Action{Kind: ActionPlayLand, Seat: 2, Card: "Forest",
		Base: &BaseChars{Name: "Forest", Types: []string{"Land"}}})
	if firedIn(evs) != 0 {
		t.Fatalf("an opponent's land fired %d triggers, want 0", firedIn(evs))
	}
}

func TestRegistryCreatureETBIncludesSelf(t *testing.T) {
	reg := NewTriggerRegistry([]TriggerSpec{
		{Card: "Beast Whisperer", Event: TriggerCreatureETB, Effect: "draw a card"}})
	s := start(t, nil)
	toMain(t, s)
	// Another seat's creature does not fire it.
	evs := ract(t, s, reg, Action{Kind: ActionCreateToken, Seat: 2, Token: &TokenSpec{
		Name: "Bear", Types: []string{"Creature"}, Power: ptr(2), Toughness: ptr(2)}})
	if firedIn(evs) != 0 {
		t.Fatalf("an opponent's creature fired %d triggers, want 0", firedIn(evs))
	}
	// The source itself entering fires it — a self-ETB trigger, on the
	// batch that resolves it to the battlefield.
	ract(t, s, reg, Action{Kind: ActionCast, Seat: 1, Card: "Beast Whisperer",
		Base: &BaseChars{Name: "Beast Whisperer", Types: []string{"Creature"}}})
	selfFired := 0
	for len(s.Stack) > 0 {
		evs = ract(t, s, reg, Action{Kind: ActionPassPriority, Seat: s.PrioritySeat})
		selfFired += firedIn(evs)
	}
	if selfFired != 1 {
		t.Fatalf("the ETB fired %d triggers, want the self-ETB", selfFired)
	}
	// And a later own creature fires it again.
	evs = ract(t, s, reg, Action{Kind: ActionCreateToken, Seat: 1, Token: &TokenSpec{
		Name: "Bear", Types: []string{"Creature"}, Power: ptr(2), Toughness: ptr(2)}})
	if firedIn(evs) != 1 {
		t.Fatalf("own creature fired %d triggers, want 1", firedIn(evs))
	}
}

func TestRegistryUpkeepFiresForTurnSeat(t *testing.T) {
	reg := NewTriggerRegistry([]TriggerSpec{
		{Card: "Phyrexian Arena", Event: TriggerUpkeep, Effect: "lose 1 life, draw a card"}})
	s := start(t, nil)
	toMain(t, s)
	// The Arena lands in the main phase; the walk around the table to
	// seat 1's next upkeep is where the registration fires and the
	// step's priority grant flushes it in the same batch.
	playCard(t, s, nil, 1, "Phyrexian Arena", "Enchantment")
	for i := 0; !(s.TurnSeat == 1 && s.Phase == "beginning" && s.Step == "untap") || i == 0; i++ {
		if i > len(s.Order)*len(turnStructure)+len(turnStructure) {
			t.Fatalf("never reached seat 1's untap (%s/%s)", s.Phase, s.Step)
		}
		ract(t, s, nil, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
	}
	evs := ract(t, s, reg, Action{Kind: ActionAdvance, Seat: 1})
	if firedIn(evs) != 1 {
		t.Fatalf("upkeep fired %d triggers, want 1 (%s)", firedIn(evs), kindsOf(evs))
	}
	if evs[len(evs)-1].Kind != EventStackPushed || evs[len(evs)-1].Mode != "triggered" {
		t.Fatalf("the upkeep trigger did not flush at the grant: %s", kindsOf(evs))
	}
	// The same Arena under seat 2 stays quiet on seat 1's upkeep.
	s2 := start(t, nil)
	toMain(t, s2)
	toSeatPriority(t, s2, nil, 2)
	playCard(t, s2, nil, 2, "Phyrexian Arena", "Enchantment")
	for i := 0; !(s2.TurnSeat == 1 && s2.Phase == "beginning" && s2.Step == "untap") || i == 0; i++ {
		if i > len(s2.Order)*len(turnStructure)+len(turnStructure) {
			t.Fatalf("never reached seat 1's untap (%s/%s)", s2.Phase, s2.Step)
		}
		ract(t, s2, nil, Action{Kind: ActionAdvance, Seat: s2.TurnSeat})
	}
	evs = ract(t, s2, reg, Action{Kind: ActionAdvance, Seat: 1})
	if firedIn(evs) != 0 {
		t.Fatalf("fired %d triggers on a turn the source's controller does not own", firedIn(evs))
	}
}

func TestRegistryEndStepFires(t *testing.T) {
	reg := NewTriggerRegistry([]TriggerSpec{
		{Card: "Hunted Nightmare", Event: TriggerEndStep, Effect: "mill a card"}})
	s := start(t, nil)
	toMain(t, s)
	playCard(t, s, nil, 1, "Hunted Nightmare", "Creature")
	var evs []Event
	for i := 0; i < len(turnStructure)+1 && !(s.Phase == "end" && s.Step == "end"); i++ {
		evs = ract(t, s, reg, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
	}
	if s.Phase != "end" || s.Step != "end" {
		t.Fatalf("never reached the end step (%s/%s)", s.Phase, s.Step)
	}
	if firedIn(evs) != 1 {
		t.Fatalf("the end step fired %d triggers, want 1 (%s)", firedIn(evs), kindsOf(evs))
	}
}

func TestRegistryOpponentCastsFiresNotOwn(t *testing.T) {
	reg := rhystic(TriggerOppCasts, "may draw a card")
	s := start(t, nil)
	toMain(t, s)
	rhysticID := playCard(t, s, nil, 1, "Rhystic Study", "Enchantment")
	if s.Objects[rhysticID].Zone != ZoneBattlefield {
		t.Fatalf("rhystic zone = %s", s.Objects[rhysticID].Zone)
	}
	// Own cast: no fire.
	evs := ract(t, s, reg, Action{Kind: ActionCast, Seat: 1, Card: "Cultivate",
		Base: &BaseChars{Name: "Cultivate", Types: []string{"Sorcery"}}})
	if firedIn(evs) != 0 {
		t.Fatalf("own cast fired %d triggers, want 0", firedIn(evs))
	}
	// Resolve the cultivate, pass to seat 2, and cast: the Rhystic
	// fires under its own controller.
	resolveStack(t, s, reg)
	toSeatPriority(t, s, reg, 2)
	evs = ract(t, s, reg, Action{Kind: ActionCast, Seat: 2, Card: "Sol Ring",
		Base: &BaseChars{Name: "Sol Ring", Types: []string{"Artifact"}}})
	if firedIn(evs) != 1 {
		t.Fatalf("opponent's cast fired %d triggers, want 1 (%s)", firedIn(evs), kindsOf(evs))
	}
	if fired := firedRow(t, evs); fired.Card != "Rhystic Study" || fired.Controller != 1 {
		t.Fatalf("fired row = %+v", fired)
	}
}

func TestRegistryDoesNotFireWhenSourceHasLeft(t *testing.T) {
	reg := rhystic(TriggerOppCasts, "may draw a card")
	s := start(t, nil)
	toMain(t, s)
	rhysticID := playCard(t, s, nil, 1, "Rhystic Study", "Enchantment")
	act(t, s, Action{Kind: ActionMoveZone, Seat: 1, Object: rhysticID,
		ToZone: ZoneExile, Cause: "exile"})
	resolveStack(t, s, reg)
	toSeatPriority(t, s, reg, 2)
	evs := ract(t, s, reg, Action{Kind: ActionCast, Seat: 2, Card: "Sol Ring",
		Base: &BaseChars{Name: "Sol Ring", Types: []string{"Artifact"}}})
	if firedIn(evs) != 0 {
		t.Fatalf("a source off the battlefield fired %d triggers", firedIn(evs))
	}
}

func TestRegistryPhasedSourceDoesNotFire(t *testing.T) {
	reg := rhystic(TriggerOppCasts, "may draw a card")
	s := start(t, nil)
	toMain(t, s)
	rhysticID := playCard(t, s, nil, 1, "Rhystic Study", "Enchantment")
	act(t, s, Action{Kind: ActionSetPhased, Seat: 1, Object: rhysticID, Phased: true})
	resolveStack(t, s, reg)
	toSeatPriority(t, s, reg, 2)
	evs := ract(t, s, reg, Action{Kind: ActionCast, Seat: 2, Card: "Sol Ring",
		Base: &BaseChars{Name: "Sol Ring", Types: []string{"Artifact"}}})
	if firedIn(evs) != 0 {
		t.Fatalf("a phased source fired %d triggers", firedIn(evs))
	}
}

func TestRegistryAttacksFiresForTheSourceItself(t *testing.T) {
	reg := NewTriggerRegistry([]TriggerSpec{
		{Card: "Rampaging Raptor", Event: TriggerAttacks, Effect: "deal 2 damage"}})
	s := start(t, nil)
	toMain(t, s)
	raptor := playCard(t, s, nil, 1, "Rampaging Raptor", "Creature")
	other := summon(t, s, 1, "Bear", 2, 2)
	toStep(t, s, "combat", "declare_attackers")
	// An unregistered attacker fires nothing.
	evs := ract(t, s, reg, Action{Kind: ActionDeclareAttackers, Seat: 1,
		Attackers: []AttackAssignment{{Object: other, TargetSeat: 2}}})
	if firedIn(evs) != 0 {
		t.Fatalf("an unregistered attacker fired %d triggers", firedIn(evs))
	}
	// The registered card attacking fires its own trigger.
	evs = ract(t, s, reg, Action{Kind: ActionDeclareAttackers, Seat: 1,
		Attackers: []AttackAssignment{{Object: raptor, TargetSeat: 2}}})
	if firedIn(evs) != 1 {
		t.Fatalf("the registered attacker fired %d triggers, want 1 (%s)", firedIn(evs), kindsOf(evs))
	}
	if fired := firedRow(t, evs); fired.Card != "Rampaging Raptor" || fired.Object != raptor {
		t.Fatalf("fired row = %+v", fired)
	}
}

func TestRegistryDiesFiresForSelfWitnessAndSpokenDeaths(t *testing.T) {
	reg := NewTriggerRegistry([]TriggerSpec{
		{Card: "Blood Artist", Event: TriggerDies, Effect: "each opponent loses 1"}})
	s := start(t, nil)
	toMain(t, s)
	artist := playCard(t, s, nil, 1, "Blood Artist", "Creature")
	victim := summon(t, s, 1, "Bear", 2, 2)
	theirs := summon(t, s, 2, "Bear", 2, 2)

	// A spoken sacrifice is a ZONE_CHANGED(cause sacrifice) — a death
	// the registry must see.
	evs := ract(t, s, reg, Action{Kind: ActionMoveZone, Seat: 1, Object: victim,
		ToZone: ZoneGraveyard, Cause: "sacrifice"})
	if firedIn(evs) != 1 {
		t.Fatalf("a sacrificed own creature fired %d triggers, want 1 (%s)", firedIn(evs), kindsOf(evs))
	}
	// Another seat's death does not fire the seat-1 witness.
	evs = ract(t, s, reg, Action{Kind: ActionMoveZone, Seat: 2, Object: theirs,
		ToZone: ZoneGraveyard, Cause: "destroy"})
	if firedIn(evs) != 0 {
		t.Fatalf("an opponent's death fired %d triggers, want 0", firedIn(evs))
	}
	// The source itself dying fires its own trigger — the dying source
	// exception.
	evs = ract(t, s, reg, Action{Kind: ActionMoveZone, Seat: 1, Object: artist,
		ToZone: ZoneGraveyard, Cause: "sacrifice"})
	if firedIn(evs) != 1 {
		t.Fatalf("the source's own death fired %d triggers, want 1 (%s)", firedIn(evs), kindsOf(evs))
	}
	if fired := firedRow(t, evs); fired.Card != "Blood Artist" || fired.Controller != 1 {
		t.Fatalf("self-death row = %+v", fired)
	}
}

func TestRegistryDiesSeesStateBasedDeaths(t *testing.T) {
	reg := NewTriggerRegistry([]TriggerSpec{
		{Card: "Blood Artist", Event: TriggerDies, Effect: "each opponent loses 1"}})
	s := start(t, nil)
	toMain(t, s)
	playCard(t, s, nil, 1, "Blood Artist", "Creature")
	bear := summon(t, s, 1, "Bear", 2, 2)
	// Lethal damage brings the CR 704 sweep, whose DIED row fires the
	// artist.
	evs := ract(t, s, reg, Action{Kind: ActionDealDamage, Seat: 2, Amount: 2, TargetObject: bear})
	sawDeath := false
	for _, e := range evs {
		if e.Kind == EventDied && e.Object == bear {
			sawDeath = true
		}
	}
	if !sawDeath {
		t.Fatalf("the damage did not produce a DIED row: %s", kindsOf(evs))
	}
	if firedIn(evs) != 1 {
		t.Fatalf("the state-based death fired %d triggers, want 1 (%s)", firedIn(evs), kindsOf(evs))
	}
}

func TestRegistryInvalidRowsAreSkipped(t *testing.T) {
	reg := NewTriggerRegistry([]TriggerSpec{
		{Card: "Fine", Event: TriggerUpkeep, Effect: "ok"},
		{Card: "", Event: TriggerUpkeep, Effect: "no card"},
		{Card: "BadKind", Event: TriggerEvent("NOT_A_KIND"), Effect: "no kind"},
		{Card: "NoEffect", Event: TriggerUpkeep, Effect: "  "},
	})
	if len(reg) != 1 {
		t.Fatalf("registry = %d cards, want the one valid row", len(reg))
	}
	if _, ok := reg.Effect("Fine", TriggerUpkeep); !ok {
		t.Fatal("the valid row did not index")
	}
}

/* ---------- ORDER_TRIGGERS ---------- */

func TestOrderTriggersReordersOwnEntries(t *testing.T) {
	s := start(t, nil)
	// Untap grants no priority: declared triggers queue without
	// flushing, which is exactly the panel's waiting set.
	ract(t, s, nil, Action{Kind: ActionDeclareTrigger, Seat: 1, Card: "A", Effect: "a"})
	ract(t, s, nil, Action{Kind: ActionDeclareTrigger, Seat: 2, Card: "B", Effect: "b"})
	ract(t, s, nil, Action{Kind: ActionDeclareTrigger, Seat: 1, Card: "C", Effect: "c"})
	if len(s.TriggerQueue) != 3 {
		t.Fatalf("queue = %d", len(s.TriggerQueue))
	}
	a, b, c := s.TriggerQueue[0].FiredOrd, s.TriggerQueue[1].FiredOrd, s.TriggerQueue[2].FiredOrd
	if a == 0 || b == 0 || c == 0 {
		t.Fatalf("fired ords = %d/%d/%d — stamping failed", a, b, c)
	}
	// Seat 1 puts C before A: a permutation of their own, B's place
	// between them untouched.
	evs := ract(t, s, nil, Action{Kind: ActionOrderTriggers, Seat: 1, Order: []int64{c, b, a}})
	if len(evs) != 1 || evs[0].Kind != EventTriggersOrdered {
		t.Fatalf("order events = %s", kindsOf(evs))
	}
	got := []string{}
	for _, q := range s.TriggerQueue {
		got = append(got, q.Card)
	}
	if got[0] != "C" || got[1] != "B" || got[2] != "A" {
		t.Fatalf("queue after order = %v", got)
	}
}

func TestOrderTriggersRejectsPermutingOthers(t *testing.T) {
	s := start(t, nil)
	ract(t, s, nil, Action{Kind: ActionDeclareTrigger, Seat: 1, Card: "A", Effect: "a"})
	ract(t, s, nil, Action{Kind: ActionDeclareTrigger, Seat: 2, Card: "B", Effect: "b"})
	ract(t, s, nil, Action{Kind: ActionDeclareTrigger, Seat: 2, Card: "D", Effect: "d"})
	ract(t, s, nil, Action{Kind: ActionDeclareTrigger, Seat: 1, Card: "C", Effect: "c"})
	a, b, d, c := s.TriggerQueue[0].FiredOrd, s.TriggerQueue[1].FiredOrd,
		s.TriggerQueue[2].FiredOrd, s.TriggerQueue[3].FiredOrd
	// Seat 1 swapping seat 2's B and D is not seat 1's to choose.
	rejected(t, s, Action{Kind: ActionOrderTriggers, Seat: 1, Order: []int64{a, d, b, c}})
	// An incomplete list is not an order.
	rejected(t, s, Action{Kind: ActionOrderTriggers, Seat: 1, Order: []int64{a, b}})
	// An unknown ordinal is not a waiting trigger.
	rejected(t, s, Action{Kind: ActionOrderTriggers, Seat: 1, Order: []int64{a, b, d, 999}})
	// Nothing waiting is not an order either.
	empty := start(t, nil)
	rejected(t, empty, Action{Kind: ActionOrderTriggers, Seat: 1, Order: nil})
	// Seat 2's own permutation is fine.
	evs := ract(t, s, nil, Action{Kind: ActionOrderTriggers, Seat: 2, Order: []int64{a, d, b, c}})
	if len(evs) != 1 {
		t.Fatalf("seat 2's own order was rejected: %s", kindsOf(evs))
	}
}

func TestOrderTriggersRoundTripsThroughTheFold(t *testing.T) {
	s := start(t, nil)
	var log []Event
	stamp := func(evs []Event) {
		next := s.LastOrd + 1
		for i := range evs {
			evs[i].Ord = next + int64(i)
		}
		s.FoldInto(evs)
		log = append(log, evs...)
	}
	for _, q := range []struct {
		seat int
		card string
	}{{1, "A"}, {1, "C"}, {2, "B"}} {
		evs, err := Apply(s, Action{Kind: ActionDeclareTrigger, Seat: q.seat, Card: q.card, Effect: "e"})
		if err != nil {
			t.Fatal(err)
		}
		stamp(evs)
	}
	// Seat 1 flips their two: C first.
	aOrd := log[0].Ord
	cOrd := log[1].Ord
	bOrd := log[2].Ord
	evs, err := Apply(s, Action{Kind: ActionOrderTriggers, Seat: 1, Order: []int64{cOrd, bOrd, aOrd}})
	if err != nil {
		t.Fatal(err)
	}
	stamp(evs)
	refolded := Fold(log)
	if len(refolded.TriggerQueue) != 3 {
		t.Fatalf("refolded queue = %d", len(refolded.TriggerQueue))
	}
	for i := range refolded.TriggerQueue {
		if refolded.TriggerQueue[i].Card != s.TriggerQueue[i].Card {
			t.Fatalf("refold %d = %s, want %s — the fold must reproduce the chosen order",
				i, refolded.TriggerQueue[i].Card, s.TriggerQueue[i].Card)
		}
	}
}

/* ---------- nudges ---------- */

func TestNudgesCoverWaitingUnresolvedAndUnusedAttack(t *testing.T) {
	reg := NewTriggerRegistry([]TriggerSpec{
		{Card: "Rampaging Raptor", Event: TriggerAttacks, Effect: "deal 2 damage"}})
	s := start(t, nil)
	// A waiting trigger (declared at untap, no priority to flush it),
	// then the upkeep grant that flushes it onto the stack.
	ract(t, s, nil, Action{Kind: ActionDeclareTrigger, Seat: 1, Card: "Rhystic Study", Effect: "may draw"})
	if len(s.TriggerQueue) != 1 {
		t.Fatalf("setup queue = %d", len(s.TriggerQueue))
	}
	waiting := s.Nudges(reg)
	if len(waiting) != 1 || waiting[0].Kind != "waiting" || waiting[0].Card != "Rhystic Study" {
		t.Fatalf("waiting nudges = %+v", waiting)
	}
	ract(t, s, nil, Action{Kind: ActionAdvance, Seat: 1})
	if len(s.Stack) != 1 || s.Stack[0].Mode != "triggered" {
		t.Fatalf("setup: stack = %+v", s.Stack)
	}
	nudges := s.Nudges(reg)
	sawUnresolved := false
	for _, n := range nudges {
		if n.Kind == "unresolved" && n.Card == "Rhystic Study" {
			sawUnresolved = true
		}
	}
	if !sawUnresolved {
		t.Fatalf("nudges = %+v, want the unresolved Rhystic", nudges)
	}
	// The unused attack trigger: seat 1's declare_attackers step, a
	// registered attacker present, nothing declared.
	resolveStack(t, s, nil)
	playCard(t, s, nil, 1, "Rampaging Raptor", "Creature")
	for i := 0; i < len(turnStructure)+1; i++ {
		if s.Phase == "combat" && s.Step == "declare_attackers" {
			break
		}
		ract(t, s, reg, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
	}
	if s.Phase != "combat" || s.Step != "declare_attackers" {
		t.Fatalf("never reached declare_attackers (%s/%s)", s.Phase, s.Step)
	}
	count := map[string]int{}
	for _, n := range s.Nudges(reg) {
		count[n.Kind]++
	}
	if count["unused_attack"] != 1 {
		t.Fatalf("unused_attack nudges = %d (%+v)", count["unused_attack"], s.Nudges(reg))
	}
	// Declaring attackers retires the nudge — the trigger is used.
	var raptor int64
	for id, o := range s.Objects {
		if o.Zone == ZoneBattlefield && o.Identity.Card == "Rampaging Raptor" {
			raptor = id
		}
	}
	ract(t, s, reg, Action{Kind: ActionDeclareAttackers, Seat: 1,
		Attackers: []AttackAssignment{{Object: raptor, TargetSeat: 2}}})
	for _, n := range s.Nudges(reg) {
		if n.Kind == "unused_attack" {
			t.Fatal("the unused-attack nudge outlived the declaration")
		}
	}
}

/* ---------- small helpers ---------- */

func ptr(n int) *int { return &n }

// resolveStack passes priority around until the stack is empty.
func resolveStack(t *testing.T, s *State, reg TriggerRegistry) {
	t.Helper()
	guard := len(s.Order)*len(s.Stack) + len(s.Order) + 2
	for i := 0; i < guard && len(s.Stack) > 0; i++ {
		if s.PrioritySeat == 0 {
			ract(t, s, reg, Action{Kind: ActionAdvance, Seat: s.TurnSeat})
			continue
		}
		ract(t, s, reg, Action{Kind: ActionPassPriority, Seat: s.PrioritySeat})
	}
	if len(s.Stack) > 0 {
		t.Fatalf("the stack never resolved: %+v", s.Stack)
	}
}

// toSeatPriority rotates priority to a seat through honest passes.
func toSeatPriority(t *testing.T, s *State, reg TriggerRegistry, seat int) {
	t.Helper()
	guard := len(s.Order) + 2
	for i := 0; i < guard && s.PrioritySeat != seat; i++ {
		ract(t, s, reg, Action{Kind: ActionPassPriority, Seat: s.PrioritySeat})
	}
	if s.PrioritySeat != seat {
		t.Fatalf("priority never reached seat %d (at %d)", seat, s.PrioritySeat)
	}
}
