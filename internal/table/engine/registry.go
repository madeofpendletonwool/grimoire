package engine

// The trigger registry (MAD-335, stage 5 of MAD-321): the "Magic ADHD
// assistant", built honestly. The engine never reads oracle text and
// never invents a trigger (ADR 11) — a trigger is REGISTERED, once per
// card name, by a player who typed it or by a model proposal a human
// confirmed, against a structural event the engine already emits. The
// registry then fires on real events into the 2c trigger queue, which
// reaches the stack in APNAP order at the next priority grant like any
// declared trigger.
//
// The registry is data in, events out: it arrives as a loaded
// TriggerRegistry on every Apply, so the reducer stays pure — no I/O,
// no clock, no model — and a fold of the same log still reproduces the
// same state without the registry at all (the fired rows are already
// TRIGGER_FIRED events in the log; the registry only ever decides
// whether a NEW row is appended).
//
// Scope semantics — with no scope column on mtg_trigger_registry, the
// scope is fixed by the kind, chosen so the overwhelming majority of
// real cards are covered and the rest stay with manual DECLARE_TRIGGER:
//
//	LAND_PLAYED           a land enters under the source's controller
//	CREATURE_ETB          a creature enters under the source's
//	                      controller (the source itself included)
//	UPKEEP                the source's controller's upkeep begins
//	OPPONENT_CASTS_SPELL  a player other than the source's controller
//	                      casts a spell
//	ATTACKS               the source itself is declared as an attacker
//	DIES                  a creature under the source's controller dies,
//	                      or the source itself dies
//	END_STEP              the source's controller's end step begins
//
// A source that has left the battlefield — gone to another zone, phased
// out, or left the game with its owner — fires nothing. The one
// exception is DIES, where the dying source's own trigger still fires:
// a "when this dies" trigger fires precisely because the source left.

import (
	"fmt"
	"strings"
)

// TriggerEvent is the registry's event vocabulary: structural happenings
// the engine emits, expressed from the registered card's point of view.
type TriggerEvent string

const (
	TriggerLandPlayed  TriggerEvent = "LAND_PLAYED"
	TriggerCreatureETB TriggerEvent = "CREATURE_ETB"
	TriggerUpkeep      TriggerEvent = "UPKEEP"
	TriggerOppCasts    TriggerEvent = "OPPONENT_CASTS_SPELL"
	TriggerAttacks     TriggerEvent = "ATTACKS"
	TriggerDies        TriggerEvent = "DIES"
	TriggerEndStep     TriggerEvent = "END_STEP"
)

// Valid reports whether kind is in the registry vocabulary.
func (k TriggerEvent) Valid() bool {
	switch k {
	case TriggerLandPlayed, TriggerCreatureETB, TriggerUpkeep,
		TriggerOppCasts, TriggerAttacks, TriggerDies, TriggerEndStep:
		return true
	}
	return false
}

// Label is the table-facing spelling — the registration picker and the
// nudge strip read the same words.
func (k TriggerEvent) Label() string {
	switch k {
	case TriggerLandPlayed:
		return "whenever you play a land"
	case TriggerCreatureETB:
		return "whenever your creature enters"
	case TriggerUpkeep:
		return "at your upkeep"
	case TriggerOppCasts:
		return "whenever an opponent casts"
	case TriggerAttacks:
		return "whenever this attacks"
	case TriggerDies:
		return "whenever your creature dies (or this dies)"
	case TriggerEndStep:
		return "at your end step"
	}
	return string(k)
}

// TriggerSpec is one registry row, as loaded from mtg_trigger_registry.
type TriggerSpec struct {
	Card   string       `json:"card"`
	Event  TriggerEvent `json:"event_kind"`
	Effect string       `json:"effect"`
}

// TriggerRegistry indexes the rows for the match: card name → kind →
// effect. Matching is by exact card name — registration resolves names
// canonically (the object context's own spelling, or the universe's),
// so the index never needs to guess at case.
type TriggerRegistry map[string]map[TriggerEvent]string

// NewTriggerRegistry builds the index, skipping rows whose kind is not
// in the vocabulary (a total read over a corpus that may outlive this
// build's vocabulary).
func NewTriggerRegistry(rows []TriggerSpec) TriggerRegistry {
	reg := TriggerRegistry{}
	for _, r := range rows {
		if r.Card == "" || !r.Event.Valid() || strings.TrimSpace(r.Effect) == "" {
			continue
		}
		if reg[r.Card] == nil {
			reg[r.Card] = map[TriggerEvent]string{}
		}
		reg[r.Card][r.Event] = r.Effect
	}
	return reg
}

// Effect looks one card's registration for a kind.
func (reg TriggerRegistry) Effect(card string, kind TriggerEvent) (string, bool) {
	kinds, ok := reg[card]
	if !ok {
		return "", false
	}
	effect, ok := kinds[kind]
	return effect, ok
}

// ApplyWithTriggers is Apply with the trigger registry in play: the
// action's rows and the state-based sweep land first, then the registry
// fires on the structural events that batch produced — into the queue,
// which flushes onto the stack at the same priority grant per CR 117.5,
// after the state-based actions, exactly where 2c put it. A nil or empty
// registry is the registry-less engine of stages 2–4, unchanged.
func ApplyWithTriggers(s *State, a Action, reg TriggerRegistry) ([]Event, error) {
	if s == nil {
		s = NewState()
	}
	evs, err := applyAction(s, a)
	if err != nil || len(evs) == 0 {
		return evs, err
	}
	working := s.clone()
	working.FoldInto(evs)
	sba := sweepStateBasedActions(working, a)
	all := evs
	all = append(all, sba...)
	fired := matchRegistryTriggers(s, all, reg)
	all = append(all, fired...)
	working.FoldInto(fired)
	return append(all, working.flushTriggers()...), nil
}

// matchRegistryTriggers walks one batch event by event, matching each
// against the state as it stood the moment that event lands — the
// presence a real trigger checks. A source that arrived earlier in the
// batch is present; a source that left later still is; the DIES
// look-back also accepts sources that died earlier in the same batch,
// because sweep deaths are simultaneous and their triggers still fire.
func matchRegistryTriggers(base *State, batch []Event, reg TriggerRegistry) []Event {
	if len(reg) == 0 || base == nil || base.Status != StatusActive {
		return nil
	}
	cur := base.clone()
	diedSoFar := map[int64]bool{}
	var out []Event
	for _, e := range batch {
		out = append(out, cur.matchOne(e, reg, diedSoFar)...)
		if batchDeath(e) {
			diedSoFar[e.Object] = true
		}
		cur.foldEvent(e)
	}
	return out
}

// matchOne fires the registry's rows for one event against the state
// the instant before it lands.
func (s *State) matchOne(e Event, reg TriggerRegistry, diedSoFar map[int64]bool) []Event {
	present := func(o *Object) bool { return o.Zone == ZoneBattlefield && !o.Phased }
	presentOrDying := func(o *Object) bool { return present(o) || diedSoFar[o.ID] }
	switch e.Kind {
	case EventLandPlayed:
		return s.fireMatching(reg, TriggerLandPlayed, func(src *Object) bool {
			return present(src) && src.Controller == e.Controller
		})
	case EventCreatureETB:
		return s.fireMatching(reg, TriggerCreatureETB, func(src *Object) bool {
			return present(src) && src.Controller == e.Controller
		})
	case EventStepEntered:
		if e.Phase == "beginning" && e.Step == "upkeep" {
			return s.fireMatching(reg, TriggerUpkeep, func(src *Object) bool {
				return present(src) && src.Controller == s.TurnSeat
			})
		}
		if e.Phase == "end" && e.Step == "end" {
			return s.fireMatching(reg, TriggerEndStep, func(src *Object) bool {
				return present(src) && src.Controller == s.TurnSeat
			})
		}
	case EventCast:
		return s.fireMatching(reg, TriggerOppCasts, func(src *Object) bool {
			return present(src) && src.Controller != e.Controller
		})
	case EventAttackersDeclared:
		var out []Event
		for _, at := range e.Attackers {
			if o, ok := s.Objects[at.Object]; ok {
				out = append(out, s.fireOne(reg, TriggerAttacks, o)...)
			}
		}
		return out
	case EventDied, EventZoneChanged:
		// A death is the sweep's asserted DIED row or a spoken
		// sacrifice or destroy — a ZONE_CHANGED to the graveyard (the
		// sweep owns DIED; the table's own words are moves). The
		// registry sees both the same way, and the dying object is read
		// pre-fold, so its controller is still there even when a later
		// sweep round ceases the token.
		if !batchDeath(e) {
			return nil
		}
		dying := controllerOf(s, e.Object)
		return s.fireMatching(reg, TriggerDies, func(src *Object) bool {
			return presentOrDying(src) && src.Controller == dying
		})
	}
	return nil
}

// batchDeath reports whether one event is a creature-death happening:
// the sweep's asserted DIED row, or a spoken sacrifice/destroy landing
// the object in the graveyard.
func batchDeath(e Event) bool {
	if e.Kind == EventDied {
		return true
	}
	return e.Kind == EventZoneChanged && e.From == ZoneBattlefield && e.ToZone == ZoneGraveyard &&
		(e.Cause == "sacrifice" || e.Cause == "destroy")
}

// fireMatching walks the objects in id order and fires every source
// whose card carries the kind and passes keep. Deterministic by
// construction: id order is arrival order.
func (s *State) fireMatching(reg TriggerRegistry, kind TriggerEvent, keep func(src *Object) bool) []Event {
	var out []Event
	for _, id := range s.objectIDs() {
		o, ok := s.Objects[id]
		if !ok || o.Identity.Card == "" {
			continue
		}
		if !keep(o) {
			continue
		}
		out = append(out, s.fireOne(reg, kind, o)...)
	}
	return out
}

// fireOne emits the TRIGGER_FIRED row for one source and kind, a
// system-asserted row stamped with the source's controller — the same
// seat a manual DECLARE_TRIGGER by that player would carry. The row's
// effect is the registered phrase; it rides the queue onto the stack as
// the ability's declaration, and resolution applies it as an
// EFFECT_DECLARED row the way any ability's does.
func (s *State) fireOne(reg TriggerRegistry, kind TriggerEvent, src *Object) []Event {
	effect, ok := reg.Effect(src.Identity.Card, kind)
	if !ok {
		return nil
	}
	sys := Action{Kind: ActionDeclareTrigger, Seat: src.Controller, Source: "system"}
	return []Event{sys.stamp(Event{Kind: EventTriggerFired, Object: src.ID,
		Card: src.Identity.Card, Effect: effect, Controller: src.Controller})}
}

// controllerOf reads an object's controller, 0 when the object is gone.
func controllerOf(s *State, id int64) int {
	if o, ok := s.Objects[id]; ok {
		return o.Controller
	}
	return 0
}

/* ---------- ORDER_TRIGGERS ---------- */

// applyOrderTriggers reorders the waiting trigger queue: CR 603.3b lets
// a player order their own triggers as they choose, and a Commander
// turn routinely stacks several. The order lists every waiting trigger
// by the ordinal of its TRIGGER_FIRED row; a player may permute only
// their own entries — every other seat's entries keep their relative
// order, because their order was never the actor's to choose. Like
// DECLARE_TRIGGER, ordering holds no priority and spends none.
func applyOrderTriggers(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if err := s.requireSeat(a); err != nil {
		return nil, err
	}
	if len(s.TriggerQueue) == 0 {
		return nil, fmt.Errorf("%w: no triggers are waiting", ErrInvalid)
	}
	if len(a.Order) != len(s.TriggerQueue) {
		return nil, fmt.Errorf("%w: the order lists %d of %d waiting triggers",
			ErrInvalid, len(a.Order), len(s.TriggerQueue))
	}
	pos := map[int64]int{}
	for i, q := range s.TriggerQueue {
		pos[q.FiredOrd] = i
	}
	seq := make([]int, 0, len(a.Order))
	seen := map[int64]bool{}
	for _, ord := range a.Order {
		if seen[ord] {
			return nil, fmt.Errorf("%w: the order lists trigger %d twice", ErrInvalid, ord)
		}
		idx, ok := pos[ord]
		if !ok {
			return nil, fmt.Errorf("%w: trigger %d is not waiting", ErrInvalid, ord)
		}
		seen[ord] = true
		seq = append(seq, idx)
	}
	for x := 0; x < len(seq); x++ {
		for y := x + 1; y < len(seq); y++ {
			ix, iy := seq[x], seq[y]
			mine := s.TriggerQueue[ix].Controller == a.Seat || s.TriggerQueue[iy].Controller == a.Seat
			if !mine && ix > iy {
				return nil, fmt.Errorf("%w: seat %d may only reorder its own triggers", ErrInvalid, a.Seat)
			}
		}
	}
	return []Event{a.stamp(Event{Kind: EventTriggersOrdered, Order: a.Order})}, nil
}

// reorderTriggerQueue folds a TRIGGERS_ORDERED row: the listed ords in
// their listed order, then anything unlisted (a foreign log's rows)
// keeps its current order after them.
func (s *State) reorderTriggerQueue(order []int64) {
	byOrd := map[int64]TriggerItem{}
	for _, q := range s.TriggerQueue {
		byOrd[q.FiredOrd] = q
	}
	out := make([]TriggerItem, 0, len(s.TriggerQueue))
	used := map[int64]bool{}
	for _, ord := range order {
		if q, ok := byOrd[ord]; ok && !used[ord] {
			out = append(out, q)
			used[ord] = true
		}
	}
	for _, q := range s.TriggerQueue {
		if !used[q.FiredOrd] {
			out = append(out, q)
		}
	}
	s.TriggerQueue = out
}

/* ---------- the "don't forget" nudges ---------- */

// Nudge is one don't-forget reminder: a trigger that fired and is
// waiting to reach the stack, a triggered ability on the stack still
// unresolved (the unpaid Rhystic), or an attack trigger the active
// player has not used. A deterministic read over the state and the
// registry — no model, no tokens.
type Nudge struct {
	Kind   string `json:"kind"` // waiting | unresolved | unused_attack
	Card   string `json:"card"`
	Effect string `json:"effect,omitempty"`
	// Seat is whose reminder this is — the trigger's controller, the
	// player whose attack trigger is unused.
	Seat int `json:"seat"`
	// Ord is the waiting trigger's TRIGGER_FIRED ordinal (waiting only).
	Ord int64 `json:"ord,omitempty"`
}

// Nudges derives the current-action pane's reminders. Manually declared
// triggers count exactly as much as registered ones — the point is that
// nothing resolves silently, whatever declared it.
func (s *State) Nudges(reg TriggerRegistry) []Nudge {
	if s == nil || s.Status != StatusActive {
		return nil
	}
	var out []Nudge
	for _, q := range s.TriggerQueue {
		if q.Card == "" && q.Effect == "" {
			continue
		}
		out = append(out, Nudge{Kind: "waiting", Card: q.Card, Effect: q.Effect,
			Seat: q.Controller, Ord: q.FiredOrd})
	}
	for _, it := range s.Stack {
		if it.Mode != "triggered" {
			continue
		}
		out = append(out, Nudge{Kind: "unresolved", Card: it.Card, Effect: it.Ability, Seat: it.Controller})
	}
	// The unused attack trigger: the step is theirs, nothing has been
	// declared, and a card with an ATTACKS registration is sitting there.
	if s.Phase == "combat" && s.Step == "declare_attackers" && len(s.Attackers) == 0 {
		for _, id := range s.objectIDs() {
			o, ok := s.Objects[id]
			if !ok || o.Zone != ZoneBattlefield || o.Phased || o.Controller != s.TurnSeat {
				continue
			}
			if effect, ok := reg.Effect(o.Identity.Card, TriggerAttacks); ok {
				out = append(out, Nudge{Kind: "unused_attack", Card: o.Identity.Card,
					Effect: effect, Seat: o.Controller})
			}
		}
	}
	return out
}
