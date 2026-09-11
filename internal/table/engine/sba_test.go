package engine

import "testing"

// Every state-based action from the issue gets a case here, plus the
// recursion cases where one action's consequences cascade.

func diedCause(evs []Event, id int64) string {
	for _, e := range evs {
		if e.Kind == EventDied && e.Object == id {
			return e.Cause
		}
	}
	return ""
}

func leftCause(evs []Event, seat int) string {
	for _, e := range evs {
		if e.Kind == EventPlayerLeft && e.TargetSeat == seat {
			return e.Cause
		}
	}
	return ""
}

func TestSBAZeroToughness(t *testing.T) {
	s := start(t, nil)
	bear := summon(t, s, 1, "Bear", 2, 2)
	evs := act(t, s, Action{Kind: ActionAddModifier, Seat: 2, Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: UntilEOT, SourceCard: "Weakness",
		Delta: Delta{Toughness: intPtr(-3)}}})
	if got := diedCause(evs, bear); got != causeZeroToughness {
		t.Fatalf("cause = %q in %+v", got, evs)
	}
	// The bear was a token: it died, hit the graveyard, and ceased.
	if _, ok := s.Objects[bear]; ok {
		t.Fatal("dead token bear still tracked")
	}
	// A nontoken dies to zero toughness the same way and stays in the
	// graveyard as a card.
	s2 := start(t, nil)
	toMain(t, s2)
	act(t, s2, Action{Kind: ActionCast, Seat: 1, Card: "Grizzly Bears",
		Base: &BaseChars{Types: []string{"Creature"}, Power: intPtr(2), Toughness: intPtr(2)}})
	for seat := 1; seat <= 3; seat++ {
		act(t, s2, Action{Kind: ActionPassPriority, Seat: seat})
	}
	bear2 := s2.NextObject - 1
	evs = act(t, s2, Action{Kind: ActionAddModifier, Seat: 2, Object: bear2, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: PermanentDuration, SourceCard: "Weakness",
		Delta: Delta{Toughness: intPtr(-2)}}})
	if got := diedCause(evs, bear2); got != causeZeroToughness {
		t.Fatalf("card bear cause = %q", got)
	}
	if o := s2.Objects[bear2]; o.Zone != ZoneGraveyard || o.Identity.Token != nil {
		t.Fatalf("bear2 = %+v", o)
	}
}

func TestSBALethalDamage(t *testing.T) {
	s := start(t, nil)
	bear := summon(t, s, 1, "Bear", 2, 2)
	evs := act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 1, TargetObject: bear})
	if got := diedCause(evs, bear); got != "" {
		t.Fatalf("one damage killed a 2/2: %+v", evs)
	}
	evs = act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 1, TargetObject: bear})
	if got := diedCause(evs, bear); got != causeLethalDamage {
		t.Fatalf("cause = %q", got)
	}
	if _, ok := s.Objects[bear]; ok {
		t.Fatal("lethally damaged token bear still tracked")
	}
	// Indestructible says no, and the damage stays marked.
	s2 := start(t, nil)
	god := summon(t, s2, 1, "God", 2, 2)
	act(t, s2, Action{Kind: ActionAddModifier, Seat: 1, Object: god, Modifier: &Modifier{
		Layer: LayerAbility, Duration: PermanentDuration, SourceCard: "Indestructible",
		Delta: Delta{AddKeywords: []string{"Indestructible"}}}})
	evs = act(t, s2, Action{Kind: ActionDealDamage, Seat: 2, Amount: 5, TargetObject: god})
	if diedCause(evs, god) != "" || s2.Objects[god].Zone != ZoneBattlefield || s2.Objects[god].Damage != 5 {
		t.Fatalf("indestructible god: zone %s damage %d", s2.Objects[god].Zone, s2.Objects[god].Damage)
	}
}

func TestSBAZeroLife(t *testing.T) {
	s := start(t, nil)
	evs := act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 40, TargetSeat: 1})
	if got := leftCause(evs, 1); got != causeZeroLife {
		t.Fatalf("cause = %q in %+v", got, evs)
	}
	if s.Seats[1].Alive || s.Seats[1].Life != 0 {
		t.Fatalf("seat1 = %+v", s.Seats[1])
	}
	// The game continues for the others, and seat 1 is out of turn order.
	if len(s.Order) != 2 || s.Order[0] != 2 || s.Order[1] != 3 {
		t.Fatalf("order = %v", s.Order)
	}
	if s.Status != StatusActive {
		t.Fatalf("status = %s", s.Status)
	}
}

func TestSBAPoison(t *testing.T) {
	s := start(t, nil)
	act(t, s, Action{Kind: ActionAdjustCounters, Seat: 2, TargetSeat: 1, CounterName: "poison", Delta: 9})
	if !s.Seats[1].Alive {
		t.Fatal("nine poison is not ten")
	}
	evs := act(t, s, Action{Kind: ActionAdjustCounters, Seat: 2, TargetSeat: 1, CounterName: "poison", Delta: 1})
	if got := leftCause(evs, 1); got != causePoison {
		t.Fatalf("cause = %q in %+v", got, evs)
	}
	// The structural name is read case-insensitively — counter names are
	// data, "Poison" is poison.
	s2 := start(t, nil)
	act(t, s2, Action{Kind: ActionAdjustCounters, Seat: 2, TargetSeat: 3, CounterName: "Poison", Delta: 10})
	if s2.Seats[3].Alive {
		t.Fatal("Poison counters dodged the threshold")
	}
}

func TestSBALegendRule(t *testing.T) {
	s := start(t, nil)
	spec := TokenSpec{Name: "Atraxa, Praetors' Voice", Types: []string{"Legendary", "Creature"}, Power: intPtr(4), Toughness: intPtr(4)}
	first := perm(t, s, 1, spec, 0)
	evs := act(t, s, Action{Kind: ActionCreateToken, Seat: 1, Token: &spec})
	second := s.NextObject - 1
	if got := diedCause(evs, second); got != causeLegendRule {
		t.Fatalf("cause = %q in %+v", got, evs)
	}
	if diedCause(evs, first) != "" || s.Objects[first].Zone != ZoneBattlefield {
		t.Fatal("the engine kept the wrong copy (or killed both)")
	}
	// Different controllers each keep theirs (CR 704.5j is per player).
	s2 := start(t, nil)
	a := perm(t, s2, 1, spec, 0)
	b := perm(t, s2, 2, spec, 0)
	if s2.Objects[a].Zone != ZoneBattlefield || s2.Objects[b].Zone != ZoneBattlefield {
		t.Fatal("legend rule crossed controllers")
	}
	// Stealing the copy puts both under one controller: the legend rule
	// fires in the steal action's own sweep, before anything else can
	// happen to the duplicate.
	evs = act(t, s2, Action{Kind: ActionAddModifier, Seat: 1, Object: b, Modifier: &Modifier{
		Layer: LayerControl, Duration: PermanentDuration, SourceCard: "Act of Treason",
		Delta: Delta{Controller: intPtr(1)}}})
	if got := diedCause(evs, b); got != causeLegendRule {
		t.Fatalf("stolen copy cause = %q in %+v", got, evs)
	}
	if s2.Objects[a].Zone != ZoneBattlefield {
		t.Fatal("the original died to the stolen copy")
	}
	// Non-legendary duplicates are not the legend rule's business.
	s3 := start(t, nil)
	plain := TokenSpec{Name: "Bear", Types: []string{"Creature"}, Power: intPtr(2), Toughness: intPtr(2)}
	perm(t, s3, 1, plain, 0)
	perm(t, s3, 1, plain, 0)
	if n := len(s3.Battlefield(1)); n != 2 {
		t.Fatalf("bears = %d", n)
	}
}

func TestSBAUnattachedAura(t *testing.T) {
	s := start(t, nil)
	// An aura entering with no host is put into the graveyard by the
	// sweep, then ceases (a token).
	evs := act(t, s, Action{Kind: ActionCreateToken, Seat: 1,
		Token: &TokenSpec{Name: "Rancor", Types: []string{"Enchantment", "Aura"}}})
	id := s.NextObject - 1
	fell, ceased := false, false
	for _, e := range evs {
		if e.Kind == EventZoneChanged && e.Object == id && e.Cause == causeUnattached && e.ToZone == ZoneGraveyard {
			fell = true
		}
		if e.Kind == EventObjectCeased && e.Object == id {
			ceased = true
		}
	}
	if !fell || !ceased {
		t.Fatalf("aura events = %+v", evs)
	}
}

func TestSBATokenCeasesOffBattlefield(t *testing.T) {
	s := start(t, nil)
	tok := summon(t, s, 1, "Goblin", 1, 1)
	evs := act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 1, TargetObject: tok})
	sawDied, sawCease := false, false
	for _, e := range evs {
		if e.Kind == EventDied && e.Object == tok {
			sawDied = true
		}
		if e.Kind == EventObjectCeased && e.Object == tok {
			sawCease = true
		}
	}
	if !sawDied || !sawCease {
		t.Fatalf("token death events = %+v", evs)
	}
	if _, ok := s.Objects[tok]; ok {
		t.Fatal("token lingers in the graveyard")
	}
	// Exiling a token ceases it too — any zone but the battlefield.
	s2 := start(t, nil)
	tok2 := summon(t, s2, 1, "Goblin", 1, 1)
	evs = act(t, s2, Action{Kind: ActionMoveZone, Seat: 2, Object: tok2, ToZone: ZoneExile, Cause: "exile"})
	sawCease = false
	for _, e := range evs {
		if e.Kind == EventObjectCeased && e.Object == tok2 {
			sawCease = true
		}
	}
	if !sawCease {
		t.Fatalf("exile events = %+v", evs)
	}
}

func TestSBANoLoyalty(t *testing.T) {
	s := start(t, nil)
	three := 3
	act(t, s, Action{Kind: ActionCreateToken, Seat: 1, Token: &TokenSpec{
		Name: "Chandra", Types: []string{"Planeswalker"}, Loyalty: &three}})
	id := s.NextObject - 1
	if c := s.Characteristics(id); c.Loyalty == nil || *c.Loyalty != 3 {
		t.Fatalf("loyalty = %+v", c.Loyalty)
	}
	evs := act(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, OnObject: id, CounterName: "loyalty", Delta: -3})
	if got := diedCause(evs, id); got != causeNoLoyalty {
		t.Fatalf("cause = %q in %+v", got, evs)
	}
	// One loyalty short of zero is still alive.
	s2 := start(t, nil)
	act(t, s2, Action{Kind: ActionCreateToken, Seat: 1, Token: &TokenSpec{
		Name: "Chandra", Types: []string{"Planeswalker"}, Loyalty: &three}})
	id2 := s2.NextObject - 1
	act(t, s2, Action{Kind: ActionAdjustCounters, Seat: 1, OnObject: id2, CounterName: "loyalty", Delta: -2})
	if s2.Objects[id2].Zone != ZoneBattlefield {
		t.Fatal("planeswalker died at one loyalty")
	}
}

// The recursion case: one action's sweep cascades. A 2/2 anthem-boosted
// to 3/3 with 2 marked damage survives — until the anthem is destroyed,
// which makes the damage lethal, which fells the aura attached to it.
func TestSBACascade(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	anthem := land(t, s, 1, "Glorious Anthem")
	bear := summon(t, s, 1, "Bear", 2, 2)
	aura := perm(t, s, 1, TokenSpec{Name: "Rancor", Types: []string{"Enchantment", "Aura"}}, bear)
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: WhileSourcePresent, SourceObj: anthem,
		Delta: Delta{Power: intPtr(1), Toughness: intPtr(1)}}})
	act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 2, TargetObject: bear})
	if s.Objects[bear].Zone != ZoneBattlefield {
		t.Fatal("3/3 with 2 damage died early")
	}
	// Destroying the anthem: modifier ends → 2/2 with 2 damage → lethal
	// death → aura unattached → aura falls → token ceases. One action.
	evs := act(t, s, Action{Kind: ActionMoveZone, Seat: 2, Object: anthem, ToZone: ZoneGraveyard, Cause: "destroy"})
	if got := diedCause(evs, bear); got != causeLethalDamage {
		t.Fatalf("bear cause = %q in %+v", got, evs)
	}
	fell, ceased := false, false
	for _, e := range evs {
		if e.Kind == EventZoneChanged && e.Object == aura && e.Cause == causeUnattached {
			fell = true
		}
		if e.Kind == EventObjectCeased && e.Object == aura {
			ceased = true
		}
	}
	if !fell || !ceased {
		t.Fatalf("aura cascade = fell %v ceased %v, events %+v", fell, ceased, evs)
	}
}

// Annihilation is P/T-neutral (equal pairs cancel), so it never rescues
// or kills by itself — but it clears the board for the next counter to
// decide. After the pair annihilates, one more -1/-1 on a 1/1 is lethal.
func TestSBAAnnihilationRecheck(t *testing.T) {
	s := start(t, nil)
	bear := summon(t, s, 1, "Bear", 1, 1)
	act(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, OnObject: bear, CounterName: "+1/+1", Delta: 1})
	evs := act(t, s, Action{Kind: ActionAdjustCounters, Seat: 2, OnObject: bear, CounterName: "-1/-1", Delta: 1})
	if got := diedCause(evs, bear); got != "" {
		t.Fatalf("annihilation killed: %+v", evs)
	}
	if o := s.Objects[bear]; o.Counters["+1/+1"] != 0 || o.Counters["-1/-1"] != 0 {
		t.Fatalf("counters = %v", o.Counters)
	}
	// The annihilated +1/+1 no longer protects the bear.
	evs = act(t, s, Action{Kind: ActionAdjustCounters, Seat: 2, OnObject: bear, CounterName: "-1/-1", Delta: 1})
	if got := diedCause(evs, bear); got != causeZeroToughness {
		t.Fatalf("cause = %q in %+v", got, evs)
	}
	if _, ok := s.Objects[bear]; ok {
		t.Fatal("token bear lingers")
	}
}

func TestSBALastStandingEndsGame(t *testing.T) {
	s := start(t, nil)
	act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 40, TargetSeat: 3})
	if s.Status != StatusActive {
		t.Fatal("two players left, game should continue")
	}
	evs := act(t, s, Action{Kind: ActionConcede, Seat: 2})
	ended := false
	for _, e := range evs {
		if e.Kind == EventGameEnded && e.Reason == reasonLastStanding {
			ended = true
		}
	}
	if !ended || s.Status != StatusFinished {
		t.Fatalf("events = %+v status = %s", evs, s.Status)
	}
	if !s.Seats[1].Alive {
		t.Fatal("the winner left too")
	}
}
