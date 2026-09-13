package engine

// The Action taxonomy: the input side. Actions are typed values; the
// grammar (MAD-330), the board UI (MAD-327) and the model fallback
// (MAD-331) all produce this same struct and Apply is the only consumer.
// Each action carries the acting seat and its entry source
// (tap | grammar | llm | voice | manual | system), which the store copies
// onto every event row it produces — audit and telemetry, how actions got
// entered.

// ActionKind is the action vocabulary from docs/table/model.md. The setup
// kinds that write rows (CREATE_GAME, SEAT_PLAYER, ATTACH_DECK) are Store
// methods rather than reducer actions — rows before log, because
// mtg_events carries the game FK and GAME_STARTED echoes the config.
// REWIND is a writer operation, not an action: it produces zero events by
// definition (Store.RewindTo).
type ActionKind string

const (
	ActionStartGame        ActionKind = "START_GAME"
	ActionEndGame          ActionKind = "END_GAME"
	ActionConcede          ActionKind = "CONCEDE"
	ActionAdvance          ActionKind = "ADVANCE"
	ActionPassPriority     ActionKind = "PASS_PRIORITY"
	ActionPlayLand         ActionKind = "PLAY_LAND"
	ActionCast             ActionKind = "CAST"
	ActionActivate         ActionKind = "ACTIVATE"
	ActionDeclareTrigger   ActionKind = "DECLARE_TRIGGER"
	ActionMoveZone         ActionKind = "MOVE_ZONE"
	ActionCreateToken      ActionKind = "CREATE_TOKEN"
	ActionTap              ActionKind = "TAP"
	ActionUntap            ActionKind = "UNTAP"
	ActionSetPhased        ActionKind = "SET_PHASED"
	ActionAttach           ActionKind = "ATTACH"
	ActionDetach           ActionKind = "DETACH"
	ActionDeclareAttackers ActionKind = "DECLARE_ATTACKERS"
	ActionDeclareBlockers  ActionKind = "DECLARE_BLOCKERS"
	ActionResolveCombat    ActionKind = "RESOLVE_COMBAT"
	ActionAdjustCounters   ActionKind = "ADJUST_COUNTERS"
	ActionSetCounters      ActionKind = "SET_COUNTERS"
	ActionChangeLife       ActionKind = "CHANGE_LIFE"
	ActionDealDamage       ActionKind = "DEAL_DAMAGE"
	ActionSetFlag          ActionKind = "SET_FLAG"
	ActionSetZoneCount     ActionKind = "SET_ZONE_COUNT"
	ActionDraw             ActionKind = "DRAW"
	ActionMill             ActionKind = "MILL"
	ActionReveal           ActionKind = "REVEAL"
	ActionLook             ActionKind = "LOOK"
	ActionDeclareEffect    ActionKind = "DECLARE_EFFECT"
	ActionAddModifier      ActionKind = "ADD_MODIFIER"
	ActionRemoveModifier   ActionKind = "REMOVE_MODIFIER"
)

// SeatConfig is one seated player as echoed into GAME_STARTED: the setup
// facts from the mtg_seats row plus the attached deck's composition, so
// the log alone rebuilds the game — including what each library held when
// play began, which is where library composition knowledge starts.
type SeatConfig struct {
	Seat         int            `json:"seat"`
	Name         string         `json:"name,omitempty"`
	UserID       string         `json:"user_id,omitempty"`
	DeckID       string         `json:"deck_id,omitempty"`
	Commander    string         `json:"commander,omitempty"`
	StartingLife int            `json:"starting_life,omitempty"`
	Deck         map[string]int `json:"deck,omitempty"`
}

// AttackAssignment is one creature declared as attacking a seat or a
// planeswalker object. Combat damage arithmetic is MAD-325's; the engine
// records the declaration, taps attackers without vigilance (a computed
// keyword), and fires the structural ATTACKERS_DECLARED event.
type AttackAssignment struct {
	Object       int64 `json:"object"`
	TargetSeat   int   `json:"target_seat,omitempty"`
	TargetObject int64 `json:"target_object,omitempty"`
}

// BlockAssignment is one creature declared as blocking, with the attackers
// it blocks in assignment order.
type BlockAssignment struct {
	Blocker   int64   `json:"blocker"`
	Attackers []int64 `json:"attackers,omitempty"`
}

// AttackOrder is one multi-blocked attacker's damage assignment order over
// its blockers (CR 509.3) — the attacking player's choice, declared with
// the blockers. An attacker absent from the orders assigns to its blockers
// in id order.
type AttackOrder struct {
	Attacker int64   `json:"attacker"`
	Blockers []int64 `json:"blockers,omitempty"`
}

// Action is a typed, validated proposal. One struct, one JSON shape — the
// event row's cause column stores this verbatim so every log entry is
// self-contained and amend prefills from the row itself. Fields are read
// per kind; the kind's reducer case is the authority on which.
type Action struct {
	Kind       ActionKind `json:"kind"`
	Seat       int        `json:"seat,omitempty"`
	Source     string     `json:"source,omitempty"`
	Confidence float64    `json:"confidence,omitempty"`
	// Disposition is the confirmation ladder's verdict (MAD-331):
	// auto | confirm | ask, stamped by the intent pipeline before
	// submission. It rides the cause column so the log says not just
	// how an action was entered but how confidently — the current
	// action pane highlights what was applied optimistically.
	Disposition string `json:"disposition,omitempty"`

	// START_GAME: the loaded seat table the log echoes, with config
	// defaults the fold adopts when set.
	Seats        []SeatConfig `json:"seats,omitempty"`
	Format       string       `json:"format,omitempty"`
	StartingLife int          `json:"starting_life,omitempty"`
	// END_GAME: why.
	Reason string `json:"reason,omitempty"`

	// PLAY_LAND / CAST / REVEAL: the card, where it comes from.
	Card     string `json:"card,omitempty"`
	FromZone Zone   `json:"from_zone,omitempty"`
	// CAST / PLAY_LAND / CREATE_TOKEN: declared base characteristics from
	// carddb or the token spec. Absent means unknown, and unknown stays
	// unknown.
	Base  *BaseChars `json:"base,omitempty"`
	Token *TokenSpec `json:"token,omitempty"`

	// CAST / ACTIVATE: declared targets.
	Targets []Target `json:"targets,omitempty"`
	// ACTIVATE: the ability's description.
	Ability string `json:"ability,omitempty"`

	// MOVE_ZONE / TAP / UNTAP / SET_PHASED / ATTACH / DETACH / counters /
	// modifiers: the object acted on.
	Object int64 `json:"object,omitempty"`
	// MOVE_ZONE: destination and cause (sacrifice | destroy | bounce |
	// exile | mill | counter | draw | cast | ...).
	ToZone Zone   `json:"to_zone,omitempty"`
	Cause  string `json:"cause,omitempty"`

	// CREATE_TOKEN / DRAW / MILL: how many.
	Count int `json:"count,omitempty"`

	// TAP / UNTAP: every applicable object (the untap-step convenience).
	All bool `json:"all,omitempty"`
	// SET_PHASED: the new phased state.
	Phased bool `json:"phased,omitempty"`
	// ATTACH: what Object becomes attached to.
	AttachTo int64 `json:"attach_to,omitempty"`

	// DECLARE_ATTACKERS / DECLARE_BLOCKERS.
	Attackers    []AttackAssignment `json:"attackers,omitempty"`
	Blockers     []BlockAssignment  `json:"blockers,omitempty"`
	AttackOrders []AttackOrder      `json:"attack_orders,omitempty"`

	// ADJUST_COUNTERS / SET_COUNTERS: the counter target — OnObject when
	// set, TargetSeat otherwise. CounterName is data, never schema.
	OnObject    int64  `json:"on_object,omitempty"`
	TargetSeat  int    `json:"target_seat,omitempty"`
	CounterName string `json:"counter_name,omitempty"`
	Delta       int    `json:"delta,omitempty"`
	To          *int   `json:"to,omitempty"`

	// CHANGE_LIFE: delta or absolute, with the spoken source if any — a
	// log entry that says why is worth several that say what.
	SourceCard string `json:"source_card,omitempty"`

	// DEAL_DAMAGE: amount > 0, from a source object or a named card, to a
	// seat or an object, combat or not.
	Amount       int   `json:"amount,omitempty"`
	SourceObj    int64 `json:"source_obj,omitempty"`
	TargetObject int64 `json:"target_object,omitempty"`
	CombatDmg    bool  `json:"combat,omitempty"`

	// SET_FLAG: flag name and value ("" clears). Monarch and initiative
	// are exclusive — the engine moves them off every other seat.
	Flag  string `json:"flag,omitempty"`
	Value string `json:"value,omitempty"`

	// SET_ZONE_COUNT: manual correction for the count-only zones.
	Zone Zone `json:"zone,omitempty"`

	// DRAW / MILL identities when spoken; REVEAL / LOOK card names.
	Cards []string `json:"cards,omitempty"`

	// DECLARE_EFFECT / DECLARE_TRIGGER: the declared shape or prose.
	Effect string `json:"effect,omitempty"`

	// ADD_MODIFIER / REMOVE_MODIFIER.
	Modifier   *Modifier `json:"modifier,omitempty"`
	ModifierID int64     `json:"modifier_id,omitempty"`
}
