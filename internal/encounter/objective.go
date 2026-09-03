package encounter

// Objectives are what a fight is about. Every encounter the builder produced
// before this file existed was *kill the things* — the roster landed on the
// requested difficulty, but "reach the far side" and "survive six rounds" are
// not rosters, they are different fights that happen to contain monsters.
//
// An objective here is a declared kind, not a sentence: a fixed vocabulary,
// each kind carrying a round budget, a success condition, a failure
// condition, and a deterministic effect on the XP budget with its reasoning
// attached. Nothing in this file touches the network or the clock — it is
// pure, alongside design.go, so every number can be asserted in a test.

import (
	"fmt"
	"math"
	"strings"
)

// Kind is an objective's declared kind. The vocabulary is exhaustive here and
// nowhere else; an unknown kind is rejected by Validate rather than defaulted.
type Kind string

// The objective kinds. Defeat is the default — the fight the builder has
// always produced — and the zero value of the objective layer.
const (
	Defeat   Kind = "defeat"
	Survive  Kind = "survive"
	Reach    Kind = "reach"
	Protect  Kind = "protect"
	Stop     Kind = "stop"
	Retrieve Kind = "retrieve"
	Escape   Kind = "escape"
)

// Kinds lists every objective kind, lowest-key first. Exhaustive on purpose:
// the UI chips, the vocabulary check and the test that pins each kind's XP
// arithmetic all read this one slice.
var Kinds = []Kind{Defeat, Survive, Reach, Protect, Stop, Retrieve, Escape}

// KindLabels renders each kind the way the chips do.
var KindLabels = map[Kind]string{
	Defeat:   "Defeat",
	Survive:  "Survive",
	Reach:    "Reach",
	Protect:  "Protect",
	Stop:     "Stop",
	Retrieve: "Retrieve",
	Escape:   "Escape",
}

// defaultRounds is the round budget each kind carries when the DM does not
// name one. Defeat has no clock: the fight ends when a side ends.
var defaultRounds = map[Kind]int{
	Defeat:   0,
	Survive:  6,
	Reach:    5,
	Protect:  5,
	Stop:     4,
	Retrieve: 5,
	Escape:   5,
}

// defaultFocus is what the objective is about when the DM does not say. The
// model names the concrete thing in its write-up; these keep the endings and
// the prompt readable either way.
var defaultFocus = map[Kind]string{
	Protect:  "the thing they are protecting",
	Stop:     "the ritual",
	Retrieve: "the prize",
}

// maxRounds bounds a declared clock: twenty rounds is already a siege scene,
// not a combat encounter, and anything past that is a typo.
const maxRounds = 20

// maxFocusLen bounds the free-text focus a request may carry.
const maxFocusLen = 200

// Objective is what a fight is about: its kind, the clock it runs on, and —
// for the kinds that need one — the thing the fight is over. The zero value
// is no declared objective, which the builder treats as Defeat.
type Objective struct {
	Kind   Kind   `json:"kind"`
	Rounds int    `json:"rounds,omitempty"` // the round budget; the default fills in
	Focus  string `json:"focus,omitempty"`  // what protect guards / stop breaks / retrieve takes
}

// Validate rejects a malformed objective. An empty kind is not unknown — it
// is absent, which is Defeat; any other kind outside the vocabulary is an
// error, never a default. The API boundary calls this before anything else.
func (o Objective) Validate() error {
	switch o.Kind {
	case "", Defeat, Survive, Reach, Protect, Stop, Retrieve, Escape:
	default:
		return fmt.Errorf("%w: unknown objective kind %q — one of %s",
			ErrInvalid, string(o.Kind), kindList())
	}
	if o.Rounds < 0 || o.Rounds > maxRounds {
		return fmt.Errorf("%w: objective rounds must be between 1 and %d", ErrInvalid, maxRounds)
	}
	if len(o.Focus) > maxFocusLen {
		return fmt.Errorf("%w: objective focus is limited to %d characters", ErrInvalid, maxFocusLen)
	}
	return nil
}

// Normalized fills the kind's defaults: the round budget when none was
// declared, and a readable focus for the kinds that want one. It is what
// Plan, the prompt and the endings all read.
func (o Objective) Normalized() Objective {
	if o.Kind == "" {
		o.Kind = Defeat
	}
	if o.Rounds <= 0 {
		o.Rounds = defaultRounds[o.Kind]
	}
	o.Focus = strings.TrimSpace(o.Focus)
	if o.Focus == "" {
		o.Focus = defaultFocus[o.Kind]
	}
	return o
}

// RoundBudget is the clock the fight runs on, in rounds; zero means the
// fight has no clock and ends when a side does. The round tracker (MAD-318)
// reads this.
func (o Objective) RoundBudget() int {
	return o.Normalized().Rounds
}

// Ending is the "how this ends" block: success, failure, and the clock if
// there is one. Every string is deterministic — the same objective always
// ends the same way on paper, whatever the model writes around it.
type Ending struct {
	Kind    Kind   `json:"kind"`
	Label   string `json:"label"`
	Rounds  int    `json:"rounds,omitempty"`
	Focus   string `json:"focus,omitempty"`
	Success string `json:"success"`
	Failure string `json:"failure"`
	Clock   string `json:"clock,omitempty"`
}

// Ending renders the objective's success, failure and clock. A defeat
// objective still ends: the last monster drops, or the party does.
func (o Objective) Ending() Ending {
	o = o.Normalized()
	e := Ending{Kind: o.Kind, Label: KindLabels[o.Kind], Rounds: o.Rounds, Focus: o.Focus}
	switch o.Kind {
	case Defeat:
		e.Success = "every monster on the board is down or has fled"
		e.Failure = "the party is down, or breaks off"
	case Survive:
		e.Success = fmt.Sprintf("the party is still standing when round %d ends", o.Rounds)
		e.Failure = "the party breaks before the last round ends"
		e.Clock = fmt.Sprintf("the encounter runs %d rounds; reinforcing waves arrive until it does", o.Rounds)
	case Reach:
		e.Success = "at least one character crosses the far edge of the board"
		e.Failure = "every character is down, or the pursuit closes before anyone crosses"
		e.Clock = fmt.Sprintf("the pursuit closes at the end of round %d", o.Rounds)
	case Protect:
		e.Success = fmt.Sprintf("the last attacker drops while %s still stands", o.Focus)
		e.Failure = fmt.Sprintf("%s falls — the fight is lost whatever else happens", o.Focus)
		e.Clock = fmt.Sprintf("%s can take about %d rounds of punishment before it goes", o.Focus, o.Rounds)
	case Stop:
		e.Success = fmt.Sprintf("%s is broken before round %d ends", o.Focus, o.Rounds)
		e.Failure = fmt.Sprintf("%s completes at the end of round %d", o.Focus, o.Rounds)
		e.Clock = fmt.Sprintf("the clock completes at the end of round %d", o.Rounds)
	case Retrieve:
		e.Success = fmt.Sprintf("a character leaves the board with %s in hand", o.Focus)
		e.Failure = fmt.Sprintf("%s is lost, or the party is", o.Focus)
		e.Clock = fmt.Sprintf("once %s is seized, there are about %d rounds to get clear", o.Focus, o.Rounds)
	case Escape:
		e.Success = "the party is out"
		e.Failure = "anyone still inside when the way closes is lost"
		e.Clock = fmt.Sprintf("the way closes at the end of round %d", o.Rounds)
	}
	return e
}

/* ---------- The adjustment layer ---------- */

// Adjustment is one deterministic change the objective makes to the budget,
// with the reason attached. It is exposed in the budget readout on purpose:
// the objective layer is arithmetic a DM can argue with, not a hidden fudge
// factor.
type Adjustment struct {
	// Rule names the arithmetic: "waves", "rung", "aim" or "hazard".
	Rule string `json:"rule"`
	// Detail is the why, in one sentence, in the words the readout shows.
	Detail string `json:"detail"`
	// Target is the adjusted-XP aim as of this rule.
	Target int `json:"target_xp"`
}

// objPlan is what the objective layer decides before the shapes are priced:
// where the aim sits on the ladder, how far the multiplier rung shifts, how
// many waves the roster arrives in, and what the readout says about all of it.
type objPlan struct {
	objective   *Objective
	adjustments []Adjustment
	terrain     *Terrain
	rungShift   int
	waves       int
	aim         int
	ceiling     int
}

// aimLadder turns the party's thresholds into one ladder of aims: the four
// bands, then half again Deadly, then twice, then three times. Position 0-3
// is the asked band; every objective step moves one rung up the ladder. A
// defeat objective never leaves its own rung, so its aim and ceiling are
// exactly what Plan has always produced.
func aimLadder(thresholds map[string]int) []int {
	deadly := thresholds[BandDeadly]
	return []int{
		thresholds[BandEasy], thresholds[BandMedium], thresholds[BandHard], deadly,
		int(math.Round(float64(deadly) * 1.5)),
		deadly * 2,
		deadly * 3,
	}
}

// bandPosition reports where a band sits on the ladder.
func bandPosition(band string) int {
	for i, name := range Bands {
		if name == band {
			return i
		}
	}
	return 1 // Medium; canonicalBand never produces anything else
}

// WavesFor derives the wave count from the round budget: one wave roughly
// every three rounds — two for the common "hold for six" — floored at two
// (a one-wave survive is defeat) and capped at four, past which the board
// is a conveyor belt rather than a fight.
func WavesFor(rounds int) int {
	w := (rounds + 2) / 3
	if w < 2 {
		w = 2
	}
	if w > 4 {
		w = 4
	}
	return w
}

// planObjective applies the objective's deterministic adjustments. Everything
// downstream — the shapes, the solo ceiling, the readout — is priced from
// what this returns, which is why the same objective always lands on the
// same numbers.
func planObjective(o Objective, band string, thresholds map[string]int, avgLevel float64) objPlan {
	p := objPlan{aim: thresholds[band], ceiling: ceilingFor(thresholds, band)}
	if o.Kind == Defeat || o.Kind == "" {
		return p // no adjustments, no terrain: the MAD-299 fight, unchanged
	}
	o = o.Normalized()
	p.objective = &o

	ladder := aimLadder(thresholds)
	pos := bandPosition(band)

	// Escape moves the aim one rung up the ladder before anything else: an
	// encounter the party is not meant to stand and win is priced above the
	// party deliberately, never by accident.
	if o.Kind == Escape {
		pos++
		p.adjustments = append(p.adjustments, Adjustment{
			Rule: "aim",
			Detail: fmt.Sprintf("the party is not meant to stand and fight — the aim moves to the %s threshold, so leaving is the win and the threat is deliberate",
				Bands[clampBandIndex(pos)]),
			Target: ladder[pos],
		})
	}

	// Survive arrives in waves: the roster is the whole fight, and each wave
	// is priced at its own multiplier.
	if o.Kind == Survive {
		p.waves = WavesFor(o.Rounds)
		p.adjustments = append(p.adjustments, Adjustment{
			Rule: "waves",
			Detail: fmt.Sprintf("the %d rounds arrive in %d waves; each wave is priced at its own multiplier — the DMG's table assumes every monster is on the board at once — and the verdict checks the total across all of them, not the first wave alone",
				o.Rounds, p.waves),
		})
		p.adjustments[len(p.adjustments)-1].Target = ladder[pos]
	}

	// The rung kinds: some objectives concentrate the budget, others spread
	// it, each for a stated mechanical reason. One rung, never more.
	switch o.Kind {
	case Reach:
		p.rungShift = 1
		p.adjustments = append(p.adjustments, Adjustment{
			Rule:   "rung",
			Detail: "the party spends its actions on distance, not damage — the budget concentrates into fewer, faster monsters that can actually intercept (multiplier one rung up)",
			Target: ladder[pos],
		})
	case Stop:
		p.rungShift = 1
		p.adjustments = append(p.adjustments, Adjustment{
			Rule:   "rung",
			Detail: "the clock caps the fight — the roster has to be breakable inside the rounds it allows (multiplier one rung up)",
			Target: ladder[pos],
		})
	case Protect:
		p.rungShift = -1
		p.adjustments = append(p.adjustments, Adjustment{
			Rule:   "rung",
			Detail: "the monsters split their attention between the party and what they came for, so the same roster lands softer — the budget buys a bigger board (multiplier one rung down)",
			Target: ladder[pos],
		})
	case Retrieve:
		p.rungShift = -1
		p.adjustments = append(p.adjustments, Adjustment{
			Rule:   "rung",
			Detail: "two fights in one — the taking and the getting away — so the budget buys a bigger roster, split between holders and chasers (multiplier one rung down)",
			Target: ladder[pos],
		})
	}

	// Terrain is generated with the objective, and a hazardous battlefield
	// prices like the DMG prices traps: by tier. A hazard moves the aim one
	// rung up so the roster alone does not carry the whole band.
	t := TerrainFor(o, band, avgLevel)
	if len(t.Hazards) > 0 {
		pos++
		p.adjustments = append(p.adjustments, Adjustment{
			Rule: "hazard",
			Detail: fmt.Sprintf("the battlefield itself deals tier-priced damage (%s, DC %d, %s %s on a failure), so the aim moves a step up — the DMG prices traps by tier and the difficulty moves with them",
				t.Hazards[0].Name, t.Hazards[0].DC, t.Hazards[0].Damage, t.Hazards[0].DamageType),
			Target: ladder[pos],
		})
	}
	p.terrain = &t

	p.aim = ladder[pos]
	p.ceiling = ladder[clampLadderIndex(pos+1)]
	return p
}

// clampBandIndex keeps a ladder position inside the band names for prose.
func clampBandIndex(i int) int {
	if i > len(Bands)-1 {
		return len(Bands) - 1
	}
	return i
}

// clampLadderIndex keeps a ceiling lookup inside the ladder.
func clampLadderIndex(i int) int {
	if i > 6 {
		return 6
	}
	return i
}

// kindList renders the vocabulary for an error message.
func kindList() string {
	parts := make([]string, 0, len(Kinds))
	for _, k := range Kinds {
		parts = append(parts, string(k))
	}
	return strings.Join(parts, ", ")
}
