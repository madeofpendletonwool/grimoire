// Package faction is the faction plan progression model (MAD-366): pure
// arithmetic over a plan, its steps and a caller-supplied modifier set. No
// database, no wall clock, no randomness — the store (store.go, same package)
// owns the rows and derives modifiers from the graph; the simulation tick
// that advances plans as time passes is the next stage and calls in here.
//
// A plan is the campaign's existing quest state machine
// (campaign.StateMachine — parsed, validated, edge-checked by the same code
// quests use) plus four things a quest does not have: an owner, a rate, a
// progress counter and a reaction rule. Progress accumulates toward the
// active step — the first step whose state the plan has not entered — and
// when it reaches the step's cost the plan moves to that step's state, which
// must be an edge the machine actually declares. Overflow carries into the
// next step, so one 30-day advance and thirty 1-day advances land on the
// same state.
package faction

import (
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// Plan statuses — campaign's schema vocabulary, aliased so this package's
// callers read one name. A plan starts dormant, is activated by the DM, and
// either completes (its last step was entered) or is abandoned; 'stalled'
// is what the plan_stalled integrity check calls an active plan that has
// stopped moving — the check may set nothing, it only reports.
const (
	PlanDormant   = campaign.PlanDormant
	PlanActive    = campaign.PlanActive
	PlanStalled   = campaign.PlanStalled
	PlanComplete  = campaign.PlanComplete
	PlanAbandoned = campaign.PlanAbandoned
)

// ValidPlanStatuses is the set the database CHECK enforces, mirrored for
// clean errors on the write path.
var ValidPlanStatuses = map[string]bool{
	PlanDormant: true, PlanActive: true, PlanStalled: true,
	PlanComplete: true, PlanAbandoned: true,
}

// Step is one entry of a plan's checklist: the work to ENTER its state. Cost
// is the progress it needs; requires names its preconditions and the
// author's chosen reaction when one breaks.
type Step struct {
	State    string        `json:"state"`
	Name     string        `json:"name"`
	Detail   string        `json:"detail"`
	Cost     float64       `json:"cost"`
	Requires []Requirement `json:"requires,omitempty"`
}

// Validate checks the step shape: a state name, a positive cost.
func (s Step) Validate() error {
	if strings.TrimSpace(s.State) == "" {
		return fmt.Errorf("%w: a step needs a state", campaign.ErrInvalid)
	}
	if s.Cost <= 0 {
		return fmt.Errorf("%w: step %q needs a cost > 0", campaign.ErrInvalid, s.State)
	}
	return nil
}

// Requirement is one precondition of a step. Exactly one clause should be
// set (entity, edge, fact or enemy plan); the label is what the arithmetic
// answer names. IfBroken is the author's chosen reaction when the world no
// longer satisfies the clause — the engine applies its factor, it never
// guesses whether a setback is a setback.
type Requirement struct {
	Label     string    `json:"label,omitempty"`
	Entity    string    `json:"entity,omitempty"`     // this entity must exist and be live
	Edge      *EdgeReq  `json:"edge,omitempty"`       // this edge must exist
	Fact      *FactReq  `json:"fact,omitempty"`       // a matching fact must hold
	EnemyPlan *EnemyReq `json:"enemy_plan,omitempty"` // no enemy_of plan may sit at the named state
	IfBroken  *Reaction `json:"if_broken,omitempty"`
}

// EdgeReq names one edge that must exist.
type EdgeReq struct {
	From string `json:"from"`
	Type string `json:"type"`
	To   string `json:"to"`
}

// FactReq names a fact that must hold: a non-superseded canon or derived
// fact with this subject and predicate, and object when set.
type FactReq struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object,omitempty"` // entity id or literal, matched against either
}

// EnemyReq is satisfied while no enemy_of faction's own plan sits at State.
type EnemyReq struct {
	State string `json:"state"`
}

// Reaction is the plan author's answer to a broken precondition: a signed
// factor applied to the advance and the reason the ledger will show.
// Factor 0 halts, 2 doubles the pace, -1 undoes work as fast as the rate
// built it.
type Reaction struct {
	Factor float64 `json:"factor"`
	Reason string  `json:"reason,omitempty"`
}

// Modifier is one signed term in an advance's arithmetic. The store derives
// them from graph state (a broken precondition, a lost edge, an enemy plan's
// position); Advance applies them.
type Modifier struct {
	Label  string  `json:"label"`
	Factor float64 `json:"factor"` // 1 = no effect; 0 = halt; >1 = accelerate; <0 = setback
	Reason string  `json:"reason,omitempty"`
}

// Plan is the progression model's whole view of one plan. The store loads
// it from faction_plans/_steps plus the transitions ledger (Reached is the
// states the plan has already entered, current state included).
type Plan struct {
	ID            string
	CampaignID    string
	FactionEntity string
	Name          string
	Machine       campaign.StateMachine
	CurrentState  string
	Steps         []Step // in pursuit order; first listed, first pursued
	Reached       []string
	Progress      float64 // work paid toward the active step
	RatePerDay    float64
	Status        string
	Visibility    string
	StartedDay    *int64
	LastAdvanced  *int64
}

// ReachedContains reports whether the plan has already entered state —
// a step whose state is reached is done, whatever progress says.
func (p Plan) ReachedContains(state string) bool {
	for _, s := range p.Reached {
		if s == state {
			return true
		}
	}
	return false
}

// ActiveStep is the first step whose state the plan has not entered. nil
// when every step is done (or the plan has no steps): progress then has
// nowhere to go and an advance is a no-op.
func (p Plan) ActiveStep() *Step {
	entered := map[string]bool{}
	for _, s := range p.Reached {
		entered[s] = true
	}
	entered[p.Machine.Initial] = true
	for i := range p.Steps {
		if !entered[p.Steps[i].State] {
			return &p.Steps[i]
		}
	}
	return nil
}

// PercentDone is the active step's completion, 0..1, for the UI's bar. 1
// when there is no active step (a complete plan reads as done, not broken).
func (p Plan) PercentDone() float64 {
	step := p.ActiveStep()
	if step == nil {
		return 1
	}
	if step.Cost <= 0 {
		return 0
	}
	if p.Progress <= 0 {
		return 0
	}
	if p.Progress >= step.Cost {
		return 1
	}
	return p.Progress / step.Cost
}

// Validate checks the plan shape: the machine (campaign's own rules), every
// step a declared state, no state claimed twice, the current state declared.
func (p Plan) Validate() error {
	if err := p.Machine.Validate(); err != nil {
		return err
	}
	declared := map[string]bool{}
	for _, s := range p.Machine.States {
		declared[s.Key] = true
	}
	seen := map[string]bool{}
	for _, s := range p.Steps {
		if err := s.Validate(); err != nil {
			return err
		}
		if !declared[s.State] {
			return fmt.Errorf("%w: step state %q is not a declared state of the machine", campaign.ErrInvalid, s.State)
		}
		if seen[s.State] {
			return fmt.Errorf("%w: step state %q declared twice", campaign.ErrInvalid, s.State)
		}
		seen[s.State] = true
	}
	if !declared[p.CurrentState] {
		return fmt.Errorf("%w: current state %q is not a declared state of the machine", campaign.ErrInvalid, p.CurrentState)
	}
	if !ValidPlanStatuses[p.Status] {
		return fmt.Errorf("%w: plan status %q", campaign.ErrInvalid, p.Status)
	}
	return nil
}

// StepMove is one state entered during an advance, with the progress that
// carried into the next step.
type StepMove struct {
	To    string  `json:"to"`
	Carry float64 `json:"carry"`
}

// Term is one named addend of an advance's arithmetic. The base term plus
// each modifier's signed contribution — the answer to "why is this plan at
// 62%?".
type Term struct {
	Label  string  `json:"label"`
	Delta  float64 `json:"delta"`
	Reason string  `json:"reason,omitempty"`
}

// Progression is what one advance did: the arithmetic (terms, total gain)
// and the moves it caused. Halted is non-empty when progress stopped before
// the days ran out — no legal edge to the next step, or no next step.
type Progression struct {
	FromState    string     `json:"from_state"`
	ToState      string     `json:"to_state"`
	FromProgress float64    `json:"from_progress"`
	ToProgress   float64    `json:"to_progress"`
	Days         int        `json:"days"`
	Gain         float64    `json:"gain"`
	Terms        []Term     `json:"terms"`
	Moves        []StepMove `json:"moves,omitempty"`
	Halted       string     `json:"halted,omitempty"`
}

// Summary renders the terms as one arithmetic line for the ledger: the
// reason string a transition row carries.
func (pr Progression) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "advance %dd: gain %s", pr.Days, formatNum(pr.Gain))
	for _, t := range pr.Terms {
		fmt.Fprintf(&b, "; %s %s", t.Label, formatNum(t.Delta))
		if t.Reason != "" {
			fmt.Fprintf(&b, " (%s)", t.Reason)
		}
	}
	if len(pr.Moves) > 0 {
		parts := make([]string, 0, len(pr.Moves))
		for _, m := range pr.Moves {
			parts = append(parts, fmt.Sprintf("%s (carry %s)", m.To, formatNum(m.Carry)))
		}
		fmt.Fprintf(&b, "; entered %s", strings.Join(parts, ", "))
	}
	if pr.Halted != "" {
		fmt.Fprintf(&b, "; halted: %s", pr.Halted)
	}
	return b.String()
}

func formatNum(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

// Advance moves a plan forward days days under a modifier set, pure. The
// gain is rate x days, plus each modifier's signed contribution
// (rate x days x (factor-1)) — arithmetic a DM can check, not a mood.
// Progress then pays the active step's cost step by step: entering a state
// is a move along an edge the machine must declare (the TransitionQuest
// rule, reused through Machine.HasEdge), overflow carries into the next
// step, and a missing edge halts the advance rather than forcing an illegal
// move. A plan that is not active does not move.
func Advance(plan Plan, days int, mods []Modifier) Progression {
	pr := Progression{
		FromState: plan.CurrentState, ToState: plan.CurrentState,
		FromProgress: plan.Progress, ToProgress: plan.Progress,
		Days:  days,
		Terms: []Term{{Label: "base", Delta: 0}},
	}
	if plan.Status != PlanActive || days <= 0 || plan.RatePerDay < 0 {
		pr.Days = max(days, 0)
		pr.Halted = haltReason(plan)
		return pr
	}
	base := plan.RatePerDay * float64(days)
	gain := base
	terms := []Term{{Label: "base", Delta: base}}
	for _, m := range mods {
		delta := base * (m.Factor - 1)
		if delta == 0 {
			continue
		}
		gain += delta
		terms = append(terms, Term{Label: m.Label, Delta: delta, Reason: m.Reason})
	}
	pr.Terms = terms
	pr.Gain = gain

	// A harsh modifier set can undo more work than the advance built;
	// progress never runs below zero.
	progress := plan.Progress + gain
	if progress < 0 {
		progress = 0
	}
	entered := map[string]bool{}
	for _, s := range plan.Reached {
		entered[s] = true
	}
	entered[plan.Machine.Initial] = true
	state := plan.CurrentState
	for {
		step := activeStepIn(plan.Steps, entered)
		if step == nil {
			// Every step done: the plan is complete wherever it sits, and
			// banks no progress toward nothing.
			pr.ToState, pr.ToProgress = state, 0
			pr.Halted = "no further step"
			return pr
		}
		if progress < step.Cost {
			break
		}
		if !plan.Machine.HasEdge(state, step.State) {
			// The machine does not declare this move; halt with the
			// progress banked at the step's threshold rather than force an
			// illegal transition or silently swallow the work.
			progress = step.Cost
			pr.ToState, pr.ToProgress = state, progress
			pr.Halted = fmt.Sprintf("no edge %s -> %s in the plan's machine", state, step.State)
			return pr
		}
		carry := progress - step.Cost
		pr.Moves = append(pr.Moves, StepMove{To: step.State, Carry: carry})
		state = step.State
		entered[state] = true
		progress = carry
	}
	pr.ToState = state
	pr.ToProgress = progress
	return pr
}

// haltReason explains a no-op advance.
func haltReason(p Plan) string {
	switch {
	case p.Status == PlanComplete:
		return "plan is complete"
	case p.Status == PlanAbandoned:
		return "plan is abandoned"
	case p.Status == PlanDormant:
		return "plan is dormant"
	case p.Status == PlanStalled:
		return "plan is stalled"
	case p.RatePerDay < 0:
		return "rate is negative"
	default:
		return ""
	}
}

// activeStepIn is ActiveStep over an explicit entered set.
func activeStepIn(steps []Step, entered map[string]bool) *Step {
	for i := range steps {
		if !entered[steps[i].State] {
			return &steps[i]
		}
	}
	return nil
}
