// Package effects is the duration and condition engine (MAD-421, stage 4
// of MAD-417): ongoing effects that count themselves down.
//
// Every ongoing effect is a row — target entity, effect reference (a spell,
// a condition, a feature), source, a concentration flag, and a remaining
// amount in rounds, minutes, hours, days, until-dispelled or until-rest.
// The condition vocabulary is the game's own set, declared once in
// internal/homebrew and enforced here the same way MAD-383 enforces the
// damage types: a condition is one of the fifteen or it is refused, never
// free text.
//
// Two clocks, one engine. A round is 6 seconds — the 2014 SRD's own
// arithmetic — so every timed duration converts exactly into canonical
// seconds and the two clocks are two ways of spending the same number:
//
//	turn advance   the combat clock; every timed effect loses 6 seconds
//	               per round ticked (Stage 5 wires the round counter to
//	               this; the engine and the rows are that half's model)
//	world clock    the campaign clock; a day advance spends 24 hours of
//	               every timed effect, and a rest additionally ends
//	               until-rest effects — travel and rests expire things,
//	               correctly
//
// The canonical seconds make the round/minute boundary exact rather than
// approximate: {10 rounds} and {1 minute} are the same duration, count
// down identically on both clocks, and render the same bytes — the case
// the golden file pins.
//
// The package is pure: no database, no wall clock, no network. Identical
// effects and an identical clock position produce byte-identical state,
// forever.
package effects

import (
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/homebrew"
)

/* ---------- the vocabulary ---------- */

// Effect kinds. What the effect reference points at: a spell by name, one
// of the game's conditions, a class feature or trait, or anything else a
// table invents.
const (
	KindSpell     = "spell"
	KindCondition = "condition"
	KindFeature   = "feature"
	KindOther     = "other"
)

// Duration units — the issue's own list. The four timed units convert
// exactly into canonical seconds; the two until-units carry no clock
// amount and end by their event (a dispel, a rest).
const (
	UnitRound          = "round"           // 6 seconds
	UnitMinute         = "minute"          // 10 rounds
	UnitHour           = "hour"            // 60 minutes
	UnitDay            = "day"             // 24 hours
	UnitUntilDispelled = "until_dispelled" // ends on dispel or manually
	UnitUntilRest      = "until_rest"      // ends on the next applied rest
)

// Row statuses and end reasons. A row that time has worn away derives as
// expired on read; a row a hand ended (a dispel, a broken concentration, a
// superseding application, the rest event) is persisted as ended with its
// reason — the provenance the visible history keeps.
const (
	StatusActive = "active"
	StatusEnded  = "ended"

	EndExpired             = "expired"              // time ran out, on either clock
	EndDispelled           = "dispelled"            // dispel magic or an equivalent hand
	EndConcentrationBroken = "concentration_broken" // the source concentrated on something new
	EndRest                = "rest"                 // the next rest ended an until-rest effect
	EndSuperseded          = "superseded"           // the same effect applied again; latest wins
	EndManual              = "manual"               // the DM ended it by hand
)

// The 2014 SRD's own arithmetic, in seconds. A round is 6 seconds, a
// minute 10 rounds, an hour 60 minutes, a day 24 hours — every unit the
// vocabulary declares divides the next exactly, which is what makes the
// two clocks one engine.
const (
	RoundSeconds  int64 = 6
	MinuteSeconds int64 = 60
	HourSeconds   int64 = 3600
	DaySeconds    int64 = 86400
)

// timedUnits are the units a clock can wear away, in coarsest-first order.
var timedUnits = []string{UnitDay, UnitHour, UnitMinute, UnitRound}

// unitSeconds maps each timed unit to its canonical size.
var unitSeconds = map[string]int64{
	UnitRound:  RoundSeconds,
	UnitMinute: MinuteSeconds,
	UnitHour:   HourSeconds,
	UnitDay:    DaySeconds,
}

// IsTimed reports whether a unit counts down against a clock.
func IsTimed(unit string) bool {
	_, ok := unitSeconds[unit]
	return ok
}

// ValidKind reports whether k is one of the effect kinds.
func ValidKind(k string) bool {
	switch k {
	case KindSpell, KindCondition, KindFeature, KindOther:
		return true
	}
	return false
}

// IsCondition reports whether name is one of the game's fifteen conditions
// — the declared vocabulary, case-insensitive, the same rule MAD-383
// applies to damage types.
func IsCondition(name string) bool {
	for _, c := range homebrew.Conditions {
		if strings.EqualFold(strings.TrimSpace(name), c) {
			return true
		}
	}
	return false
}

// ConditionName canonicalizes a condition name to the vocabulary's own
// spelling, or "" when it is not one of the fifteen.
func ConditionName(name string) string {
	for _, c := range homebrew.Conditions {
		if strings.EqualFold(strings.TrimSpace(name), c) {
			return c
		}
	}
	return ""
}

// Conditions is the declared condition vocabulary, in the homebrew
// package's canonical order — the read surface offers it, free text is
// refused.
func Conditions() []string {
	return homebrew.Conditions
}

/* ---------- durations ---------- */

// Duration is the declared amount and unit — the spell's own words
// ("1 minute", "8 hours", "10 days"), kept verbatim for provenance while
// the engine works in canonical seconds.
type Duration struct {
	Amount int    `json:"amount"`
	Unit   string `json:"unit"`
}

// Validate checks a duration against the grammar: a timed unit needs an
// amount of at least 1; an until-unit carries no amount at all.
func (d Duration) Validate() error {
	switch d.Unit {
	case UnitRound, UnitMinute, UnitHour, UnitDay:
		if d.Amount < 1 {
			return fmt.Errorf("a %s duration needs an amount of at least 1, got %d", d.Unit, d.Amount)
		}
		return nil
	case UnitUntilDispelled, UnitUntilRest:
		if d.Amount != 0 {
			return fmt.Errorf("%s carries no amount; leave the amount out", d.Unit)
		}
		return nil
	default:
		return fmt.Errorf("duration unit %q", d.Unit)
	}
}

// Seconds converts a timed duration to canonical seconds. Until-units
// report ok=false: they have no clock amount to spend.
func (d Duration) Seconds() (int64, bool) {
	sec, ok := unitSeconds[d.Unit]
	if !ok {
		return 0, false
	}
	return int64(d.Amount) * sec, true
}

// String renders the declared duration in the game's own vocabulary:
// "1 minute", "8 hours", "until dispelled", "until rest".
func (d Duration) String() string {
	switch d.Unit {
	case UnitUntilDispelled:
		return "until dispelled"
	case UnitUntilRest:
		return "until rest"
	case UnitRound:
		return plural(d.Amount, "round")
	default:
		return plural(d.Amount, d.Unit)
	}
}

// Effect is one ongoing effect in the pure model: what is on whom, from
// whom, whether it rides concentration, and for how long. Identifying
// fields (ID, Target, Source) travel with it so engine output joins back
// onto rows without a second lookup.
type Effect struct {
	ID            string `json:"id,omitempty"`
	TargetID      string `json:"target_id"`
	TargetName    string `json:"target_name,omitempty"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Ref           string `json:"ref,omitempty"`
	SourceID      string `json:"source_id,omitempty"`
	SourceName    string `json:"source_name,omitempty"`
	Concentration bool   `json:"concentration,omitempty"`
	Duration      `json:"duration"`
}

// Validate checks the effect against the grammar: a kind, a name, a legal
// duration — and the condition vocabulary when the effect claims to be a
// condition. Concentration on a bare condition is refused: a condition is
// its own state with no concentrator; the spell that imposes it carries
// the concentration, not the condition it leaves behind.
func (e Effect) Validate() error {
	if !ValidKind(e.Kind) {
		return fmt.Errorf("effect kind %q", e.Kind)
	}
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("a %s effect needs a name", e.Kind)
	}
	if e.Kind == KindCondition {
		if !IsCondition(e.Name) {
			return fmt.Errorf("%q is not one of the game's conditions (%s)", e.Name, strings.Join(homebrew.Conditions, ", "))
		}
		if e.Concentration {
			return fmt.Errorf("a condition is not concentrated on; the spell that imposes it carries the concentration")
		}
	}
	return e.Duration.Validate()
}

/* ---------- the two clocks ---------- */

// World is the world-clock position an effect is judged against: how many
// campaign-clock days have elapsed since the effect's anchor day, and
// whether an applied rest has happened since it was applied. Both are
// read-time derivations — the campaign clock and the rests table are the
// truth, not a cached total.
type World struct {
	DaysSinceAnchor int64
	RestSinceApply  bool
}

// Running is one effect's derived state: canonical seconds remaining
// (until-units carry 0 and never time out), whether it has ended, and the
// end reason when it has.
type Running struct {
	Effect
	Remaining int64  `json:"remaining_seconds"`
	Ended     bool   `json:"ended"`
	EndReason string `json:"end_reason,omitempty"`
}

// Run derives one effect's state against the world clock. A timed effect
// spends 24 hours per elapsed day; an until-rest effect ends when a rest
// has happened since it was applied; an until-dispelled effect never ends
// against time. A backward clock (a DM fixing a typo) gives time back —
// the clock ledger's own rule, honoured rather than second-guessed.
func Run(e Effect, w World) Running {
	r := Running{Effect: e}
	sec, timed := e.Duration.Seconds()
	if !timed {
		switch e.Unit {
		case UnitUntilRest:
			if w.RestSinceApply {
				r.Ended, r.EndReason = true, EndRest
			}
		}
		return r
	}
	r.Remaining = sec - w.DaysSinceAnchor*DaySeconds
	if r.Remaining <= 0 {
		r.Ended, r.EndReason = true, EndExpired
		r.Remaining = 0
	}
	return r
}

// TickTurns advances the combat clock: every timed running loses one round
// per round ticked and expires when its time is spent. Until-units are
// untouched — a turn does not dispel anything. The input is not mutated; a
// fresh slice comes back, so a tick is as re-runnable as a sim step.
func TickTurns(rs []Running, rounds int64) []Running {
	if rounds < 0 {
		rounds = 0
	}
	out := make([]Running, len(rs))
	copy(out, rs)
	for i := range out {
		if out[i].Ended || !IsTimed(out[i].Unit) {
			continue
		}
		out[i].Remaining -= rounds * RoundSeconds
		if out[i].Remaining <= 0 {
			out[i].Remaining = 0
			out[i].Ended, out[i].EndReason = true, EndExpired
		}
	}
	return out
}

/* ---------- rendering ---------- */

// Render spells canonical seconds in the game's own vocabulary, greedily
// from the largest unit down: whole days, then hours, then minutes, then
// the sub-minute remainder in rounds. At most the two most significant
// non-zero parts render — "9 days 23 hours", "1 minute", "15 rounds" —
// and the round/minute boundary lands exactly where the game puts it:
// sixty seconds renders "1 minute" whether it was declared a minute or
// ten rounds.
func Render(sec int64) string {
	if sec <= 0 {
		return "expired"
	}
	days := sec / DaySeconds
	rem := sec % DaySeconds
	hours := rem / HourSeconds
	rem %= HourSeconds
	minutes := rem / MinuteSeconds
	rem %= MinuteSeconds
	rounds := rem / RoundSeconds
	var parts []string
	for _, p := range []struct {
		n  int64
		in string
	}{
		{days, "day"}, {hours, "hour"}, {minutes, "minute"}, {rounds, "round"},
	} {
		if p.n > 0 {
			parts = append(parts, plural(int(p.n), p.in))
		}
		if len(parts) == 2 {
			break
		}
	}
	if len(parts) == 0 {
		return "expired" // less than a round left: spent
	}
	return strings.Join(parts, " ")
}

// Display renders a running effect's remaining time: the until-units in
// their own words, timed durations through Render.
func (r Running) Display() string {
	if r.Ended {
		return "expired"
	}
	switch r.Unit {
	case UnitUntilDispelled:
		return "until dispelled"
	case UnitUntilRest:
		return "until rest"
	}
	return Render(r.Remaining)
}

// plural renders "1 round" / "2 rounds".
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
