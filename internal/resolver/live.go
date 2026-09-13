package resolver

// The live-state half of the rules judge (MAD-333, stage 5 of MAD-321):
// substitution of the resolver's input, not a rewrite. The folded engine
// state — board, stack, priority, step — becomes the Board/Sequence/Note
// the existing prompt assembly and citation behaviour already consume, so
// a question asked mid-game is answered against the real table with no
// board re-entry. Anything the table does not honestly know (hand
// contents, library order) is rendered as unknown rather than guessed;
// the resolver's standing honesty rules do the rest.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// FromGame builds the resolver's Input from the live folded state. The
// battlefield becomes the Board; the waiting triggers plus the stack
// (top first — the order they resolve) become the Sequence; the Note
// carries the live position, the players' visible numbers, the asking
// seat's known-in-hand cards, and the question itself. A nil state (a
// game with no log yet) still carries the question through the Note.
func FromGame(st *engine.State, asker int, question string) Input {
	question = strings.TrimSpace(question)
	if st == nil {
		return Input{Note: "QUESTION: " + question}
	}
	names := seatNames(st)
	return Input{
		Board:    Board{Permanents: liveBoard(st, names)},
		Sequence: Sequence{Steps: liveSequence(st, names)},
		Note:     liveNote(st, names, asker, question),
	}
}

/* ---------- the board ---------- */

// liveBoard renders every unphased battlefield object in arrival order —
// ids are minted monotonically, so id order is also timestamp order, the
// ordering the layer walk in the prompt cares about.
func liveBoard(st *engine.State, names map[int]string) []Permanent {
	var out []Permanent
	for id := int64(1); id < st.NextObject; id++ {
		o, ok := st.Objects[id]
		if !ok || o.Zone != engine.ZoneBattlefield || o.Phased {
			continue
		}
		out = append(out, Permanent{
			Name:       objectLabel(o),
			Controller: names[o.Controller],
			Tapped:     o.Tapped,
			Counters:   countersLine(o.Counters),
			Note:       objectNote(st, o),
		})
	}
	return out
}

// objectLabel names an object the way the table does: its card, its token
// spec, or an honest "unknown card" — never a guessed name.
func objectLabel(o *engine.Object) string {
	if o.Identity.Card != "" {
		return o.Identity.Card
	}
	if o.Identity.Token != nil && o.Identity.Token.Name != "" {
		return o.Identity.Token.Name + " token"
	}
	return "unknown card"
}

// countersLine renders an object's counters compactly ("+1/+1×2, shield 1"),
// zero counts skipped, names sorted so the line is deterministic.
func countersLine(counters map[string]int) string {
	var parts []string
	for _, name := range sortedCounterKeys(counters) {
		if n := counters[name]; n != 0 {
			parts = append(parts, fmt.Sprintf("%s×%d", name, n))
		}
	}
	return strings.Join(parts, ", ")
}

// objectNote carries what a judge question actually keys on: the computed
// P/T and keywords (the layer walk, already done), attachments in both
// directions, and marked damage. All optional; all joined compactly.
func objectNote(st *engine.State, o *engine.Object) string {
	var parts []string
	c := st.Characteristics(o.ID)
	if c.Power != nil || c.Toughness != nil {
		parts = append(parts, fmt.Sprintf("%s/%s", intOr(c.Power), intOr(c.Toughness)))
	}
	if len(c.Keywords) > 0 {
		kw := c.Keywords
		if len(kw) > 6 {
			kw = kw[:6]
		}
		parts = append(parts, strings.Join(kw, " "))
	}
	if o.AttachedTo != 0 {
		if t, ok := st.Objects[o.AttachedTo]; ok {
			parts = append(parts, "attached to "+objectLabel(t))
		}
	}
	if len(o.Attachments) > 0 {
		var labels []string
		for _, id := range o.Attachments {
			if a, ok := st.Objects[id]; ok {
				labels = append(labels, objectLabel(a))
			}
		}
		if len(labels) > 0 {
			parts = append(parts, "carries "+strings.Join(labels, ", "))
		}
	}
	if o.Damage > 0 {
		parts = append(parts, fmt.Sprintf("%d damage marked", o.Damage))
	}
	return strings.Join(parts, ", ")
}

/* ---------- the sequence ---------- */

// liveSequence renders what is pending, in resolution order: waiting
// triggers first (they go on the stack above everything else at the next
// priority grant, CR 603.3), then the current stack top-down — so the
// sequence order reads as "what happens next, in order".
func liveSequence(st *engine.State, names map[int]string) []Step {
	var steps []Step
	for _, t := range st.TriggerQueue {
		text := "waiting trigger — " + triggerLabel(st, t)
		if t.Effect != "" {
			text += ": " + t.Effect
		}
		text += " (goes on top of the stack when priority is next granted)"
		steps = append(steps, Step{Controller: names[t.Controller], Text: text})
	}
	for i := len(st.Stack) - 1; i >= 0; i-- {
		it := st.Stack[i]
		steps = append(steps, Step{Controller: names[it.Controller], Text: stackLine(st, it, names)})
	}
	return steps
}

// triggerLabel names a waiting trigger by its source card or object.
func triggerLabel(st *engine.State, t engine.TriggerItem) string {
	if t.Card != "" {
		return t.Card
	}
	if t.SourceObj != 0 {
		if o, ok := st.Objects[t.SourceObj]; ok {
			return objectLabel(o)
		}
	}
	return "a triggered ability"
}

// stackLine spells one stack object the way the table would: what it is,
// who controls it, what it targets. Targets resolve to seat and object
// names so the model never has to guess what "object 12" is.
func stackLine(st *engine.State, it engine.StackItem, names map[int]string) string {
	var what string
	switch it.Mode {
	case "cast":
		what = "casts " + stackName(st, it)
	case "activated":
		what = "activates " + it.Ability
		if it.Card != "" {
			what = "activates " + it.Card + " — " + it.Ability
		}
	default:
		what = "has a triggered ability on the stack — " + stackName(st, it)
	}
	if ts := targetsLine(st, it.Targets, names); ts != "" {
		what += " targeting " + ts
	}
	return what
}

// stackName resolves a stack item to its card name — the declared card, or
// the object it was minted from.
func stackName(st *engine.State, it engine.StackItem) string {
	if it.Card != "" {
		return it.Card
	}
	if it.Object != 0 {
		if o, ok := st.Objects[it.Object]; ok {
			return objectLabel(o)
		}
	}
	return "a spell"
}

// targetsLine renders declared targets: seat names, object names, or
// named cards, comma-joined.
func targetsLine(st *engine.State, targets []engine.Target, names map[int]string) string {
	var parts []string
	for _, t := range targets {
		switch {
		case t.Seat != 0:
			parts = append(parts, names[t.Seat])
		case t.Object != 0:
			if o, ok := st.Objects[t.Object]; ok {
				parts = append(parts, objectLabel(o))
			} else {
				parts = append(parts, fmt.Sprintf("object %d", t.Object))
			}
		case t.Card != "":
			parts = append(parts, t.Card)
		}
	}
	return strings.Join(parts, ", ")
}

/* ---------- the note: position, players, the question ---------- */

// liveNote assembles the free-text context: the live position (turn, step,
// priority holder — the facts that disambiguate "can I respond"), the
// players' visible numbers, the tracked zones beyond the battlefield, the
// asking seat's known-in-hand cards, and the question itself.
func liveNote(st *engine.State, names map[int]string, asker int, question string) string {
	var b strings.Builder
	b.WriteString("This board and stack are the LIVE game state, folded from the game's event log — treat them as authoritative for what is where and what is pending.\n")

	if st.Status == engine.StatusActive {
		fmt.Fprintf(&b, "Live position: turn %d, %s's turn, %s.", st.Turn, names[st.TurnSeat], stepLabel(st.Phase, st.Step))
		if st.PrioritySeat != 0 {
			fmt.Fprintf(&b, " %s holds priority.", names[st.PrioritySeat])
		} else {
			b.WriteString(" No player holds priority (untap or cleanup).")
		}
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "The game is %s.\n", st.Status)
	}

	for _, seat := range seatOrder(st) {
		p := st.Seats[seat]
		if p == nil {
			continue
		}
		line := names[seat]
		if p.Alive {
			line += fmt.Sprintf(": %d life", p.Life)
		} else {
			line += ": eliminated"
		}
		for _, name := range sortedCounterKeys(p.Counters) {
			if n := p.Counters[name]; n != 0 {
				line += fmt.Sprintf(", %s %d", name, n)
			}
		}
		line += fmt.Sprintf(", hand %s, library %s", countWord(p.Hand), countWord(p.Library))
		if p.Commander != "" {
			line += ", commander: " + p.Commander
		}
		b.WriteString(line + "\n")
		// Tracked zones beyond the battlefield, when they hold anything.
		for _, zone := range []engine.Zone{engine.ZoneGraveyard, engine.ZoneExile, engine.ZoneCommand} {
			objs := st.ZoneObjects(seat, zone)
			if len(objs) == 0 {
				continue
			}
			b.WriteString(zoneTally(names[seat], string(zone), objs))
		}
	}

	// The asking seat's own spoken draws: knowledge, not contents — the
	// hand stays count-only, and the prompt must never pretend otherwise.
	if p := st.Seats[asker]; p != nil && len(p.HandKnown) > 0 {
		fmt.Fprintf(&b, "%s's cards known to be in hand (spoken or revealed only): %s.\n",
			names[asker], strings.Join(p.HandKnown, ", "))
	}
	b.WriteString("Hand contents beyond that and library order are NOT tracked — say so plainly rather than guessing.\n")

	if question != "" {
		b.WriteString("QUESTION: " + question)
	}
	return strings.TrimRight(b.String(), "\n")
}

// zoneTally collapses a tracked zone's objects into one line, identical
// names collapsed to a count: "Bob's graveyard: Island×2, Sol Ring."
func zoneTally(seat, zone string, objs []*engine.Object) string {
	counts := map[string]int{}
	var order []string
	for _, o := range objs {
		name := objectLabel(o)
		if counts[name] == 0 {
			order = append(order, name)
		}
		counts[name]++
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		if counts[name] > 1 {
			parts = append(parts, fmt.Sprintf("%s×%d", name, counts[name]))
		} else {
			parts = append(parts, name)
		}
	}
	return fmt.Sprintf("%s's %s: %s.\n", seat, zone, strings.Join(parts, ", "))
}

/* ---------- small helpers ---------- */

// stepLabels mirrors the play surface's step spelling so every pane and
// every prompt agree on what "precombat main" means.
var stepLabels = map[string]string{
	"beginning|untap":            "untap step",
	"beginning|upkeep":           "upkeep",
	"beginning|draw":             "draw step",
	"precombat_main|main":        "precombat main phase",
	"combat|beginning_of_combat": "beginning of combat",
	"combat|declare_attackers":   "declare attackers",
	"combat|declare_blockers":    "declare blockers",
	"combat|combat_damage":       "combat damage",
	"combat|end_of_combat":       "end of combat",
	"postcombat_main|main":       "postcombat main phase",
	"end|end":                    "end step",
	"end|cleanup":                "cleanup",
}

func stepLabel(phase, step string) string {
	if label, ok := stepLabels[phase+"|"+step]; ok {
		return label
	}
	if step != "" {
		return step
	}
	return phase
}

// seatNames maps every seat to its display name — the seated name, else
// "Seat N", the same spelling the play surface shows.
func seatNames(st *engine.State) map[int]string {
	out := map[int]string{}
	for seat, p := range st.Seats {
		if p != nil && p.Name != "" {
			out[seat] = p.Name
		} else {
			out[seat] = fmt.Sprintf("Seat %d", seat)
		}
	}
	return out
}

// seatOrder is turn order when the game runs, else ascending seats.
func seatOrder(st *engine.State) []int {
	if len(st.Order) > 0 {
		return st.Order
	}
	seats := make([]int, 0, len(st.Seats))
	for seat := range st.Seats {
		seats = append(seats, seat)
	}
	sort.Ints(seats)
	return seats
}

func sortedCounterKeys(m map[string]int) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// countWord spells a count the note can say: the number when known, an
// honest "?" when not — unknown is a value, never zero.
func countWord(c engine.Count) string {
	if c.Known {
		return fmt.Sprintf("%d", c.N)
	}
	return "?"
}

func intOr(v *int) string {
	if v == nil {
		return "?"
	}
	return fmt.Sprintf("%d", *v)
}
