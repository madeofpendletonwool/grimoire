package engine

// The Event taxonomy: the output side, the rows mtg_events stores. ★ in the
// model doc marks the structural kinds — the ones fixed by the Comprehensive
// Rules that the trigger registry (MAD-335) can fire on. The full
// vocabulary is defined here even where a later stage is the producer:
// the vocabulary lives in the Go type system and grows with the engine,
// which is why kind is not CHECK-constrained in SQL.

// EventKind is one row kind in the log.
type EventKind string

const (
	EventGameStarted       EventKind = "GAME_STARTED"
	EventGameEnded         EventKind = "GAME_ENDED"
	EventTurnStarted       EventKind = "TURN_STARTED"
	EventStepEntered       EventKind = "STEP_ENTERED" // ★
	EventTurnEnded         EventKind = "TURN_ENDED"
	EventPlayerLeft        EventKind = "PLAYER_LEFT"   // elimination or concession
	EventObjectCeased      EventKind = "OBJECT_CEASED" // a token off the battlefield (CR 704.5d)
	EventPriorityPassed    EventKind = "PRIORITY_PASSED"
	EventStackPushed       EventKind = "STACK_PUSHED"
	EventStackResolved     EventKind = "STACK_RESOLVED"
	EventObjectCreated     EventKind = "OBJECT_CREATED"
	EventZoneChanged       EventKind = "ZONE_CHANGED"
	EventLandPlayed        EventKind = "LAND_PLAYED"        // ★
	EventCreatureETB       EventKind = "CREATURE_ETB"       // ★
	EventDied              EventKind = "DIED"               // ★ (sba_* causes asserted by the sweep)
	EventCast              EventKind = "CAST"               // ★
	EventAttackersDeclared EventKind = "ATTACKERS_DECLARED" // ★
	EventBlockersDeclared  EventKind = "BLOCKERS_DECLARED"
	EventCombatResolved    EventKind = "COMBAT_RESOLVED"
	EventDamageDealt       EventKind = "DAMAGE_DEALT"
	EventDamageMarked      EventKind = "DAMAGE_MARKED"
	EventLifeChanged       EventKind = "LIFE_CHANGED"
	EventCounterChanged    EventKind = "COUNTER_CHANGED"
	EventFlagChanged       EventKind = "FLAG_CHANGED"
	EventTapChanged        EventKind = "TAP_CHANGED"
	EventPhaseChanged      EventKind = "PHASE_CHANGED"
	EventAttached          EventKind = "ATTACHED"
	EventUnattached        EventKind = "UNATTACHED"
	EventModifierAdded     EventKind = "MODIFIER_ADDED"
	EventModifierRemoved   EventKind = "MODIFIER_REMOVED"
	EventTriggerFired      EventKind = "TRIGGER_FIRED" // queues; stacks at the next priority grant
	EventCardDrawn         EventKind = "CARD_DRAWN"
	EventCardKnown         EventKind = "CARD_KNOWN"
	EventCardRevealed      EventKind = "CARD_REVEALED"
	EventEffectDeclared    EventKind = "EFFECT_DECLARED"
	EventZoneCountSet      EventKind = "ZONE_COUNT_SET"
)

// Structural reports the ★ kinds — fixed by the CR, independent of card
// text, safe for the trigger registry to fire on.
func (k EventKind) Structural() bool {
	switch k {
	case EventStepEntered, EventLandPlayed, EventCreatureETB, EventCast,
		EventAttackersDeclared, EventDied:
		return true
	}
	return false
}

// Visibility is the hidden-zone gate (ADR 13): a seat row is invisible to
// every other seat as a row, and its public consequences — if any — are
// separate public events. The engine's own fold includes seat-visible rows
// (it must, to serve a seat its own view); the gate is the query.
type Visibility string

const (
	VisibilityPublic Visibility = "public"
	VisibilitySeat   Visibility = "seat"
)

// Event is one thing that happened. In memory the reducer mints events
// without ID, Ord or CreatedAt; the single writer assigns those at persist
// time, in one transaction, so ordinals are per-game contiguous from 1.
// Cause is the Action JSON that produced the row and Batch the stamp one
// Submit minted for all of its rows, both filled by the store — the fold
// never reads either, which is what keeps re-folding from the database
// honest.
type Event struct {
	ID          string     `json:"id,omitempty"`
	Ord         int64      `json:"ord,omitempty"`
	Kind        EventKind  `json:"kind"`
	ActorSeat   int        `json:"actor_seat,omitempty"`
	Source      string     `json:"source,omitempty"`
	Cause       string     `json:"cause,omitempty"`
	Batch       string     `json:"batch,omitempty"`
	Visibility  Visibility `json:"visibility,omitempty"`
	VisibleSeat int        `json:"visible_seat,omitempty"`
	CreatedAt   int64      `json:"created_at,omitempty"`

	// GAME_STARTED: the config echo that makes the log self-describing.
	Seats        []SeatConfig `json:"seats,omitempty"`
	Format       string       `json:"format,omitempty"`
	StartingLife int          `json:"starting_life,omitempty"`

	// GAME_ENDED: why.
	Reason string `json:"reason,omitempty"`

	// TURN_STARTED / TURN_ENDED / STEP_ENTERED.
	Turn     int    `json:"turn,omitempty"`
	TurnSeat int    `json:"turn_seat,omitempty"`
	Phase    string `json:"phase,omitempty"`
	Step     string `json:"step,omitempty"`

	// PRIORITY_PASSED: who passed, who holds priority next.
	SeatTo int `json:"seat_to,omitempty"`

	// STACK_PUSHED / STACK_RESOLVED: the stack item and where a resolving
	// spell went.
	Mode       string   `json:"mode,omitempty"`
	Ability    string   `json:"ability,omitempty"`
	Controller int      `json:"controller,omitempty"`
	Targets    []Target `json:"targets,omitempty"`

	// OBJECT_CREATED / ZONE_CHANGED / CAST / LAND_PLAYED / CREATURE_ETB /
	// DIED: object identity and movement.
	Object   int64      `json:"object,omitempty"`
	Identity Identity   `json:"identity,omitempty"`
	Base     *BaseChars `json:"base,omitempty"`
	Owner    int        `json:"owner,omitempty"`
	From     Zone       `json:"from,omitempty"`
	ToZone   Zone       `json:"to_zone,omitempty"`
	// ZONE_CHANGED causes: sacrifice, destroy, bounce, exile, mill,
	// counter, draw, cast, resolve, ... DIED causes (MAD-324):
	// sba_zero_toughness, sba_lethal_damage, ...

	// CAST / STACK_PUSHED: the card a spell was cast as.
	Card string `json:"card,omitempty"`

	// ATTACKERS_DECLARED / BLOCKERS_DECLARED.
	Attackers    []AttackAssignment `json:"attackers,omitempty"`
	Blockers     []BlockAssignment  `json:"blockers,omitempty"`
	AttackOrders []AttackOrder      `json:"attack_orders,omitempty"`

	// DAMAGE_DEALT / DAMAGE_MARKED / LIFE_CHANGED: source rides the event
	// so the log entry says why.
	Amount       int    `json:"amount,omitempty"`
	Combat       bool   `json:"combat,omitempty"`
	TargetSeat   int    `json:"target_seat,omitempty"`
	TargetObject int64  `json:"target_object,omitempty"`
	SourceObj    int64  `json:"source_obj,omitempty"`
	SourceCard   string `json:"source_card,omitempty"`
	Delta        int    `json:"delta,omitempty"`
	// COUNTER_CHANGED / ZONE_COUNT_SET / LIFE_CHANGED absolutes.
	To *int `json:"to,omitempty"`

	// COUNTER_CHANGED / FLAG_CHANGED / ZONE_COUNT_SET.
	Name  string `json:"name,omitempty"`
	Flag  string `json:"flag,omitempty"`
	Value string `json:"value,omitempty"`
	Zone  Zone   `json:"zone,omitempty"`

	// TAP_CHANGED / PHASE_CHANGED. ATTACHED / UNATTACHED use Object and
	// TargetObject for the edge's two ends.
	Tapped bool `json:"tapped,omitempty"`
	Phased bool `json:"phased,omitempty"`

	// MODIFIER_ADDED / MODIFIER_REMOVED.
	Modifier   *Modifier `json:"modifier,omitempty"`
	ModifierID int64     `json:"modifier_id,omitempty"`

	// CARD_DRAWN / CARD_KNOWN / CARD_REVEALED: CARD_DRAWN carries count
	// only — identity is seat-visible in a CARD_KNOWN or public in a
	// CARD_REVEALED, never smuggled through the public draw row.
	// Identified lets the fold know the draw's identities were spoken
	// without reading ahead in the log; Drawn marks the CARD_KNOWN that
	// carries a draw's identities, as opposed to a look at cards that
	// stay where they are.
	Count      int      `json:"count,omitempty"`
	Cards      []string `json:"cards,omitempty"`
	Identified bool     `json:"identified,omitempty"`
	Drawn      bool     `json:"drawn,omitempty"`

	// TRIGGER_FIRED / EFFECT_DECLARED: the declared spec or prose.
	Effect string `json:"effect,omitempty"`
}

// Public builds a public event stamped with the action's audit trail.
func (e Event) Public() Event { e.Visibility = VisibilityPublic; return e }

// seatVisible builds a seat-visible event: invisible to every other seat
// as a row, per ADR 13.
func (e Event) seatVisible(seat int) Event {
	e.Visibility = VisibilitySeat
	e.VisibleSeat = seat
	return e
}
