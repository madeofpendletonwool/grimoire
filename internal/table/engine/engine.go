// Package engine is the Magic table's deterministic core (MAD-323, stage 2a
// of MAD-321): players, zones, objects, counters, modifiers, the reducer and
// the fold. docs/table/model.md is the contract and ADR 9 is the shape — the
// event log is the only writer, state is a fold, and nothing here does I/O,
// reads a clock, or calls a model. Card behaviour is declared, never
// simulated (ADR 11): base characteristics arrive as data on the actions, and
// everything else is a modifier or a counter.
//
// The package lives at internal/table/engine rather than the bare
// internal/table the ADR 9 naming note describes, because MAD-425's D&D
// projector screen landed on the bare name after that note was written. The
// family spelling survives: later table-side packages (the grammar, the
// intent pipeline) are its siblings under internal/table/.
package engine

import "fmt"

// ErrInvalid marks a rejected action. A rejected action produces zero events
// by construction — Apply either returns events or an error, never both —
// and that invariant is what makes optimistic application safe to build on.
var ErrInvalid = fmt.Errorf("engine: invalid action")

// Status is the game lifecycle: setup → active → finished. Lifecycle
// metadata only. Current turn, phase, step, priority holder, who is alive —
// all of it is fold state and is never stored on the game row.
type Status string

const (
	StatusSetup    Status = "setup"
	StatusActive   Status = "active"
	StatusFinished Status = "finished"
)

// Zone is one of the seven zones a seat can hold cards in. The tracking
// level below is part of the type system so no caller has to remember it:
// the battlefield and its neighbours are tracked object by object, the hand
// is a count, the library is a composition.
type Zone string

const (
	ZoneBattlefield Zone = "battlefield"
	ZoneStack       Zone = "stack"
	ZoneGraveyard   Zone = "graveyard"
	ZoneExile       Zone = "exile"
	ZoneCommand     Zone = "command"
	ZoneHand        Zone = "hand"
	ZoneLibrary     Zone = "library"
)

// TrackingLevel names how honestly the engine can speak about a zone.
type TrackingLevel string

const (
	// TrackTracked zones hold objects with identity, counters, modifiers,
	// attachments and tapped state.
	TrackTracked TrackingLevel = "tracked"
	// TrackCountOnly zones hold a count. The hand: contents opaque unless
	// explicitly revealed, which lands as CARD_KNOWN / CARD_REVEALED rows.
	TrackCountOnly TrackingLevel = "count_only"
	// TrackComposition zones hold a multiset of card names, never an
	// order. Order is not modelled and the engine refuses to answer as
	// though it were.
	TrackComposition TrackingLevel = "composition"
)

// Tracking reports the model doc's tracking level for a zone.
func (z Zone) Tracking() TrackingLevel {
	switch z {
	case ZoneBattlefield, ZoneStack, ZoneGraveyard, ZoneExile, ZoneCommand:
		return TrackTracked
	case ZoneHand:
		return TrackCountOnly
	case ZoneLibrary:
		return TrackComposition
	}
	return TrackTracked
}

// Count is a zone count that may be unknown. A tracker that joins a game in
// progress does not know how many cards are in a library, and collapsing
// "we don't know" into "zero" is the failure that makes a tracker
// untrustworthy — so unknown is a real value here, not a missing one.
type Count struct {
	Known bool `json:"known"`
	N     int  `json:"n"`
}

// KnownCount builds a count the engine can speak to.
func KnownCount(n int) Count { return Count{Known: true, N: n} }

// UnknownCount builds a count the engine must not answer for.
func UnknownCount() Count { return Count{} }

// Player is one seat's fold state: life, generic counters, flags, the
// count-only zones, and the damage bookkeeping the Comprehensive Rules make
// derivable from the log. Setup facts (name, bound user, commander) are
// echoed here from GAME_STARTED so the fold is self-describing, but their
// home is the mtg_seats row.
type Player struct {
	Seat         int               `json:"seat"`
	Name         string            `json:"name,omitempty"`
	UserID       string            `json:"user_id,omitempty"`
	DeckID       string            `json:"deck_id,omitempty"`
	Commander    string            `json:"commander,omitempty"`
	StartingLife int               `json:"starting_life,omitempty"`
	Life         int               `json:"life"`
	Alive        bool              `json:"alive"`
	Counters     map[string]int    `json:"counters,omitempty"`
	Flags        map[string]string `json:"flags,omitempty"`
	Hand         Count             `json:"hand"`
	// HandKnown is the identities this seat has seen of its own cards —
	// draws spoken aloud, looks. It is knowledge, not contents: the hand
	// stays count-only until something is revealed.
	HandKnown []string `json:"hand_known,omitempty"`
	Library   Count    `json:"library"`
	// LibraryComp is the remaining multiset of card names, an upper bound
	// per name once unidentified cards have left a known library.
	LibraryComp map[string]int `json:"library_comp,omitempty"`
	// LibraryExact is false once unidentified cards left a known library:
	// the composition is then bounded, and probability questions that need
	// exactness must say so rather than guess.
	LibraryExact bool `json:"library_exact,omitempty"`
	// LandsThisTurn is this seat's land drops this turn — a per-player
	// fact the CR makes one-per-turn-per-player, reset by the fold at
	// each TURN_ENDED anchor.
	LandsThisTurn int `json:"lands_this_turn,omitempty"`
	// CommanderDamage is damage taken per commander card name, derived
	// from DAMAGE_DEALT rows whose source is that seat's commander. It is
	// never a separate counter to drift — refolding the log recomputes it.
	CommanderDamage map[string]int `json:"commander_damage,omitempty"`
}

// StackItem is one object on the stack: a spell (which is a real object id),
// or an activated or triggered ability (which is a declaration with a
// controller and targets, not an object).
type StackItem struct {
	Object     int64    `json:"object,omitempty"`
	Mode       string   `json:"mode,omitempty"` // cast | activated | triggered
	Ability    string   `json:"ability,omitempty"`
	Controller int      `json:"controller,omitempty"`
	Targets    []Target `json:"targets,omitempty"`
}

// Target is what a spell or ability was pointed at when it was put on the
// stack: a seat, an object, or a named card.
type Target struct {
	Seat   int    `json:"seat,omitempty"`
	Object int64  `json:"object,omitempty"`
	Card   string `json:"card,omitempty"`
}

// State is the fold of one game's event log. It is derived, never stored as
// truth (ADR 9): every field here is recomputable from mtg_events alone,
// which is what makes rewind-to-ordinal a truncation rather than a
// compensating-transaction scheme.
type State struct {
	Status       Status          `json:"status"`
	Format       string          `json:"format,omitempty"`
	StartingLife int             `json:"starting_life,omitempty"`
	Seats        map[int]*Player `json:"seats"`
	// Order is turn order: seat positions ascending. Multiplayer elimination
	// (MAD-324) prunes it; stage 2a cycles it as seated.
	Order        []int             `json:"order,omitempty"`
	Objects      map[int64]*Object `json:"objects"`
	NextObject   int64             `json:"next_object"`
	NextModifier int64             `json:"next_modifier"`
	Turn         int               `json:"turn,omitempty"`
	TurnSeat     int               `json:"turn_seat,omitempty"`
	Phase        string            `json:"phase,omitempty"`
	Step         string            `json:"step,omitempty"`
	PrioritySeat int               `json:"priority_seat,omitempty"`
	// Passed is the seats that passed priority in succession, in order.
	// All alive seats passing either resolves the stack top or ends the
	// step; any STACK_PUSHED clears it. The finer priority windows are
	// MAD-324's; these two CR rules are enough for the log to be honest.
	Passed        []int       `json:"passed,omitempty"`
	Stack         []StackItem `json:"stack,omitempty"`
	LandsThisTurn int         `json:"lands_this_turn,omitempty"`
	// CommanderCasts counts casts from the command zone per commander
	// name. Commander tax is derived from it (MAD-324); the count itself
	// is the fold's to keep, never a stored field.
	CommanderCasts map[string]int `json:"commander_casts,omitempty"`
	// Attackers and Blockers are the current combat declarations, kept so
	// the board can render them between declaration and resolution. The
	// damage arithmetic is MAD-325's.
	Attackers []AttackAssignment `json:"attackers,omitempty"`
	Blockers  []BlockAssignment  `json:"blockers,omitempty"`
	// LastOrd is the highest event ordinal folded. Bookkeeping for
	// clients resuming a stream, not game truth.
	LastOrd int64 `json:"last_ord,omitempty"`
}

// NewState builds the empty state every fold starts from. Object and
// modifier ids mint from 1: zero is the unset sentinel throughout the
// payload vocabulary.
func NewState() *State {
	return &State{
		Status:       StatusSetup,
		Seats:        map[int]*Player{},
		Objects:      map[int64]*Object{},
		NextObject:   1,
		NextModifier: 1,
		Passed:       []int{},
		Stack:        []StackItem{},
	}
}

// player looks a seat up, wrapping the miss in ErrInvalid so callers can
// branch on rejected versus broken.
func (s *State) player(seat int) (*Player, error) {
	p, ok := s.Seats[seat]
	if !ok {
		return nil, fmt.Errorf("%w: seat %d is not in this game", ErrInvalid, seat)
	}
	return p, nil
}

// object looks an object up by its engine-minted id.
func (s *State) object(id int64) (*Object, error) {
	o, ok := s.Objects[id]
	if !ok {
		return nil, fmt.Errorf("%w: object %d does not exist", ErrInvalid, id)
	}
	return o, nil
}

// Battlefield lists a seat's unphased battlefield objects in id order — ids
// are minted monotonically, so id order is arrival order, which is also
// timestamp order for the modifier stack.
func (s *State) Battlefield(seat int) []*Object {
	var out []*Object
	for id := int64(1); id < s.NextObject; id++ {
		o, ok := s.Objects[id]
		if !ok || o.Zone != ZoneBattlefield || o.Phased {
			continue
		}
		if seat != 0 && o.Controller != seat {
			continue
		}
		out = append(out, o)
	}
	return out
}

// ZoneObjects lists the objects a seat holds in a tracked zone, in id order.
func (s *State) ZoneObjects(seat int, zone Zone) []*Object {
	var out []*Object
	for id := int64(1); id < s.NextObject; id++ {
		o, ok := s.Objects[id]
		if !ok || o.Zone != zone {
			continue
		}
		if seat != 0 && o.Owner != seat {
			continue
		}
		out = append(out, o)
	}
	return out
}
