// Package replay is the session replay (MAD-426, stage 9 of MAD-417):
// the mechanical event log, played back.
//
// By the time a fight ends it has already been written down — the
// tracker journals every roll-consumed turn, every application of
// damage and healing, every death save, every condition, in order, as
// an append-only log that never forgets. Replay is therefore not new
// data and new tables; it is a *derivation*: the same pure grammar the
// tracker ran live (internal/combat), folded over the journal a second
// time, up to any point. The never-stored-balances rule makes every
// intermediate state free — the state at seq N is a function of the
// frozen lineup and the first N rows, nothing else, and scrubbing is
// just folding again.
//
// The fold is disciplined about anchors: where a journal row records
// the absolute it produced (the hp after a hit, the death-save ledger,
// the legendary budget), the replay re-runs the rule and asserts the
// journal's own number. A journal that disagrees with its grammar is a
// drift error, reported loudly — replay refuses to invent state. The
// end-state checksum (Verify) is the same discipline at full length:
// folding the whole journal must reproduce the recorded combatant rows
// exactly, byte for byte, or the replay says so.
//
// The package is pure over its inputs: give Fold a lineup and journal
// rows and it derives state with no database, no clock, no network.
// The store (store.go) reads through narrow windows onto the combat
// and session stores — the same standing every other mechanical reader
// takes — and owns nothing.
package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/combat"
)

/* ---------- the journal, normalized ---------- */

// Event is one combat_log row in the fold's own shape: everything the
// replay needs, nothing the persistence layer keeps for itself. Round
// is walked at load (journal rows carry the round only on turn rows;
// every row in between inherits it) so the timeline can group without
// re-deriving.
type Event struct {
	Seq         int64          `json:"seq"`
	Kind        string         `json:"kind"`
	Round       int            `json:"round"`
	CombatantID string         `json:"combatant_id,omitempty"`
	Amount      int            `json:"amount,omitempty"`
	Note        string         `json:"note,omitempty"`
	Actor       string         `json:"-"`
	Payload     map[string]any `json:"-"`
	CreatedAt   string         `json:"created_at,omitempty"`
}

/* ---------- the derived state ---------- */

// State is the battle as the journal has made it so far: the round and
// turn pointer, the standing, and every combatant's live numbers. It is
// a complete answer to "what did the table see at seq N".
type State struct {
	Round      int                `json:"round"`
	TurnIndex  int                `json:"turn_index"` // -1 until the first turn
	Status     string             `json:"status"`     // active | ended
	Combatants []combat.Combatant `json:"order"`
}

// Turn names whose turn it is, or "" before the first turn.
func (s State) Turn() string {
	if s.TurnIndex < 0 || s.TurnIndex >= len(s.Combatants) {
		return ""
	}
	return s.Combatants[s.TurnIndex].Name
}

/* ---------- seeding ---------- */

// Seed rebuilds one combatant's opening state from the recorded row
// plus the journal. The frozen fields (identity, initiative, AC, the
// statblock snapshot, position) never change mid-fight, so the row
// still holds them; every mutable field resets to its opening value.
// Opening hp is the one number the journal alone cannot name for a pc
// who walked in wounded — so it anchors on the first damage or heal
// row's recorded "before", and for a combatant no row ever touched the
// recorded row still *is* the opening state, because nothing overwrote
// it. Either way the fold starts exactly where the table did.
func Seed(row combat.Combatant, events []Event) combat.Combatant {
	c := row
	c.HPReduction, c.TempHP = 0, 0
	c.Downed, c.Stable, c.Dead = false, false, false
	c.DeathSuccesses, c.DeathFailures = 0, 0
	c.ReactionSpent, c.LegendaryUsed = false, 0
	c.Conditions = nil
	c.Reveal = ""
	if anchor, ok := firstHPAnchor(events, row.ID); ok {
		c.HP = anchor
	}
	return c
}

// firstHPAnchor finds the hp the combatant stood at just before their
// first damage or heal — the journal's own "before" on that row.
func firstHPAnchor(events []Event, combatantID string) (int, bool) {
	for _, ev := range events {
		if ev.CombatantID != combatantID {
			continue
		}
		switch ev.Kind {
		case "damage", "heal":
			if before, ok := pInt(ev.Payload, "before"); ok {
				return before, true
			}
		}
	}
	return 0, false
}

// Seeds builds the whole opening lineup from the recorded rows.
func Seeds(rows []combat.Combatant, events []Event) []combat.Combatant {
	out := make([]combat.Combatant, len(rows))
	for i, r := range rows {
		out[i] = Seed(r, events)
	}
	return out
}

/* ---------- the fold ---------- */

// Fold derives the battle's state after every journal row with seq <=
// upto. upto 0 is the opening lineup — nobody has acted. The pure
// grammar runs exactly as it did live; every anchor the journal
// records is asserted, and a journal that drifts from its own rules is
// an error, never a guess.
func Fold(seeds []combat.Combatant, events []Event, upto int64) (State, error) {
	live := make([]combat.Combatant, len(seeds))
	copy(live, seeds)
	st := State{Round: 1, TurnIndex: -1, Status: combat.StatusActive, Combatants: live}
	at := func(id string) (*combat.Combatant, error) {
		for i := range live {
			if live[i].ID == id {
				return &live[i], nil
			}
		}
		return nil, fmt.Errorf("journal row names combatant %q, who is not in this battle", id)
	}
	for _, ev := range events {
		if ev.Seq > upto {
			break
		}
		switch ev.Kind {
		case "start":
			if r, ok := pInt(ev.Payload, "round"); ok && r > 0 {
				st.Round = r
			}
		case "turn":
			idx, ok := pInt(ev.Payload, "turn_index")
			if !ok || idx < 0 || idx >= len(live) {
				return st, drift(ev, "turn_index", ev.Payload["turn_index"], "a position in the order")
			}
			// The same side effects the write ran, in the same order:
			// the outgoing turn ends, the incoming turn starts, and a
			// wrap wears every local condition one round.
			if st.TurnIndex >= 0 && st.TurnIndex < len(live) {
				out := live[st.TurnIndex].EndTurn()
				live[st.TurnIndex] = out
			}
			incoming := live[idx].StartTurn()
			live[idx] = incoming
			if r, ok := pInt(ev.Payload, "new_round"); ok {
				st.Round = r
				for i := range live {
					ticked, _ := combat.TickConditions(live[i])
					live[i] = ticked
				}
			} else if r, ok := pInt(ev.Payload, "round"); ok {
				st.Round = r
			}
			st.TurnIndex = idx
		case "damage":
			c, err := at(ev.CombatantID)
			if err != nil {
				return st, drift(ev, "combatant", ev.CombatantID, "a fighter in this battle")
			}
			asked, _ := pInt(ev.Payload, "asked")
			if asked == 0 {
				asked = ev.Amount // the column carries the effective; only a payload-less row needs it
			}
			if before, ok := pInt(ev.Payload, "before"); ok && before != c.HP {
				return st, drift(ev, c.Name+" hp before", before, c.HP)
			}
			_, reduceMax := ev.Payload["max_hp_reduced_by"]
			after, out, err := combat.ApplyDamage(*c, asked, pStr(ev.Payload, "damage_type"), reduceMax)
			if err != nil {
				return st, drift(ev, "damage", asked, err.Error())
			}
			if v, ok := pInt(ev.Payload, "after"); ok && v != after.HP {
				return st, drift(ev, c.Name+" hp after", v, after.HP)
			}
			if v, ok := pInt(ev.Payload, "effective"); ok && v != out.Effective {
				return st, drift(ev, c.Name+" effective damage", v, out.Effective)
			}
			*c = after
		case "heal":
			c, err := at(ev.CombatantID)
			if err != nil {
				return st, drift(ev, "combatant", ev.CombatantID, "a fighter in this battle")
			}
			if before, ok := pInt(ev.Payload, "before"); ok && before != c.HP {
				return st, drift(ev, c.Name+" hp before", before, c.HP)
			}
			after, err := combat.ApplyHeal(*c, ev.Amount)
			if err != nil {
				return st, drift(ev, "heal", ev.Amount, err.Error())
			}
			if v, ok := pInt(ev.Payload, "after"); ok && v != after.HP {
				return st, drift(ev, c.Name+" hp after", v, after.HP)
			}
			*c = after
		case "temp_hp":
			c, err := at(ev.CombatantID)
			if err != nil {
				return st, drift(ev, "combatant", ev.CombatantID, "a fighter in this battle")
			}
			after, err := combat.GrantTempHP(*c, ev.Amount)
			if err != nil {
				return st, drift(ev, "temp hp", ev.Amount, err.Error())
			}
			if v, ok := pInt(ev.Payload, "temp_hp"); ok && v != after.TempHP {
				return st, drift(ev, c.Name+" temp hp after", v, after.TempHP)
			}
			*c = after
		case "death_save":
			c, err := at(ev.CombatantID)
			if err != nil {
				return st, drift(ev, "combatant", ev.CombatantID, "a fighter in this battle")
			}
			after, _, err := combat.RecordDeathSave(*c, pStr(ev.Payload, "result"))
			if err != nil {
				return st, drift(ev, "death save", pStr(ev.Payload, "result"), err.Error())
			}
			if v, ok := pInt(ev.Payload, "successes"); ok && v != after.DeathSuccesses {
				return st, drift(ev, c.Name+" death-save successes", v, after.DeathSuccesses)
			}
			if v, ok := pInt(ev.Payload, "failures"); ok && v != after.DeathFailures {
				return st, drift(ev, c.Name+" death-save failures", v, after.DeathFailures)
			}
			*c = after
		case "condition":
			c, err := at(ev.CombatantID)
			if err != nil {
				return st, drift(ev, "combatant", ev.CombatantID, "a fighter in this battle")
			}
			rounds, _ := pInt(ev.Payload, "rounds")
			after, cond, err := combat.ApplyLocalCondition(*c, pStr(ev.Payload, "condition"), rounds, ev.Actor)
			if err != nil {
				return st, drift(ev, "condition", pStr(ev.Payload, "condition"), err.Error())
			}
			if id := pStr(ev.Payload, "id"); id != "" && id != cond.ID {
				return st, drift(ev, "condition id", id, cond.ID)
			}
			*c = after
		case "condition_end":
			c, err := at(ev.CombatantID)
			if err != nil {
				return st, drift(ev, "combatant", ev.CombatantID, "a fighter in this battle")
			}
			after, _, err := combat.EndLocalCondition(*c, pStr(ev.Payload, "id"))
			if err != nil {
				return st, drift(ev, "condition end", pStr(ev.Payload, "id"), err.Error())
			}
			*c = after
		case "legendary":
			c, err := at(ev.CombatantID)
			if err != nil {
				return st, drift(ev, "combatant", ev.CombatantID, "a fighter in this battle")
			}
			cost, _ := pInt(ev.Payload, "cost")
			after, err := c.SpendLegendary(cost)
			if err != nil {
				return st, drift(ev, "legendary", cost, err.Error())
			}
			if v, ok := pInt(ev.Payload, "used"); ok && v != after.LegendaryUsed {
				return st, drift(ev, c.Name+" legendary used", v, after.LegendaryUsed)
			}
			*c = after
		case "reaction":
			c, err := at(ev.CombatantID)
			if err != nil {
				return st, drift(ev, "combatant", ev.CombatantID, "a fighter in this battle")
			}
			spent, _ := ev.Payload["reaction_spent"].(bool)
			c.ReactionSpent = spent
		case "reveal":
			c, err := at(ev.CombatantID)
			if err != nil {
				return st, drift(ev, "combatant", ev.CombatantID, "a fighter in this battle")
			}
			c.Reveal = pStr(ev.Payload, "reveal")
		case "end":
			st.Status = combat.StatusEnded
			if r, ok := pInt(ev.Payload, "round"); ok && r != st.Round {
				return st, drift(ev, "final round", r, st.Round)
			}
		default:
			return st, fmt.Errorf("replay: journal row %d is kind %q, which the fold does not know", ev.Seq, ev.Kind)
		}
	}
	return st, nil
}

/* ---------- the checksum ---------- */

// canonFighter is one combatant's mechanical state in the checksum's
// canonical shape — only the numbers the journal moves, in a fixed
// order, with no presentation (reveal is the room's business, not the
// mechanics') and no nil-vs-empty ambiguity.
type canonFighter struct {
	ID             string           `json:"id"`
	Name           string           `json:"name"`
	HP             int              `json:"hp"`
	TempHP         int              `json:"temp_hp"`
	HPReduction    int              `json:"hp_reduction"`
	Downed         bool             `json:"downed"`
	Stable         bool             `json:"stable"`
	Dead           bool             `json:"dead"`
	DeathSuccesses int              `json:"death_successes"`
	DeathFailures  int              `json:"death_failures"`
	ReactionSpent  bool             `json:"reaction_spent"`
	LegendaryUsed  int              `json:"legendary_used"`
	Conditions     []canonCondition `json:"conditions,omitempty"`
}

// canonCondition is one standing condition, rounded to the same cents
// everywhere.
type canonCondition struct {
	Name          string `json:"name"`
	Rounds        int    `json:"rounds"`
	Concentration bool   `json:"concentration,omitempty"`
}

// canonState is a whole battle's mechanical state, the unit the
// checksum hashes and Verify compares.
type canonState struct {
	Round      int            `json:"round"`
	TurnIndex  int            `json:"turn_index"`
	Combatants []canonFighter `json:"combatants"`
}

func canonOf(st State) canonState {
	out := canonState{Round: st.Round, TurnIndex: st.TurnIndex, Combatants: make([]canonFighter, 0, len(st.Combatants))}
	for _, c := range st.Combatants {
		f := canonFighter{
			ID: c.ID, Name: c.Name, HP: c.HP, TempHP: c.TempHP, HPReduction: c.HPReduction,
			Downed: c.Downed, Stable: c.Stable, Dead: c.Dead,
			DeathSuccesses: c.DeathSuccesses, DeathFailures: c.DeathFailures,
			ReactionSpent: c.ReactionSpent, LegendaryUsed: c.LegendaryUsed,
		}
		for _, cond := range c.Conditions {
			f.Conditions = append(f.Conditions, canonCondition{Name: cond.Name, Rounds: cond.Rounds, Concentration: cond.Concentration})
		}
		out.Combatants = append(out.Combatants, f)
	}
	return out
}

// canonOfRows builds the same canonical state from the recorded rows —
// what the fold must reproduce.
func canonOfRows(round, turnIndex int, rows []combat.Combatant) canonState {
	return canonOf(State{Round: round, TurnIndex: turnIndex, Combatants: rows})
}

// Checksum renders a state's canonical form and hashes it — one
// sha256, stable across runs and installs, for the assertion that a
// replay is the battle and not a vibe.
func Checksum(st State) string {
	b, err := json.Marshal(canonOf(st))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Canon renders a state's canonical JSON (sorted struct order, fixed
// field order) — the bytes the checksum is over.
func Canon(st State) string {
	b, err := json.Marshal(canonOf(st))
	if err != nil {
		return ""
	}
	return string(b)
}

/* ---------- drift ---------- */

// Drift is a journal row that disagrees with the grammar it records —
// replay's loudest answer, because a quiet patch-up would make the
// checksum mean nothing.
type Drift struct {
	Seq  int64
	Kind string
	What string
	Want any
	Got  any
}

func (d Drift) Error() string {
	return fmt.Sprintf("replay: journal row %d (%s) drifted: %s — journal says %v, the rules derive %v",
		d.Seq, d.Kind, d.What, d.Want, d.Got)
}

func drift(ev Event, what string, want, got any) error {
	return Drift{Seq: ev.Seq, Kind: ev.Kind, What: what, Want: want, Got: got}
}

/* ---------- payload readers ---------- */

// JSON numbers decode as float64; the journal also spells a few things
// (rounds, costs) as they were written. These readers take the journal
// as it decodes, not as it was typed.

func pInt(m map[string]any, key string) (int, bool) {
	switch v := m[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

func pStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}
