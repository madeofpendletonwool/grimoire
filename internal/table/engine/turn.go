package engine

// The turn skeleton: phases and steps in Comprehensive Rules order. The
// sequence itself is fixed by the CR and depends on no card text, which is
// why ADVANCE can walk it deterministically. The priority window rules on
// top of it — who may act when, the two-boomerang check, state-based
// actions at every grant — are MAD-324's; this file owns the order.

import "sort"

// stepRef is one entry in the turn structure.
type stepRef struct {
	Phase string
	Step  string
}

// turnStructure is CR 302/304/305/508/509/510/511/512/513/514 in order:
// the beginning three, precombat main, the combat five, postcombat main,
// and the end two.
var turnStructure = []stepRef{
	{"beginning", "untap"},
	{"beginning", "upkeep"},
	{"beginning", "draw"},
	{"precombat_main", "main"},
	{"combat", "beginning_of_combat"},
	{"combat", "declare_attackers"},
	{"combat", "declare_blockers"},
	{"combat", "combat_damage"},
	{"combat", "end_of_combat"},
	{"postcombat_main", "main"},
	{"end", "end"},
	{"end", "cleanup"},
}

// stepIndex finds a phase/step pair in the structure; -1 when absent.
func stepIndex(phase, step string) int {
	for i, r := range turnStructure {
		if r.Phase == phase && r.Step == step {
			return i
		}
	}
	return -1
}

// nextStep returns the phase and step after the state's current one,
// skipping the first turn's draw step (CR 103.7a: the player who goes
// first skips it), and whether the walk fell off the end of the turn —
// the caller then ends the turn and starts the next seat's.
func nextStep(s *State) (phase, step string, turnEnds bool) {
	i := stepIndex(s.Phase, s.Step)
	if i < 0 {
		// An unpositioned state (fresh setup) starts at the top.
		i = -1
	}
	for i++; i < len(turnStructure); i++ {
		if s.Turn == 1 && turnStructure[i].Phase == "beginning" && turnStructure[i].Step == "draw" {
			continue
		}
		return turnStructure[i].Phase, turnStructure[i].Step, false
	}
	return "", "", true
}

// nextSeat walks turn order forward from a seat. Stage 2a cycles the seats
// as seated; elimination pruning is MAD-324's.
func nextSeat(s *State, from int) int {
	if len(s.Order) == 0 {
		return from
	}
	for i, seat := range s.Order {
		if seat == from {
			return s.Order[(i+1)%len(s.Order)]
		}
	}
	return s.Order[0]
}

// sortSeats orders a seat list ascending — turn order is position
// ascending, and GAME_STARTED's echo is stored sorted so the fold is
// byte-stable across writers.
func sortSeats(seats []int) {
	sort.Ints(seats)
}
