package engine

import (
	"testing"
)

// Combat damage arithmetic (MAD-325): the matrix the issue names — first
// strike + deathtouch, trample + deathtouch assignment, double strike +
// lifelink, protection preventing damage — plus the assignment orders,
// the gates, and the deaths that assert between the two damage steps.

// creature puts a creature token with keywords and colors on a seat's
// battlefield and returns its id — summon, with the keywords the matrix
// needs.
func creature(t *testing.T, s *State, seat int, name string, power, toughness int, keywords ...string) int64 {
	t.Helper()
	id := s.NextObject
	act(t, s, Action{Kind: ActionCreateToken, Seat: seat, Token: &TokenSpec{
		Name: name, Types: []string{"Creature"}, Power: &power, Toughness: &toughness,
		Keywords: keywords}})
	return id
}

// combatColored is creature with colors, for protection's sake.
func combatColored(t *testing.T, s *State, seat int, name string, power, toughness int, colors, keywords []string) int64 {
	t.Helper()
	id := s.NextObject
	act(t, s, Action{Kind: ActionCreateToken, Seat: seat, Token: &TokenSpec{
		Name: name, Types: []string{"Creature"}, Power: &power, Toughness: &toughness,
		Colors: colors, Keywords: keywords}})
	return id
}

// combat drives one combat: attackers declared, blockers (with the
// attacker's damage assignment orders) when given, then RESOLVE_COMBAT,
// whose events come back. Seat 1 is the attacker throughout.
func combat(t *testing.T, s *State, atk []AttackAssignment, blk []BlockAssignment, orders []AttackOrder) []Event {
	t.Helper()
	toStep(t, s, "combat", "declare_attackers")
	act(t, s, Action{Kind: ActionDeclareAttackers, Seat: s.TurnSeat, Attackers: atk})
	if blk != nil {
		toStep(t, s, "combat", "declare_blockers")
		act(t, s, Action{Kind: ActionDeclareBlockers, Seat: atk[0].TargetSeat,
			Blockers: blk, AttackOrders: orders})
	}
	toStep(t, s, "combat", "combat_damage")
	return act(t, s, Action{Kind: ActionResolveCombat, Seat: s.TurnSeat})
}

// find returns the first event of a kind matching pred.
func find(evs []Event, kind EventKind, pred func(Event) bool) *Event {
	for i := range evs {
		if evs[i].Kind == kind && (pred == nil || pred(evs[i])) {
			return &evs[i]
		}
	}
	return nil
}

func TestCombatUnblockedDamageToSeat(t *testing.T) {
	s := start(t, nil)
	bear := creature(t, s, 1, "Bear", 2, 2)
	evs := combat(t, s, []AttackAssignment{{Object: bear, TargetSeat: 2}}, nil, nil)
	if e := find(evs, EventDamageDealt, func(e Event) bool {
		return e.TargetSeat == 2 && e.Combat && e.Amount == 2 && e.SourceObj == bear
	}); e == nil {
		t.Fatalf("no combat damage to seat 2: %s", kindsOf(evs))
	}
	if s.Seats[2].Life != 38 {
		t.Fatalf("seat 2 life = %d", s.Seats[2].Life)
	}
	if evs[len(evs)-1].Kind != EventCombatResolved || !s.CombatResolved {
		t.Fatal("combat not anchored resolved")
	}
	// The anchor is a promise: resolving twice is a second combat.
	rejected(t, s, Action{Kind: ActionResolveCombat, Seat: 1})
}

func TestCombatBlockedMutualDeath(t *testing.T) {
	s := start(t, nil)
	bear := creature(t, s, 1, "Bear", 2, 2)
	gob := creature(t, s, 2, "Goblin", 2, 2)
	combat(t, s, []AttackAssignment{{Object: bear, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: gob, Attackers: []int64{bear}}}, nil)
	if _, ok := s.Objects[bear]; ok {
		t.Fatal("bear survived")
	}
	if _, ok := s.Objects[gob]; ok {
		t.Fatal("goblin survived")
	}
	if s.Seats[2].Life != 40 {
		t.Fatalf("blocked damage reached the seat: %d", s.Seats[2].Life)
	}
}

// The matrix: first strike + deathtouch. The viper's two points are
// lethal to anything (CR 702.2c), and they land in the first step, so the
// giant dies before it can assign its own five back — the death asserts
// between the steps, and the second step has nothing left to assign.
func TestFirstStrikeDeathtouchKillsBlockerFirst(t *testing.T) {
	s := start(t, nil)
	viper := creature(t, s, 1, "Viper", 2, 2, "First Strike", "Deathtouch")
	giant := creature(t, s, 2, "Giant", 5, 5)
	evs := combat(t, s, []AttackAssignment{{Object: viper, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: giant, Attackers: []int64{viper}}}, nil)
	// Deathtouch assigns exactly one point — that is lethal, and the
	// rest of the viper's power lands nowhere without trample.
	if e := find(evs, EventDamageMarked, func(e Event) bool { return e.Object == giant }); e == nil || e.Amount != 1 {
		t.Fatalf("giant not marked: %s", kindsOf(evs))
	}
	died := find(evs, EventDied, func(e Event) bool { return e.Object == giant })
	if died == nil || died.Cause != causeLethalDamage {
		t.Fatalf("giant death = %+v", died)
	}
	if _, ok := s.Objects[giant]; ok {
		t.Fatal("giant survived deathtouch")
	}
	if o := s.Objects[viper]; o.Zone != ZoneBattlefield || o.Damage != 0 {
		t.Fatalf("viper = %+v", o)
	}
	if s.Seats[2].Life != 40 {
		t.Fatalf("seat 2 life = %d", s.Seats[2].Life)
	}
}

// The matrix: trample + deathtouch assignment. One point is lethal, so
// one point satisfies the blocker and three walk over (CR 702.2c,
// 510.1c).
func TestTrampleDeathtouchAssignment(t *testing.T) {
	s := start(t, nil)
	behemoth := creature(t, s, 1, "Behemoth", 4, 4, "Trample", "Deathtouch")
	wall := creature(t, s, 2, "Wall", 2, 2)
	combat(t, s, []AttackAssignment{{Object: behemoth, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: wall, Attackers: []int64{behemoth}}}, nil)
	if _, ok := s.Objects[wall]; ok {
		t.Fatal("wall survived one point of deathtouch")
	}
	if s.Seats[2].Life != 37 {
		t.Fatalf("seat 2 life = %d", s.Seats[2].Life)
	}
	if o := s.Objects[behemoth]; o.Zone != ZoneBattlefield {
		t.Fatalf("behemoth in %s", o.Zone)
	}
}

// The matrix: double strike + lifelink. Two damage steps, two gains —
// four life for four damage, all of it combat.
func TestDoubleStrikeLifelink(t *testing.T) {
	s := start(t, nil)
	mantis := creature(t, s, 1, "Mantis", 2, 2, "Double Strike", "Lifelink")
	evs := combat(t, s, []AttackAssignment{{Object: mantis, TargetSeat: 2}}, nil, nil)
	steps, gains := 0, 0
	for _, e := range evs {
		if e.Kind == EventDamageDealt && e.TargetSeat == 2 && e.Combat && e.Amount == 2 {
			steps++
		}
		if e.Kind == EventLifeChanged && e.TargetSeat == 1 && e.Delta == 2 {
			gains++
		}
	}
	if steps != 2 || gains != 2 {
		t.Fatalf("steps = %d gains = %d (%s)", steps, gains, kindsOf(evs))
	}
	if s.Seats[2].Life != 36 || s.Seats[1].Life != 44 {
		t.Fatalf("life 1=%d 2=%d", s.Seats[1].Life, s.Seats[2].Life)
	}
}

// The matrix: protection preventing damage. The cudgel cannot mark the
// knight, and because prevented damage is never lethal damage it cannot
// trample over it either — the knight's own two land normally.
func TestProtectionPreventsDamage(t *testing.T) {
	s := start(t, nil)
	cudgel := combatColored(t, s, 1, "Cudgel", 3, 3, []string{"Green"}, []string{"Trample"})
	knight := creature(t, s, 2, "Knight", 2, 2, "Protection from Green")
	evs := combat(t, s, []AttackAssignment{{Object: cudgel, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: knight, Attackers: []int64{cudgel}}}, nil)
	if e := find(evs, EventDamageDealt, func(e Event) bool { return e.TargetObject == knight }); e != nil {
		t.Fatalf("protection did not prevent: %+v", *e)
	}
	if e := find(evs, EventDamageDealt, func(e Event) bool { return e.TargetSeat == 2 }); e != nil {
		t.Fatalf("trampled over protection: %+v", *e)
	}
	if o := s.Objects[knight]; o.Zone != ZoneBattlefield || o.Damage != 0 {
		t.Fatalf("knight = %+v", o)
	}
	if o := s.Objects[cudgel]; o.Damage != 2 || o.Zone != ZoneBattlefield {
		t.Fatalf("cudgel = %+v", o)
	}
	if s.Seats[2].Life != 40 {
		t.Fatalf("seat 2 life = %d", s.Seats[2].Life)
	}
}

// The attacker's damage assignment order over multiple blockers
// (CR 509.3): lethal in order, and which blocker survives flips with the
// order. With 4 power against a 3/3 and a 2/2, first-listed takes its
// lethal and the second takes the one point left.
func TestAttackOrderDecidesSurvivors(t *testing.T) {
	// Order [big, small]: 3 to big (dies), 1 to small (survives).
	s := start(t, nil)
	attacker := creature(t, s, 1, "Attacker", 4, 4)
	big := creature(t, s, 2, "Big", 3, 3)
	small := creature(t, s, 2, "Small", 2, 2)
	combat(t, s, []AttackAssignment{{Object: attacker, TargetSeat: 2}},
		[]BlockAssignment{
			{Blocker: big, Attackers: []int64{attacker}},
			{Blocker: small, Attackers: []int64{attacker}},
		}, []AttackOrder{{Attacker: attacker, Blockers: []int64{big, small}}})
	if _, ok := s.Objects[big]; ok {
		t.Fatal("big survived being first in order")
	}
	if s.Objects[small].Zone != ZoneBattlefield || s.Objects[small].Damage != 1 {
		t.Fatalf("small = %+v", s.Objects[small])
	}
	// Order [small, big]: 2 to small (dies), 2 to big (survives).
	s2 := start(t, nil)
	attacker2 := creature(t, s2, 1, "Attacker", 4, 4)
	big2 := creature(t, s2, 2, "Big", 3, 3)
	small2 := creature(t, s2, 2, "Small", 2, 2)
	combat(t, s2, []AttackAssignment{{Object: attacker2, TargetSeat: 2}},
		[]BlockAssignment{
			{Blocker: big2, Attackers: []int64{attacker2}},
			{Blocker: small2, Attackers: []int64{attacker2}},
		}, []AttackOrder{{Attacker: attacker2, Blockers: []int64{small2, big2}}})
	if _, ok := s2.Objects[small2]; ok {
		t.Fatal("small survived being first in order")
	}
	if s2.Objects[big2].Zone != ZoneBattlefield || s2.Objects[big2].Damage != 2 {
		t.Fatalf("big = %+v", s2.Objects[big2])
	}
	// No declared order: blocker ids ascending — big (the lower id)
	// first, so big dies to its three and small carries the spare point.
	s3 := start(t, nil)
	attacker3 := creature(t, s3, 1, "Attacker", 4, 4)
	big3 := creature(t, s3, 2, "Big", 3, 3)
	small3 := creature(t, s3, 2, "Small", 2, 2)
	combat(t, s3, []AttackAssignment{{Object: attacker3, TargetSeat: 2}},
		[]BlockAssignment{
			{Blocker: big3, Attackers: []int64{attacker3}},
			{Blocker: small3, Attackers: []int64{attacker3}},
		}, nil)
	if _, ok := s3.Objects[big3]; ok || s3.Objects[small3].Damage != 1 {
		t.Fatalf("default order not ascending: small = %+v", s3.Objects[small3])
	}
}

// The blocker's order over several attackers (its assignment list): a
// 3-power blocker against a 3/3 and a 2/2 kills whichever it lists first
// and marks the other.
func TestBlockerOrderDecidesSurvivors(t *testing.T) {
	s := start(t, nil)
	big := creature(t, s, 1, "Big", 3, 3)
	small := creature(t, s, 1, "Small", 2, 2)
	blocker := creature(t, s, 2, "Guard", 3, 4)
	combat(t, s, []AttackAssignment{
		{Object: big, TargetSeat: 2}, {Object: small, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: blocker, Attackers: []int64{big, small}}}, nil)
	if _, ok := s.Objects[big]; ok {
		t.Fatal("first-listed attacker survived")
	}
	if o := s.Objects[small]; o.Zone != ZoneBattlefield || o.Damage != 0 {
		t.Fatalf("small = %+v", o)
	}
}

// A blocker killed in the first strike step never assigns back; a
// first-striker without double strike does not assign twice.
func TestFirstStrikeBeatsMutualDeath(t *testing.T) {
	s := start(t, nil)
	knight := creature(t, s, 1, "Knight", 3, 3, "First Strike")
	ogre := creature(t, s, 2, "Ogre", 3, 3)
	combat(t, s, []AttackAssignment{{Object: knight, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: ogre, Attackers: []int64{knight}}}, nil)
	if _, ok := s.Objects[ogre]; ok {
		t.Fatal("ogre survived first strike")
	}
	if o := s.Objects[knight]; o.Zone != ZoneBattlefield || o.Damage != 0 {
		t.Fatalf("knight = %+v", o)
	}
}

// A blocked attacker whose blocker died in the first step stays blocked:
// without trample its remaining power lands nowhere; with trample it
// walks over the gap.
func TestBlockedStaysBlockedUnlessTrample(t *testing.T) {
	s := start(t, nil)
	plain := creature(t, s, 1, "Rush", 4, 4, "First Strike")
	gob := creature(t, s, 2, "Goblin", 2, 2)
	combat(t, s, []AttackAssignment{{Object: plain, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: gob, Attackers: []int64{plain}}}, nil)
	if s.Seats[2].Life != 40 {
		t.Fatalf("non-trampler hit the seat: %d", s.Seats[2].Life)
	}
	s2 := start(t, nil)
	trampler := creature(t, s2, 1, "Stampede", 4, 4, "First Strike", "Trample")
	gob2 := creature(t, s2, 2, "Goblin", 2, 2)
	combat(t, s2, []AttackAssignment{{Object: trampler, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: gob2, Attackers: []int64{trampler}}}, nil)
	if s2.Seats[2].Life != 38 {
		t.Fatalf("trample over the gap = %d", s2.Seats[2].Life)
	}
}

func TestIndestructibleSurvivesCombatLethal(t *testing.T) {
	s := start(t, nil)
	demon := creature(t, s, 1, "Demon", 3, 3, "Indestructible")
	gob := creature(t, s, 2, "Goblin", 3, 3)
	combat(t, s, []AttackAssignment{{Object: demon, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: gob, Attackers: []int64{demon}}}, nil)
	if _, ok := s.Objects[gob]; ok {
		t.Fatal("goblin survived")
	}
	if o := s.Objects[demon]; o.Zone != ZoneBattlefield || o.Damage != 3 {
		t.Fatalf("demon = %+v", o)
	}
	// Deathtouch does not help either: the damage is lethal, the
	// creature just refuses to die.
	s2 := start(t, nil)
	demon2 := creature(t, s2, 1, "Demon", 3, 3, "Indestructible")
	viper := creature(t, s2, 2, "Viper", 1, 1, "Deathtouch")
	combat(t, s2, []AttackAssignment{{Object: demon2, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: viper, Attackers: []int64{demon2}}}, nil)
	if o := s2.Objects[demon2]; o.Zone != ZoneBattlefield || o.Damage != 1 {
		t.Fatalf("demon = %+v", o)
	}
	if _, ok := s2.Objects[viper]; ok {
		t.Fatal("viper survived three damage")
	}
}

// Deathtouch lethality on the receiving side: two points from a
// deathtouch source kill a 6/6 — the per-source tracking is what the
// state-based action reads.
func TestDeathtouchLethalAnyToughness(t *testing.T) {
	s := start(t, nil)
	viper := creature(t, s, 1, "Viper", 2, 2, "Deathtouch")
	giant := creature(t, s, 2, "Giant", 6, 6)
	combat(t, s, []AttackAssignment{{Object: viper, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: giant, Attackers: []int64{viper}}}, nil)
	if _, ok := s.Objects[giant]; ok {
		t.Fatal("giant survived two points of deathtouch")
	}
	if _, ok := s.Objects[viper]; ok {
		t.Fatal("viper survived six damage")
	}
}

// Combat damage at a planeswalker removes loyalty, it does not mark
// (CR 306.7); a planeswalker with no loyalty dies by state-based action.
func TestCombatDamageToPlaneswalkerRemovesLoyalty(t *testing.T) {
	s := start(t, nil)
	jace := perm(t, s, 2, TokenSpec{Name: "Jace", Types: []string{"Planeswalker"}, Loyalty: intPtr(4)}, 0)
	bear := creature(t, s, 1, "Bear", 2, 2)
	evs := combat(t, s, []AttackAssignment{{Object: bear, TargetObject: jace}}, nil, nil)
	if e := find(evs, EventDamageDealt, func(e Event) bool { return e.TargetObject == jace }); e == nil {
		t.Fatalf("no damage at the walker: %s", kindsOf(evs))
	}
	if c := s.Characteristics(jace); c.Loyalty == nil || *c.Loyalty != 2 {
		t.Fatalf("loyalty = %+v", c.Loyalty)
	}
	if o := s.Objects[jace]; o.Damage != 0 {
		t.Fatalf("walker marked damage = %d", o.Damage)
	}
}

// Commander damage derives from DAMAGE_DEALT combat rows — the combat
// path feeds the same derivation the manual path does.
func TestCombatCommanderDamage(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	act(t, s, Action{Kind: ActionCast, Seat: 1, FromZone: ZoneCommand,
		Card: "Atraxa, Praetors' Voice",
		Base: &BaseChars{Types: []string{"Creature"}, Power: intPtr(4), Toughness: intPtr(4)}})
	for seat := 1; seat <= 3; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	atraxa := int64(1) // the commander object, stable across the zone moves
	combat(t, s, []AttackAssignment{{Object: atraxa, TargetSeat: 2}}, nil, nil)
	if s.Seats[2].CommanderDamage["Atraxa, Praetors' Voice"] != 4 {
		t.Fatalf("commander damage = %v", s.Seats[2].CommanderDamage)
	}
}

func TestResolveCombatGating(t *testing.T) {
	s := start(t, nil)
	bear := creature(t, s, 1, "Bear", 2, 2)
	// Wrong step, wrong actor, no attackers.
	toMain(t, s)
	rejected(t, s, Action{Kind: ActionResolveCombat, Seat: 1})
	toStep(t, s, "combat", "declare_attackers")
	rejected(t, s, Action{Kind: ActionResolveCombat, Seat: 2}) // not the active player
	act(t, s, Action{Kind: ActionDeclareAttackers, Seat: 1, Attackers: []AttackAssignment{{Object: bear, TargetSeat: 2}}})
	toStep(t, s, "combat", "declare_blockers")
	rejected(t, s, Action{Kind: ActionResolveCombat, Seat: 1}) // still the wrong step
	toStep(t, s, "combat", "combat_damage")
	// The stack must be empty: an instant on the stack holds combat.
	act(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Giant Growth",
		Base: &BaseChars{Types: []string{"Instant"}}})
	rejected(t, s, Action{Kind: ActionResolveCombat, Seat: 1})
	for seat := 1; seat <= 3; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	// The step cannot be left unresolved — ADVANCE and the all-pass
	// rotation both refuse until the damage has been dealt. Two passes
	// are fine; the third is the step-ending one.
	rejected(t, s, Action{Kind: ActionAdvance, Seat: 1})
	act(t, s, Action{Kind: ActionPassPriority, Seat: 1})
	act(t, s, Action{Kind: ActionPassPriority, Seat: 2})
	rejected(t, s, Action{Kind: ActionPassPriority, Seat: 3})
	act(t, s, Action{Kind: ActionResolveCombat, Seat: 1})
	act(t, s, Action{Kind: ActionAdvance, Seat: 1})
	if s.Step != "end_of_combat" || s.Attackers != nil {
		t.Fatalf("step = %s attackers = %+v", s.Step, s.Attackers)
	}
}

func TestResolveCombatRejectsUnknownStats(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	// A creature whose power was never declared — the tracker cast the
	// card without its characteristics. The arithmetic refuses.
	act(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Mystery Creature",
		Base: &BaseChars{Types: []string{"Creature"}}})
	for seat := 1; seat <= 3; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	id := s.NextObject - 1
	toStep(t, s, "combat", "declare_attackers")
	act(t, s, Action{Kind: ActionDeclareAttackers, Seat: 1, Attackers: []AttackAssignment{{Object: id, TargetSeat: 2}}})
	toStep(t, s, "combat", "combat_damage")
	rejected(t, s, Action{Kind: ActionResolveCombat, Seat: 1})
}

func TestMarkedDamageClearsAtTurnEnd(t *testing.T) {
	s := start(t, nil)
	attacker := creature(t, s, 1, "Bruiser", 5, 5)
	gob := creature(t, s, 2, "Goblin", 2, 2)
	combat(t, s, []AttackAssignment{{Object: attacker, TargetSeat: 2}},
		[]BlockAssignment{{Blocker: gob, Attackers: []int64{attacker}}}, nil)
	if o := s.Objects[attacker]; o.Damage != 2 || o.DamageBySource[gob] != 2 {
		t.Fatalf("damage = %d by-source = %v", o.Damage, o.DamageBySource)
	}
	toTurn(t, s, 2)
	if o := s.Objects[attacker]; o.Damage != 0 || o.DamageBySource != nil {
		t.Fatalf("damage survived cleanup: %+v", o)
	}
}

func TestLifelinkOnManualDamage(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	cleric := creature(t, s, 1, "Cleric", 1, 1, "Lifelink")
	act(t, s, Action{Kind: ActionDealDamage, Seat: 1, SourceObj: cleric, TargetSeat: 3, Amount: 2})
	if s.Seats[3].Life != 38 || s.Seats[1].Life != 42 {
		t.Fatalf("life 1=%d 3=%d", s.Seats[1].Life, s.Seats[3].Life)
	}
}

func TestDeclareBlockersValidation(t *testing.T) {
	s := start(t, nil)
	bear := creature(t, s, 1, "Bear", 2, 2)
	gob := creature(t, s, 2, "Goblin", 1, 1)
	swan := creature(t, s, 3, "Swan", 1, 1)
	wall := perm(t, s, 2, TokenSpec{Name: "Wall", Types: []string{"Artifact"}}, 0)
	toStep(t, s, "combat", "declare_attackers")
	act(t, s, Action{Kind: ActionDeclareAttackers, Seat: 1, Attackers: []AttackAssignment{{Object: bear, TargetSeat: 2}}})
	toStep(t, s, "combat", "declare_blockers")
	// Only creatures block; only the attacked seat's creatures block the
	// attacker; a blocker cannot block the same attacker twice.
	rejected(t, s, Action{Kind: ActionDeclareBlockers, Seat: 2,
		Blockers: []BlockAssignment{{Blocker: wall, Attackers: []int64{bear}}}})
	rejected(t, s, Action{Kind: ActionDeclareBlockers, Seat: 3,
		Blockers: []BlockAssignment{{Blocker: swan, Attackers: []int64{bear}}}})
	rejected(t, s, Action{Kind: ActionDeclareBlockers, Seat: 2,
		Blockers: []BlockAssignment{{Blocker: gob, Attackers: []int64{bear, bear}}}})
	act(t, s, Action{Kind: ActionDeclareBlockers, Seat: 2,
		Blockers: []BlockAssignment{{Blocker: gob, Attackers: []int64{bear}}}})
	// An attack order must name exactly the blockers blocking that
	// attacker.
	rejected(t, s, Action{Kind: ActionDeclareBlockers, Seat: 2,
		Blockers:     []BlockAssignment{{Blocker: gob, Attackers: []int64{bear}}},
		AttackOrders: []AttackOrder{{Attacker: bear, Blockers: []int64{gob, swan}}}})
}
