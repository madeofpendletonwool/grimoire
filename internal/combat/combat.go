// Package combat is the combat tracker (MAD-422, stage 5 of MAD-417):
// the whole battle as one state machine.
//
// MAD-318's tracker scoped the fight from the monster side; this package
// makes every combatant the same kind of thing. PCs arrive from their
// sheets — the initiative bonus is the sheet's own dex arithmetic, AC and
// max HP are the definition the sheet holds. Monsters and companions
// arrive from the statblock machinery — the bestiary mirror or the
// campaign's homebrew — and the ranger's wolf is a wolf: the same
// statblock snapshot, fighting on the party's side.
//
// The state machine is the model. Initiative is one merged order, ties
// rolled through the dice engine (MAD-420) — every number on a
// combatant row has provenance back to a stored roll. Round and turn are
// persisted, so the battle survives reload exactly where the table left
// it. On-turn automation is mechanical and therefore here: durations
// decrement a round at the wrap (the effects engine's combat clock,
// MAD-421), recharge prompts surface when a monster's turn starts,
// legendary budgets reset when its turn ends, and lair reminders fire on
// initiative count 20 — losing ties, exactly as the rule says.
//
// The hit-point grammar follows the 2014 rules: temp HP absorbs first,
// resistance halves (rounding down), immunity zeroes, vulnerability
// doubles; a pc dropped to 0 is downed and dying — three death-save
// successes stable, three failures die, a natural 20 wakes at 1 hp —
// while monsters and companions simply die at 0. Damage carried past 0
// equal to the max is death outright, whatever the side. Every change
// flows through the append-only combat log and mirrors into the session
// log as kind 'combat' events — combat is the densest stream of
// mechanical events in the game, and the journal is what Stage 9's
// replay will read.
//
// Conditions are the duration engine's rows for entity-backed
// combatants (pcs — they persist past the battle, ticked by the same
// engine on both clocks) and combat-local state for statblock-backed
// ones — the same fifteen-condition vocabulary either way, because a
// goblin's poison does not outlive the goblin.
//
// The package is pure: no database, no wall clock, no network. The
// store (store.go) owns the rows, the dice rolls and the session mirror;
// identical combatants and an identical log produce identical state,
// forever.
package combat

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

/* ---------- the vocabulary ---------- */

// Combatant kinds — where the numbers came from.
const (
	KindPC        = "pc"        // a sheet: AC, max HP and the dex bonus are the sheet's
	KindMonster   = "monster"   // a statblock on the foe side
	KindCompanion = "companion" // a statblock on the party side: the wolf, the sidekick
)

// Sides — whose fight it is. Companions are party; everything the DM
// brought is foe.
const (
	SideParty = "party"
	SideFoe   = "foe"
)

// Combat statuses.
const (
	StatusActive = "active"
	StatusEnded  = "ended"
)

// Death-save results, the vocabulary the endpoint takes. The natural 20
// and natural 1 are their own kinds because the dice already told the
// table which face landed — the tracker records the consequence.
const (
	SaveSuccess     = "success"
	SaveFail        = "fail"
	SaveCritSuccess = "crit-success" // nat 20: awake at 1 hp
	SaveCritFail    = "crit-fail"    // nat 1: two failures
)

// LairInitiative is the count lair actions fire on. Losing ties: the
// action goes after everyone rolling above 20 and before everyone
// rolling 20 or under — the crossing the turn engine watches for.
const LairInitiative = 20

// Reveal modes — the DM's per-monster choice of what the table screen
// (MAD-425) may read. RevealOff is every battle's default: the monster's
// numbers are the DM's alone. RevealHP and RevealWord put one foe's hit
// points on the room's screen, as numbers or as the table's health word.
const (
	RevealOff  = ""     // hidden — the projector shows the name and nothing else
	RevealHP   = "hp"   // exact hit points
	RevealWord = "word" // the health word (bloodied)
)

// ValidRevealMode reports whether m is one of the reveal kinds.
func ValidRevealMode(m string) bool {
	switch m {
	case RevealOff, RevealHP, RevealWord:
		return true
	}
	return false
}

// ValidSaveResult reports whether r is one of the death-save kinds.
func ValidSaveResult(r string) bool {
	switch r {
	case SaveSuccess, SaveFail, SaveCritSuccess, SaveCritFail:
		return true
	}
	return false
}

/* ---------- the snapshot ---------- */

// RechargeAbility is one ability whose usage string carries a recharge —
// "Breath Weapon (recharge 5-6)" — surfaced as a prompt when the
// monster's turn starts.
type RechargeAbility struct {
	Name  string `json:"name"`
	Usage string `json:"usage"`
}

// Snapshot is the set of numbers a fight needs, frozen at combat start.
// The sheet and the statblock are definitions — slow-changing, shared,
// correctable; the snapshot is tonight's truth, immune to a sheet edit
// mid-battle and honest about what the table actually fought.
type Snapshot struct {
	Ref          string            `json:"ref,omitempty"`       // bestiary key or homebrew slug
	Statblock    string            `json:"statblock,omitempty"` // the statblock's own name
	Label        string            `json:"label,omitempty"`     // pcs: the classes label
	CR           string            `json:"cr,omitempty"`
	XP           int               `json:"xp,omitempty"`
	Homebrew     bool              `json:"homebrew,omitempty"`
	InitBonus    int               `json:"init_bonus,omitempty"`
	Resist       []string          `json:"resist,omitempty"`
	Immune       []string          `json:"immune,omitempty"`
	Vulnerable   []string          `json:"vulnerable,omitempty"`
	LegendaryMax int               `json:"legendary_max,omitempty"` // derived: the statblock's legendary-action count
	Lair         bool              `json:"lair,omitempty"`
	Recharge     []RechargeAbility `json:"recharge,omitempty"`
}

// SnapshotOfSheet freezes a pc's sheet into the fight's numbers. An
// unstructured sheet (no block, or the block carried nothing) freezes
// zeros — the combatant fights, the response says which numbers were
// missing.
func SnapshotOfSheet(s sheet.Sheet) Snapshot {
	snap := Snapshot{
		Label:      s.ClassesLabel(),
		InitBonus:  s.Mod("dex"),
		Resist:     lowerAll(s.Resistances),
		Immune:     lowerAll(s.Immunities),
		Vulnerable: lowerAll(s.Vulnerabilities),
	}
	return snap
}

// SnapshotOfCreature freezes a statblock into the fight's numbers. The
// legendary budget is derived — the statblock engine parses the action
// list, and the count of LEGENDARY_ACTION entries is the budget — and
// the recharge prompts are the actions whose usage string says so.
func SnapshotOfCreature(c encounter.Creature) Snapshot {
	snap := Snapshot{
		Ref:        c.Slug,
		Statblock:  c.Name,
		CR:         c.CR,
		XP:         c.XP,
		Homebrew:   c.Homebrew,
		Resist:     splitTypes(c.Resist),
		Immune:     splitTypes(c.Immune),
		Vulnerable: splitTypes(c.Vulnerable),
		Lair:       c.LairAction,
	}
	if c.Abilities != nil {
		snap.InitBonus = (c.Abilities.Dex - 10) / 2
	}
	for _, a := range c.Actions {
		switch {
		case strings.EqualFold(a.Kind, "LEGENDARY_ACTION"):
			snap.LegendaryMax++
		case strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.Usage)), "recharge"):
			snap.Recharge = append(snap.Recharge, RechargeAbility{Name: a.Name, Usage: a.Usage})
		}
	}
	return snap
}

// splitTypes turns a statblock's display string ("bludgeoning, piercing,
// and slashing from nonmagical attacks") into lowered, comma-split
// fragments the damage grammar can match against. The fragments keep
// their qualifiers — "fire" matches fire, and a qualified line matches
// only its own words.
func splitTypes(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, " and ", ",")
	s = strings.ReplaceAll(s, ", and ", ",")
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, strings.ToLower(part))
		}
	}
	return out
}

func lowerAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, strings.ToLower(s))
		}
	}
	return out
}

// matches reports whether the damage type hits one of the snapshot's
// lists. A qualified resistance ("bludgeoning from nonmagical attacks")
// matches its leading type — the qualifier is the table's call, not the
// tracker's.
func matches(list []string, dtype string) bool {
	for _, t := range list {
		if t == dtype || strings.HasPrefix(t, dtype+" ") {
			return true
		}
	}
	return false
}

/* ---------- the combatant ---------- */

// Condition is one condition on a statblock-backed combatant — the same
// fifteen-word vocabulary the effects engine enforces, tracked locally
// because a monster's poison does not outlive the monster. Rounds counts
// down at the round wrap; zero expires it.
type Condition struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Rounds        int    `json:"rounds"`
	Source        string `json:"source,omitempty"`
	Concentration bool   `json:"concentration,omitempty"`
}

// Combatant is one fighter in the pure model: the frozen snapshot, the
// rolled initiative, and the mid-play state. Identifying fields travel
// with it so engine output joins back onto rows without a second lookup.
type Combatant struct {
	ID             string      `json:"id,omitempty"`
	CombatID       string      `json:"combat_id,omitempty"`
	EntityID       string      `json:"entity_id,omitempty"`
	Name           string      `json:"name"`
	Side           string      `json:"side"`
	Kind           string      `json:"kind"`
	Snapshot       Snapshot    `json:"statblock"`
	Initiative     int         `json:"initiative"`
	InitFormula    string      `json:"init_formula,omitempty"`
	AC             int         `json:"ac"`
	MaxHP          int         `json:"max_hp"`
	HPReduction    int         `json:"hp_reduction"`
	HP             int         `json:"hp"`
	TempHP         int         `json:"temp_hp"`
	Downed         bool        `json:"downed"`
	Stable         bool        `json:"stable"`
	Dead           bool        `json:"dead"`
	DeathSuccesses int         `json:"death_successes"`
	DeathFailures  int         `json:"death_failures"`
	ReactionSpent  bool        `json:"reaction_spent"`
	LegendaryUsed  int         `json:"legendary_used"`
	Conditions     []Condition `json:"conditions,omitempty"`
	Reveal         string      `json:"reveal,omitempty"` // the table screen's exposure, foes only (MAD-425)
	Position       int         `json:"position"`
	CreatedAt      time.Time   `json:"-"`
	UpdatedAt      time.Time   `json:"-"`
}

// EffectiveMax is the max HP the combatant can hold right now — the
// sheet's number less what vampires and their kin have worn away.
func (c Combatant) EffectiveMax() int {
	max := c.MaxHP - c.HPReduction
	if max < 0 {
		return 0
	}
	return max
}

// CanAct reports whether the combatant takes turns: the dead do not, and
// neither does a downed pc — their turn is a death save, which the turn
// engine prompts for rather than skipping.
func (c Combatant) CanAct() bool { return !c.Dead }

// StartTurn resets what a turn's start returns to the combatant: the
// reaction comes back (one per round, regained at the start of your
// turn), and nothing else — legendary budgets return at turn end, the
// rule's own spelling.
func (c Combatant) StartTurn() Combatant {
	c.ReactionSpent = false
	return c
}

// EndTurn resets what a turn's end returns: the legendary budget
// replenishes (available for the rest of the round, spent outside the
// creature's own turn — exactly the legend's grammar).
func (c Combatant) EndTurn() Combatant {
	if c.Snapshot.LegendaryMax > 0 {
		c.LegendaryUsed = 0
	}
	return c
}

// SpendLegendary spends cost of the budget; the caller validates the
// ability's own cost, the grammar validates the budget.
func (c Combatant) SpendLegendary(cost int) (Combatant, error) {
	if c.Snapshot.LegendaryMax == 0 {
		return c, fmt.Errorf("%s has no legendary actions", c.Name)
	}
	if cost < 1 {
		cost = 1
	}
	if c.LegendaryUsed+cost > c.Snapshot.LegendaryMax {
		return c, fmt.Errorf("%s has %d of %d legendary actions left; %d asked", c.Name,
			c.Snapshot.LegendaryMax-c.LegendaryUsed, c.Snapshot.LegendaryMax, cost)
	}
	c.LegendaryUsed += cost
	return c, nil
}

/* ---------- damage and healing ---------- */

// DamageOutcome is what one application of damage did: the type and
// amount asked for, what resistance made of it, what temp HP drank, the
// hp before and after, and the state transitions the hit forced. The
// summary is the session event's own words.
type DamageOutcome struct {
	Asked        int    `json:"asked"`
	Type         string `json:"type,omitempty"`
	Effective    int    `json:"effective"`           // after immunity, resistance, vulnerability
	Absorbed     int    `json:"absorbed_by_temp_hp"` // what temp HP drank
	Overflow     int    `json:"carried_past_zero"`   // damage past 0 — massive-damage arithmetic
	Before       int    `json:"before"`
	After        int    `json:"after"`
	ReducedMax   int    `json:"max_hp_reduced_by"` // the vampire's bite
	WentDown     bool   `json:"went_down"`
	BecameStable bool   `json:"-"`
	Died         bool   `json:"died"`
	DeathFail    bool   `json:"auto_failed_death_save"` // damage while at 0
}

// Summary renders the outcome in the table's own words.
func (o DamageOutcome) Summary(c Combatant) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s takes %d", c.Name, o.Effective)
	if o.Type != "" {
		fmt.Fprintf(&b, " %s", o.Type)
	}
	if o.Asked != o.Effective {
		fmt.Fprintf(&b, " (from %d)", o.Asked)
	}
	if o.Absorbed > 0 {
		fmt.Fprintf(&b, ", %d to temp hp", o.Absorbed)
	}
	if o.ReducedMax > 0 {
		fmt.Fprintf(&b, ", max hp %d lower", o.ReducedMax)
	}
	switch {
	case o.Died && o.Overflow > 0 && c.Kind == KindPC:
		fmt.Fprintf(&b, " — massive damage kills %s outright", c.Name)
	case o.Died:
		fmt.Fprintf(&b, " — %s dies", c.Name)
	case o.WentDown:
		fmt.Fprintf(&b, " — %s goes down", c.Name)
	case o.DeathFail:
		fmt.Fprintf(&b, " at 0 hp — a failed death save (%d/3)", c.DeathFailures)
	}
	fmt.Fprintf(&b, " · %d/%d hp", o.After, c.EffectiveMax())
	return b.String()
}

// ApplyDamage runs the 2014 hit-point grammar over one hit: the type is
// transformed against the snapshot's lists (immunity zeroes, resistance
// halves rounding down, vulnerability doubles), temp HP drinks what it
// can, and what lands at 0 follows the side's own rule — pcs go down
// dying (damage while already at 0 is an auto-failed death save; damage
// carried past 0 equal to the max is death outright), everything else
// dies. reduceMax is the vampire's bite: the max HP wears by the damage
// that actually landed.
func ApplyDamage(c Combatant, amount int, dtype string, reduceMax bool) (Combatant, DamageOutcome, error) {
	if amount < 1 {
		return c, DamageOutcome{}, fmt.Errorf("damage needs a positive amount, got %d", amount)
	}
	if c.Dead {
		return c, DamageOutcome{}, fmt.Errorf("%s is already dead", c.Name)
	}
	dtype = strings.ToLower(strings.TrimSpace(dtype))
	out := DamageOutcome{Asked: amount, Type: dtype, Before: c.HP}
	effective := amount
	switch {
	case dtype != "" && matches(c.Snapshot.Immune, dtype):
		effective = 0
	case dtype != "" && matches(c.Snapshot.Resist, dtype):
		effective = amount / 2 // the 2014 rule rounds down
	case dtype != "" && matches(c.Snapshot.Vulnerable, dtype):
		effective = amount * 2
	}
	out.Effective = effective

	// Temp HP drinks first; it is gone once drunk.
	absorbed := c.TempHP
	if absorbed > effective {
		absorbed = effective
	}
	c.TempHP -= absorbed
	out.Absorbed = absorbed

	remaining := effective - absorbed
	if remaining > 0 {
		if remaining > c.HP {
			out.Overflow = remaining - c.HP
		}
		if c.HP-remaining < 0 {
			c.HP = 0
		} else {
			c.HP -= remaining
		}
	}
	if reduceMax && remaining > 0 {
		c.HPReduction += remaining
		out.ReducedMax = remaining
		if c.HP > c.EffectiveMax() {
			c.HP = c.EffectiveMax()
		}
	}
	out.After = c.HP

	if remaining == 0 {
		return c, out, nil // fully absorbed or immune: nobody falls
	}
	if c.HP > 0 {
		return c, out, nil
	}

	// At zero — or the ceiling itself fell to zero. The side's own rule.
	maxGone := c.EffectiveMax() == 0 && out.ReducedMax > 0
	switch {
	case c.Kind == KindPC:
		wasDowned := c.Downed
		if (out.Overflow >= c.EffectiveMax() && c.EffectiveMax() > 0) || maxGone {
			c.Dead = true
			out.Died = true
			return c, out, nil
		}
		c.Downed = true
		c.Stable = false
		out.WentDown = !wasDowned
		if wasDowned {
			// Damage while already at 0 is a failed death save.
			c.DeathFailures++
			out.DeathFail = true
			if c.DeathFailures >= 3 {
				c.Dead = true
				out.Died = true
			}
		}
	default:
		c.Dead = true
		out.Died = true
	}
	return c, out, nil
}

// ApplyHeal raises hp by amount (capped at the effective max). Any hp at
// all wakes a dying pc and clears the death-save ledger — healing does
// not remember how close the grave was.
func ApplyHeal(c Combatant, amount int) (Combatant, error) {
	if amount < 1 {
		return c, fmt.Errorf("healing needs a positive amount, got %d", amount)
	}
	if c.Dead {
		return c, fmt.Errorf("%s is dead; healing does not reach that far", c.Name)
	}
	c.HP += amount
	if max := c.EffectiveMax(); c.HP > max {
		c.HP = max
	}
	if c.HP > 0 {
		c.Downed, c.Stable, c.DeathSuccesses, c.DeathFailures = false, false, 0, 0
	}
	return c, nil
}

// GrantTempHP sets temp hp to the larger of what stands and what was
// granted — temp hit points do not stack, and a bigger coat replaces a
// smaller one.
func GrantTempHP(c Combatant, amount int) (Combatant, error) {
	if amount < 1 {
		return c, fmt.Errorf("temp hp needs a positive amount, got %d", amount)
	}
	if amount > c.TempHP {
		c.TempHP = amount
	}
	return c, nil
}

/* ---------- death saves ---------- */

// SaveOutcome is what one death-save result changed.
type SaveOutcome struct {
	Result       string `json:"result"`
	Successes    int    `json:"successes"`
	Failures     int    `json:"failures"`
	BecameStable bool   `json:"became_stable"`
	Died         bool   `json:"died"`
	Woke         bool   `json:"woke"`
}

// RecordDeathSave folds one death-save result into the ledger: a natural
// 20 wakes the pc at 1 hp with the ledger cleared, a natural 1 is two
// failures, three successes stabilize, three failures die.
func RecordDeathSave(c Combatant, result string) (Combatant, SaveOutcome, error) {
	if !ValidSaveResult(result) {
		return c, SaveOutcome{}, fmt.Errorf("death-save result %q", result)
	}
	if c.Dead {
		return c, SaveOutcome{}, fmt.Errorf("%s is already dead", c.Name)
	}
	if c.Kind != KindPC {
		return c, SaveOutcome{}, fmt.Errorf("%s does not make death saves", c.Name)
	}
	if !c.Downed {
		return c, SaveOutcome{}, fmt.Errorf("%s is not down", c.Name)
	}
	if c.Stable {
		return c, SaveOutcome{}, fmt.Errorf("%s is stable; only damage changes that", c.Name)
	}
	out := SaveOutcome{Result: result}
	switch result {
	case SaveSuccess:
		c.DeathSuccesses++
	case SaveCritSuccess:
		c.HP = 1
		c.Downed, c.Stable, c.DeathSuccesses, c.DeathFailures = false, false, 0, 0
		out.Woke = true
	case SaveFail:
		c.DeathFailures++
	case SaveCritFail:
		c.DeathFailures += 2
	}
	out.Successes, out.Failures = c.DeathSuccesses, c.DeathFailures
	if !out.Woke {
		switch {
		case c.DeathSuccesses >= 3:
			c.Stable = true
			out.BecameStable = true
		case c.DeathFailures >= 3:
			c.Dead = true
			out.Died = true
		}
	}
	return c, out, nil
}

// Summary renders the save in the table's own words.
func (o SaveOutcome) Summary(c Combatant) string {
	switch {
	case o.Woke:
		return fmt.Sprintf("%s rolls a natural 20 on a death save and wakes at 1 hp", c.Name)
	case o.BecameStable:
		return fmt.Sprintf("%s's third death-save success — stable at 0 hp", c.Name)
	case o.Died:
		return fmt.Sprintf("%s's third death-save failure — dead", c.Name)
	case o.Result == SaveCritFail:
		return fmt.Sprintf("%s rolls a natural 1 on a death save — two failures (%d/3)", c.Name, o.Failures)
	default:
		return fmt.Sprintf("%s's death save: %s (%d %s, %d %s)", c.Name,
			o.Result, o.Successes, pluralize(o.Successes, "success"), o.Failures, pluralize(o.Failures, "failure"))
	}
}

func pluralize(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

/* ---------- conditions (combat-local, statblock-backed only) ---------- */

// ApplyLocalCondition adds one condition to a statblock-backed
// combatant's list. The vocabulary is the effects engine's own fifteen;
// pcs carry their conditions as engine rows instead — those persist past
// the battle, and this method refuses them by design.
func ApplyLocalCondition(c Combatant, name string, rounds int, source string) (Combatant, Condition, error) {
	if c.Kind == KindPC {
		return c, Condition{}, fmt.Errorf("%s's conditions are duration-engine rows — apply them through the effects API", c.Name)
	}
	if rounds < 1 {
		return c, Condition{}, fmt.Errorf("a condition needs at least 1 round, got %d", rounds)
	}
	cond := Condition{ID: newConditionID(c), Name: name, Rounds: rounds, Source: source}
	for _, ex := range c.Conditions {
		if strings.EqualFold(ex.Name, name) {
			// Conditions do not stack; the latest application wins
			// and keeps the standing row's identity.
			cond.ID = ex.ID
		}
	}
	kept := make([]Condition, 0, len(c.Conditions)+1)
	for _, ex := range c.Conditions {
		if ex.ID != cond.ID {
			kept = append(kept, ex)
		}
	}
	c.Conditions = append(kept, cond)
	return c, cond, nil
}

// EndLocalCondition removes one condition by id.
func EndLocalCondition(c Combatant, condID string) (Combatant, Condition, error) {
	for _, ex := range c.Conditions {
		if ex.ID == condID {
			kept := make([]Condition, 0, len(c.Conditions)-1)
			for _, other := range c.Conditions {
				if other.ID != condID {
					kept = append(kept, other)
				}
			}
			c.Conditions = kept
			return c, ex, nil
		}
	}
	return c, Condition{}, fmt.Errorf("condition %s", condID)
}

// TickConditions wears every local condition one round and returns what
// expired — the combat clock's half of the duration engine, for the
// combatants the engine's rows cannot target.
func TickConditions(c Combatant) (Combatant, []Condition) {
	var expired []Condition
	kept := make([]Condition, 0, len(c.Conditions))
	for _, cond := range c.Conditions {
		cond.Rounds--
		if cond.Rounds <= 0 {
			expired = append(expired, cond)
			continue
		}
		kept = append(kept, cond)
	}
	c.Conditions = kept
	return c, expired
}

// newConditionID names a condition after its combatant and the live
// list's size — deterministic, unique within the list (the list is the
// truth; a re-applied condition keeps the standing identity).
func newConditionID(c Combatant) string {
	return fmt.Sprintf("%s-cond-%d", c.ID, len(c.Conditions)+1)
}

/* ---------- the order ---------- */

// Order sorts combatants into the initiative order: highest first, ties
// broken deterministically by name. The rolled tie-breaks happen at
// combat start (the dice engine owns them); this sort is the stable
// spine under whatever the rolls produced.
func Order(cs []Combatant) []Combatant {
	out := make([]Combatant, len(cs))
	copy(out, cs)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Initiative != out[j].Initiative {
			return out[i].Initiative > out[j].Initiative
		}
		if a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name); a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// NextAlive advances from position from to the next combatant who can
// still take a turn, reporting whether the scan wrapped the order (the
// boundary crossed is the round's own seam). A from of -1 is the battle's
// opening — the order's own top takes the first turn. A table where
// nobody else can act steps to the same combatant again — the state
// machine does not decide the battle is over; it reports the counts and
// the DM ends it.
func NextAlive(order []Combatant, from int) (next int, wrapped bool) {
	if len(order) == 0 {
		return 0, false
	}
	if from < 0 {
		for i, c := range order {
			if c.CanAct() {
				return i, false
			}
		}
		return 0, false
	}
	if from >= len(order) {
		from = 0
	}
	for step := 1; step <= len(order); step++ {
		i := (from + step) % len(order)
		if order[i].CanAct() {
			return i, i <= from
		}
	}
	return from, true
}

// LairFires reports whether a lair action reminder fires on the step
// from prev's initiative to next's: the action goes at count 20, losing
// all ties — after everyone above 20, before everyone at 20 or under.
// A wrap fires it when the round's own top act sits at or under 20.
func LairFires(prevInit, nextInit int, wrapped bool) bool {
	if wrapped {
		return nextInit <= LairInitiative
	}
	return prevInit > LairInitiative && nextInit <= LairInitiative
}

/* ---------- rendering ---------- */

// OrderLine renders the initiative order as one line the session log
// and the response share: "Velren 22, Goblin A 15, Goblin B 9".
func OrderLine(order []Combatant) string {
	parts := make([]string, 0, len(order))
	for _, c := range order {
		parts = append(parts, fmt.Sprintf("%s %d", c.Name, c.Initiative))
	}
	return strings.Join(parts, ", ")
}
