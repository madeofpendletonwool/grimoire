package faction

import (
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

func machine(initial string, states []string, edges ...[2]string) campaign.StateMachine {
	m := campaign.StateMachine{Initial: initial, States: states}
	for _, e := range edges {
		m.Edges = append(m.Edges, campaign.StateEdge{From: e[0], To: e[1]})
	}
	return m
}

// threeStepPlan is the acceptance shape: initial "mustering" and three steps
// of cost 10/20/30 along a linear machine.
func threeStepPlan() Plan {
	return Plan{
		ID: "p1", CampaignID: "c1", FactionEntity: "f1", Name: "The Root Takes Hold",
		Machine: machine("mustering",
			[]string{"mustering", "seed_the_mines", "sink_roots", "open_the_gate"},
			[2]string{"mustering", "seed_the_mines"},
			[2]string{"seed_the_mines", "sink_roots"},
			[2]string{"sink_roots", "open_the_gate"}),
		CurrentState: "mustering",
		Steps: []Step{
			{State: "seed_the_mines", Name: "Seed the mines", Cost: 10},
			{State: "sink_roots", Name: "Sink roots", Cost: 20},
			{State: "open_the_gate", Name: "Open the gate", Cost: 30},
		},
		Reached:    []string{"mustering"},
		RatePerDay: 1,
		Status:     PlanActive,
	}
}

func TestAdvanceTable(t *testing.T) {
	cases := []struct {
		name         string
		plan         Plan
		days         int
		mods         []Modifier
		wantTo       string
		wantProgress float64
		wantMoves    int
		wantHalted   bool
		wantGain     float64
	}{
		{
			name: "partial step", plan: threeStepPlan(), days: 5,
			wantTo: "mustering", wantProgress: 5, wantGain: 5,
		},
		{
			name: "exactly one step", plan: threeStepPlan(), days: 10,
			wantTo: "seed_the_mines", wantProgress: 0, wantMoves: 1, wantGain: 10,
		},
		{
			name: "carry across one step", plan: threeStepPlan(), days: 15,
			wantTo: "seed_the_mines", wantProgress: 5, wantMoves: 1, wantGain: 15,
		},
		{
			name: "two steps in one advance", plan: threeStepPlan(), days: 30,
			wantTo: "sink_roots", wantProgress: 0, wantMoves: 2, wantGain: 30,
		},
		{
			name: "every step, complete", plan: threeStepPlan(), days: 60,
			wantTo: "open_the_gate", wantProgress: 0, wantMoves: 3, wantGain: 60,
			wantHalted: true, // no further step
		},
		{
			name: "faster rate", plan: rate(threeStepPlan(), 2.5), days: 4,
			wantTo: "seed_the_mines", wantProgress: 0, wantMoves: 1, wantGain: 10,
		},
		{
			name: "accelerating modifier doubles the gain", plan: threeStepPlan(), days: 10,
			mods: []Modifier{{Label: "shrine_destroyed", Factor: 2, Reason: "the cult accelerates"}},
			wantTo: "seed_the_mines", wantProgress: 10, wantMoves: 1, wantGain: 20,
		},
		{
			name: "halting modifier banks nothing new", plan: progress(threeStepPlan(), 4), days: 10,
			mods: []Modifier{{Label: "mine_closed", Factor: 0}},
			wantTo: "mustering", wantProgress: 4, wantGain: 0,
		},
		{
			name: "setback modifier cannot go below zero", plan: threeStepPlan(), days: 10,
			mods: []Modifier{{Label: "cell_exposed", Factor: -2}},
			wantTo: "mustering", wantProgress: 0, wantGain: -20,
		},
		{
			name: "dormant plan does not move", plan: status(threeStepPlan(), PlanDormant), days: 90,
			wantTo: "mustering", wantProgress: 0, wantHalted: true,
		},
		{
			name: "abandoned plan does not move", plan: status(threeStepPlan(), PlanAbandoned), days: 90,
			wantTo: "mustering", wantProgress: 0, wantHalted: true,
		},
		{
			name: "zero days is a no-op", plan: threeStepPlan(), days: 0,
			wantTo: "mustering", wantProgress: 0,
		},
		{
			name: "no steps banks nothing", plan: steps(threeStepPlan(), nil), days: 30,
			wantTo: "mustering", wantProgress: 0, wantHalted: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Advance(tc.plan, tc.days, tc.mods)
			if got.ToState != tc.wantTo || got.ToProgress != tc.wantProgress {
				t.Fatalf("want %s@%v, got %s@%v", tc.wantTo, tc.wantProgress, got.ToState, got.ToProgress)
			}
			if len(got.Moves) != tc.wantMoves {
				t.Fatalf("want %d move(s), got %d: %+v", tc.wantMoves, len(got.Moves), got.Moves)
			}
			if (got.Halted != "") != tc.wantHalted {
				t.Fatalf("halted=%q, want %v", got.Halted, tc.wantHalted)
			}
			if tc.wantGain != 0 && got.Gain != tc.wantGain {
				t.Fatalf("gain: want %v, got %v (terms %+v)", tc.wantGain, got.Gain, got.Terms)
			}
		})
	}
}

// TestAdvanceCarryRule is the acceptance criterion: one 30-day advance and
// thirty 1-day advances over a three-step plan land on the same state.
func TestAdvanceCarryRule(t *testing.T) {
	one := Advance(threeStepPlan(), 30, nil)

	many := threeStepPlan()
	var pr Progression
	for i := 0; i < 30; i++ {
		pr = Advance(many, 1, nil)
		many.CurrentState = pr.ToState
		many.Progress = pr.ToProgress
		many.Reached = reachedAfter(many, pr)
	}
	if one.ToState != many.CurrentState || one.ToProgress != many.Progress {
		t.Fatalf("carry rule violated: one 30d advance -> %s@%v, thirty 1d -> %s@%v",
			one.ToState, one.ToProgress, many.CurrentState, many.Progress)
	}
	if len(one.Moves) != 2 || one.Moves[0].To != "seed_the_mines" || one.Moves[1].To != "sink_roots" {
		t.Fatalf("the 30-day advance should enter two states: %+v", one.Moves)
	}
}

// reachedAfter folds a progression's moves into the plan's entered set.
func reachedAfter(p Plan, pr Progression) []string {
	out := append([]string{}, p.Reached...)
	for _, m := range pr.Moves {
		known := false
		for _, r := range out {
			if r == m.To {
				known = true
				break
			}
		}
		if !known {
			out = append(out, m.To)
		}
	}
	return out
}

// TestAdvanceRefusesUndeclaredEdge is the acceptance criterion: a plan never
// moves along an edge its machine does not declare. The machine here has no
// edge out of mustering, so the work banks at the step's threshold and the
// advance halts instead of forcing the move.
func TestAdvanceRefusesUndeclaredEdge(t *testing.T) {
	p := threeStepPlan()
	p.Machine = machine("mustering",
		[]string{"mustering", "seed_the_mines", "sink_roots", "open_the_gate"},
		[2]string{"seed_the_mines", "sink_roots"}, // no mustering -> seed_the_mines
	)
	got := Advance(p, 40, nil)
	if got.ToState != "mustering" || len(got.Moves) != 0 {
		t.Fatalf("an undeclared edge must not be crossed: %+v", got)
	}
	if got.ToProgress != 10 {
		t.Fatalf("progress banks at the step's threshold, got %v", got.ToProgress)
	}
	if !strings.Contains(got.Halted, "no edge mustering -> seed_the_mines") {
		t.Fatalf("the halt must name the missing edge: %q", got.Halted)
	}
}

// TestAdvanceResumesAfterManualTransition: a plan partway through its steps
// (states already entered recorded in Reached) accumulates against the next
// step, not the first.
func TestAdvanceResumesAfterManualTransition(t *testing.T) {
	p := threeStepPlan()
	p.CurrentState = "seed_the_mines"
	p.Reached = []string{"mustering", "seed_the_mines"}
	got := Advance(p, 25, nil)
	if got.ToState != "sink_roots" || got.ToProgress != 5 || len(got.Moves) != 1 {
		t.Fatalf("resume from a mid-plan state: %+v", got)
	}
}

func TestProgressionSummaryNamesItsTerms(t *testing.T) {
	pr := Advance(threeStepPlan(), 30, []Modifier{
		{Label: "shrine_destroyed", Factor: 1.5, Reason: "the cult accelerates"},
		{Label: "spies_caught", Factor: 0.5},
	})
	s := pr.Summary()
	for _, want := range []string{"advance 30d", "base 30", "shrine_destroyed 15", "spies_caught -15", "entered seed_the_mines"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary %q missing %q", s, want)
		}
	}
}

func TestPlanValidate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Plan)
		want bool // want an error
	}{
		{"a sound plan", func(p *Plan) {}, false},
		{"step outside the machine", func(p *Plan) { p.Steps[1].State = "elsewhere" }, true},
		{"a step state twice", func(p *Plan) { p.Steps[2].State = p.Steps[0].State }, true},
		{"zero cost", func(p *Plan) { p.Steps[0].Cost = 0 }, true},
		{"current state outside the machine", func(p *Plan) { p.CurrentState = "elsewhere" }, true},
		{"unknown status", func(p *Plan) { p.Status = "victorious" }, true},
		{"bad machine", func(p *Plan) { p.Machine.Initial = "unheard_of" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := threeStepPlan()
			tc.mut(&p)
			err := p.Validate()
			if (err != nil) != tc.want {
				t.Fatalf("want error=%v, got %v", tc.want, err)
			}
		})
	}
}

func TestPercentDone(t *testing.T) {
	p := threeStepPlan()
	if got := p.PercentDone(); got != 0 {
		t.Fatalf("fresh plan: want 0, got %v", got)
	}
	p.Progress = 5
	if got := p.PercentDone(); got != 0.5 {
		t.Fatalf("half-paid step: want 0.5, got %v", got)
	}
	p.Reached = []string{"mustering", "seed_the_mines", "sink_roots", "open_the_gate"}
	if got := p.PercentDone(); got != 1 {
		t.Fatalf("complete plan: want 1, got %v", got)
	}
}

func rate(p Plan, r float64) Plan     { p.RatePerDay = r; return p }
func status(p Plan, s string) Plan    { p.Status = s; return p }
func progress(p Plan, v float64) Plan { p.Progress = v; return p }
func steps(p Plan, s []Step) Plan     { p.Steps = s; return p }
