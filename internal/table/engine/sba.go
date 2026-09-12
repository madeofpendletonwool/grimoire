package engine

// State-based actions (CR 704), the deterministic check run against the
// post-action state whenever the engine would move the game forward — the
// same moments a player would receive priority (CR 704.3). The check runs
// as a sweep over a working clone: one finding per round, folded in, then
// re-checked, until nothing fires. Consequences that depend on the whole
// modifier stack at that instant (a death) are asserted as events so
// viewers never re-run CR 704 — that is model-doc invariant 4.

import (
	"sort"
	"strconv"
)

// sbaCauses is the fixed cause vocabulary for engine-asserted rows.
const (
	causeZeroLife       = "sba_zero_life"
	causePoison         = "sba_poison"
	causeCommanderDmg   = "sba_commander_damage"
	causeZeroToughness  = "sba_zero_toughness"
	causeLethalDamage   = "sba_lethal_damage"
	causeLegendRule     = "sba_legend_rule"
	causeNoLoyalty      = "sba_no_loyalty"
	causeUnattached     = "sba_unattached"
	causeTokenCeased    = "sba_token_ceased"
	causeConcession     = "concession"
	reasonLastStanding  = "last_standing"
	poisonThreshold     = 10
	commanderThreshold  = 21
	commanderTaxPerCast = 2
	maxSBARounds        = 4096
)

// sweepStateBasedActions runs the CR 704 loop over a working state (the
// post-action position) until no more actions fire, returning every
// asserted row in order. The working state is advanced by folding each
// round's rows, so later rounds see earlier consequences — annihilation
// can drop a creature to zero toughness, an aura's host dying can leave
// the aura unattached — exactly the recursion CR 704.3 mandates.
func sweepStateBasedActions(s *State, a Action) []Event {
	if s.Status != StatusActive {
		return nil
	}
	sba := Action{Kind: a.Kind, Seat: a.Seat, Source: "system"}
	var out []Event
	for i := 0; i < maxSBARounds && s.Status == StatusActive; i++ {
		evs := s.sbaRound(sba)
		if len(evs) == 0 {
			break
		}
		out = append(out, evs...)
		s.FoldInto(evs)
	}
	return out
}

// sbaRound reports the first state-based action that fires, with its
// event rows, in a fixed order over deterministic key sets: player losses
// first, then object deaths, the legend rule, unattached auras, counter
// annihilation, token cessation, and the last-standing win. One finding
// per round keeps simultaneous actions honest — a duplicate legendary
// dying of lethal damage is already gone when the legend scan runs.
func (s *State) sbaRound(sba Action) []Event {
	// CR 704.5a/c and 903.10a: a player loses on life at zero or less,
	// ten poison counters, or 21+ combat damage from one commander.
	for _, seat := range s.seatIDs() {
		p := s.Seats[seat]
		if !p.Alive {
			continue
		}
		if p.Life <= 0 {
			return s.leaveEvents(sba, seat, causeZeroLife, "")
		}
		if counterValue(p.Counters, "poison") >= poisonThreshold {
			return s.leaveEvents(sba, seat, causePoison, "")
		}
		for _, name := range sortedKeys(p.CommanderDamage) {
			if p.CommanderDamage[name] >= commanderThreshold {
				return s.leaveEvents(sba, seat, causeCommanderDmg, name)
			}
		}
	}

	// CR 704.5f/g/i: creatures die at zero toughness or of lethal marked
	// damage (indestructible says no), planeswalkers at no loyalty.
	// Lethal damage includes the deathtouch rule (CR 702.2c): any nonzero
	// damage from a deathtouch source is lethal, tracked per source.
	for _, id := range s.objectIDs() {
		o, ok := s.Objects[id]
		if !ok || o.Zone != ZoneBattlefield || o.Phased {
			continue
		}
		c := s.Characteristics(id)
		creature := hasString(c.Types, "Creature")
		if creature && c.Toughness != nil {
			if *c.Toughness <= 0 {
				return s.deathEvents(sba, o, causeZeroToughness)
			}
			if !hasString(c.Keywords, "Indestructible") &&
				(o.Damage >= *c.Toughness || s.deathtouchMarked(o)) {
				return s.deathEvents(sba, o, causeLethalDamage)
			}
		}
		if hasString(c.Types, "Planeswalker") && c.Loyalty != nil && *c.Loyalty <= 0 {
			return s.deathEvents(sba, o, causeNoLoyalty)
		}
	}

	// CR 704.5j: the legend rule. Same name, same controller, legendary —
	// the engine keeps the oldest (deterministic; the correction path is
	// one amend away if the table meant otherwise).
	seen := map[string]bool{}
	for _, id := range s.objectIDs() {
		o, ok := s.Objects[id]
		if !ok || o.Zone != ZoneBattlefield || o.Phased {
			continue
		}
		c := s.Characteristics(id)
		if !hasString(c.Types, "Legendary") || c.Name == "" {
			continue
		}
		key := legendKey(c.Controller, c.Name)
		if seen[key] {
			return s.deathEvents(sba, o, causeLegendRule)
		}
		seen[key] = true
	}

	// CR 704.5p: an aura on the battlefield attached to nothing is put
	// into its owner's graveyard. (Equipment may sit unattached; an
	// equipment's host leaving already broke the edge in the fold.)
	for _, id := range s.objectIDs() {
		o, ok := s.Objects[id]
		if !ok || o.Zone != ZoneBattlefield || o.Phased || o.AttachedTo != 0 {
			continue
		}
		if hasString(s.Characteristics(id).Types, "Aura") {
			evs := s.detachAll(sba, o)
			return append(evs, sba.stamp(Event{Kind: EventZoneChanged, Object: o.ID,
				From: ZoneBattlefield, ToZone: ZoneGraveyard, Cause: causeUnattached}))
		}
	}

	// CR 704.5r: +1/+1 and -1/-1 counters on the same permanent
	// annihilate pairwise.
	for _, id := range s.objectIDs() {
		o, ok := s.Objects[id]
		if !ok || o.Zone != ZoneBattlefield {
			continue
		}
		plus, minus := o.Counters["+1/+1"], o.Counters["-1/-1"]
		pairs := min(plus, minus)
		if pairs <= 0 {
			continue
		}
		return []Event{
			sba.stamp(Event{Kind: EventCounterChanged, Object: o.ID, Name: "+1/+1", Delta: -pairs}),
			sba.stamp(Event{Kind: EventCounterChanged, Object: o.ID, Name: "-1/-1", Delta: -pairs}),
		}
	}

	// CR 704.5d: a token in a zone other than the battlefield ceases to
	// exist.
	for _, id := range s.objectIDs() {
		o, ok := s.Objects[id]
		if ok && o.Identity.Token != nil && o.Zone != ZoneBattlefield {
			return []Event{sba.stamp(Event{Kind: EventObjectCeased, Object: o.ID, Cause: causeTokenCeased})}
		}
	}

	// CR 104.2a via 800.5: one player left standing ends the game.
	if s.aliveCount() == 1 && len(s.Seats) >= 2 {
		return []Event{sba.stamp(Event{Kind: EventGameEnded, Reason: reasonLastStanding})}
	}
	return nil
}

// deathEvents builds one battlefield death: the attachment and
// while-source-present consequences first (so the log reads correctly),
// then the DIED row the fold moves the object with.
func (s *State) deathEvents(sba Action, o *Object, cause string) []Event {
	evs := s.detachAll(sba, o)
	return append(evs, sba.stamp(Event{Kind: EventDied, Object: o.ID, Cause: cause}))
}

// leaveEvents builds a player leaving the game (CR 800.4): everything the
// player owns that is on the battlefield drops its edges and ends the
// effects it was the source of, control they hold over others' objects
// ends, then the PLAYER_LEFT row, and — if it was their turn — the turn
// ends and passes to the next player in the pruned order. The fold does
// the owned-object removal; these rows are the readable log of it.
func (s *State) leaveEvents(a Action, seat int, cause, card string) []Event {
	var evs []Event
	for _, id := range s.objectIDs() {
		o := s.Objects[id]
		if o.Zone != ZoneBattlefield {
			continue
		}
		if o.Owner == seat {
			evs = append(evs, s.detachAll(a, o)...)
			continue
		}
		// CR 800.4a: effects giving the leaving player control end.
		for _, mod := range o.Modifiers {
			if mod.Layer == LayerControl && mod.Delta.Controller != nil && *mod.Delta.Controller == seat {
				evs = append(evs, a.stamp(Event{Kind: EventModifierRemoved, Object: o.ID, ModifierID: mod.ID}))
			}
		}
	}
	evs = append(evs, a.stamp(Event{Kind: EventPlayerLeft, TargetSeat: seat, Cause: cause, Card: card}))
	if s.TurnSeat == seat {
		next := nextSeat(s, seat)
		evs = append(evs,
			a.stamp(Event{Kind: EventTurnEnded, Turn: s.Turn}),
			a.stamp(Event{Kind: EventTurnStarted, Turn: s.Turn + 1, TurnSeat: next}),
			a.stamp(Event{Kind: EventStepEntered, Phase: "beginning", Step: "untap"}))
	}
	return evs
}

// deathtouchMarked reports whether the object carries nonzero damage
// from a deathtouch source — lethal for any toughness (CR 702.2c). The
// check reads the source's computed keywords, so printed deathtouch
// survives the source's own death.
func (s *State) deathtouchMarked(o *Object) bool {
	for srcID, dmg := range o.DamageBySource {
		if dmg <= 0 {
			continue
		}
		if src, ok := s.Objects[srcID]; ok && !src.Phased &&
			hasString(s.Keywords(srcID), "Deathtouch") {
			return true
		}
	}
	return false
}

// CommanderTax returns the commander tax owed for the next cast of a
// commander from the command zone: {2} per previous cast from there
// (CR 903.8), derived from the log the fold keeps — never a stored field.
func (s *State) CommanderTax(card string) int {
	return commanderTaxPerCast * s.CommanderCasts[card]
}

// seatIDs lists the seats ascending — deterministic key order.
func (s *State) seatIDs() []int {
	out := make([]int, 0, len(s.Seats))
	for seat := range s.Seats {
		out = append(out, seat)
	}
	sort.Ints(out)
	return out
}

// legendKey groups legendary permanents for the legend rule: controller
// and case-folded name — two players may each keep their own copy, one
// player cannot keep two.
func legendKey(controller int, name string) string {
	return strconv.Itoa(controller) + "|" + foldASCII(name)
}

// foldASCII lowercases ASCII in place — names are compared
// case-insensitively the way equalFold does, without an import.
func foldASCII(s string) string {
	out := []byte(s)
	for i := range out {
		if 'A' <= out[i] && out[i] <= 'Z' {
			out[i] += 'a' - 'A'
		}
	}
	return string(out)
}

// counterValue reads a structurally-understood counter
// case-insensitively — the name is data, "Poison" and "poison" are the
// same counter to the rules that key on it.
func counterValue(counters map[string]int, name string) int {
	if v, ok := counters[name]; ok {
		return v
	}
	for k, v := range counters {
		if equalFold(k, name) {
			return v
		}
	}
	return 0
}

// sortedKeys lists a map's keys ascending, for deterministic scans.
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
