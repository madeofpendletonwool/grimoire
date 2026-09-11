package engine

import "testing"

// fourSeats is the commander pod the issue asks for: four seats, four
// commanders, the damage bookkeeping across all of them.
func fourSeats() []SeatConfig {
	return []SeatConfig{
		{Seat: 1, Name: "Collin", Commander: "Atraxa, Praetors' Voice"},
		{Seat: 2, Name: "Bob", Commander: "Krenko, Mob Boss"},
		{Seat: 3, Name: "Alice", Commander: "Urza, Lord High Artificer"},
		{Seat: 4, Name: "Dave", Commander: "Nekusar, the Mindrazer"},
	}
}

// commanderOf finds a seat's commander object wherever it is.
func commanderOf(t *testing.T, s *State, seat int) int64 {
	t.Helper()
	card := s.Seats[seat].Commander
	for _, id := range s.objectIDs() {
		if o, ok := s.Objects[id]; ok && o.Identity.Card == card {
			return id
		}
	}
	t.Fatalf("no commander object for seat %d", seat)
	return 0
}

// The issue's named case: commander damage across four seats, including
// a commander that changed control. Seat 2 steals seat 1's Atraxa and
// attacks seat 4 with her — the damage is still Atraxa's, attributed
// against seat 4, and 21 of it ends seat 4's game even though seat 2
// was the one swinging (CR 903.10a: the card is the commander, whoever
// controls it).
func TestCommanderDamageAcrossFourSeatsWithControlChange(t *testing.T) {
	s := start(t, fourSeats())
	atraxa := commanderOf(t, s, 1)
	// The steal: a layer-2 control modifier, never a write.
	act(t, s, Action{Kind: ActionAddModifier, Seat: 2, Object: atraxa, Modifier: &Modifier{
		Layer: LayerControl, Duration: PermanentDuration, SourceCard: "Act of Treason",
		Delta: Delta{Controller: intPtr(2)}}})
	if c := s.Characteristics(atraxa); c.Controller != 2 {
		t.Fatalf("steal failed: controller %d", c.Controller)
	}
	// Twenty combat damage from the stolen Atraxa: not yet lethal.
	act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 20,
		SourceObj: atraxa, TargetSeat: 4, CombatDmg: true})
	if got := s.Seats[4].CommanderDamage["Atraxa, Praetors' Voice"]; got != 20 {
		t.Fatalf("commander damage = %d", got)
	}
	if !s.Seats[4].Alive {
		t.Fatal("seat 4 left early")
	}
	act(t, s, Action{Kind: ActionChangeLife, Seat: 4, To: intPtr(40)})
	// Damage from the other commanders tracks separately, per source —
	// and the life that leaves with it has to go back, or seat 4 dies of
	// life loss before the commander threshold can speak.
	krenko := commanderOf(t, s, 2)
	urza := commanderOf(t, s, 3)
	act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 10,
		SourceObj: krenko, TargetSeat: 4, CombatDmg: true})
	act(t, s, Action{Kind: ActionDealDamage, Seat: 3, Amount: 11,
		SourceObj: urza, TargetSeat: 4, CombatDmg: true})
	act(t, s, Action{Kind: ActionChangeLife, Seat: 4, To: intPtr(40)})
	// 20 Atraxa + 10 Krenko + 11 Urza = 41 combat damage, but no source
	// reached 21: seat 4 is alive (CR 903.10a is per commander).
	if !s.Seats[4].Alive {
		t.Fatal("mixed commander damage crossed the threshold")
	}
	// One more point from the stolen Atraxa ends it — and the event
	// names the commander that dealt the 21.
	evs := act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 1,
		SourceObj: atraxa, TargetSeat: 4, CombatDmg: true})
	left := false
	for _, e := range evs {
		if e.Kind == EventPlayerLeft && e.TargetSeat == 4 {
			left = e.Cause == causeCommanderDmg && e.Card == "Atraxa, Praetors' Voice"
		}
	}
	if !left || s.Seats[4].Alive {
		t.Fatalf("events = %+v seat4 = %+v", evs, s.Seats[4])
	}
	// Non-combat commander damage never counts toward 21 (already true
	// on seat 2: 21 non-combat pings from Krenko leave them alive).
	s2 := start(t, fourSeats())
	k2 := commanderOf(t, s2, 2)
	act(t, s2, Action{Kind: ActionDealDamage, Seat: 2, Amount: 21,
		SourceObj: k2, TargetSeat: 1})
	if !s2.Seats[1].Alive || len(s2.Seats[1].CommanderDamage) != 0 {
		t.Fatalf("non-combat damage became commander damage: %+v", s2.Seats[1])
	}
}

func TestCommanderTax(t *testing.T) {
	s := start(t, nil)
	atraxa := "Atraxa, Praetors' Voice"
	if tax := s.CommanderTax(atraxa); tax != 0 {
		t.Fatalf("first cast tax = %d", tax)
	}
	// castFromCommand is the tax loop: cast, resolve, then exercise the
	// zone-change replacement back to the command zone (CR 903.9) so the
	// next cast is from there.
	castFromCommand := func(seat int, card string) {
		t.Helper()
		act(t, s, Action{Kind: ActionCast, Seat: seat, FromZone: ZoneCommand, Card: card,
			Base: &BaseChars{Types: []string{"Legendary", "Creature"}, Power: intPtr(4), Toughness: intPtr(4)}})
		for seat := 1; seat <= 3; seat++ {
			act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
		}
		id := commanderOf(t, s, seat)
		act(t, s, Action{Kind: ActionMoveZone, Seat: seat, Object: id,
			ToZone: ZoneCommand, Cause: "commander-replacement"})
	}
	toMain(t, s)
	castFromCommand(1, atraxa)
	if tax := s.CommanderTax(atraxa); tax != 2 {
		t.Fatalf("second cast tax = %d", tax)
	}
	// Casts from anywhere but the command zone do not accrue the tax
	// (only casts from the command zone are the tax base).
	act(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Cultivate"})
	for seat := 1; seat <= 3; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	if tax := s.CommanderTax(atraxa); tax != 2 {
		t.Fatalf("tax drifted on an unrelated cast: %d", tax)
	}
	castFromCommand(1, atraxa)
	if tax := s.CommanderTax(atraxa); tax != 4 {
		t.Fatalf("third cast tax = %d", tax)
	}
	// Other seats' commanders have their own ledger.
	if tax := s.CommanderTax("Krenko, Mob Boss"); tax != 0 {
		t.Fatalf("krenko tax = %d", tax)
	}
}

// The zone-change replacement choice (CR 903.9): a commander that would
// go to the graveyard may go to the command zone instead. The choice is
// an ordinary zone move — amend or a direct MOVE_ZONE both express it —
// and returning to the command zone does not reset the tax.
func TestCommanderZoneChangeReplacement(t *testing.T) {
	s := start(t, nil)
	atraxa := "Atraxa, Praetors' Voice"
	toMain(t, s)
	act(t, s, Action{Kind: ActionCast, Seat: 1, FromZone: ZoneCommand, Card: atraxa,
		Base: &BaseChars{Types: []string{"Creature"}, Power: intPtr(4), Toughness: intPtr(4)}})
	for seat := 1; seat <= 3; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	id := commanderOf(t, s, 1)
	if s.Objects[id].Zone != ZoneBattlefield {
		t.Fatalf("commander zone = %s", s.Objects[id].Zone)
	}
	// She dies: the sweep's default destination is the graveyard.
	evs := act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 4, TargetObject: id})
	if got := diedCause(evs, id); got != causeLethalDamage {
		t.Fatalf("cause = %q", got)
	}
	if s.Objects[id].Zone != ZoneGraveyard {
		t.Fatalf("dead commander zone = %s", s.Objects[id].Zone)
	}
	// The owner's replacement choice: to the command zone instead.
	act(t, s, Action{Kind: ActionMoveZone, Seat: 1, Object: id,
		ToZone: ZoneCommand, Cause: "commander-replacement"})
	if s.Objects[id].Zone != ZoneCommand {
		t.Fatalf("replacement zone = %s", s.Objects[id].Zone)
	}
	// Cast her again: the tax is {4} now — returning never reset it.
	act(t, s, Action{Kind: ActionCast, Seat: 1, FromZone: ZoneCommand, Card: atraxa,
		Base: &BaseChars{Types: []string{"Creature"}, Power: intPtr(4), Toughness: intPtr(4)}})
	if tax := s.CommanderTax(atraxa); tax != 4 {
		t.Fatalf("tax after replacement = %d", tax)
	}
	// The direct form: choosing command at destruction time, without the
	// graveyard hop.
	s2 := start(t, nil)
	krenko := "Krenko, Mob Boss"
	toMain(t, s2)
	act(t, s2, Action{Kind: ActionPassPriority, Seat: 1}) // seat 2 responds
	act(t, s2, Action{Kind: ActionCast, Seat: 2, FromZone: ZoneCommand, Card: krenko,
		Base: &BaseChars{Types: []string{"Legendary", "Creature"}, Power: intPtr(3), Toughness: intPtr(3)}})
	for seat := 2; seat <= 3; seat++ {
		act(t, s2, Action{Kind: ActionPassPriority, Seat: seat})
	}
	act(t, s2, Action{Kind: ActionPassPriority, Seat: 1})
	id2 := commanderOf(t, s2, 2)
	act(t, s2, Action{Kind: ActionMoveZone, Seat: 2, Object: id2,
		ToZone: ZoneCommand, Cause: "commander-replacement"})
	if s2.Objects[id2].Zone != ZoneCommand || s2.CommanderTax(krenko) != 2 {
		t.Fatalf("krenko = %s tax = %d", s2.Objects[id2].Zone, s2.CommanderTax(krenko))
	}
}
