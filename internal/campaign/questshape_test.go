package campaign

// The quest shape arithmetic (MAD-371): the topology a generator fills is
// law before a model is asked anything, so every property the issue
// accepts — fork counts, exclusivity, no dead ends, no unreachable states,
// an ending within reach, the endings each shape sustains — is asserted
// here on the built graph, with no model at all.

import (
	"fmt"
	"testing"
)

// machineOf assembles a topology into a StateMachine, using the internal
// slot ids as keys — they are already legal keys. This is the same assembly
// the generator performs with the model's names.
func machineOf(t *QuestTopology) StateMachine {
	m := StateMachine{Initial: t.Initial}
	for _, s := range t.States {
		m.States = append(m.States, State{Key: s.ID, Label: s.ID, Terminal: s.Terminal})
	}
	m.Edges = append(m.Edges, func() []StateEdge {
		var out []StateEdge
		for _, e := range t.Edges {
			out = append(out, StateEdge{From: e.From, To: e.To})
		}
		return out
	}()...)
	return m
}

// questFindings runs the quest graph checks over one machine the way the
// engine will once the quest lands.
func questFindings(m StateMachine) []Finding {
	snap := Snapshot{Quests: []Quest{{
		ID: "q-test", Name: "The test quest", Status: QuestActive,
		Machine: m, CurrentState: m.Initial,
	}}}
	var out []Finding
	for _, f := range Check(&snap) {
		switch f.Check {
		case CheckQuestStateUnreachable, CheckQuestDeadEnd, CheckQuestNoEnding, CheckQuestTransitionInvalid:
			out = append(out, f)
		}
	}
	return out
}

func TestQuestTopology_ForkCountIsArithmetic(t *testing.T) {
	for _, kind := range []string{QuestKindInvestigation, QuestKindRetrieval, QuestKindEscort,
		QuestKindSiege, QuestKindMystery, QuestKindBetrayal} {
		for b := 0; b <= MaxQuestBranchPoints; b++ {
			for d := b + 1; d <= MaxQuestDepth; d++ {
				if d < MinQuestDepth {
					continue
				}
				topo, err := BuildQuestTopology(kind, b, d)
				if err != nil {
					t.Fatalf("%s b=%d d=%d: %v", kind, b, d, err)
				}
				if len(topo.Forks) != b {
					t.Fatalf("%s b=%d d=%d: forks = %d, want %d", kind, b, d, len(topo.Forks), b)
				}
				// The fork count is a property of the machine, not of the
				// list: exactly b states with two outgoing edges, and no
				// state with more.
				m := machineOf(&topo)
				outgoing := map[string]int{}
				for _, e := range m.Edges {
					outgoing[e.From]++
				}
				two := 0
				for _, st := range m.States {
					switch {
					case outgoing[st.Key] == 2:
						two++
					case outgoing[st.Key] > 2:
						t.Fatalf("%s b=%d d=%d: state %q has %d outgoing edges", kind, b, d, st.Key, outgoing[st.Key])
					}
				}
				if two != b {
					t.Fatalf("%s b=%d d=%d: states with two outgoing edges = %d, want %d", kind, b, d, two, b)
				}
			}
		}
	}
}

func TestQuestTopology_ForksAreExclusive(t *testing.T) {
	for _, kind := range []string{QuestKindInvestigation, QuestKindMystery, QuestKindBetrayal} {
		for b := 1; b <= MaxQuestBranchPoints; b++ {
			topo, err := BuildQuestTopology(kind, b, 6)
			if err != nil {
				t.Fatalf("%s b=%d: %v", kind, b, err)
			}
			m := machineOf(&topo)
			if problems := ForksExclusive(m); len(problems) > 0 {
				t.Fatalf("%s b=%d: forks not exclusive: %v", kind, b, problems)
			}
			// And asserted on the graph itself, not just through the
			// helper: no path from either arm of a fork to the other.
			// Rejoining downstream is the point — the arms converge on
			// the trail again, never on each other.
			arms := map[string][]string{}
			for _, e := range m.Edges {
				arms[e.From] = append(arms[e.From], e.To)
			}
			for _, fork := range topo.Forks {
				a, b2 := arms[fork][0], arms[fork][1]
				ra, rb := m.Reachable(a), m.Reachable(b2)
				if ra[b2] {
					t.Fatalf("%s b=%d: fork %q arm %q reaches its sibling %q", kind, b, fork, a, b2)
				}
				if rb[a] {
					t.Fatalf("%s b=%d: fork %q arm %q reaches its sibling %q", kind, b, fork, b2, a)
				}
			}
		}
	}
}

func TestQuestTopology_PassesMachineAndQuestChecks(t *testing.T) {
	for _, kind := range QuestShapes() {
		for b := 0; b <= MaxQuestBranchPoints; b++ {
			topo, err := BuildQuestTopology(kind.Key, b, 5)
			if err != nil {
				t.Fatalf("%s b=%d: %v", kind.Key, b, err)
			}
			m := machineOf(&topo)
			if err := m.Validate(); err != nil {
				t.Fatalf("%s b=%d: machine invalid: %v", kind.Key, b, err)
			}
			if findings := questFindings(m); len(findings) > 0 {
				t.Fatalf("%s b=%d: quest checks fired: %+v", kind.Key, b, findings)
			}
		}
	}
}

func TestQuestTopology_StateAndEndingCounts(t *testing.T) {
	cases := []struct {
		kind                string
		b, d                int
		states, endings     int
		abandoned, failures int
	}{
		// states = depth + 2B + endings; endings = 1 when b=0, else
		// 2 + the shape's detached ending when an inner fork exists.
		{QuestKindInvestigation, 0, 4, 5, 1, 0, 0},
		{QuestKindInvestigation, 2, 4, 10, 2, 0, 1},
		{QuestKindRetrieval, 3, 5, 13, 2, 0, 1},
		{QuestKindMystery, 1, 3, 7, 2, 0, 1},   // no inner fork: red herring unfunded
		{QuestKindMystery, 2, 4, 11, 3, 1, 1},  // the cold trail exists
		{QuestKindBetrayal, 2, 4, 11, 3, 0, 2}, // the betrayal ending exists
		{QuestKindSiege, 4, 8, 18, 2, 0, 1},
	}
	for _, c := range cases {
		topo, err := BuildQuestTopology(c.kind, c.b, c.d)
		if err != nil {
			t.Fatalf("%s b=%d d=%d: %v", c.kind, c.b, c.d, err)
		}
		if len(topo.States) != c.states {
			t.Fatalf("%s b=%d d=%d: states = %d, want %d", c.kind, c.b, c.d, len(topo.States), c.states)
		}
		endings := topo.Endings()
		if len(endings) != c.endings {
			t.Fatalf("%s b=%d d=%d: endings = %d, want %d", c.kind, c.b, c.d, len(endings), c.endings)
		}
		var abandoned, failed int
		for _, e := range endings {
			switch e.Terminal {
			case TerminalAbandoned:
				abandoned++
			case TerminalFailure:
				failed++
			}
		}
		if abandoned != c.abandoned {
			t.Fatalf("%s b=%d d=%d: abandoned endings = %d, want %d", c.kind, c.b, c.d, abandoned, c.abandoned)
		}
		if failed != c.failures {
			t.Fatalf("%s b=%d d=%d: failure endings = %d, want %d", c.kind, c.b, c.d, failed, c.failures)
		}
	}
}

func TestQuestTopology_RevealSlots(t *testing.T) {
	topo, err := BuildQuestTopology(QuestKindInvestigation, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	// Two forks, four arms: every arm is a clue on the path.
	if got := len(topo.RevealSlots()); got != 4 {
		t.Fatalf("reveal slots = %d, want 4", got)
	}
	straight, err := BuildQuestTopology(QuestKindRetrieval, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(straight.RevealSlots()); got != 1 {
		t.Fatalf("straight-line reveal slots = %d, want 1 (the last beat)", got)
	}
}

func TestQuestTopology_Bounds(t *testing.T) {
	for _, bad := range []struct {
		kind string
		b, d int
		want string
	}{
		{"musical", 1, 4, "not a quest shape"},
		{QuestKindSiege, -1, 4, "outside 0-4"},
		{QuestKindSiege, 5, 8, "outside 0-4"},
		{QuestKindSiege, 1, 1, "outside 2-8"},
		{QuestKindSiege, 1, 9, "outside 2-8"},
		{QuestKindSiege, 3, 3, "depth of at least 4"},
	} {
		_, err := BuildQuestTopology(bad.kind, bad.b, bad.d)
		if err == nil || !contains([]string{fmt.Sprint(err)}, bad.want) && !hasSubstr(err.Error(), bad.want) {
			t.Fatalf("%s b=%d d=%d: err = %v, want it to mention %q", bad.kind, bad.b, bad.d, err, bad.want)
		}
	}
}

func hasSubstr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestQuestStateKey(t *testing.T) {
	cases := map[string]string{
		"Find the Survivor":        "find-the-survivor",
		"  Accuse! (the steward) ": "accuse-the-steward",
		"The Duke's Men":           "the-duke-s-men",
		"!!!":                      "",
		"Trust":                    "trust",
	}
	for in, want := range cases {
		if got := QuestStateKey(in); got != want {
			t.Errorf("QuestStateKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultQuestKindFor(t *testing.T) {
	cases := map[string]string{
		QuestModeInvestigate: QuestKindInvestigation,
		QuestModeFight:       QuestKindSiege,
		QuestModeBetray:      QuestKindBetrayal,
		"":                   QuestKindRetrieval,
	}
	for mode, want := range cases {
		if got := DefaultQuestKindFor(mode); got != want {
			t.Errorf("DefaultQuestKindFor(%q) = %q, want %q", mode, got, want)
		}
	}
}
