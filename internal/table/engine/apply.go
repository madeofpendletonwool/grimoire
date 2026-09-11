package engine

// The reducer. Apply(state, action) → ([]Event, error) is a pure function:
// no I/O, no clock, no model, no randomness. It validates the action
// against the state and returns the events the log would append — every
// validation happens before the first event is built, so a rejected action
// returns zero events and an error, never both. State advances only
// through Fold; Apply never mutates what it is handed.

import (
	"fmt"
	"strings"
)

// maxTokenBatch bounds one CREATE_TOKEN action. Token doublers get loud,
// but five figures of rows from one action is a runaway, not a game.
const maxTokenBatch = 1000

// Apply validates an action against a state and produces its events: the
// action's own rows first, then the state-based actions that fire on the
// position those rows leave the game in (CR 704.3 — checked whenever a
// player would receive priority, which after any action is the next thing
// that happens). The sweep folds its rounds against a clone, so the
// handed-in state is never touched and the caller folds one flat list.
func Apply(s *State, a Action) ([]Event, error) {
	if s == nil {
		s = NewState()
	}
	evs, err := applyAction(s, a)
	if err != nil || len(evs) == 0 {
		return evs, err
	}
	working := s.clone()
	working.FoldInto(evs)
	return append(evs, sweepStateBasedActions(working, a)...), nil
}

func applyAction(s *State, a Action) ([]Event, error) {
	switch a.Kind {
	case ActionStartGame:
		return applyStartGame(s, a)
	case ActionEndGame:
		return applyEndGame(s, a)
	case ActionConcede:
		return applyConcede(s, a)
	case ActionAdvance:
		return applyAdvance(s, a)
	case ActionPassPriority:
		return applyPassPriority(s, a)
	case ActionPlayLand:
		return applyPlayLand(s, a)
	case ActionCast:
		return applyCast(s, a)
	case ActionActivate:
		return applyActivate(s, a)
	case ActionMoveZone:
		return applyMoveZone(s, a)
	case ActionCreateToken:
		return applyCreateToken(s, a)
	case ActionTap:
		return applyTap(s, a, true)
	case ActionUntap:
		return applyTap(s, a, false)
	case ActionSetPhased:
		return applySetPhased(s, a)
	case ActionAttach:
		return applyAttach(s, a)
	case ActionDetach:
		return applyDetach(s, a)
	case ActionDeclareAttackers:
		return applyDeclareAttackers(s, a)
	case ActionDeclareBlockers:
		return applyDeclareBlockers(s, a)
	case ActionResolveCombat:
		return nil, fmt.Errorf("%w: RESOLVE_COMBAT combat damage arithmetic lands with MAD-325", ErrInvalid)
	case ActionAdjustCounters:
		return applyAdjustCounters(s, a)
	case ActionSetCounters:
		return applySetCounters(s, a)
	case ActionChangeLife:
		return applyChangeLife(s, a)
	case ActionDealDamage:
		return applyDealDamage(s, a)
	case ActionSetFlag:
		return applySetFlag(s, a)
	case ActionSetZoneCount:
		return applySetZoneCount(s, a)
	case ActionDraw:
		return applyDraw(s, a)
	case ActionMill:
		return applyMill(s, a)
	case ActionReveal:
		return applyReveal(s, a)
	case ActionLook:
		return applyLook(s, a)
	case ActionDeclareEffect:
		return applyDeclareEffect(s, a)
	case ActionAddModifier:
		return applyAddModifier(s, a)
	case ActionRemoveModifier:
		return applyRemoveModifier(s, a)
	}
	return nil, fmt.Errorf("%w: unknown action kind %q", ErrInvalid, a.Kind)
}

/* ---------- helpers ---------- */

// stamp copies the action's audit trail onto an event and defaults its
// visibility to public.
func (a Action) stamp(e Event) Event {
	e.ActorSeat = a.Seat
	e.Source = a.Source
	if e.Visibility == "" {
		e.Visibility = VisibilityPublic
	}
	return e
}

// requireActive gates the play actions: the game must be underway.
func (s *State) requireActive(a Action) error {
	if s.Status != StatusActive {
		return fmt.Errorf("%w: game is %s, not active", ErrInvalid, s.Status)
	}
	return nil
}

// requireSeat validates the acting seat is in the game and still in it —
// an eliminated seat acts on nothing.
func (s *State) requireSeat(a Action) error {
	p, err := s.player(a.Seat)
	if err != nil {
		return err
	}
	if !p.Alive {
		return fmt.Errorf("%w: seat %d has left the game", ErrInvalid, a.Seat)
	}
	return nil
}

// requirePriority gates the actions that consume priority (CR 116/117:
// casting, activating, playing a land, passing): the actor must hold it,
// and during untap and cleanup nobody does (CR 502.3, 514.3a). Manual
// bookkeeping actions stay ungated — they are the tracker's correction
// surface, not game actions.
func (s *State) requirePriority(a Action) error {
	if s.PrioritySeat == 0 {
		return fmt.Errorf("%w: no one has priority during the %s step", ErrInvalid, s.Step)
	}
	if a.Seat != s.PrioritySeat {
		return fmt.Errorf("%w: seat %d does not hold priority (seat %d does)", ErrInvalid, a.Seat, s.PrioritySeat)
	}
	return nil
}

// targetSeat resolves a target seat: the explicit target when given, else
// the acting seat. Eliminated seats are not valid targets — they are not
// in the game anymore.
func (s *State) targetSeat(a Action) (int, error) {
	seat := a.TargetSeat
	if seat == 0 {
		seat = a.Seat
	}
	p, err := s.player(seat)
	if err != nil {
		return 0, err
	}
	if !p.Alive {
		return 0, fmt.Errorf("%w: seat %d has left the game", ErrInvalid, seat)
	}
	return seat, nil
}

// objectIDs lists the state's object ids ascending.
func (s *State) objectIDs() []int64 {
	ids := make([]int64, 0, len(s.Objects))
	for id := range s.Objects {
		ids = append(ids, id)
	}
	sortInt64s(ids)
	return ids
}

func sortInt64s(v []int64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// handEmpty reports a known-empty hand: a tracker that never counted the
// hand cannot veto a cast, and one that did can.
func (s *State) handEmpty(seat int) bool {
	p, ok := s.Seats[seat]
	return ok && p.Hand.Known && p.Hand.N < 1
}

// minter hands out the ids Apply needs without mutating state: the fold
// mints the same ids later from the events' payloads.
type minter struct{ nextObject, nextModifier int64 }

func newMinter(s *State) minter { return minter{s.NextObject, s.NextModifier} }

/* ---------- lifecycle ---------- */

func applyStartGame(s *State, a Action) ([]Event, error) {
	if s.Status != StatusSetup {
		return nil, fmt.Errorf("%w: game is already %s", ErrInvalid, s.Status)
	}
	if len(a.Seats) == 0 {
		return nil, fmt.Errorf("%w: no seats to start the game with", ErrInvalid)
	}
	seen := map[int]bool{}
	order := []int{}
	for _, sc := range a.Seats {
		if sc.Seat <= 0 {
			return nil, fmt.Errorf("%w: seat position %d is not positive", ErrInvalid, sc.Seat)
		}
		if seen[sc.Seat] {
			return nil, fmt.Errorf("%w: duplicate seat position %d", ErrInvalid, sc.Seat)
		}
		seen[sc.Seat] = true
		order = append(order, sc.Seat)
	}
	sortSeats(order)
	format := a.Format
	if format == "" {
		format = "commander"
	}
	life := a.StartingLife
	if life == 0 {
		life = 40
	}
	evs := []Event{
		a.stamp(Event{Kind: EventGameStarted, Seats: a.Seats, Format: format, StartingLife: life}),
		a.stamp(Event{Kind: EventTurnStarted, Turn: 1, TurnSeat: order[0]}),
		a.stamp(Event{Kind: EventStepEntered, Phase: "beginning", Step: "untap"}),
	}
	return evs, nil
}

func applyEndGame(s *State, a Action) ([]Event, error) {
	if s.Status == StatusFinished {
		return nil, fmt.Errorf("%w: game is already finished", ErrInvalid)
	}
	return []Event{a.stamp(Event{Kind: EventGameEnded, Reason: a.Reason})}, nil
}

// applyConcede is CR 104.3a: a player may concede at any time — no
// priority needed. The leaving chain itself is leaveEvents; the sweep
// then runs over the position it leaves behind (last standing ends the
// game).
func applyConcede(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if err := s.requireSeat(a); err != nil {
		return nil, err
	}
	return s.leaveEvents(a, a.Seat, causeConcession, ""), nil
}

/* ---------- turn and priority ---------- */

func applyAdvance(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	// CR 500.2: a step or phase doesn't end while the stack is non-empty.
	// Resolve it with passes first; the log stays honest that way.
	if len(s.Stack) > 0 {
		return nil, fmt.Errorf("%w: the stack must resolve before the step advances", ErrInvalid)
	}
	return s.advanceSequence(a), nil
}

// advanceSequence builds the step-walk events for the state's current
// position: one STEP_ENTERED, or — falling off cleanup — the TURN_ENDED
// anchor, the next seat's TURN_STARTED, and the untap entry.
func (s *State) advanceSequence(a Action) []Event {
	phase, step, ends := nextStep(s)
	if ends {
		next := nextSeat(s, s.TurnSeat)
		return []Event{
			a.stamp(Event{Kind: EventTurnEnded, Turn: s.Turn}),
			a.stamp(Event{Kind: EventTurnStarted, Turn: s.Turn + 1, TurnSeat: next}),
			a.stamp(Event{Kind: EventStepEntered, Phase: "beginning", Step: "untap"}),
		}
	}
	return []Event{a.stamp(Event{Kind: EventStepEntered, Phase: phase, Step: step})}
}

// applyPassPriority is the CR 117 rotation. Only the holder may pass; the
// pass moves priority to the next seat in turn order among the living;
// and when every living seat has passed in succession the stack top
// resolves (priority returning to the active player, CR 117.3b) or, with
// the stack empty, the step or phase ends (CR 117.4) — the same walk
// ADVANCE makes.
func applyPassPriority(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if err := s.requireSeat(a); err != nil {
		return nil, err
	}
	if err := s.requirePriority(a); err != nil {
		return nil, err
	}
	passed := append(append([]int{}, s.Passed...), a.Seat)
	if len(passed) >= len(s.Order) {
		if len(s.Stack) > 0 {
			return s.resolveTop(a), nil
		}
		return s.advanceSequence(a), nil
	}
	return []Event{a.stamp(Event{Kind: EventPriorityPassed, SeatTo: nextSeat(s, a.Seat)})}, nil
}

// resolveTop builds the events for the stack's top item resolving. A spell
// object moves — permanents to the battlefield (creatures announcing
// CREATURE_ETB), the rest to the graveyard; an ability simply comes off.
// Unknown types resolve to the graveyard: the caller corrects with
// MOVE_ZONE, which is the cheap-correction path working as designed.
func (s *State) resolveTop(a Action) []Event {
	item := s.Stack[len(s.Stack)-1]
	var evs []Event
	res := Event{Kind: EventStackResolved, Mode: item.Mode, Ability: item.Ability,
		Controller: item.Controller, Targets: item.Targets, Object: item.Object}
	if item.Object != 0 {
		if o, ok := s.Objects[item.Object]; ok {
			dest := ZoneGraveyard
			if isPermanentType(o.Base.Types) {
				dest = ZoneBattlefield
			}
			res.ToZone = dest
			evs = append(evs, a.stamp(res),
				a.stamp(Event{Kind: EventZoneChanged, Object: o.ID, From: ZoneStack, ToZone: dest, Cause: "resolve"}))
			if dest == ZoneBattlefield && hasString(o.Base.Types, "Creature") {
				evs = append(evs, a.stamp(Event{Kind: EventCreatureETB, Object: o.ID, Card: o.Identity.Card, Controller: o.Controller}))
			}
			return evs
		}
	}
	return append(evs, a.stamp(res))
}

// isPermanentType reports whether a type line makes its object a permanent.
func isPermanentType(types []string) bool {
	for _, t := range types {
		switch strings.ToLower(t) {
		case "artifact", "creature", "enchantment", "land", "planeswalker", "battle":
			return true
		}
	}
	return false
}

/* ---------- zones and objects ---------- */

func applyPlayLand(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if err := s.requireSeat(a); err != nil {
		return nil, err
	}
	// Playing a land is a special action (CR 116.2a): it needs priority.
	if err := s.requirePriority(a); err != nil {
		return nil, err
	}
	if p := s.Seats[a.Seat]; p != nil && p.LandsThisTurn >= 1 {
		return nil, fmt.Errorf("%w: seat %d's land drop this turn is already used", ErrInvalid, a.Seat)
	}
	from := a.FromZone
	if from == "" {
		from = ZoneHand
	}
	if from != ZoneHand && from != ZoneGraveyard && from != ZoneExile && from != ZoneLibrary {
		return nil, fmt.Errorf("%w: cannot play a land from %s", ErrInvalid, from)
	}
	if from == ZoneHand && s.handEmpty(a.Seat) {
		return nil, fmt.Errorf("%w: seat %d has an empty hand", ErrInvalid, a.Seat)
	}
	m := newMinter(s)
	if from == ZoneGraveyard || from == ZoneExile {
		obj, err := s.takeNamedObject(a.Seat, from, a.Card)
		if err != nil {
			return nil, err
		}
		evs := []Event{
			a.stamp(Event{Kind: EventZoneChanged, Object: obj, From: from, ToZone: ZoneBattlefield, Cause: "play"}),
			a.stamp(Event{Kind: EventLandPlayed, Object: obj, Card: a.Card, Controller: a.Seat}),
		}
		return evs, nil
	}
	id := m.nextObject
	base := BaseChars{}
	if a.Base != nil {
		base = *a.Base
	}
	evs := []Event{
		a.stamp(Event{Kind: EventObjectCreated, Object: id, Identity: Identity{Card: a.Card}, Base: &base,
			Owner: a.Seat, Controller: a.Seat, From: from, ToZone: ZoneBattlefield}),
		a.stamp(Event{Kind: EventLandPlayed, Object: id, Card: a.Card, Controller: a.Seat}),
	}
	if hasString(base.Types, "Creature") {
		evs = append(evs, a.stamp(Event{Kind: EventCreatureETB, Object: id, Card: a.Card, Controller: a.Seat}))
	}
	return evs, nil
}

// takeNamedObject finds an object to consume from a tracked zone: an exact
// name match owned by the seat first, then any unknown-identity object the
// seat owns, so casting or playing from a partially tracked zone still
// works honestly.
func (s *State) takeNamedObject(seat int, zone Zone, card string) (int64, error) {
	fallback := int64(0)
	for _, id := range s.objectIDs() {
		o := s.Objects[id]
		if o.Zone != zone || o.Owner != seat {
			continue
		}
		if o.Identity.Card == card && card != "" {
			return id, nil
		}
		if o.Identity.Card == "" && o.Identity.Token == nil && fallback == 0 {
			fallback = id
		}
	}
	if fallback != 0 {
		return fallback, nil
	}
	return 0, fmt.Errorf("%w: no %q owned by seat %d in %s", ErrInvalid, card, seat, zone)
}

func applyCast(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if err := s.requireSeat(a); err != nil {
		return nil, err
	}
	// Casting needs priority (CR 117.2c). Timing legality beyond that —
	// sorcery speed, flash — depends on card text the engine does not
	// simulate (ADR 11), so the structural check stops here.
	if err := s.requirePriority(a); err != nil {
		return nil, err
	}
	card := strings.TrimSpace(a.Card)
	if card == "" {
		return nil, fmt.Errorf("%w: a cast needs a card name", ErrInvalid)
	}
	from := a.FromZone
	if from == "" {
		from = ZoneHand
	}
	base := BaseChars{}
	if a.Base != nil {
		base = *a.Base
	}
	if from == ZoneHand && s.handEmpty(a.Seat) {
		return nil, fmt.Errorf("%w: seat %d has an empty hand", ErrInvalid, a.Seat)
	}
	m := newMinter(s)
	evs := []Event{}
	switch from {
	case ZoneHand, ZoneLibrary:
		id := m.nextObject
		evs = append(evs,
			a.stamp(Event{Kind: EventObjectCreated, Object: id, Identity: Identity{Card: card}, Base: &base,
				Owner: a.Seat, Controller: a.Seat, From: from, ToZone: ZoneStack}),
			a.stamp(Event{Kind: EventCast, Card: card, From: from, Targets: a.Targets, Controller: a.Seat, Object: id}),
			a.stamp(Event{Kind: EventStackPushed, Mode: "cast", Object: id, Controller: a.Seat, Targets: a.Targets, Card: card}))
	case ZoneGraveyard, ZoneExile, ZoneCommand, ZoneBattlefield:
		obj, err := s.takeNamedObject(a.Seat, from, card)
		if err != nil {
			return nil, err
		}
		evs = append(evs,
			a.stamp(Event{Kind: EventZoneChanged, Object: obj, From: from, ToZone: ZoneStack, Cause: "cast"}),
			a.stamp(Event{Kind: EventCast, Card: card, From: from, Targets: a.Targets, Controller: a.Seat, Object: obj, Base: a.Base}),
			a.stamp(Event{Kind: EventStackPushed, Mode: "cast", Object: obj, Controller: a.Seat, Targets: a.Targets, Card: card}))
	default:
		return nil, fmt.Errorf("%w: cannot cast from %s", ErrInvalid, from)
	}
	return evs, nil
}

func applyActivate(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if err := s.requireSeat(a); err != nil {
		return nil, err
	}
	// Activating an ability needs priority (CR 117.2c).
	if err := s.requirePriority(a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Ability) == "" {
		return nil, fmt.Errorf("%w: an activation needs its ability described", ErrInvalid)
	}
	if a.Object != 0 {
		if _, err := s.object(a.Object); err != nil {
			return nil, err
		}
	}
	return []Event{a.stamp(Event{Kind: EventStackPushed, Mode: "activated", Object: a.Object,
		Ability: a.Ability, Controller: a.Seat, Targets: a.Targets})}, nil
}

func applyMoveZone(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	o, err := s.object(a.Object)
	if err != nil {
		return nil, err
	}
	if a.ToZone == "" {
		return nil, fmt.Errorf("%w: MOVE_ZONE needs a destination", ErrInvalid)
	}
	if a.ToZone == ZoneStack {
		return nil, fmt.Errorf("%w: use CAST to put an object on the stack", ErrInvalid)
	}
	if o.Zone == a.ToZone {
		return nil, fmt.Errorf("%w: object %d is already in %s", ErrInvalid, o.ID, a.ToZone)
	}
	if o.Identity.Token != nil && (a.ToZone == ZoneHand || a.ToZone == ZoneLibrary) {
		return nil, fmt.Errorf("%w: a token cannot move to %s", ErrInvalid, a.ToZone)
	}
	from := o.Zone
	evs := []Event{}
	if from == ZoneBattlefield {
		evs = append(evs, s.detachAll(a, o)...)
	}
	evs = append(evs, a.stamp(Event{Kind: EventZoneChanged, Object: o.ID, From: from, ToZone: a.ToZone, Cause: a.Cause}))
	if a.ToZone == ZoneBattlefield && hasString(o.Base.Types, "Creature") {
		evs = append(evs, a.stamp(Event{Kind: EventCreatureETB, Object: o.ID, Card: o.Identity.Card, Controller: o.Controller}))
	}
	return evs, nil
}

// detachAll builds the consequence events for a battlefield object
// leaving: its attachments fall off (an attachment whose target left is
// unattached by state-based rule — the fold enforces the edge stays legal),
// it detaches from its own host, and the while_source_present modifiers it
// was the source of end. Those removals are asserted as MODIFIER_REMOVED
// rows because they are not cheaply derivable, and the log should show them.
func (s *State) detachAll(a Action, o *Object) []Event {
	var evs []Event
	for _, att := range o.Attachments {
		evs = append(evs, a.stamp(Event{Kind: EventUnattached, Object: att, TargetObject: o.ID}))
	}
	if o.AttachedTo != 0 {
		evs = append(evs, a.stamp(Event{Kind: EventUnattached, Object: o.ID, TargetObject: o.AttachedTo}))
	}
	for _, id := range s.objectIDs() {
		other := s.Objects[id]
		for _, mod := range other.Modifiers {
			if mod.SourceObj == o.ID && mod.Duration == WhileSourcePresent {
				evs = append(evs, a.stamp(Event{Kind: EventModifierRemoved, Object: other.ID, ModifierID: mod.ID}))
			}
		}
	}
	return evs
}

func applyCreateToken(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if err := s.requireSeat(a); err != nil {
		return nil, err
	}
	if a.Token == nil {
		return nil, fmt.Errorf("%w: CREATE_TOKEN needs a token spec", ErrInvalid)
	}
	count := a.Count
	if count == 0 {
		count = 1
	}
	if count < 0 || count > maxTokenBatch {
		return nil, fmt.Errorf("%w: token count %d is out of range", ErrInvalid, count)
	}
	// An attachment may enter attached to a host (an aura, CR 303.4f);
	// the edge is made in the same batch so the unattached-aura
	// state-based action never sees it blink.
	host := int64(0)
	if a.AttachTo != 0 {
		h, err := s.object(a.AttachTo)
		if err != nil {
			return nil, err
		}
		if h.Zone != ZoneBattlefield || h.Phased {
			return nil, fmt.Errorf("%w: object %d cannot host a new token", ErrInvalid, a.AttachTo)
		}
		host = h.ID
	}
	base := tokenBase(a.Token)
	m := newMinter(s)
	evs := []Event{}
	for i := 0; i < count; i++ {
		id := m.nextObject + int64(i)
		evs = append(evs, a.stamp(Event{Kind: EventObjectCreated, Object: id,
			Identity: Identity{Token: a.Token}, Base: &base, Owner: a.Seat, Controller: a.Seat, ToZone: ZoneBattlefield}))
		if hasString(base.Types, "Creature") {
			evs = append(evs, a.stamp(Event{Kind: EventCreatureETB, Object: id, Controller: a.Seat}))
		}
		if host != 0 {
			evs = append(evs, a.stamp(Event{Kind: EventAttached, Object: id, TargetObject: host}))
		}
	}
	return evs, nil
}

// tokenBase renders a token spec as the base characteristics its objects
// enter with.
func tokenBase(t *TokenSpec) BaseChars {
	return BaseChars{Name: t.Name, Types: t.Types, Colors: t.Colors,
		Power: t.Power, Toughness: t.Toughness, Loyalty: t.Loyalty}
}

func applyTap(s *State, a Action, tapped bool) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if err := s.requireSeat(a); err != nil {
		return nil, err
	}
	var evs []Event
	if a.All {
		for _, id := range s.objectIDs() {
			o := s.Objects[id]
			if o.Zone == ZoneBattlefield && !o.Phased && o.Controller == a.Seat && o.Tapped != tapped {
				evs = append(evs, a.stamp(Event{Kind: EventTapChanged, Object: id, Tapped: tapped}))
			}
		}
		if len(evs) == 0 {
			return nil, fmt.Errorf("%w: nothing to %s", ErrInvalid, tapWord(tapped))
		}
		return evs, nil
	}
	o, err := s.object(a.Object)
	if err != nil {
		return nil, err
	}
	if o.Zone != ZoneBattlefield {
		return nil, fmt.Errorf("%w: object %d is in %s, not on the battlefield", ErrInvalid, o.ID, o.Zone)
	}
	if o.Tapped == tapped {
		return nil, fmt.Errorf("%w: object %d is already %s", ErrInvalid, o.ID, tapState(tapped))
	}
	return []Event{a.stamp(Event{Kind: EventTapChanged, Object: o.ID, Tapped: tapped})}, nil
}

func tapWord(tapped bool) string {
	if tapped {
		return "tap"
	}
	return "untap"
}

func tapState(tapped bool) string {
	if tapped {
		return "tapped"
	}
	return "untapped"
}

func applySetPhased(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	o, err := s.object(a.Object)
	if err != nil {
		return nil, err
	}
	if o.Zone != ZoneBattlefield {
		return nil, fmt.Errorf("%w: object %d phases only on the battlefield", ErrInvalid, o.ID)
	}
	if o.Phased == a.Phased {
		return nil, fmt.Errorf("%w: object %d is already %s", ErrInvalid, o.ID, phaseState(a.Phased))
	}
	return []Event{a.stamp(Event{Kind: EventPhaseChanged, Object: o.ID, Phased: a.Phased})}, nil
}

func phaseState(phased bool) string {
	if phased {
		return "phased out"
	}
	return "phased in"
}

func applyAttach(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	o, err := s.object(a.Object)
	if err != nil {
		return nil, err
	}
	host, err := s.object(a.AttachTo)
	if err != nil {
		return nil, err
	}
	if o.ID == host.ID {
		return nil, fmt.Errorf("%w: an object cannot attach to itself", ErrInvalid)
	}
	if o.Zone != ZoneBattlefield || host.Zone != ZoneBattlefield {
		return nil, fmt.Errorf("%w: attachment edges exist only on the battlefield", ErrInvalid)
	}
	// Attachment legality as far as structure reaches: nothing phases in
	// and out with its host, so an edge to a phased object is illegal
	// (the aura half of CR 704.5p's "attached to nothing" is the sweep's).
	if o.Phased || host.Phased {
		return nil, fmt.Errorf("%w: phased objects cannot attach or host", ErrInvalid)
	}
	if o.AttachedTo == host.ID {
		return nil, fmt.Errorf("%w: object %d is already attached to %d", ErrInvalid, o.ID, host.ID)
	}
	var evs []Event
	if o.AttachedTo != 0 {
		// Re-equipping is UNATTACHED + ATTACHED with a cause, so the log
		// reads correctly.
		evs = append(evs, a.stamp(Event{Kind: EventUnattached, Object: o.ID, TargetObject: o.AttachedTo}))
	}
	evs = append(evs, a.stamp(Event{Kind: EventAttached, Object: o.ID, TargetObject: host.ID}))
	return evs, nil
}

func applyDetach(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	o, err := s.object(a.Object)
	if err != nil {
		return nil, err
	}
	if o.AttachedTo == 0 {
		return nil, fmt.Errorf("%w: object %d is not attached to anything", ErrInvalid, o.ID)
	}
	return []Event{a.stamp(Event{Kind: EventUnattached, Object: o.ID, TargetObject: o.AttachedTo})}, nil
}

/* ---------- combat declarations ---------- */

// Combat damage arithmetic — the assignment orders and the keyword
// interactions that are pure arithmetic — is MAD-325's. What lands here is
// the honest half the log needs now: the declarations themselves (gated
// to their steps, CR 508/509), attackers tapped unless vigilance says
// otherwise (a computed keyword), and the structural events the trigger
// registry fires on.

func applyDeclareAttackers(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if err := s.requireSeat(a); err != nil {
		return nil, err
	}
	// CR 508.1: the active player declares attackers during the declare
	// attackers step of their own turn.
	if s.Phase != "combat" || s.Step != "declare_attackers" {
		return nil, fmt.Errorf("%w: attackers are declared in the declare_attackers step (now %s/%s)", ErrInvalid, s.Phase, s.Step)
	}
	if a.Seat != s.TurnSeat {
		return nil, fmt.Errorf("%w: seat %d is not the active player", ErrInvalid, a.Seat)
	}
	if len(a.Attackers) == 0 {
		return nil, fmt.Errorf("%w: DECLARE_ATTACKERS needs assignments", ErrInvalid)
	}
	seen := map[int64]bool{}
	taps := map[int64]bool{}
	for _, at := range a.Attackers {
		if seen[at.Object] {
			return nil, fmt.Errorf("%w: object %d attacks twice", ErrInvalid, at.Object)
		}
		seen[at.Object] = true
		o, err := s.object(at.Object)
		if err != nil {
			return nil, err
		}
		if o.Zone != ZoneBattlefield {
			return nil, fmt.Errorf("%w: object %d is in %s, not on the battlefield", ErrInvalid, at.Object, o.Zone)
		}
		if at.TargetSeat == 0 && at.TargetObject == 0 {
			return nil, fmt.Errorf("%w: attacker %d has no target", ErrInvalid, at.Object)
		}
		if at.TargetSeat != 0 {
			if _, err := s.player(at.TargetSeat); err != nil {
				return nil, err
			}
		}
		// Attackers tap unless they have vigilance — and vigilance here is
		// a computed keyword, base plus modifiers, which is the point of
		// computing characteristics at all.
		if o.Tapped {
			return nil, fmt.Errorf("%w: object %d is tapped and cannot attack", ErrInvalid, at.Object)
		}
		if !hasString(s.Keywords(o.ID), "Vigilance") {
			taps[at.Object] = true
		}
	}
	var evs []Event
	for _, id := range s.objectIDs() {
		if taps[id] {
			evs = append(evs, a.stamp(Event{Kind: EventTapChanged, Object: id, Tapped: true}))
		}
	}
	evs = append(evs, a.stamp(Event{Kind: EventAttackersDeclared, Attackers: a.Attackers}))
	return evs, nil
}

func applyDeclareBlockers(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if err := s.requireSeat(a); err != nil {
		return nil, err
	}
	// CR 509.1: blockers are declared during the declare blockers step —
	// by whichever seats are being attacked, which the acting seat's own
	// assignments are responsible for.
	if s.Phase != "combat" || s.Step != "declare_blockers" {
		return nil, fmt.Errorf("%w: blockers are declared in the declare_blockers step (now %s/%s)", ErrInvalid, s.Phase, s.Step)
	}
	if len(a.Blockers) == 0 {
		return nil, fmt.Errorf("%w: DECLARE_BLOCKERS needs assignments", ErrInvalid)
	}
	for _, bl := range a.Blockers {
		o, err := s.object(bl.Blocker)
		if err != nil {
			return nil, err
		}
		if o.Zone != ZoneBattlefield {
			return nil, fmt.Errorf("%w: blocker %d is in %s, not on the battlefield", ErrInvalid, bl.Blocker, o.Zone)
		}
		for _, at := range bl.Attackers {
			if _, err := s.object(at); err != nil {
				return nil, err
			}
		}
	}
	return []Event{a.stamp(Event{Kind: EventBlockersDeclared, Blockers: a.Blockers})}, nil
}

/* ---------- numbers ---------- */

func applyAdjustCounters(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	name := normalizeCounter(a.CounterName)
	if name == "" {
		return nil, fmt.Errorf("%w: a counter needs a name", ErrInvalid)
	}
	if a.Delta == 0 {
		return nil, fmt.Errorf("%w: counter delta is zero", ErrInvalid)
	}
	if a.OnObject != 0 {
		if _, err := s.object(a.OnObject); err != nil {
			return nil, err
		}
		return []Event{a.stamp(Event{Kind: EventCounterChanged, Object: a.OnObject, Name: name, Delta: a.Delta})}, nil
	}
	seat, err := s.targetSeat(a)
	if err != nil {
		return nil, err
	}
	return []Event{a.stamp(Event{Kind: EventCounterChanged, TargetSeat: seat, Name: name, Delta: a.Delta})}, nil
}

func applySetCounters(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	name := normalizeCounter(a.CounterName)
	if name == "" {
		return nil, fmt.Errorf("%w: a counter needs a name", ErrInvalid)
	}
	if a.To == nil {
		return nil, fmt.Errorf("%w: SET_COUNTERS needs an absolute value", ErrInvalid)
	}
	if a.OnObject != 0 {
		if _, err := s.object(a.OnObject); err != nil {
			return nil, err
		}
		return []Event{a.stamp(Event{Kind: EventCounterChanged, Object: a.OnObject, Name: name, To: a.To})}, nil
	}
	seat, err := s.targetSeat(a)
	if err != nil {
		return nil, err
	}
	return []Event{a.stamp(Event{Kind: EventCounterChanged, TargetSeat: seat, Name: name, To: a.To})}, nil
}

// normalizeCounter makes counter names data worth comparing: trimmed and
// folded. The engine understands a handful structurally — "+1/+1" and
// "-1/-1" in the P/T computation and the annihilation rule, poison at
// ten and loyalty at zero as state-based actions — and every other name
// is carried verbatim until a rule needs it.
func normalizeCounter(name string) string {
	return strings.TrimSpace(name)
}

func applyChangeLife(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	seat, err := s.targetSeat(a)
	if err != nil {
		return nil, err
	}
	if a.Delta == 0 && a.To == nil {
		return nil, fmt.Errorf("%w: life change needs a delta or an absolute", ErrInvalid)
	}
	return []Event{a.stamp(Event{Kind: EventLifeChanged, TargetSeat: seat, Delta: a.Delta, To: a.To,
		SourceCard: a.SourceCard, SourceObj: a.SourceObj})}, nil
}

func applyDealDamage(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if a.Amount <= 0 {
		return nil, fmt.Errorf("%w: damage amount must be positive", ErrInvalid)
	}
	if a.SourceObj != 0 {
		if _, err := s.object(a.SourceObj); err != nil {
			return nil, err
		}
	}
	if a.TargetSeat != 0 && a.TargetObject != 0 {
		return nil, fmt.Errorf("%w: damage needs one target, a seat or an object", ErrInvalid)
	}
	var evs []Event
	dealt := Event{Kind: EventDamageDealt, Amount: a.Amount, Combat: a.CombatDmg,
		SourceObj: a.SourceObj, SourceCard: a.SourceCard, TargetSeat: a.TargetSeat, TargetObject: a.TargetObject}
	if a.TargetSeat != 0 {
		if _, err := s.player(a.TargetSeat); err != nil {
			return nil, err
		}
		// Damage produces its LIFE_CHANGED consequence asserted, so
		// viewers fold trivially and prevention stays an engine concern.
		evs = append(evs, a.stamp(dealt),
			a.stamp(Event{Kind: EventLifeChanged, TargetSeat: a.TargetSeat, Delta: -a.Amount,
				SourceCard: a.SourceCard, SourceObj: a.SourceObj}))
		return evs, nil
	}
	if _, err := s.object(a.TargetObject); err != nil {
		return nil, err
	}
	evs = append(evs, a.stamp(dealt),
		a.stamp(Event{Kind: EventDamageMarked, Object: a.TargetObject, Amount: a.Amount, SourceObj: a.SourceObj}))
	return evs, nil
}

func applySetFlag(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	seat, err := s.targetSeat(a)
	if err != nil {
		return nil, err
	}
	flag := strings.ToLower(strings.TrimSpace(a.Flag))
	if flag == "" {
		return nil, fmt.Errorf("%w: a flag needs a name", ErrInvalid)
	}
	var evs []Event
	// Monarch and initiative are exclusive: one holder table-wide. The
	// engine asserts the clearing rows so the log shows the crown moving.
	if a.Value != "" && (flag == "monarch" || flag == "initiative") {
		for _, other := range s.Order {
			if other == seat {
				continue
			}
			if p := s.Seats[other]; p != nil && p.Flags[flag] == a.Value {
				evs = append(evs, a.stamp(Event{Kind: EventFlagChanged, TargetSeat: other, Flag: flag}))
			}
		}
	}
	evs = append(evs, a.stamp(Event{Kind: EventFlagChanged, TargetSeat: seat, Flag: flag, Value: a.Value}))
	return evs, nil
}

func applySetZoneCount(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	seat, err := s.targetSeat(a)
	if err != nil {
		return nil, err
	}
	if a.Zone != ZoneHand && a.Zone != ZoneLibrary {
		return nil, fmt.Errorf("%w: SET_ZONE_COUNT applies to hand or library", ErrInvalid)
	}
	if a.To == nil || *a.To < 0 {
		return nil, fmt.Errorf("%w: SET_ZONE_COUNT needs a non-negative value", ErrInvalid)
	}
	return []Event{a.stamp(Event{Kind: EventZoneCountSet, TargetSeat: seat, Zone: a.Zone, To: a.To})}, nil
}

/* ---------- hidden information ---------- */

func applyDraw(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if _, err := s.player(a.Seat); err != nil {
		return nil, err
	}
	count := a.Count
	if count == 0 {
		count = 1
	}
	if count < 1 {
		return nil, fmt.Errorf("%w: draw count %d is negative", ErrInvalid, count)
	}
	if len(a.Cards) > 0 && len(a.Cards) != count {
		return nil, fmt.Errorf("%w: %d identities given for %d cards", ErrInvalid, len(a.Cards), count)
	}
	p := s.Seats[a.Seat]
	if p.Library.Known && p.Library.N < count {
		// Drawing from an empty library is a loss by state-based action
		// (CR 704.5c), but it depends on library knowledge being exact:
		// here it is simply not a state change the tracker can record
		// honestly.
		return nil, fmt.Errorf("%w: seat %d has %d cards in library, %d drawn", ErrInvalid, a.Seat, p.Library.N, count)
	}
	identified := len(a.Cards) == count
	evs := []Event{a.stamp(Event{Kind: EventCardDrawn, TargetSeat: a.Seat, Count: count, Identified: identified})}
	if identified {
		// The identities are this seat's alone until spoken: seat-visible,
		// invisible to every other seat as a row (ADR 13).
		evs = append(evs, a.stamp(Event{Kind: EventCardKnown, TargetSeat: a.Seat, Cards: a.Cards,
			From: ZoneLibrary, Drawn: true}.seatVisible(a.Seat)))
	}
	return evs, nil
}

func applyMill(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	seat, err := s.targetSeat(a)
	if err != nil {
		return nil, err
	}
	if a.Count < 1 {
		return nil, fmt.Errorf("%w: mill count %d is not positive", ErrInvalid, a.Count)
	}
	if len(a.Cards) > 0 && len(a.Cards) != a.Count {
		return nil, fmt.Errorf("%w: %d identities given for %d cards", ErrInvalid, len(a.Cards), a.Count)
	}
	p := s.Seats[seat]
	if p.Library.Known && p.Library.N < a.Count {
		return nil, fmt.Errorf("%w: seat %d has %d cards in library, %d milled", ErrInvalid, seat, p.Library.N, a.Count)
	}
	m := newMinter(s)
	evs := []Event{}
	for i := 0; i < a.Count; i++ {
		card := ""
		if i < len(a.Cards) {
			card = a.Cards[i]
		}
		id := m.nextObject + int64(i)
		// Milled face-up carries its identity; milled face-down enters the
		// graveyard as an unknown card — never a guessed one.
		evs = append(evs, a.stamp(Event{Kind: EventObjectCreated, Object: id, Identity: Identity{Card: card},
			Owner: seat, Controller: seat, From: ZoneLibrary, ToZone: ZoneGraveyard}))
	}
	return evs, nil
}

func applyReveal(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if _, err := s.player(a.Seat); err != nil {
		return nil, err
	}
	if len(a.Cards) == 0 {
		return nil, fmt.Errorf("%w: REVEAL needs the card names", ErrInvalid)
	}
	zone := a.FromZone
	if zone == "" {
		zone = ZoneHand
	}
	return []Event{a.stamp(Event{Kind: EventCardRevealed, TargetSeat: a.Seat, Cards: a.Cards, From: zone})}, nil
}

func applyLook(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if _, err := s.player(a.Seat); err != nil {
		return nil, err
	}
	if len(a.Cards) == 0 {
		return nil, fmt.Errorf("%w: LOOK needs the card names", ErrInvalid)
	}
	zone := a.FromZone
	if zone == "" {
		zone = ZoneLibrary
	}
	return []Event{a.stamp(Event{Kind: EventCardKnown, TargetSeat: a.Seat, Cards: a.Cards, From: zone}.seatVisible(a.Seat))}, nil
}

/* ---------- declared effects ---------- */

func applyDeclareEffect(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Effect) == "" {
		return nil, fmt.Errorf("%w: DECLARE_EFFECT needs its effect described", ErrInvalid)
	}
	if a.SourceObj != 0 {
		if _, err := s.object(a.SourceObj); err != nil {
			return nil, err
		}
	}
	return []Event{a.stamp(Event{Kind: EventEffectDeclared, Effect: a.Effect, SourceObj: a.SourceObj, SourceCard: a.SourceCard})}, nil
}

func applyAddModifier(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if _, err := s.object(a.Object); err != nil {
		return nil, err
	}
	if a.Modifier == nil {
		return nil, fmt.Errorf("%w: ADD_MODIFIER needs a modifier", ErrInvalid)
	}
	mod := *a.Modifier
	if !mod.Layer.Valid() {
		return nil, fmt.Errorf("%w: unknown layer %q", ErrInvalid, mod.Layer)
	}
	if !mod.Duration.Valid() {
		return nil, fmt.Errorf("%w: unknown duration %q", ErrInvalid, mod.Duration)
	}
	if mod.Delta.empty() {
		return nil, fmt.Errorf("%w: modifier carries no delta", ErrInvalid)
	}
	if mod.SourceObj != 0 {
		if _, err := s.object(mod.SourceObj); err != nil {
			return nil, err
		}
	}
	m := newMinter(s)
	mod.ID = m.nextModifier
	return []Event{a.stamp(Event{Kind: EventModifierAdded, Object: a.Object, Modifier: &mod})}, nil
}

func applyRemoveModifier(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	o, err := s.object(a.Object)
	if err != nil {
		return nil, err
	}
	for _, mod := range o.Modifiers {
		if mod.ID == a.ModifierID {
			return []Event{a.stamp(Event{Kind: EventModifierRemoved, Object: o.ID, ModifierID: a.ModifierID})}, nil
		}
	}
	return nil, fmt.Errorf("%w: object %d has no modifier %d", ErrInvalid, o.ID, a.ModifierID)
}
