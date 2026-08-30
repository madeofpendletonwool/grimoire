package campaign

// Quest shapes (MAD-371): the deterministic topology a quest designer fills.
// The direct analogue of encounter.Shape and story.Shape applied to a quest:
// a handful of honest quest shapes — investigation, retrieval, escort, siege,
// mystery with a red herring, betrayal — each one fixing the state count,
// where the forks are, which branches rejoin and which run to their own
// ending, and how many terminal states of each kind exist. The model names
// states and writes their detail; it does not decide how many branches there
// are. A fork is exclusive by construction — the two edges out of a fork
// lead to states with no path back to each other, a property of the built
// graph that ForksExclusive checks before any batch is staged.

import (
	"fmt"
	"sort"
	"strings"
)

/* ---------- the shape catalogue ---------- */

// Quest shape kinds.
const (
	QuestKindInvestigation = "investigation"
	QuestKindRetrieval     = "retrieval"
	QuestKindEscort        = "escort"
	QuestKindSiege         = "siege"
	QuestKindMystery       = "mystery"
	QuestKindBetrayal      = "betrayal"
)

// QuestShape is one honest quest shape: what a quest of this kind is for.
type QuestShape struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Purpose string `json:"purpose"`
}

// questShapes is the catalogue, in the fixed order the generator offers it.
var questShapes = []QuestShape{
	{Key: QuestKindInvestigation, Label: "Investigation",
		Purpose: "A question with teeth: the party pulls threads, and every fork is which thread they pull."},
	{Key: QuestKindRetrieval, Label: "Retrieval",
		Purpose: "Something is lost, stolen or hoarded; every fork is how the party goes after it."},
	{Key: QuestKindEscort, Label: "Escort",
		Purpose: "Someone or something must get from here to there alive; every fork is the road's next bad choice."},
	{Key: QuestKindSiege, Label: "Siege",
		Purpose: "A place to take or hold; every fork is where the party commits its strength."},
	{Key: QuestKindMystery, Label: "Mystery with a red herring",
		Purpose: "A question where one trail is bait: the wrong fork runs cold onto an ending of its own."},
	{Key: QuestKindBetrayal, Label: "Betrayal",
		Purpose: "An ally is false; one fork trusts them and pays for it early."},
}

// QuestShapes lists every legal quest shape.
func QuestShapes() []QuestShape {
	out := make([]QuestShape, len(questShapes))
	copy(out, questShapes)
	return out
}

// QuestShapeFor returns the shape for a kind key. The second return is false
// for an unknown or empty kind.
func QuestShapeFor(kind string) (QuestShape, bool) {
	for _, sh := range questShapes {
		if sh.Key == kind {
			return sh, true
		}
	}
	return QuestShape{}, false
}

// DefaultQuestKindFor picks the shape a mode implies: an investigative mode
// is an investigation, a fight is a siege, a betrayal is a betrayal; anything
// else is a retrieval, the workaday quest.
func DefaultQuestKindFor(mode string) string {
	switch mode {
	case QuestModeInvestigate:
		return QuestKindInvestigation
	case QuestModeFight:
		return QuestKindSiege
	case QuestModeBetray:
		return QuestKindBetrayal
	}
	return QuestKindRetrieval
}

// Quest modes — the coarse signal a hook deterministically implies, before
// any model is asked. ReadHook's output, and the default-kind input.
const (
	QuestModeInvestigate = "investigate"
	QuestModeFight       = "fight"
	QuestModeBetray      = "betrayal"
)

/* ---------- topology bounds ---------- */

// Input bounds. Branch points cap at four because a machine past four forks
// is a flowchart, not a quest; depth caps at eight because a quest past
// eight beats on one thread is a campaign.
const (
	MaxQuestBranchPoints = 4
	MinQuestDepth        = 2
	MaxQuestDepth        = 8
)

/* ---------- the topology ---------- */

// Slot roles. A spine slot is one beat on the main path; a branch slot is
// one arm of a fork; an ending slot is terminal.
const (
	QuestSlotSpine  = "spine"
	QuestSlotBranch = "branch"
	QuestSlotEnding = "ending"
)

// QuestSlot is one state of the topology before the model names it: its
// stable internal id, its role, which fork it belongs to (branch slots),
// its terminal marker (ending slots) and whether it sits on the clue path —
// a reveal slot is where a secret surfaces, which is what makes a quest a
// path to a secret rather than a decoration on one.
type QuestSlot struct {
	ID       string `json:"id"`
	Role     string `json:"role"`
	Brief    string `json:"brief"`
	Fork     int    `json:"fork"`               // 1-based; 0 when the slot is no fork arm
	Arm      string `json:"arm,omitempty"`      // a | b for branch slots
	Terminal string `json:"terminal,omitempty"` // ending slots only
	Reveal   bool   `json:"reveal,omitempty"`
	KeyHint  string `json:"key_hint"`
}

// QuestEdgeSlot is one declared move of the topology, between internal ids.
type QuestEdgeSlot struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// QuestTopology is a quest's whole shape as arithmetic: the slots, the
// declared moves, the initial state, and which slots are forks. IDs are
// stable within one build — the generator hands them to the model and maps
// the names it gets back onto them.
type QuestTopology struct {
	Kind         string          `json:"kind"`
	BranchPoints int             `json:"branch_points"`
	Depth        int             `json:"depth"`
	Initial      string          `json:"initial"`
	States       []QuestSlot     `json:"states"`
	Edges        []QuestEdgeSlot `json:"edges"`
	Forks        []string        `json:"forks"` // internal ids of the fork states, fork order
}

// RevealSlots lists the clue-path slots in declaration order.
func (t QuestTopology) RevealSlots() []QuestSlot {
	var out []QuestSlot
	for _, s := range t.States {
		if s.Reveal {
			out = append(out, s)
		}
	}
	return out
}

// Endings lists the terminal slots.
func (t QuestTopology) Endings() []QuestSlot {
	var out []QuestSlot
	for _, s := range t.States {
		if s.Role == QuestSlotEnding {
			out = append(out, s)
		}
	}
	return out
}

// endingIDs for the kind's detach arm, when the shape detaches one.
const (
	endingSuccess  = "ending-success"
	endingFailure  = "ending-failure"
	endingCold     = "ending-cold"
	endingBetrayed = "ending-betrayed"
)

// BuildQuestTopology fixes the machine's shape: a spine of depth beats, the
// last of which carries the final fork when there is one; branchPoints-1
// inner forks spread along the spine; every inner fork's arms rejoin the
// spine except the one arm the shape detaches onto an ending of its own.
// Nothing the model returns can change a count, a position or an edge.
func BuildQuestTopology(kind string, branchPoints, depth int) (QuestTopology, error) {
	_, ok := QuestShapeFor(kind)
	if !ok {
		return QuestTopology{}, fmt.Errorf("%w: %q is not a quest shape — the shapes are investigation, retrieval, escort, siege, mystery and betrayal", ErrInvalid, kind)
	}
	if branchPoints < 0 || branchPoints > MaxQuestBranchPoints {
		return QuestTopology{}, fmt.Errorf("%w: %d branch points is outside 0-%d", ErrInvalid, branchPoints, MaxQuestBranchPoints)
	}
	if depth < MinQuestDepth || depth > MaxQuestDepth {
		return QuestTopology{}, fmt.Errorf("%w: depth %d is outside %d-%d", ErrInvalid, depth, MinQuestDepth, MaxQuestDepth)
	}
	// Each fork needs its own spine state: the final fork sits on the last
	// beat, the inner forks on distinct earlier ones.
	if depth < branchPoints+1 {
		return QuestTopology{}, fmt.Errorf("%w: %d branch points need depth of at least %d (one spine state per fork)", ErrInvalid, branchPoints, branchPoints+1)
	}

	t := QuestTopology{Kind: kind, BranchPoints: branchPoints, Depth: depth, Initial: "beat-1"}

	// The spine: depth beats, the quest's main path.
	spineBrief := func(i int) string {
		switch {
		case i == 0:
			return "The hook lands: the quest is offered, accepted or forced on the party."
		case i == depth-1:
			if branchPoints > 0 {
				return "The confrontation: the final fork, where the quest's outcome is chosen."
			}
			return "The payoff: where the quest resolves."
		default:
			return "The trail: a beat on the main path, the situation tightening."
		}
	}
	for i := 0; i < depth; i++ {
		t.States = append(t.States, QuestSlot{
			ID: fmt.Sprintf("beat-%d", i+1), Role: QuestSlotSpine,
			Brief: spineBrief(i), KeyHint: fmt.Sprintf("beat-%d", i+1),
		})
	}
	beat := func(i int) string { return t.States[i].ID } // spine ids are declaration order

	// The endings the shape's branch count sustains. With no fork at all the
	// spine runs straight to its success; the detached endings only exist
	// when an inner fork exists to detach.
	endings := []QuestSlot{{ID: endingSuccess, Role: QuestSlotEnding, Terminal: TerminalSuccess,
		Brief: "Success: the quest pays off.", KeyHint: "triumph"}}
	if branchPoints > 0 {
		endings = append(endings, QuestSlot{ID: endingFailure, Role: QuestSlotEnding, Terminal: TerminalFailure,
			Brief: "Failure: the quest is lost.", KeyHint: "disaster"})
	}
	detach := kindDetaches(kind) && branchPoints >= 2
	if detach {
		switch kind {
		case QuestKindMystery:
			endings = append(endings, QuestSlot{ID: endingCold, Role: QuestSlotEnding, Terminal: TerminalAbandoned,
				Brief: "The cold trail: the red herring's own ending, the question left unanswered.", KeyHint: "case-goes-cold"})
		case QuestKindBetrayal:
			endings = append(endings, QuestSlot{ID: endingBetrayed, Role: QuestSlotEnding, Terminal: TerminalFailure,
				Brief: "The betrayal lands: the trusted ally walks away with it.", KeyHint: "betrayed"})
		}
	}
	t.States = append(t.States, endings...)

	// The final fork: the last beat chooses between its two arms, and the
	// arms run to the endings — genuinely exclusive, by construction.
	if branchPoints > 0 {
		forkID := beat(depth - 1)
		t.Forks = append(t.Forks, forkID)
		for arm, target := range map[string]string{"a": endingSuccess, "b": endingFailure} {
			id := fmt.Sprintf("fork-%d-%s", branchPoints, arm)
			t.States = append(t.States, QuestSlot{
				ID: id, Role: QuestSlotBranch, Fork: branchPoints, Arm: arm, Reveal: true,
				Brief:   fmt.Sprintf("The final fork's %s outcome, off %q.", armLabel(arm), forkID),
				KeyHint: fmt.Sprintf("final-%s", arm),
			})
			t.Edges = append(t.Edges, QuestEdgeSlot{From: forkID, To: id}, QuestEdgeSlot{From: id, To: target})
		}
	} else {
		// A straight quest still reveals its secret on the last beat.
		t.Edges = append(t.Edges, QuestEdgeSlot{From: beat(depth - 1), To: endingSuccess})
		t.States[depth-1].Reveal = true
	}

	// The inner forks, spread along the spine. Each fork state's forward
	// spine edge is replaced by its two arms; the arms rejoin the spine at
	// the next beat — except the detached arm, which runs to its own ending.
	inner := branchPoints - 1
	if inner > 0 {
		at := func(i int) int { // spine index of inner fork i, spread over 1..depth-2
			return 1 + (i*(depth-2))/inner
		}
		for i := 0; i < inner; i++ {
			forkID := beat(at(i))
			join := beat(at(i) + 1)
			t.Forks = append(t.Forks, forkID)
			for _, arm := range []string{"a", "b"} {
				id := fmt.Sprintf("fork-%d-%s", i+1, arm)
				detached := detach && i == 0 && detachedArm(kind) == arm
				brief := fmt.Sprintf("Fork %d's %s branch off %q; it rejoins the trail at %q.", i+1, armLabel(arm), forkID, join)
				var target string
				if detached {
					target = endingForDetached(kind)
					brief = fmt.Sprintf("Fork %d's %s branch off %q; it does not rejoin — it runs to its own ending.", i+1, armLabel(arm), forkID)
				} else {
					target = join
				}
				t.States = append(t.States, QuestSlot{
					ID: id, Role: QuestSlotBranch, Fork: i + 1, Arm: arm, Reveal: true,
					Brief: brief, KeyHint: fmt.Sprintf("fork-%d-%s", i+1, arm),
				})
				t.Edges = append(t.Edges, QuestEdgeSlot{From: forkID, To: id}, QuestEdgeSlot{From: id, To: target})
			}
		}
		// The spine's forward edges, minus the states a fork replaced.
		for i := 0; i < depth-1; i++ {
			if t.isFork(beat(i)) {
				continue
			}
			t.Edges = append(t.Edges, QuestEdgeSlot{From: beat(i), To: beat(i + 1)})
		}
	} else {
		for i := 0; i < depth-1; i++ {
			t.Edges = append(t.Edges, QuestEdgeSlot{From: beat(i), To: beat(i + 1)})
		}
	}

	// Stable edge order: fork order, then spine order.
	sort.SliceStable(t.Edges, func(i, j int) bool {
		return t.Edges[i].From < t.Edges[j].From
	})
	return t, nil
}

func (t QuestTopology) isFork(id string) bool {
	for _, f := range t.Forks {
		if f == id {
			return true
		}
	}
	return false
}

// kindDetaches reports whether the shape detaches an inner fork's arm onto
// an ending of its own: the mystery's red herring, the betrayal's trusting
// arm. Every other branch rejoins the trail.
func kindDetaches(kind string) bool {
	return kind == QuestKindMystery || kind == QuestKindBetrayal
}

func detachedArm(kind string) string {
	if kind == QuestKindMystery {
		return "b" // the red herring
	}
	return "a" // the trusting arm
}

func endingForDetached(kind string) string {
	if kind == QuestKindMystery {
		return endingCold
	}
	return endingBetrayed
}

func armLabel(arm string) string {
	if arm == "a" {
		return "first"
	}
	return "second"
}

/* ---------- keys and exclusivity ---------- */

// stateKeySanitize turns a model-named state name into its machine key: the
// name lowercased, everything that is not a letter or digit collapsed to a
// single dash. Empty in, empty out — the generator's validation treats that
// as a problem the model repairs.
func stateKeySanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := true // leading dashes are dropped
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// QuestStateKey exposes the slug rule to the generator and its tests.
func QuestStateKey(name string) string {
	return stateKeySanitize(name)
}

// ForksExclusive checks every fork of a machine: a state with two or more
// outgoing edges is a fork, and the branches it offers must be genuinely
// exclusive — no path from one branch's entry to the other's, either way.
// Every problem is a string a repair prompt can quote; an empty return
// means every fork is exclusive.
func ForksExclusive(m StateMachine) []string {
	targets := map[string][]string{}
	for _, e := range m.Edges {
		if !contains(targets[e.From], e.To) {
			targets[e.From] = append(targets[e.From], e.To)
		}
	}
	var forks []string
	for _, st := range m.States {
		if len(targets[st.Key]) >= 2 {
			forks = append(forks, st.Key)
		}
	}
	sort.Strings(forks)
	var problems []string
	for _, f := range forks {
		arms := targets[f]
		for i := 0; i < len(arms); i++ {
			for j := i + 1; j < len(arms); j++ {
				a, b := arms[i], arms[j]
				if a == b {
					continue
				}
				ra, rb := m.Reachable(a), m.Reachable(b)
				if ra[b] || rb[a] {
					problems = append(problems, fmt.Sprintf(
						"fork %q: branches %q and %q can reach each other; the two outcomes of a fork must be exclusive", f, a, b))
				}
			}
		}
	}
	return problems
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
