package engine

// The fold. fold(events) → State is deterministic and total: it never
// errors and never panics, because the log's integrity is enforced at
// write time — the reducer validates, the store assigns contiguous
// ordinals — and a fold that could fail would make rewind a gamble instead
// of a truncation. Rows that reference things the fold has not seen (a
// corrupted or foreign log) are skipped, not guessed at.
//
// The derived-never-stored rule from ADR 9 lives here: expiry of
// until_end_of_turn is computed from TURN_ENDED anchors rather than stored,
// commander damage is recomputed from DAMAGE_DEALT rows rather than kept
// as a counter, and marked damage clears at cleanup exactly when the
// anchor says so.

// Fold builds the state a whole log folds to.
func Fold(events []Event) *State {
	s := NewState()
	for i := range events {
		s.foldEvent(events[i])
	}
	return s
}

// FoldInto advances a state by more events — the incremental form, used by
// streaming clients and by the prefix property tests to prove
// fold(log[:n]) is the state after the first n events.
func (s *State) FoldInto(events []Event) *State {
	if s == nil {
		s = NewState()
	}
	for i := range events {
		s.foldEvent(events[i])
	}
	return s
}

func (s *State) foldEvent(e Event) {
	if e.Ord > s.LastOrd {
		s.LastOrd = e.Ord
	}
	switch e.Kind {
	case EventGameStarted:
		s.foldGameStarted(e)
	case EventGameEnded:
		s.Status = StatusFinished
	case EventTurnStarted:
		s.Turn = e.Turn
		s.TurnSeat = e.TurnSeat
		s.PrioritySeat = e.TurnSeat
		s.resetTurnScoped()
		s.Passed = nil
		s.Attackers = nil
		s.Blockers = nil
		s.AttackOrders = nil
		s.CombatResolved = false
	case EventStepEntered:
		s.Phase = e.Phase
		s.Step = e.Step
		s.Passed = nil
		s.CombatResolved = false
		// CR 511.2: creatures stop attacking and blocking at end of
		// combat; the declarations are over even though the rows remain.
		if e.Phase == "combat" && e.Step == "end_of_combat" {
			s.Attackers = nil
			s.Blockers = nil
			s.AttackOrders = nil
		}
		// Entering most steps grants priority to the active player
		// (CR 117.2a); untap and cleanup grant it to no one (CR 502.3,
		// 514.3a). Zero is the no-priority sentinel.
		if stepGrantsPriority(e.Phase, e.Step) {
			s.PrioritySeat = s.TurnSeat
		} else {
			s.PrioritySeat = 0
		}
	case EventTurnEnded:
		// The expiry anchor: everything that lasts until end of turn ends
		// here — modifiers by computation, marked damage with them, the
		// land drop and the pass cycle with the turn.
		for _, o := range s.Objects {
			o.Damage = 0
			o.DamageBySource = nil
			o.Modifiers = dropUntilEOT(o.Modifiers)
		}
		s.resetTurnScoped()
		s.Passed = nil
	case EventPriorityPassed:
		s.PrioritySeat = e.SeatTo
		s.Passed = append(s.Passed, e.ActorSeat)
	case EventStackPushed:
		if e.Mode == "triggered" {
			// A trigger reaching the stack drains its queue entry; the
			// push rides the priority grant that flushed it and hands
			// priority to no one (CR 117.5 — the player who was about to
			// receive priority still does).
			s.drainQueuedTrigger(StackItem{Controller: e.Controller, Ability: e.Ability, Card: e.Card})
		}
		s.Stack = append(s.Stack, StackItem{Object: e.Object, Mode: e.Mode, Card: e.Card,
			Ability: e.Ability, Controller: e.Controller, Targets: e.Targets})
		// The player who put it there receives priority (CR 117.2c —
		// casting, activating, and special actions return priority to
		// the actor) and the pass cycle starts over. Triggered abilities
		// are the exception: they were not that player's act.
		if e.Controller != 0 && e.Mode != "triggered" {
			s.PrioritySeat = e.Controller
		}
		s.Passed = nil
	case EventStackResolved:
		if len(s.Stack) > 0 {
			s.Stack = s.Stack[:len(s.Stack)-1]
		}
		// CR 117.3b: after the top object resolves the active player
		// receives priority, and everyone may respond again.
		s.PrioritySeat = s.TurnSeat
		s.Passed = nil
	case EventObjectCreated:
		s.foldObjectCreated(e)
	case EventZoneChanged:
		s.foldZoneChanged(e)
	case EventDied:
		// A death asserted by the engine (CR 704) or declared at the
		// table: it is a battlefield → graveyard move, folded through
		// the same bookkeeping any zone change gets.
		if o, ok := s.Objects[e.Object]; ok && o.Zone == ZoneBattlefield {
			s.foldZoneChanged(Event{Kind: EventZoneChanged, Object: e.Object,
				From: ZoneBattlefield, ToZone: ZoneGraveyard, Cause: e.Cause})
		}
	case EventPlayerLeft:
		s.foldPlayerLeft(e)
	case EventObjectCeased:
		// A token off the battlefield ceases to exist (CR 704.5d); the
		// row is the whole of it. Edges cannot survive to here — leaving
		// the battlefield cleared them.
		delete(s.Objects, e.Object)
	case EventLandPlayed:
		if p, ok := s.Seats[e.Controller]; ok {
			p.LandsThisTurn++
		}
	case EventCast:
		// Casts from the command zone are the commander tax base
		// (CommanderTax derives from it). A declared base on the cast
		// is the card's characteristics — the commander object mints
		// from config with none, so this is where it learns them.
		if e.From == ZoneCommand && e.Card != "" {
			s.CommanderCasts[e.Card]++
		}
		if e.Base != nil {
			if o, ok := s.Objects[e.Object]; ok {
				o.Base = *e.Base
			}
		}
	case EventAttackersDeclared:
		s.Attackers = e.Attackers
	case EventBlockersDeclared:
		s.Blockers = e.Blockers
		s.AttackOrders = e.AttackOrders
	case EventCombatResolved:
		s.CombatResolved = true
	case EventDamageDealt:
		s.foldDamageDealt(e)
	case EventDamageMarked:
		if o, ok := s.Objects[e.Object]; ok {
			o.Damage += e.Amount
			if e.SourceObj != 0 {
				if o.DamageBySource == nil {
					o.DamageBySource = map[int64]int{}
				}
				o.DamageBySource[e.SourceObj] += e.Amount
			}
		}
	case EventLifeChanged:
		if p, ok := s.Seats[e.TargetSeat]; ok {
			if e.To != nil {
				p.Life = *e.To
			} else {
				p.Life += e.Delta
			}
		}
	case EventCounterChanged:
		if e.Object != 0 {
			if o, ok := s.Objects[e.Object]; ok {
				if o.Counters == nil {
					o.Counters = map[string]int{}
				}
				applyCounter(o.Counters, e)
			}
			return
		}
		if p, ok := s.Seats[e.TargetSeat]; ok {
			if p.Counters == nil {
				p.Counters = map[string]int{}
			}
			applyCounter(p.Counters, e)
		}
	case EventFlagChanged:
		if p, ok := s.Seats[e.TargetSeat]; ok {
			if p.Flags == nil {
				p.Flags = map[string]string{}
			}
			if e.Value == "" {
				delete(p.Flags, e.Flag)
			} else {
				p.Flags[e.Flag] = e.Value
			}
		}
	case EventTapChanged:
		if o, ok := s.Objects[e.Object]; ok {
			o.Tapped = e.Tapped
		}
	case EventPhaseChanged:
		if o, ok := s.Objects[e.Object]; ok {
			o.Phased = e.Phased
		}
	case EventAttached:
		att, aok := s.Objects[e.Object]
		host, hok := s.Objects[e.TargetObject]
		if !aok || !hok {
			return
		}
		att.AttachedTo = host.ID
		if !containsID(host.Attachments, att.ID) {
			host.Attachments = append(host.Attachments, att.ID)
		}
	case EventUnattached:
		if att, ok := s.Objects[e.Object]; ok {
			old := att.AttachedTo
			att.AttachedTo = 0
			if host, ok := s.Objects[old]; ok {
				host.Attachments = dropID(host.Attachments, att.ID)
			}
		}
	case EventModifierAdded:
		if o, ok := s.Objects[e.Object]; ok && e.Modifier != nil {
			o.Modifiers = append(o.Modifiers, *e.Modifier)
			if e.Modifier.ID >= s.NextModifier {
				s.NextModifier = e.Modifier.ID + 1
			}
		}
	case EventModifierRemoved:
		if o, ok := s.Objects[e.Object]; ok {
			o.Modifiers = dropModifierID(o.Modifiers, e.ModifierID)
		}
	case EventTriggerFired:
		s.TriggerQueue = append(s.TriggerQueue, TriggerItem{SourceObj: e.Object, Card: e.Card,
			Effect: e.Effect, Controller: e.Controller, Targets: e.Targets})
	case EventCardDrawn:
		if p, ok := s.Seats[e.TargetSeat]; ok {
			p.Hand = Count{Known: true, N: p.Hand.N + e.Count}
			if p.Library.Known {
				p.Library.N -= e.Count
				if p.Library.N < 0 {
					p.Library.N = 0
				}
			}
			if !e.Identified && len(p.LibraryComp) > 0 {
				// Cards the system cannot name left a known library: the
				// composition is an upper bound per name from here on, and
				// the probability layer must say so rather than guess.
				p.LibraryExact = false
			}
		}
	case EventCardKnown:
		// Drawn identities left the library and joined the seat's known
		// hand; a look's did not — the row is the whole of a look.
		if e.Drawn {
			if p, ok := s.Seats[e.TargetSeat]; ok {
				for _, c := range e.Cards {
					removeFromComp(p, c)
					p.HandKnown = append(p.HandKnown, c)
				}
			}
		}
	case EventCardRevealed, EventEffectDeclared:
		// Public log surface; no state to derive.
	case EventZoneCountSet:
		if p, ok := s.Seats[e.TargetSeat]; ok {
			if e.To != nil {
				switch e.Zone {
				case ZoneHand:
					p.Hand = KnownCount(*e.To)
				case ZoneLibrary:
					p.Library = KnownCount(*e.To)
				}
			}
		}
	}
}

// resetTurnScoped clears the per-turn facts at a turn boundary.
func (s *State) resetTurnScoped() {
	for _, p := range s.Seats {
		p.LandsThisTurn = 0
	}
}

// foldGameStarted rebuilds the game from its echo: seats, life, the
// known-card universe each library began as, and the commander objects in
// the command zone — the one place objects are created by config rather
// than by an action, because every seat's commander is public fact.
func (s *State) foldGameStarted(e Event) {
	s.Status = StatusActive
	s.Format = e.Format
	s.StartingLife = e.StartingLife
	s.CommanderCasts = map[string]int{}
	order := []int{}
	for _, sc := range e.Seats {
		life := sc.StartingLife
		if life == 0 {
			life = e.StartingLife
		}
		p := &Player{
			Seat: sc.Seat, Name: sc.Name, UserID: sc.UserID, DeckID: sc.DeckID,
			Commander: sc.Commander, StartingLife: life, Life: life, Alive: true,
			// The hand and a deckless library start unknown, not zero:
			// the tracker has not been told what it cannot see, and
			// collapsing "not told" into "empty" is the failure that
			// makes a tracker untrustworthy. Draws and counts make them
			// known; a deck makes the library's size known.
			Hand:      UnknownCount(),
			Library:   UnknownCount(),
			HandKnown: []string{},
		}
		if len(sc.Deck) > 0 {
			total := 0
			comp := map[string]int{}
			for name, n := range sc.Deck {
				comp[name] = n
				total += n
			}
			p.Library = KnownCount(total)
			// Deck keeps the full echo and LibraryComp tracks what
			// remains; they start equal but must never share a map,
			// because the fold decrements the composition as named
			// cards leave the library and the universe (MAD-329)
			// reads Deck afterwards.
			p.Deck = copyIntMap(comp)
			p.LibraryComp = comp
			p.LibraryExact = true
		}
		s.Seats[sc.Seat] = p
		order = append(order, sc.Seat)
		if sc.Commander != "" {
			id := s.NextObject
			s.NextObject++
			s.Objects[id] = &Object{ID: id, Identity: Identity{Card: sc.Commander},
				Owner: sc.Seat, Controller: sc.Seat, Zone: ZoneCommand, Counters: map[string]int{}}
		}
	}
	sortSeats(order)
	s.Order = order
}

func (s *State) foldObjectCreated(e Event) {
	o := &Object{
		ID: e.Object, Identity: e.Identity, Owner: e.Owner, Controller: e.Controller,
		Zone: e.ToZone, Counters: map[string]int{},
	}
	if e.Base != nil {
		o.Base = *e.Base
	}
	if o.Identity.Token != nil && e.Base == nil {
		o.Base = tokenBase(o.Identity.Token)
	}
	if e.Object >= s.NextObject {
		s.NextObject = e.Object + 1
	}
	s.Objects[e.Object] = o
	if e.From != "" {
		if p, ok := s.Seats[o.Owner]; ok {
			leaveZone(p, e.From, o.Identity.Card)
		}
	}
}

// foldZoneChanged moves an object, and does the bookkeeping the move
// implies: count-only zones count in and out, composition subtracts named
// departures, and leaving the battlefield ends everything that only existed
// there — counters, modifiers, marked damage, tap and phase state. A card
// that returns is fresh; that is what blinking means.
func (s *State) foldZoneChanged(e Event) {
	o, ok := s.Objects[e.Object]
	if !ok {
		return
	}
	if e.From != "" {
		if p, ok := s.Seats[o.Owner]; ok {
			leaveZone(p, e.From, o.Identity.Card)
		}
	}
	if e.From == ZoneBattlefield {
		o.Modifiers = nil
		o.Counters = map[string]int{}
		o.Damage = 0
		o.DamageBySource = nil
		o.Tapped = false
		o.Phased = false
		for _, att := range o.Attachments {
			if a, ok := s.Objects[att]; ok {
				a.AttachedTo = 0
			}
		}
		o.Attachments = nil
		if o.AttachedTo != 0 {
			if host, ok := s.Objects[o.AttachedTo]; ok {
				host.Attachments = dropID(host.Attachments, o.ID)
			}
			o.AttachedTo = 0
		}
	}
	switch e.ToZone {
	case ZoneHand:
		if p, ok := s.Seats[o.Owner]; ok {
			p.Hand = Count{Known: true, N: p.Hand.N + 1}
		}
		delete(s.Objects, o.ID)
	case ZoneLibrary:
		if p, ok := s.Seats[o.Owner]; ok {
			if p.Library.Known {
				p.Library.N++
			}
			if o.Identity.Card != "" && p.LibraryComp != nil {
				p.LibraryComp[o.Identity.Card]++
			}
		}
		delete(s.Objects, o.ID)
	default:
		o.Zone = e.ToZone
	}
}

// foldPlayerLeft is CR 800.4: a player leaving the game. Their seat stays
// for history but leaves turn order; every object they own leaves the
// game with them (edges and dangling references sanitized); their spells
// and abilities come off the stack; and if it was their turn, the next
// player in the pruned order takes over at the untap step. The asserted
// consequences (UNATTACHED, MODIFIER_REMOVED rows, the TURN_ENDED /
// TURN_STARTED anchors) precede this row in the log — the recovery here
// is the fold's safety net for logs that lack them.
func (s *State) foldPlayerLeft(e Event) {
	seat := e.TargetSeat
	if p, ok := s.Seats[seat]; ok {
		p.Alive = false
	}
	order := s.Order[:0:0]
	for _, v := range s.Order {
		if v != seat {
			order = append(order, v)
		}
	}
	s.Order = order
	passed := s.Passed[:0:0]
	for _, v := range s.Passed {
		if v != seat {
			passed = append(passed, v)
		}
	}
	s.Passed = passed

	// Their waiting triggers leave with them (CR 800.4 — their abilities
	// cease).
	queue := s.TriggerQueue[:0:0]
	for _, q := range s.TriggerQueue {
		if q.Controller != seat {
			queue = append(queue, q)
		}
	}
	s.TriggerQueue = queue

	// Owned objects leave the game; break any edge that pointed at them
	// from either side so nothing dangles.
	gone := map[int64]bool{}
	for id, o := range s.Objects {
		if o.Owner == seat {
			gone[id] = true
		}
	}
	for id := range gone {
		delete(s.Objects, id)
	}
	for _, o := range s.Objects {
		if gone[o.AttachedTo] {
			o.AttachedTo = 0
		}
		keep := o.Attachments[:0:0]
		for _, att := range o.Attachments {
			if !gone[att] {
				keep = append(keep, att)
			}
		}
		o.Attachments = keep
	}

	// Their spells (owned objects now gone) and their abilities come off
	// the stack.
	stack := s.Stack[:0:0]
	for _, item := range s.Stack {
		if item.Controller == seat {
			continue
		}
		if item.Object != 0 && gone[item.Object] {
			continue
		}
		stack = append(stack, item)
	}
	s.Stack = stack

	if s.TurnSeat == seat {
		s.TurnSeat = nextSeat(s, seat)
		s.Phase, s.Step = "beginning", "untap"
		s.PrioritySeat = 0
		s.Attackers = nil
		s.Blockers = nil
	} else if s.PrioritySeat == seat {
		s.PrioritySeat = nextSeat(s, seat)
	}
}

// foldDamageDealt derives commander damage: combat damage from an object
// that is its controller's... its owner seat's commander accumulates
// against the player dealt it. Never a stored counter, never a separate
// event — refold the log and it is here again.
func (s *State) foldDamageDealt(e Event) {
	if !e.Combat || e.TargetSeat == 0 || e.SourceObj == 0 || e.Amount <= 0 {
		return
	}
	o, ok := s.Objects[e.SourceObj]
	if !ok || !s.isCommander(o) {
		return
	}
	p, ok := s.Seats[e.TargetSeat]
	if !ok {
		return
	}
	if p.CommanderDamage == nil {
		p.CommanderDamage = map[string]int{}
	}
	p.CommanderDamage[o.Identity.Card] += e.Amount
}

// leaveZone books a card out of a count-only zone: the hand counts down,
// the library counts down and loses the named card from its composition
// when the departure carried an identity.
func leaveZone(p *Player, zone Zone, card string) {
	switch zone {
	case ZoneHand:
		if p.Hand.Known {
			p.Hand.N--
			if p.Hand.N < 0 {
				p.Hand.N = 0
			}
		}
	case ZoneLibrary:
		if p.Library.Known {
			p.Library.N--
			if p.Library.N < 0 {
				p.Library.N = 0
			}
		}
		if card != "" {
			removeFromComp(p, card)
		} else if len(p.LibraryComp) > 0 {
			p.LibraryExact = false
		}
	}
}

// removeFromComp takes one named card out of a known composition, clamped
// at zero — a total fold never produces negative knowledge.
func removeFromComp(p *Player, card string) {
	if p.LibraryComp == nil {
		return
	}
	if p.LibraryComp[card] > 0 {
		p.LibraryComp[card]--
	}
	if p.LibraryComp[card] <= 0 {
		delete(p.LibraryComp, card)
	}
}

// applyCounter folds one COUNTER_CHANGED into a counter map, delta or
// absolute.
func applyCounter(counters map[string]int, e Event) {
	if e.To != nil {
		counters[e.Name] = *e.To
		return
	}
	counters[e.Name] += e.Delta
}

func dropUntilEOT(mods []Modifier) []Modifier {
	out := mods[:0:0]
	for _, m := range mods {
		if m.Duration != UntilEOT {
			out = append(out, m)
		}
	}
	return out
}

func dropModifierID(mods []Modifier, id int64) []Modifier {
	out := mods[:0:0]
	for _, m := range mods {
		if m.ID != id {
			out = append(out, m)
		}
	}
	return out
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func dropID(ids []int64, id int64) []int64 {
	out := ids[:0:0]
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}
