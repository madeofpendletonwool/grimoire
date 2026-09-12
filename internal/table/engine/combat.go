package engine

// Combat damage arithmetic (MAD-325): CR 510 as pure arithmetic over the
// declared combat. What belongs here is structural and card-agnostic —
// the two damage steps first strike and double strike create, damage
// assignment orders (the attacker's over its blockers, the blocker's over
// its attackers), lethal assignment with deathtouch counting one point as
// lethal, trample overflow, lifelink gains, protection prevention, and
// the deaths that assert between the steps. Nothing here reads oracle
// text: characteristics are the computed ones, and a participant whose
// power or toughness was never declared rejects the resolution rather
// than guessing at it (ADR 11's honesty rule, applied to arithmetic).

import (
	"fmt"
	"strings"
)

// applyResolveCombat is RESOLVE_COMBAT: assign and deal the combat
// damage step's damage. With first strike or double strike anywhere in
// combat there are two steps (CR 510.4) — first-strikers and
// double-strikers assign in the first, double-strikers and creatures with
// neither in the second — and the first step's deaths assert before the
// second assigns, because a creature killed by first strike never deals
// its own damage back.
func applyResolveCombat(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if s.Phase != "combat" || s.Step != "combat_damage" {
		return nil, fmt.Errorf("%w: combat damage is assigned in the combat_damage step (now %s/%s)", ErrInvalid, s.Phase, s.Step)
	}
	if a.Seat != s.TurnSeat {
		return nil, fmt.Errorf("%w: seat %d is not the active player", ErrInvalid, a.Seat)
	}
	if len(s.Stack) > 0 {
		return nil, fmt.Errorf("%w: combat damage is assigned with the stack empty", ErrInvalid)
	}
	if s.CombatResolved {
		return nil, fmt.Errorf("%w: combat damage was already resolved this step", ErrInvalid)
	}
	if len(s.Attackers) == 0 {
		return nil, fmt.Errorf("%w: no attackers are declared", ErrInvalid)
	}
	working := s.clone()
	if err := working.requireKnownCombatStats(); err != nil {
		return nil, err
	}

	var evs []Event
	stampAll := func(rows []Event) []Event {
		for i := range rows {
			rows[i] = a.stamp(rows[i])
		}
		return rows
	}
	if working.combatHasStrikeStep() {
		first := stampAll(working.combatDamageStep(true))
		evs = append(evs, first...)
		working.FoldInto(first)
		// State-based actions assert between the steps (CR 704.3 runs
		// before the second step's assignment).
		evs = append(evs, sweepStateBasedActions(working, a)...)
		if working.Status != StatusActive || working.Phase != "combat" || working.Step != "combat_damage" {
			// A consequence ended the game or moved the turn mid-combat;
			// the anchor is the last honest row.
			return append(evs, a.stamp(Event{Kind: EventCombatResolved})), nil
		}
		evs = append(evs, stampAll(working.combatDamageStep(false))...)
	} else {
		evs = append(evs, stampAll(working.combatDamageStep(false))...)
	}
	return append(evs, a.stamp(Event{Kind: EventCombatResolved})), nil
}

// requireKnownCombatStats is the honest gate: every creature still in
// combat must have its power and toughness declared. Arithmetic over an
// unknown stat is a guess, and the engine does not guess — the resolution
// rejects naming the object, and the correction path is declaring the
// characteristics.
func (s *State) requireKnownCombatStats() error {
	for i := range s.Attackers {
		if err := s.requireCombatantStats(s.Attackers[i].Object); err != nil {
			return err
		}
	}
	for i := range s.Blockers {
		if err := s.requireCombatantStats(s.Blockers[i].Blocker); err != nil {
			return err
		}
	}
	return nil
}

func (s *State) requireCombatantStats(id int64) error {
	if _, ok := s.combatant(id); !ok {
		return nil // no longer in combat: it assigns nothing
	}
	c := s.Characteristics(id)
	if c.Power == nil {
		return fmt.Errorf("%w: object %d's power is unknown — declare its characteristics to resolve combat", ErrInvalid, id)
	}
	if c.Toughness == nil {
		return fmt.Errorf("%w: object %d's toughness is unknown — declare its characteristics to resolve combat", ErrInvalid, id)
	}
	return nil
}

// combatant reports a combat participant that is still on the
// battlefield and unphased — the dead, gone and phased out assign
// nothing.
func (s *State) combatant(id int64) (*Object, bool) {
	o, ok := s.Objects[id]
	if !ok || o.Zone != ZoneBattlefield || o.Phased {
		return nil, false
	}
	return o, true
}

// combatHasStrikeStep reports whether any creature in combat has first
// strike or double strike, which makes the first combat damage step
// happen at all (CR 510.4).
func (s *State) combatHasStrikeStep() bool {
	for i := range s.Attackers {
		if s.participantStrikes(s.Attackers[i].Object) {
			return true
		}
	}
	for i := range s.Blockers {
		if s.participantStrikes(s.Blockers[i].Blocker) {
			return true
		}
	}
	return false
}

func (s *State) participantStrikes(id int64) bool {
	if _, ok := s.combatant(id); !ok {
		return false
	}
	kw := s.Keywords(id)
	return hasString(kw, "First Strike") || hasString(kw, "Double Strike")
}

// assignsInStep reports whether computed keywords assign in a damage
// step: first strike or double strike in the first step, double strike or
// neither in the second (CR 510.4 — first strike only means never the
// second).
func assignsInStep(keywords []string, first bool) bool {
	fs, ds := hasString(keywords, "First Strike"), hasString(keywords, "Double Strike")
	if first {
		return fs || ds
	}
	return ds || !fs
}

// combatDamageStep builds one damage step's rows against the state as it
// stands: attackers in declaration order, then blockers in id order, all
// assignment computed against the same marked damage — combat damage in
// one step is simultaneous (CR 510.1a).
func (s *State) combatDamageStep(first bool) []Event {
	var evs []Event
	for i := range s.Attackers {
		at := s.Attackers[i]
		o, ok := s.combatant(at.Object)
		if !ok || !assignsInStep(s.Keywords(at.Object), first) {
			continue
		}
		evs = append(evs, s.attackDamage(o, s.Characteristics(at.Object), at)...)
	}
	for _, id := range s.objectIDs() {
		bl := s.blockAssignment(id)
		if bl == nil {
			continue
		}
		o, ok := s.combatant(id)
		if !ok || !assignsInStep(s.Keywords(id), first) {
			continue
		}
		evs = append(evs, s.blockDamage(o, s.Characteristics(id), bl)...)
	}
	return evs
}

// attackDamage assigns one attacker's combat damage: full power at the
// defended seat or planeswalker when unblocked, lethal in assignment
// order through its blockers when blocked (a creature stays blocked even
// once its blockers are gone — only trample walks through that gap), and
// trample overflow to what it attacked.
func (s *State) attackDamage(o *Object, c Characteristics, at AttackAssignment) []Event {
	blockers := s.attackerBlockers(at.Object)
	if len(blockers) == 0 {
		return s.damageToPlayerOrWalker(o, c, at.TargetSeat, at.TargetObject, deref(c.Power))
	}
	var live []*Object
	for _, b := range blockers {
		if t, ok := s.combatant(b); ok {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		if hasString(c.Keywords, "Trample") {
			return s.damageToPlayerOrWalker(o, c, at.TargetSeat, at.TargetObject, deref(c.Power))
		}
		return nil
	}
	evs, remaining, stalled := s.assignOrdered(o, c, live)
	if stalled || !hasString(c.Keywords, "Trample") || remaining <= 0 {
		return evs
	}
	return append(evs, s.damageToPlayerOrWalker(o, c, at.TargetSeat, at.TargetObject, remaining)...)
}

// blockDamage assigns one blocker's damage over the attackers it blocks,
// in the defender's declared order. Blockers do not trample.
func (s *State) blockDamage(o *Object, c Characteristics, bl *BlockAssignment) []Event {
	var live []*Object
	for _, at := range bl.Attackers {
		if t, ok := s.combatant(at); ok {
			live = append(live, t)
		}
	}
	evs, _, _ := s.assignOrdered(o, c, live)
	return evs
}

// assignOrdered walks a damage assignment order: lethal damage to each
// target in turn before the next (CR 510.1), deathtouch making a single
// point lethal for assignment (CR 702.2c), and prevention stalling the
// walk — damage that would be prevented is never lethal damage, so an
// attacker the blocker is protected from cannot get past it, and cannot
// trample over it either.
func (s *State) assignOrdered(src *Object, c Characteristics, targets []*Object) (evs []Event, remaining int, stalled bool) {
	remaining = deref(c.Power)
	for _, t := range targets {
		if remaining <= 0 {
			break
		}
		n := min(remaining, s.assignmentLethal(t, c))
		rows, prevented := s.damageToObject(src, c, t, n)
		if prevented {
			return evs, remaining, true
		}
		evs = append(evs, rows...)
		remaining -= n
	}
	return evs, remaining, false
}

// assignmentLethal is what a source must assign to a creature for the
// assignment to count it lethal: one point with deathtouch, otherwise
// toughness minus damage already marked, floored at one (CR 510.1c).
func (s *State) assignmentLethal(target *Object, srcC Characteristics) int {
	if hasString(srcC.Keywords, "Deathtouch") {
		return 1
	}
	tc := s.Characteristics(target.ID)
	return max(1, deref(tc.Toughness)-target.Damage)
}

// damageToObject builds one source → creature-or-planeswalker combat
// damage: the DAMAGE_DEALT row, marked damage on a creature, loyalty
// counters off a planeswalker (CR 306.7 — combat damage to a planeswalker
// removes loyalty, it does not mark), and the lifelink gain. Returns
// prevented=true when protection stops the damage before it exists.
func (s *State) damageToObject(src *Object, c Characteristics, target *Object, amount int) ([]Event, bool) {
	if amount <= 0 {
		return nil, false
	}
	if s.damagePrevented(target.ID, src, c) {
		return nil, true
	}
	sys := Action{Source: "system"}
	dealt := Event{Kind: EventDamageDealt, Amount: amount, Combat: true,
		SourceObj: src.ID, SourceCard: objectName(src), TargetObject: target.ID}
	evs := []Event{sys.stamp(dealt)}
	if s.IsCreature(target.ID) {
		evs = append(evs, sys.stamp(Event{Kind: EventDamageMarked, Object: target.ID,
			Amount: amount, SourceObj: src.ID}))
	}
	if tc := s.Characteristics(target.ID); tc.Loyalty != nil {
		evs = append(evs, sys.stamp(Event{Kind: EventCounterChanged, Object: target.ID,
			Name: "loyalty", Delta: -amount}))
	}
	return append(evs, s.lifelinkGain(src.ID, amount)...), false
}

// damageToPlayerOrWalker builds damage at the defended seat, or — when
// the attack was at a planeswalker — the object it was after.
func (s *State) damageToPlayerOrWalker(src *Object, c Characteristics, seat int, target int64, amount int) []Event {
	if target != 0 {
		t, ok := s.combatant(target)
		if !ok {
			return nil
		}
		evs, _ := s.damageToObject(src, c, t, amount)
		return evs
	}
	if amount <= 0 || seat == 0 {
		return nil
	}
	sys := Action{Source: "system"}
	evs := []Event{
		sys.stamp(Event{Kind: EventDamageDealt, Amount: amount, Combat: true,
			SourceObj: src.ID, SourceCard: objectName(src), TargetSeat: seat}),
		sys.stamp(Event{Kind: EventLifeChanged, TargetSeat: seat, Delta: -amount,
			SourceObj: src.ID, SourceCard: objectName(src)}),
	}
	return append(evs, s.lifelinkGain(src.ID, amount)...)
}

// attackerBlockers lists an attacker's blockers in its damage assignment
// order: the attacker's declared order (CR 509.3) when there is one,
// blocker id order otherwise. Dead blockers stay listed; the assignment
// filters them.
func (s *State) attackerBlockers(attacker int64) []int64 {
	for i := range s.AttackOrders {
		if s.AttackOrders[i].Attacker == attacker {
			return s.AttackOrders[i].Blockers
		}
	}
	var ids []int64
	for i := range s.Blockers {
		if containsID(s.Blockers[i].Attackers, attacker) {
			ids = append(ids, s.Blockers[i].Blocker)
		}
	}
	sortInt64s(ids)
	return ids
}

// blockAssignment finds the declaration a blocker stands behind.
func (s *State) blockAssignment(blocker int64) *BlockAssignment {
	for i := range s.Blockers {
		if s.Blockers[i].Blocker == blocker {
			return &s.Blockers[i]
		}
	}
	return nil
}

// damagePrevented reports whether a target's protection stops damage
// from the source: each "Protection from ..." keyword names qualities,
// and a quality matches the source by color, by type (singularized), by
// name, or by being everything.
func (s *State) damagePrevented(targetID int64, src *Object, c Characteristics) bool {
	for _, kw := range s.Keywords(targetID) {
		for _, q := range protectionQualities(kw) {
			if qualityMatches(q, c) {
				return true
			}
		}
	}
	return false
}

const protectionPrefix = "protection from "

// protectionQualities parses one protection keyword into its qualities;
// "Protection from Black and Blue" is two. Anything else is not a
// protection keyword.
func protectionQualities(kw string) []string {
	if len(kw) <= len(protectionPrefix) || !equalFold(kw[:len(protectionPrefix)], protectionPrefix) {
		return nil
	}
	var out []string
	for _, part := range strings.FieldsFunc(kw[len(protectionPrefix):], func(r rune) bool { return r == ',' }) {
		for _, word := range strings.Split(part, " and ") {
			word = strings.TrimSpace(word)
			if word != "" {
				out = append(out, word)
			}
		}
	}
	return out
}

// qualityMatches reports whether a protection quality describes the
// source: everything matches all, colors match colors, plurals match
// types ("Creatures" matches a Creature), and a quality may name the
// card itself.
func qualityMatches(q string, c Characteristics) bool {
	switch {
	case equalFold(q, "everything") || equalFold(q, "all") ||
		equalFold(q, "all colors") || equalFold(q, "all sources"):
		return true
	}
	for _, col := range c.Colors {
		if equalFold(q, col) {
			return true
		}
	}
	for _, ty := range c.Types {
		if equalFold(singularQuality(q), ty) {
			return true
		}
	}
	return c.Name != "" && equalFold(q, c.Name)
}

// singularQuality strips the plural s so "Creatures" matches the
// "Creature" type.
func singularQuality(q string) string {
	if len(q) > 1 && q[len(q)-1] == 's' {
		return q[:len(q)-1]
	}
	return q
}

// objectName names an object for damage rows: its card, its token, or
// its declared base name.
func objectName(o *Object) string {
	if o.Identity.Card != "" {
		return o.Identity.Card
	}
	if o.Identity.Token != nil && o.Identity.Token.Name != "" {
		return o.Identity.Token.Name
	}
	return o.Base.Name
}
