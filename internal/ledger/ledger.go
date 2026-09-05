// Package ledger is the resource ledger (MAD-419, stage 2 of MAD-417): the
// mechanical state of every tracked thing a character owns.
//
// The sheet (internal/sheet) is the definition — slots, hit dice, the purse
// — slow-changing configuration. This package is the state: every change is
// an append-only transaction, and the current value of anything is always
// derived by folding the log in order, never stored. The same rule that
// makes a sim tick re-runnable makes "why is the wizard out of 3rd levels?"
// answerable: the question resolves to the actual spends, in order, each
// with provenance.
//
// One grammar covers everything tracked. A pool is
// {owner, kind, size, recovery, granularity}:
//
//	spell slots    kind=slot, name="3", recovery long (short for pact magic)
//	hit dice       kind=hit_dice, recovery manual (the half-regain is a
//	               transaction, see below)
//	ki, rage...    kind=feature, recovery as the 2014 rules say
//	ammunition...  kind=item, recovery manual
//	currency       kind=currency, recovery manual, unbounded above size
//
// Rests are batch transactions with 2014 PHB semantics, and the recovery
// grammar alone decides what resets — no feature gets a special case:
//
//	short rest  resets pools with recovery short (pact magic included)
//	long rest   resets pools with recovery short, long or dawn, and crosses
//	            the night: the campaign clock advances a day, and everything
//	            attached to it reacts — the schedule, the faction plans, the sim
//
// Hit dice are the one number the grammar deliberately does not auto-reset,
// because the 2014 long rest does not refill them: it returns "up to half of
// the total, minimum one die". The rest engine therefore emits an explicit
// regain transaction for them (half the total, minimum one, capped by what
// is spent) — visible in the batch like every other change, correctable by
// a DM set, never a silent rule.
//
// The package is pure: no database, no wall clock, no network. Identical
// pools and an identical log produce byte-identical balances, forever.
package ledger

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

/* ---------- the vocabulary ---------- */

// Pool kinds. The kind carries the semantics the recovery grammar cannot:
// boundedness (a slot pool refills to its size; a purse can grow past its
// declared contents) and where the sheet's numbers came from.
const (
	KindSlot     = "slot"
	KindHitDice  = "hit_dice"
	KindFeature  = "feature"
	KindItem     = "item"
	KindCurrency = "currency"
)

// Recovery modes — the one grammar a rest consults.
const (
	RecoveryShort  = "short"  // pact magic, ki, warlock everything
	RecoveryLong   = "long"   // spell slots, rage, Action Surge, Second Wind
	RecoveryDawn   = "dawn"   // recharges when a new day dawns
	RecoveryManual = "manual" // hit dice, ammunition, rations, currency
)

// Rest kinds.
const (
	RestShort = "short"
	RestLong  = "long"
)

// Transaction kinds.
const (
	TxnSpend  = "spend"
	TxnRegain = "regain"
	TxnSet    = "set"   // DM correction to an absolute value
	TxnReset  = "reset" // a rest's grammar-driven refill; amount is the size
)

// RecoveryRank orders pools canonically within a kind, so derivation and
// every rendered list share one spelling: slot "1" before slot "9", cp
// before pp. It is also the order golden files pin.
var currencyOrder = map[string]int{"cp": 0, "sp": 1, "ep": 2, "gp": 3, "pp": 4}

// kindRank orders the kinds: slots first (the question everyone asks), then
// hit dice, features, items, and the purse last.
var kindRank = map[string]int{
	KindSlot: 0, KindHitDice: 1, KindFeature: 2, KindItem: 3, KindCurrency: 4,
}

// Pool is one tracked thing: its kind, its sub-name within the kind ("3" for
// third-level slots, "ki", "arrows", "gp"), its size, its recovery grammar
// and its spend granularity — the smallest amount one transaction may move,
// which is 1 for everything 5e tracks and exists so the grammar is honest
// about it.
type Pool struct {
	ID          string `json:"id,omitempty"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	Size        int    `json:"size"`
	Recovery    string `json:"recovery"`
	Granularity int    `json:"granularity,omitempty"`
	Source      string `json:"source,omitempty"` // sheet | dm
}

// Key is the pool's canonical identity independent of its row id.
func (p Pool) Key() string { return p.Kind + ":" + p.Name }

// Less orders two pools canonically: by kind, then by name within the kind
// (slot levels numerically, currency by value, everything else by label).
func (p Pool) Less(q Pool) bool {
	if p.Kind != q.Kind {
		return kindRank[p.Kind] < kindRank[q.Kind]
	}
	if p.Kind == KindSlot {
		a, _ := strconv.Atoi(p.Name)
		b, _ := strconv.Atoi(q.Name)
		return a < b
	}
	if p.Kind == KindCurrency {
		return currencyOrder[p.Name] < currencyOrder[q.Name]
	}
	return strings.ToLower(p.DisplayName()) < strings.ToLower(q.DisplayName())
}

// DisplayName is what a surface calls the pool: the label when one was
// authored, else the name (feature and item pools are their own names; a
// slot pool's name is its level).
func (p Pool) DisplayName() string {
	if p.Label != "" {
		return p.Label
	}
	return p.Name
}

// Bounded reports whether the pool's balance has a ceiling: slots, hit dice
// and feature uses refill to a fixed size, so they can neither overspend
// nor overfill. Items and currency are unbounded upward — a quiver gains
// arrows, a purse gains gold — while a spend still cannot go below zero.
func (p Pool) Bounded() bool {
	switch p.Kind {
	case KindSlot, KindHitDice, KindFeature:
		return true
	default:
		return false
	}
}

// ResetOn reports whether the given rest resets this pool by its recovery
// grammar. A long rest resets everything a short rest would plus the
// long-recovery pools and the dawns crossed — both directions are the
// false-positive fixture: pact magic (short) DOES reset on a long rest;
// regular slots (long) do NOT reset on a short one.
func (p Pool) ResetOn(restKind string) bool {
	switch restKind {
	case RestShort:
		return p.Recovery == RecoveryShort
	case RestLong:
		return p.Recovery == RecoveryShort || p.Recovery == RecoveryLong || p.Recovery == RecoveryDawn
	default:
		return false
	}
}

// Validate checks a pool definition against the grammar.
func (p Pool) Validate() error {
	switch p.Kind {
	case KindSlot, KindHitDice, KindFeature, KindItem, KindCurrency:
	default:
		return fmt.Errorf("pool kind %q", p.Kind)
	}
	if p.Name = strings.TrimSpace(p.Name); p.Name == "" {
		return fmt.Errorf("a %s pool needs a name", p.Kind)
	}
	if p.Kind == KindSlot {
		lvl, err := strconv.Atoi(p.Name)
		if err != nil || lvl < 1 || lvl > 9 {
			return fmt.Errorf("slot pool %q is not a spell level 1..9", p.Name)
		}
	}
	if p.Kind == KindCurrency {
		if _, ok := currencyOrder[p.Name]; !ok {
			return fmt.Errorf("currency pool %q is not one of cp, sp, ep, gp, pp", p.Name)
		}
	}
	switch p.Recovery {
	case RecoveryShort, RecoveryLong, RecoveryDawn, RecoveryManual:
	default:
		return fmt.Errorf("recovery %q", p.Recovery)
	}
	if p.Size < 0 {
		return fmt.Errorf("pool %s has negative size %d", p.Key(), p.Size)
	}
	if p.Granularity < 1 {
		p.Granularity = 1
	}
	return nil
}

/* ---------- the sheet's definitions ---------- */

// PoolsOf derives a sheet's pool definitions — the numbers the sheet
// actually declares, and nothing it does not:
//
//   - every slot level in the spellcasting table, recovery long — or short
//     when the sheet's caster is pact magic: a warlock with no other
//     spellcasting class. (A multiclass warlock's combined table cannot
//     carry the separate pact pool the 2014 rules keep; such a sheet reads
//     as long-recovery, and the DM registers the pact pool by hand. A known
//     limit of the sheet's single slot table, not of the grammar.)
//   - one hit-die pool sized to the total level, recovery manual: the long
//     rest's half-regain is an explicit transaction, not a grammar reset.
//   - the five-coin purse, recovery manual, each coin its own pool.
//
// Features and inventory lines carry no numbers on the sheet, so they seed
// nothing: ki points, rage and arrows are DM-registered pools that use this
// same grammar unchanged.
func PoolsOf(s sheet.Sheet) []Pool {
	pools := make([]Pool, 0, 16)
	pact := pactCaster(s)
	if s.Spellcasting != nil {
		for lvl, n := range s.Spellcasting.Slots {
			if n > 0 {
				level, err := strconv.Atoi(lvl)
				if err != nil || level < 1 || level > 9 {
					continue // not a spell level the grammar knows; the sheet's validation owns that complaint
				}
				recovery := RecoveryLong
				if pact {
					recovery = RecoveryShort
				}
				pools = append(pools, Pool{
					Kind: KindSlot, Name: lvl, Label: ordinalLevel(level) + "-level slots",
					Size: n, Recovery: recovery, Source: "sheet",
				})
			}
		}
	}
	if total := s.TotalLevel(); total > 0 {
		pools = append(pools, Pool{
			Kind: KindHitDice, Name: "hit_dice", Label: "hit dice",
			Size: total, Recovery: RecoveryManual, Source: "sheet",
		})
	}
	if !s.Currency.IsZero() {
		for _, coin := range []struct {
			name, label string
			count       int
		}{
			{"cp", "copper", s.Currency.CP},
			{"sp", "silver", s.Currency.SP},
			{"ep", "electrum", s.Currency.EP},
			{"gp", "gold", s.Currency.GP},
			{"pp", "platinum", s.Currency.PP},
		} {
			if coin.count > 0 {
				pools = append(pools, Pool{
					Kind: KindCurrency, Name: coin.name, Label: coin.label,
					Size: coin.count, Recovery: RecoveryManual, Source: "sheet",
				})
			}
		}
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Less(pools[j]) })
	return pools
}

// pactCaster reports whether the sheet's slot table IS pact magic: warlock
// levels and no other spellcasting class. The 2014 multiclass rules keep a
// warlock's pact slots beside the combined table; a sheet carrying both is
// read as the combined table (long), and the DM registers the pact pool.
func pactCaster(s sheet.Sheet) bool {
	if s.Spellcasting == nil || len(s.Spellcasting.Slots) == 0 {
		return false
	}
	var warlock, other bool
	for _, c := range s.Classes {
		name := strings.ToLower(strings.TrimSpace(c.Class))
		if name == "" || c.Level <= 0 {
			continue
		}
		if name == "warlock" {
			warlock = true
		} else if classCasts(name) {
			other = true
		}
	}
	return warlock && !other
}

// classCasts names the 2014 classes whose Spellcasting feature contributes
// to the multiclass slot table. A class outside the list (a homebrew or a
// half-caster naming) is treated as casting — the conservative reading for
// "is this table pact magic" is to say no when anything else might cast.
func classCasts(name string) bool {
	switch name {
	case "barbarian", "fighter", "monk", "rogue":
		return false
	default:
		return true
	}
}

// ordinalLevel renders a slot level's ordinal: 1st, 2nd, 3rd, 4th...
func ordinalLevel(lvl int) string {
	switch lvl {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	default:
		return strconv.Itoa(lvl) + "th"
	}
}

/* ---------- the derivation ---------- */

// Transaction is one append-only change to one pool. Order is the log's
// order; Amount's meaning follows Kind: spend and regain move by their
// amount, set names the new absolute value, reset names the size rest
// restored.
type Transaction struct {
	ID        string `json:"id,omitempty"`
	PoolID    string `json:"pool_id,omitempty"`
	Pool      string `json:"pool"` // kind:name, the identity survives re-sync
	Kind      string `json:"kind"`
	Amount    int    `json:"amount"`
	RestID    string `json:"rest_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	EventID   string `json:"session_event_id,omitempty"`
	Actor     string `json:"actor,omitempty"`
	Note      string `json:"note,omitempty"`
	Day       int64  `json:"day,omitempty"`
}

// Validate checks a transaction against its pool's grammar and the balance
// it would apply to. Unbounded pools may fill past their size; nothing may
// go below zero; a set must land inside the bounds; every amount moves in
// whole granules.
func (t Transaction) Validate(p Pool, balance int) error {
	switch t.Kind {
	case TxnSpend, TxnRegain:
	default:
		return fmt.Errorf("transaction kind %q", t.Kind)
	}
	if t.Amount < 1 {
		return fmt.Errorf("a %s needs a positive amount, got %d", t.Kind, t.Amount)
	}
	if p.Granularity > 1 && t.Amount%p.Granularity != 0 {
		return fmt.Errorf("pool %s moves in units of %d", p.Key(), p.Granularity)
	}
	switch t.Kind {
	case TxnSpend:
		if t.Amount > balance {
			return fmt.Errorf("pool %s holds %d; cannot spend %d", p.Key(), balance, t.Amount)
		}
	case TxnRegain:
		if p.Bounded() && balance+t.Amount > p.Size {
			return fmt.Errorf("pool %s holds %d of %d; cannot regain %d", p.Key(), balance, p.Size, t.Amount)
		}
	}
	return nil
}

// ValidateSet checks a DM correction against its pool's bounds.
func ValidateSet(p Pool, value int) error {
	if value < 0 {
		return fmt.Errorf("pool %s cannot be set to %d", p.Key(), value)
	}
	if p.Bounded() && value > p.Size {
		return fmt.Errorf("pool %s is bounded by its size %d; cannot set to %d", p.Key(), p.Size, value)
	}
	return nil
}

// Balance is one pool's derived state: the definition it folds against, the
// current value, and how much of the pool is left to spend. The JSON shape
// is the derivation's stable contract — same pools, same log, same bytes.
type Balance struct {
	Pool     Pool `json:"pool"`
	Current  int  `json:"current"`
	Spent    int  `json:"spent"` // size - current, may be negative past size on unbounded pools
	TxnCount int  `json:"transactions"`
}

// Derive folds a transaction log into balances. The pools list is the
// definition; transactions referencing an unknown pool are folded by key
// onto the pool if it exists and ignored otherwise (a deleted pool's
// history stays in the log, inert). Pools come out in canonical order and
// transactions fold in slice order — the caller's log order is the truth.
func Derive(pools []Pool, txns []Transaction) []Balance {
	sorted := make([]Pool, len(pools))
	copy(sorted, pools)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Less(sorted[j]) })
	byKey := make(map[string]*Balance, len(sorted))
	out := make([]Balance, 0, len(sorted))
	for i := range sorted {
		b := Balance{Pool: sorted[i], Current: sorted[i].Size}
		out = append(out, b)
		byKey[sorted[i].Key()] = &out[len(out)-1]
	}
	for _, t := range txns {
		b, ok := byKey[t.Pool]
		if !ok {
			continue
		}
		switch t.Kind {
		case TxnSpend:
			b.Current -= t.Amount
		case TxnRegain:
			b.Current += t.Amount
		case TxnSet, TxnReset:
			b.Current = t.Amount
		}
		b.TxnCount++
	}
	for i := range out {
		out[i].Spent = out[i].Pool.Size - out[i].Current
	}
	return out
}

/* ---------- rests ---------- */

// PlannedTxn is one transaction a rest intends to write: the pool it hits
// (by row id once resolved against the store, by key in the pure plan), the
// transaction kind, the amount, and the human reason it fired.
type PlannedTxn struct {
	PoolID  string `json:"pool_id,omitempty"`
	Pool    string `json:"pool"`
	Entity  string `json:"entity,omitempty"`
	Kind    string `json:"kind"`
	Amount  int    `json:"amount"`
	From    int    `json:"from"`
	To      int    `json:"to"`
	Reason  string `json:"reason"`
	ItemRef string `json:"item,omitempty"` // the staged batch item that carries it
}

// RestPlan computes one character's transactions for a rest against the
// pools and their derived balances: every pool the recovery grammar resets
// gets a reset to size; on a long rest the hit-dice pool gains the 2014
// half-regain (half the total, minimum one die, capped by what is spent) as
// an explicit transaction. Pools already at their target contribute
// nothing — a rest is not log noise.
func RestPlan(restKind string, pools []Pool, balances []Balance) []PlannedTxn {
	byKey := make(map[string]Balance, len(balances))
	for _, b := range balances {
		byKey[b.Pool.Key()] = b
	}
	sorted := make([]Pool, len(pools))
	copy(sorted, pools)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Less(sorted[j]) })
	var plan []PlannedTxn
	for _, p := range sorted {
		b, ok := byKey[p.Key()]
		if !ok {
			continue
		}
		switch {
		case p.ResetOn(restKind):
			if b.Current < p.Size {
				plan = append(plan, PlannedTxn{
					Pool: p.Key(), Kind: TxnReset, Amount: p.Size,
					From: b.Current, To: p.Size,
					Reason: fmt.Sprintf("%s rest: %s-recovery pool resets", restKind, p.Recovery),
				})
			}
		case restKind == RestLong && p.Kind == KindHitDice:
			regain := HitDiceRegain(p.Size, p.Size-b.Current)
			if regain > 0 {
				plan = append(plan, PlannedTxn{
					Pool: p.Key(), Kind: TxnRegain, Amount: regain,
					From: b.Current, To: b.Current + regain,
					Reason: "long rest: up to half of total hit dice return (2014 PHB)",
				})
			}
		}
	}
	return plan
}

// HitDiceRegain is the 2014 long-rest rule as a number: up to half of the
// total, minimum one die, capped by what is actually spent.
func HitDiceRegain(total, spent int) int {
	if spent <= 0 || total <= 0 {
		return 0
	}
	regain := total / 2
	if regain < 1 {
		regain = 1
	}
	if regain > spent {
		regain = spent
	}
	return regain
}

// RestSummary renders what a rest did to one character, for the session
// event and the review queue's item summaries: concrete, in the campaign's
// own vocabulary, no numbers invented.
func RestSummary(restKind string, plan []PlannedTxn) string {
	if len(plan) == 0 {
		if restKind == RestLong {
			return "finishes a long rest at full strength; night passes"
		}
		return "finishes a short rest at full strength"
	}
	var parts []string
	for _, t := range plan {
		switch t.Kind {
		case TxnReset:
			parts = append(parts, fmt.Sprintf("%s restored to %d", t.Pool, t.To))
		default:
			parts = append(parts, fmt.Sprintf("%s %d -> %d", t.Pool, t.From, t.To))
		}
	}
	verb := "short"
	if restKind == RestLong {
		verb = "long"
	}
	return verb + " rest: " + strings.Join(parts, ", ")
}
