package engine

import (
	"strings"
	"testing"
)

func TestMoveZoneResetsBattlefieldState(t *testing.T) {
	s := start(t, nil)
	// A resolved card (not a token — tokens cannot move to a count-only
	// zone) carries the battlefield-only state that must end with a bounce.
	act(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Grizzly Bears",
		Base: &BaseChars{Types: []string{"Creature"}, Power: intPtr(2), Toughness: intPtr(2)}})
	for seat := 1; seat <= 3; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	id := s.NextObject - 1
	act(t, s, Action{Kind: ActionTap, Seat: 1, Object: id})
	act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 1, TargetObject: id})
	act(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, OnObject: id, CounterName: "+1/+1", Delta: 2})
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: id, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: UntilEOT, SourceCard: "Giant Growth",
		Delta: Delta{Power: intPtr(3), Toughness: intPtr(3)}}})
	o := s.Objects[id]
	if !o.Tapped || o.Damage != 1 || o.Counters["+1/+1"] != 2 || len(o.Modifiers) != 1 {
		t.Fatalf("setup: %+v", o)
	}
	// Bounced: the card returns to a count-only hand and everything that
	// existed only on the battlefield ends with it.
	act(t, s, Action{Kind: ActionMoveZone, Seat: 2, Object: id, ToZone: ZoneHand, Cause: "bounce"})
	if _, ok := s.Objects[id]; ok {
		t.Fatal("bounced object still tracked")
	}
	if s.Seats[1].Hand.N != 1 {
		t.Fatalf("hand = %d", s.Seats[1].Hand.N)
	}
}

func TestMoveZoneToLibraryAndTokenRule(t *testing.T) {
	s := start(t, nil)
	// A deckless library's count is unknown; putting a card into it does
	// not invent a total. Make the count known first, then move.
	act(t, s, Action{Kind: ActionSetZoneCount, Seat: 1, Zone: ZoneLibrary, To: intPtr(0)})
	id := land(t, s, 1, "Forest")
	act(t, s, Action{Kind: ActionMoveZone, Seat: 1, Object: id, ToZone: ZoneLibrary, Cause: "put"})
	if _, ok := s.Objects[id]; ok {
		t.Fatal("library object still tracked")
	}
	if s.Seats[1].Library.N != 1 {
		t.Fatalf("library = %d", s.Seats[1].Library.N)
	}
	tok := summon(t, s, 1, "Soldier", 1, 1)
	rejected(t, s, Action{Kind: ActionMoveZone, Seat: 1, Object: tok, ToZone: ZoneHand})
	rejected(t, s, Action{Kind: ActionMoveZone, Seat: 1, Object: tok, ToZone: ZoneLibrary})
	rejected(t, s, Action{Kind: ActionMoveZone, Seat: 1, Object: tok, ToZone: ZoneStack})
	rejected(t, s, Action{Kind: ActionMoveZone, Seat: 1, Object: tok}) // no destination
	rejected(t, s, Action{Kind: ActionMoveZone, Seat: 1, Object: 999, ToZone: ZoneGraveyard})
}

func TestMoveZoneToBattlefieldAnnouncesETB(t *testing.T) {
	s := start(t, nil)
	evs := act(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Reclamation Sage",
		Base: &BaseChars{Types: []string{"Creature"}, Power: intPtr(2), Toughness: intPtr(1)}})
	for seat := 1; seat <= 3; seat++ {
		evs = act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	sawETB := false
	for _, e := range evs {
		if e.Kind == EventCreatureETB {
			sawETB = true
		}
	}
	if !sawETB {
		t.Fatal("creature entered the battlefield with no CREATURE_ETB")
	}
}

func TestTokens(t *testing.T) {
	s := start(t, nil)
	before := s.NextObject
	act(t, s, Action{Kind: ActionCreateToken, Seat: 1, Count: 3, Token: &TokenSpec{
		Name: "Soldier", Types: []string{"Creature"}, Power: intPtr(1), Toughness: intPtr(1)}})
	if n := len(s.Battlefield(1)); n != 3 {
		t.Fatalf("battlefield = %d", n)
	}
	if s.NextObject != before+3 {
		t.Fatalf("ids minted to %d", s.NextObject)
	}
	rejected(t, s, Action{Kind: ActionCreateToken, Seat: 1}) // no spec
	rejected(t, s, Action{Kind: ActionCreateToken, Seat: 1, Count: -1, Token: &TokenSpec{Name: "X"}})
	rejected(t, s, Action{Kind: ActionCreateToken, Seat: 1, Count: maxTokenBatch + 1, Token: &TokenSpec{Name: "X"}})
	// Count defaults to one; noncreature tokens announce no ETB.
	evs := act(t, s, Action{Kind: ActionCreateToken, Seat: 2, Token: &TokenSpec{
		Name: "Treasure", Types: []string{"Artifact"}}})
	for _, e := range evs {
		if e.Kind == EventCreatureETB {
			t.Fatal("treasure announced a CREATURE_ETB")
		}
	}
}

func TestTapUntap(t *testing.T) {
	s := start(t, nil)
	a := summon(t, s, 1, "Bear A", 2, 2)
	b := summon(t, s, 1, "Bear B", 2, 2)
	act(t, s, Action{Kind: ActionTap, Seat: 1, Object: a})
	if !s.Objects[a].Tapped || s.Objects[b].Tapped {
		t.Fatal("tap state wrong")
	}
	rejected(t, s, Action{Kind: ActionTap, Seat: 1, Object: a}) // already tapped
	act(t, s, Action{Kind: ActionUntap, Seat: 1, All: true})
	if s.Objects[a].Tapped || s.Objects[b].Tapped {
		t.Fatal("untap all failed")
	}
	rejected(t, s, Action{Kind: ActionUntap, Seat: 1, All: true}) // nothing to untap
	// A stack object cannot be tapped.
	id := s.NextObject
	act(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Bear C"})
	rejected(t, s, Action{Kind: ActionTap, Seat: 1, Object: id})
	// All only touches the acting seat's permanents.
	c := summon(t, s, 2, "Goblin", 1, 1)
	act(t, s, Action{Kind: ActionTap, Seat: 1, Object: c}) // single: any controller
	act(t, s, Action{Kind: ActionUntap, Seat: 2, Object: c})
	act(t, s, Action{Kind: ActionTap, Seat: 1, All: true})
	if s.Objects[c].Tapped {
		t.Fatal("tap all crossed controllers")
	}
}

func TestPhased(t *testing.T) {
	s := start(t, nil)
	id := summon(t, s, 1, "Spectral Bear", 2, 2)
	act(t, s, Action{Kind: ActionSetPhased, Seat: 1, Object: id, Phased: true})
	if !s.Objects[id].Phased || len(s.Battlefield(1)) != 0 {
		t.Fatal("phased bear still on the battlefield listing")
	}
	if n := len(s.ZoneObjects(1, ZoneBattlefield)); n != 1 {
		t.Fatalf("zone listing should include phased: %d", n)
	}
	rejected(t, s, Action{Kind: ActionSetPhased, Seat: 1, Object: id, Phased: true})
	act(t, s, Action{Kind: ActionSetPhased, Seat: 1, Object: id})
	if s.Objects[id].Phased {
		t.Fatal("phase in failed")
	}
}

func TestAttachDetach(t *testing.T) {
	s := start(t, nil)
	bear := summon(t, s, 1, "Bear", 2, 2)
	sword := summon(t, s, 1, "Sword of Fire and Ice", 0, 0)
	act(t, s, Action{Kind: ActionAttach, Seat: 1, Object: sword, AttachTo: bear})
	if s.Objects[sword].AttachedTo != bear || len(s.Objects[bear].Attachments) != 1 {
		t.Fatal("edge not made")
	}
	rejected(t, s, Action{Kind: ActionAttach, Seat: 1, Object: sword, AttachTo: bear}) // already
	rejected(t, s, Action{Kind: ActionAttach, Seat: 1, Object: sword, AttachTo: sword})
	// Re-equip is UNATTACHED + ATTACHED so the log reads correctly.
	other := summon(t, s, 1, "Other Bear", 2, 2)
	evs := act(t, s, Action{Kind: ActionAttach, Seat: 1, Object: sword, AttachTo: other})
	sawUnattach, sawAttach := false, false
	for _, e := range evs {
		if e.Kind == EventUnattached {
			sawUnattach = true
		}
		if e.Kind == EventAttached {
			sawAttach = true
		}
	}
	if !sawUnattach || !sawAttach {
		t.Fatalf("re-equip events = %+v", evs)
	}
	act(t, s, Action{Kind: ActionDetach, Seat: 1, Object: sword})
	if s.Objects[sword].AttachedTo != 0 || len(s.Objects[other].Attachments) != 0 {
		t.Fatal("detach failed")
	}
	rejected(t, s, Action{Kind: ActionDetach, Seat: 1, Object: sword})
}

func TestAttachmentFallsWhenHostLeaves(t *testing.T) {
	s := start(t, nil)
	bear := summon(t, s, 1, "Bear", 2, 2)
	aura := summon(t, s, 1, "Rancor", 0, 0)
	act(t, s, Action{Kind: ActionAttach, Seat: 1, Object: aura, AttachTo: bear})
	act(t, s, Action{Kind: ActionMoveZone, Seat: 2, Object: bear, ToZone: ZoneGraveyard, Cause: "destroy"})
	if s.Objects[aura].AttachedTo != 0 {
		t.Fatal("aura stayed attached to a dead bear")
	}
}

func TestWhileSourcePresentEndsWithSource(t *testing.T) {
	s := start(t, nil)
	anthem := land(t, s, 1, "Glorious Anthem")
	bear := summon(t, s, 1, "Bear", 2, 2)
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: WhileSourcePresent, SourceObj: anthem,
		Delta: Delta{Power: intPtr(1), Toughness: intPtr(1)}}})
	if s.Characteristics(bear).Power == nil || *s.Characteristics(bear).Power != 3 {
		t.Fatalf("anthem not applied: %+v", s.Characteristics(bear))
	}
	// The anthem leaving is asserted as MODIFIER_REMOVED — not derivable
	// cheaply, so the log shows it.
	evs := act(t, s, Action{Kind: ActionMoveZone, Seat: 2, Object: anthem, ToZone: ZoneGraveyard, Cause: "destroy"})
	removed := false
	for _, e := range evs {
		if e.Kind == EventModifierRemoved && e.Object == bear {
			removed = true
		}
	}
	if !removed {
		t.Fatalf("no MODIFIER_REMOVED: %+v", evs)
	}
	if p := s.Characteristics(bear).Power; p == nil || *p != 2 {
		t.Fatalf("anthem survived its source: %+v", s.Characteristics(bear))
	}
}

func TestCounters(t *testing.T) {
	s := start(t, nil)
	// Player counters: named, arbitrary, unbounded — poison and energy and
	// whatever the next set invents are all just rows.
	act(t, s, Action{Kind: ActionAdjustCounters, Seat: 2, TargetSeat: 1, CounterName: "poison", Delta: 4})
	act(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, CounterName: "energy", Delta: 3})
	if s.Seats[1].Counters["poison"] != 4 || s.Seats[1].Counters["energy"] != 3 {
		t.Fatalf("counters = %v", s.Seats[1].Counters)
	}
	act(t, s, Action{Kind: ActionSetCounters, Seat: 1, CounterName: "poison", To: intPtr(0)})
	if s.Seats[1].Counters["poison"] != 0 {
		t.Fatal("set failed")
	}
	// Object counters, including the structural +1/+1 pair.
	id := summon(t, s, 1, "Bear", 2, 2)
	act(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, OnObject: id, CounterName: "+1/+1", Delta: 2})
	act(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, OnObject: id, CounterName: "stun", Delta: 1})
	c := s.Characteristics(id)
	if *c.Power != 4 || *c.Toughness != 4 {
		t.Fatalf("pt = %d/%d", *c.Power, *c.Toughness)
	}
	if s.Objects[id].Counters["stun"] != 1 {
		t.Fatalf("stun = %d", s.Objects[id].Counters["stun"])
	}
	rejected(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, CounterName: "", Delta: 1})
	rejected(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, CounterName: "poison"})
	rejected(t, s, Action{Kind: ActionSetCounters, Seat: 1, CounterName: "poison"})
	rejected(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, OnObject: 999, CounterName: "x", Delta: 1})
	rejected(t, s, Action{Kind: ActionAdjustCounters, Seat: 42, CounterName: "x", Delta: 1})
}

func TestLifeChanges(t *testing.T) {
	s := start(t, nil)
	act(t, s, Action{Kind: ActionChangeLife, Seat: 2, TargetSeat: 1, Delta: -7, SourceCard: "Lightning Bolt"})
	if s.Seats[1].Life != 33 {
		t.Fatalf("life = %d", s.Seats[1].Life)
	}
	act(t, s, Action{Kind: ActionChangeLife, Seat: 1, To: intPtr(20)})
	if s.Seats[1].Life != 20 {
		t.Fatal("absolute set failed")
	}
	// Life at zero or below is a representable state; the loss is a
	// state-based action (MAD-324), not a write rejection here.
	act(t, s, Action{Kind: ActionChangeLife, Seat: 1, Delta: -25})
	if s.Seats[1].Life != -5 {
		t.Fatalf("life = %d", s.Seats[1].Life)
	}
	rejected(t, s, Action{Kind: ActionChangeLife, Seat: 1})
	rejected(t, s, Action{Kind: ActionChangeLife, Seat: 1, TargetSeat: 9, Delta: 1})
}

func TestDamage(t *testing.T) {
	s := start(t, nil)
	bear := summon(t, s, 2, "Bear", 2, 2)
	atraxa := summon(t, s, 1, "Atraxa", 4, 4)
	// The token spec named her, but commander-ness comes from the seat's
	// commander record — make the identity real for the fold.
	s.Objects[atraxa].Identity = Identity{Card: "Atraxa, Praetors' Voice"}

	// Damage to a player: DAMAGE_DEALT plus its asserted LIFE_CHANGED.
	evs := act(t, s, Action{Kind: ActionDealDamage, Seat: 1, Amount: 6,
		SourceObj: atraxa, TargetSeat: 2, CombatDmg: true})
	kinds := map[EventKind]int{}
	for _, e := range evs {
		kinds[e.Kind]++
	}
	if kinds[EventDamageDealt] != 1 || kinds[EventLifeChanged] != 1 {
		t.Fatalf("kinds = %v", kinds)
	}
	if s.Seats[2].Life != 34 {
		t.Fatalf("life = %d", s.Seats[2].Life)
	}
	// Commander damage derives from the combat rows — never a counter.
	if s.Seats[2].CommanderDamage["Atraxa, Praetors' Voice"] != 6 {
		t.Fatalf("commander damage = %v", s.Seats[2].CommanderDamage)
	}
	// Non-commander sources and non-combat damage do not accumulate.
	act(t, s, Action{Kind: ActionDealDamage, Seat: 1, Amount: 3, SourceObj: bear, TargetSeat: 2, CombatDmg: true})
	act(t, s, Action{Kind: ActionDealDamage, Seat: 1, Amount: 2, SourceObj: atraxa, TargetSeat: 2})
	if s.Seats[2].CommanderDamage["Atraxa, Praetors' Voice"] != 6 {
		t.Fatalf("commander damage drifted: %v", s.Seats[2].CommanderDamage)
	}
	// Damage to an object is marked, and marked damage clears at cleanup.
	act(t, s, Action{Kind: ActionDealDamage, Seat: 2, Amount: 1, TargetObject: bear})
	if s.Objects[bear].Damage != 1 {
		t.Fatalf("marked = %d", s.Objects[bear].Damage)
	}
	rejected(t, s, Action{Kind: ActionDealDamage, Seat: 1, Amount: 0, TargetSeat: 2})
	rejected(t, s, Action{Kind: ActionDealDamage, Seat: 1, Amount: -3, TargetSeat: 2})
	rejected(t, s, Action{Kind: ActionDealDamage, Seat: 1, Amount: 3, TargetSeat: 2, TargetObject: bear})
	rejected(t, s, Action{Kind: ActionDealDamage, Seat: 1, Amount: 3, SourceObj: 999, TargetSeat: 2})
}

func TestFlagsAndExclusivity(t *testing.T) {
	s := start(t, nil)
	act(t, s, Action{Kind: ActionSetFlag, Seat: 1, Flag: "city's blessing", Value: "true"})
	if s.Seats[1].Flags["city's blessing"] != "true" {
		t.Fatal("blessing not set")
	}
	act(t, s, Action{Kind: ActionSetFlag, Seat: 1, Flag: "monarch", Value: "true"})
	act(t, s, Action{Kind: ActionSetFlag, Seat: 2, Flag: "monarch", Value: "true"})
	if s.Seats[1].Flags["monarch"] != "" || s.Seats[2].Flags["monarch"] != "true" {
		t.Fatalf("monarch not exclusive: %v %v", s.Seats[1].Flags, s.Seats[2].Flags)
	}
	act(t, s, Action{Kind: ActionSetFlag, Seat: 3, Flag: "initiative", Value: "true"})
	act(t, s, Action{Kind: ActionSetFlag, Seat: 1, Flag: "initiative", Value: "true"})
	if s.Seats[3].Flags["initiative"] != "" || s.Seats[1].Flags["initiative"] != "true" {
		t.Fatal("initiative not exclusive")
	}
	act(t, s, Action{Kind: ActionSetFlag, Seat: 1, Flag: "monarch"})
	if _, ok := s.Seats[1].Flags["monarch"]; ok {
		t.Fatal("clear failed")
	}
	rejected(t, s, Action{Kind: ActionSetFlag, Seat: 1, Flag: " ", Value: "x"})
}

func TestDraw(t *testing.T) {
	s := start(t, []SeatConfig{
		{Seat: 1, Name: "Collin", Deck: map[string]int{"Forest": 10, "Cultivate": 2}},
		{Seat: 2, Name: "Bob"},
	})
	// A draw with spoken identities: one public count row plus one
	// seat-visible identity row.
	evs := act(t, s, Action{Kind: ActionDraw, Seat: 1, Count: 2, Cards: []string{"Forest", "Cultivate"}})
	var sawPublic, sawSeat bool
	for _, e := range evs {
		switch {
		case e.Kind == EventCardDrawn && e.Visibility == VisibilityPublic:
			sawPublic = true
		case e.Kind == EventCardKnown && e.Visibility == VisibilitySeat && e.VisibleSeat == 1:
			sawSeat = true
		case e.Kind == EventCardKnown && e.Visibility == VisibilityPublic:
			t.Fatal("identities leaked into a public row")
		}
	}
	if !sawPublic || !sawSeat {
		t.Fatalf("draw events = %+v", evs)
	}
	p := s.Seats[1]
	if p.Hand.N != 2 || p.Library.N != 10 || p.LibraryComp["Forest"] != 9 || p.LibraryComp["Cultivate"] != 1 {
		t.Fatalf("hand %d library %d comp %v", p.Hand.N, p.Library.N, p.LibraryComp)
	}
	if !p.LibraryExact {
		t.Fatal("identified draws must keep the composition exact")
	}
	if len(p.HandKnown) != 2 {
		t.Fatalf("hand known = %v", p.HandKnown)
	}
	// A draw nobody named: counts move, composition becomes an upper bound.
	act(t, s, Action{Kind: ActionDraw, Seat: 1})
	if s.Seats[1].LibraryExact {
		t.Fatal("unidentified draw must mark the composition bounded")
	}
	// Looking does not remove.
	act(t, s, Action{Kind: ActionLook, Seat: 1, Cards: []string{"Forest"}, FromZone: ZoneLibrary})
	if s.Seats[1].LibraryComp["Forest"] != 9 || s.Seats[1].Hand.N != 3 {
		t.Fatalf("look changed state: %+v", s.Seats[1])
	}
	// Overdraw against a known library is rejected.
	rejected(t, s, Action{Kind: ActionDraw, Seat: 1, Count: 99})
	rejected(t, s, Action{Kind: ActionDraw, Seat: 1, Count: 2, Cards: []string{"Forest"}})
	// A deckless seat's library is unknown: any draw is allowed.
	act(t, s, Action{Kind: ActionDraw, Seat: 2, Count: 7})
	if s.Seats[2].Hand.N != 7 {
		t.Fatalf("hand = %d", s.Seats[2].Hand.N)
	}
}

func TestMill(t *testing.T) {
	s := start(t, []SeatConfig{
		{Seat: 1, Name: "Collin", Deck: map[string]int{"Forest": 5, "Cultivate": 2}},
		{Seat: 2, Name: "Bob"},
	})
	evs := act(t, s, Action{Kind: ActionMill, Seat: 1, Count: 2, Cards: []string{"Cultivate", "Forest"}})
	if len(evs) != 2 || evs[0].Kind != EventObjectCreated {
		t.Fatalf("mill events = %+v", evs)
	}
	p := s.Seats[1]
	if p.Library.N != 5 || p.LibraryComp["Cultivate"] != 1 {
		t.Fatalf("library = %d comp = %v", p.Library.N, p.LibraryComp)
	}
	if n := len(s.ZoneObjects(1, ZoneGraveyard)); n != 2 {
		t.Fatalf("graveyard = %d", n)
	}
	// Milling face-down lands unknown identities, never guesses.
	act(t, s, Action{Kind: ActionMill, Seat: 1, Count: 1})
	last := s.NextObject - 1
	if s.Objects[last].Identity.Card != "" {
		t.Fatalf("face-down mill guessed %q", s.Objects[last].Identity.Card)
	}
	if s.Seats[1].LibraryExact {
		t.Fatal("unidentified mill must mark the composition bounded")
	}
	rejected(t, s, Action{Kind: ActionMill, Seat: 1, Count: 99})
	rejected(t, s, Action{Kind: ActionMill, Seat: 1})
}

func TestRevealAndLook(t *testing.T) {
	s := start(t, nil)
	evs := act(t, s, Action{Kind: ActionReveal, Seat: 2, Cards: []string{" Damnation ", "Force of Will"}})
	if evs[0].Kind != EventCardRevealed || evs[0].Visibility != VisibilityPublic {
		t.Fatalf("reveal = %+v", evs[0])
	}
	look := act(t, s, Action{Kind: ActionLook, Seat: 2, Cards: []string{"top card"}, FromZone: ZoneLibrary})
	if look[0].Visibility != VisibilitySeat || look[0].VisibleSeat != 2 {
		t.Fatalf("look visibility = %+v", look[0])
	}
	rejected(t, s, Action{Kind: ActionReveal, Seat: 2})
	rejected(t, s, Action{Kind: ActionLook, Seat: 2})
}

func TestDeclareEffect(t *testing.T) {
	s := start(t, nil)
	id := summon(t, s, 1, "Bonehoard Dracosaur", 4, 4)
	evs := act(t, s, Action{Kind: ActionDeclareEffect, Seat: 1, SourceObj: id,
		Effect: "When this enters, deal 2 damage to any target"})
	if evs[0].Kind != EventEffectDeclared || evs[0].SourceObj != id {
		t.Fatalf("effect = %+v", evs[0])
	}
	rejected(t, s, Action{Kind: ActionDeclareEffect, Seat: 1, Effect: "  "})
}

func TestSetZoneCount(t *testing.T) {
	s := start(t, nil)
	act(t, s, Action{Kind: ActionSetZoneCount, Seat: 1, Zone: ZoneLibrary, To: intPtr(99)})
	act(t, s, Action{Kind: ActionSetZoneCount, Seat: 1, Zone: ZoneHand, To: intPtr(7)})
	if s.Seats[1].Library.N != 99 || s.Seats[1].Hand.N != 7 {
		t.Fatalf("counts = %+v", s.Seats[1])
	}
	rejected(t, s, Action{Kind: ActionSetZoneCount, Seat: 1, Zone: ZoneGraveyard, To: intPtr(3)})
	rejected(t, s, Action{Kind: ActionSetZoneCount, Seat: 1, Zone: ZoneHand, To: intPtr(-1)})
}

func TestModifiersValidation(t *testing.T) {
	s := start(t, nil)
	id := summon(t, s, 1, "Bear", 2, 2)
	rejected(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: id}) // no modifier
	rejected(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: id, Modifier: &Modifier{
		Layer: "nonsense", Duration: PermanentDuration, Delta: Delta{Power: intPtr(1)}}})
	rejected(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: id, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: "forever"}})
	rejected(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: id, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: PermanentDuration}}) // empty delta
	rejected(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: id, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: PermanentDuration, SourceObj: 999, Delta: Delta{Power: intPtr(1)}}})
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: id, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: PermanentDuration, SourceCard: "Giant Growth",
		Delta: Delta{Power: intPtr(3), Toughness: intPtr(3)}}})
	mid := s.Objects[id].Modifiers[0].ID
	act(t, s, Action{Kind: ActionRemoveModifier, Seat: 1, Object: id, ModifierID: mid})
	rejected(t, s, Action{Kind: ActionRemoveModifier, Seat: 1, Object: id, ModifierID: mid})
}

func TestDeclareAttackersTapAndVigilance(t *testing.T) {
	s := start(t, nil)
	bear := summon(t, s, 1, "Bear", 2, 2)
	knight := summon(t, s, 1, "Knight", 2, 2)
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: knight, Modifier: &Modifier{
		Layer: LayerAbility, Duration: PermanentDuration, SourceCard: "Always Watching",
		Delta: Delta{AddKeywords: []string{"Vigilance"}}}})
	evs := act(t, s, Action{Kind: ActionDeclareAttackers, Seat: 1, Attackers: []AttackAssignment{
		{Object: bear, TargetSeat: 2}, {Object: knight, TargetSeat: 3}}})
	sawTap := false
	for _, e := range evs {
		if e.Kind == EventTapChanged && e.Object == bear {
			sawTap = true
		}
		if e.Kind == EventTapChanged && e.Object == knight {
			t.Fatal("vigilance knight tapped")
		}
	}
	if !sawTap {
		t.Fatal("bear attacked untapped")
	}
	if !s.Objects[bear].Tapped || s.Objects[knight].Tapped {
		t.Fatal("tap state wrong after attackers")
	}
	rejected(t, s, Action{Kind: ActionDeclareAttackers, Seat: 1, Attackers: []AttackAssignment{{Object: bear, TargetSeat: 2}}}) // tapped now
	rejected(t, s, Action{Kind: ActionDeclareAttackers, Seat: 1, Attackers: []AttackAssignment{}})
	rejected(t, s, Action{Kind: ActionDeclareAttackers, Seat: 1, Attackers: []AttackAssignment{{Object: 999, TargetSeat: 2}}})
	rejected(t, s, Action{Kind: ActionDeclareAttackers, Seat: 1, Attackers: []AttackAssignment{{Object: knight}}})
	// Blockers record and stay untapped.
	gob := summon(t, s, 2, "Goblin", 1, 1)
	act(t, s, Action{Kind: ActionDeclareBlockers, Seat: 2, Blockers: []BlockAssignment{{Blocker: gob, Attackers: []int64{knight}}}})
	if s.Objects[gob].Tapped {
		t.Fatal("blocker tapped")
	}
	if len(s.Blockers) != 1 {
		t.Fatal("blockers not recorded")
	}
	// Combat resolution is scoped out — declared, rejected cleanly.
	rejected(t, s, Action{Kind: ActionResolveCombat, Seat: 1})
}

func TestErrorCarriesErrInvalid(t *testing.T) {
	s := start(t, nil)
	_, err := Apply(s, Action{Kind: ActionDraw, Seat: 42, Count: 1})
	if err == nil || !strings.Contains(err.Error(), "seat 42") {
		t.Fatalf("err = %v", err)
	}
}
