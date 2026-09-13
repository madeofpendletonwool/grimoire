package intent

// The fallback's prompt (MAD-331): the current game state and the
// known-card universe, and one utterance the deterministic grammar
// refused. The model's whole job is to emit ONE typed action in a small
// JSON shape the gate then validates against the state — the model
// proposes, the deterministic lookups dispose. ADR 10's line: the
// engine is the authority, the LLM is a parser.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

// knownCardsCap bounds the card list the prompt carries. Four Commander
// decks run ~400 unique names; past that the list stops helping
// identification and starts costing tokens, and the model's answer
// should not depend on seat 4's fifteenth land anyway.
const knownCardsCap = 400

// SystemPrompt is the standing instruction: one action, fenced JSON,
// names from the list or the utterance, NONE when unsure. The prompt
// requests; the gate enforces. Every constraint here has a gate check
// behind it, because a prompt is a suggestion and the gate is the
// product.
func SystemPrompt() string {
	var b strings.Builder
	b.WriteString(`You are the intent parser at a Magic: the Gathering table. The table's deterministic grammar could not parse what a player just said. Your only job: decide whether it is exactly ONE game action, and if so emit it as typed JSON the engine can apply.

Reply with exactly one fenced json block and nothing else — no prose, no explanation.

THE SHAPE:
` + "```json\n{\"kind\": \"...\", \"confidence\": 0.0, ...kind fields}\n```" + `

kinds and their fields:
- {"kind":"PASS_PRIORITY"} — pass priority
- {"kind":"ADVANCE"} — next step or phase
- {"kind":"CONCEDE"} — the acting player concedes
- {"kind":"CAST","card":"..."} — cast a spell; card required
- {"kind":"PLAY_LAND","card":"..."} — land drop; card optional when unnamed
- {"kind":"DRAW","count":N} — draw; count defaults to 1
- {"kind":"MILL","target":"<seat name>","count":N}
- {"kind":"CHANGE_LIFE","target":"<seat name>","delta":-3,"card":"<source, optional>"}
- {"kind":"DEAL_DAMAGE","target":"<seat name>","amount":3,"card":"<source, optional>"}
- {"kind":"TAP","object":"<battlefield card name>"} / {"kind":"UNTAP","object":"..."}
- {"kind":"CREATE_TOKEN","count":N,"token":{"name":"Soldier","power":1,"toughness":1}}
- {"kind":"MOVE_ZONE","object":"<battlefield card name>","cause":"sacrifice|destroy|bounce|exile"}
- {"kind":"ADJUST_COUNTERS","on":"<battlefield card name>","counter":"+1/+1","delta":N}
- {"kind":"ADJUST_COUNTERS","target":"<seat name>","counter":"energy","delta":N}
- {"kind":"SET_FLAG","target":"<seat name>","flag":"monarch|initiative|city's blessing"}
- {"kind":"NONE"} — the honest refusal

RULES YOU MAY NOT BREAK:
1. When the utterance is not exactly one of these actions — a question, table chatter, a rules discussion, several actions at once — emit {"kind":"NONE"}. A wrong action is worse than none: wrong boards are only fixed when someone notices.
2. Card names come from the KNOWN CARDS list, or are a short span copied verbatim from the utterance. Never invent a name, never complete a partial one beyond that list.
3. "target" and "object" name things exactly as the game state lists them (seat names, battlefield card names). If the name is not there, emit NONE rather than approximating.
4. Numbers are literal integers; a spoken number word ("six") is its digit.
5. "confidence" is 0.0 to 1.0 — your read on the mapping. Say what you believe; the table will not apply a guess silently either way.
6. Emit exactly one action. If the utterance names several, pick NONE.`)
	return b.String()
}

// UserMessage assembles the prompt turn: the state, the known cards,
// then the utterance. Deterministic order throughout so the same table
// renders the same prompt. This is the exact text the model receives;
// it stays pure so tests can read it.
func UserMessage(st *engine.State, seat int, utterance string, u *universe.Universe) string {
	var b strings.Builder
	b.WriteString("=== THE TABLE ===\n")
	fmt.Fprintf(&b, "%s\n", stateLine(st))
	writeSeats(&b, st)
	writeBattlefield(&b, st)
	writeStack(&b, st)
	b.WriteString("\n=== KNOWN CARDS ===\n")
	b.WriteString(knownCards(st, u, seat))
	b.WriteString("\n=== WHAT WAS SAID ===\n")
	who := seatName(st, seat)
	fmt.Fprintf(&b, "%s: %q\n", who, utterance)
	return b.String()
}

// stateLine is the one-line position: turn, whose turn, step, priority.
func stateLine(st *engine.State) string {
	if st == nil || st.Status != engine.StatusActive {
		return "(the game is not active)"
	}
	parts := []string{fmt.Sprintf("Turn %d", st.Turn), fmt.Sprintf("%s's turn", seatName(st, st.TurnSeat))}
	if st.Phase != "" {
		label := st.Step
		if label == "" || label == "main" {
			label = st.Phase + " main"
		}
		parts = append(parts, label)
	}
	if st.PrioritySeat != 0 {
		parts = append(parts, "priority: "+seatName(st, st.PrioritySeat))
	}
	return strings.Join(parts, " — ")
}

// writeSeats renders one line per seat: name, life, the nonzero
// counters, hand and library counts, commander. Order is turn order
// when the game is running, seat order otherwise.
func writeSeats(b *strings.Builder, st *engine.State) {
	if st == nil || len(st.Seats) == 0 {
		b.WriteString("Seats: (none)\n")
		return
	}
	b.WriteString("Seats:\n")
	for _, seat := range seatOrder(st) {
		p := st.Seats[seat]
		if p == nil {
			continue
		}
		var parts []string
		if p.Alive {
			parts = append(parts, fmt.Sprintf("%d life", p.Life))
		} else {
			parts = append(parts, "eliminated")
		}
		for _, name := range sortedCounterNames(p.Counters) {
			parts = append(parts, fmt.Sprintf("%s %d", name, p.Counters[name]))
		}
		parts = append(parts, fmt.Sprintf("hand %s", countWord(p.Hand)),
			fmt.Sprintf("library %s", countWord(p.Library)))
		if p.Commander != "" {
			parts = append(parts, "commander: "+p.Commander)
		}
		fmt.Fprintf(b, "  %d %s — %s\n", seat, seatName(st, seat), strings.Join(parts, " — "))
	}
}

// writeBattlefield renders the battlefield grouped by controller, in
// arrival order, identical objects collapsed to a count. Tap state,
// counters and tokens spelled the way a table reads them.
func writeBattlefield(b *strings.Builder, st *engine.State) {
	if st == nil {
		return
	}
	type line struct{ text string }
	bySeat := map[int][]line{}
	for id := int64(1); id < st.NextObject; id++ {
		o, ok := st.Objects[id]
		if !ok || o.Zone != engine.ZoneBattlefield || o.Phased {
			continue
		}
		name := "unknown card"
		if o.Identity.Card != "" {
			name = o.Identity.Card
		} else if o.Identity.Token != nil && o.Identity.Token.Name != "" {
			name = o.Identity.Token.Name + " token"
		}
		text := name
		if o.Tapped {
			text += " (tapped)"
		}
		for _, cn := range sortedCounterNames(o.Counters) {
			text += fmt.Sprintf(" %s×%d", cn, o.Counters[cn])
		}
		bySeat[o.Controller] = append(bySeat[o.Controller], line{text})
	}
	if len(bySeat) == 0 {
		b.WriteString("Battlefield: (empty)\n")
		return
	}
	b.WriteString("Battlefield:\n")
	// Collapse runs of identical objects: "Sol Ring; 3× 1/1 Soldier token".
	for _, seat := range seatOrder(st) {
		lines, ok := bySeat[seat]
		if !ok {
			continue
		}
		var parts []string
		i := 0
		for i < len(lines) {
			j := i
			for j+1 < len(lines) && lines[j+1].text == lines[i].text {
				j++
			}
			n := j - i + 1
			if n > 1 {
				parts = append(parts, fmt.Sprintf("%d× %s", n, lines[i].text))
			} else {
				parts = append(parts, lines[i].text)
			}
			i = j + 1
		}
		fmt.Fprintf(b, "  %s: %s\n", seatName(st, seat), strings.Join(parts, "; "))
	}
}

// writeStack renders the stack top first, the way it resolves.
func writeStack(b *strings.Builder, st *engine.State) {
	if st == nil || len(st.Stack) == 0 {
		b.WriteString("Stack: (empty)\n")
		return
	}
	b.WriteString("Stack (top first):\n")
	for i := len(st.Stack) - 1; i >= 0; i-- {
		it := st.Stack[i]
		name := it.Card
		if name == "" && it.Object != 0 {
			if o, ok := st.Objects[it.Object]; ok && o.Identity.Card != "" {
				name = o.Identity.Card
			}
		}
		if name == "" {
			name = it.Ability
		}
		if name == "" {
			name = "an ability"
		}
		fmt.Fprintf(b, "  %s (%s)\n", name, seatName(st, it.Controller))
	}
}

// knownCards lists the universe's card names for the prompt: the
// speaking seat's deck first, then the rest of the table in seating
// order, deduplicated, capped. The cap note is honest when it bites.
func knownCards(st *engine.State, u *universe.Universe, seat int) string {
	if u == nil || u.Attached() == 0 {
		return "(no decklists attached — names must come from the utterance)\n"
	}
	var names []string
	seen := map[string]bool{}
	add := func(cards map[string]int) {
		for name := range cards {
			if name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	add(u.Cards(seat))
	for _, s := range seatOrder(st) {
		if s != seat {
			add(u.Cards(s))
		}
	}
	sort.Strings(names)
	if len(names) > knownCardsCap {
		names = names[:knownCardsCap]
	}
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte('\n')
	}
	if len(seen) > len(names) {
		fmt.Fprintf(&b, "(list capped at %d of %d known names)\n", len(names), len(seen))
	}
	return b.String()
}

/* ---------- small render helpers, all deterministic ---------- */

func seatName(st *engine.State, seat int) string {
	if st != nil {
		if p := st.Seats[seat]; p != nil && p.Name != "" {
			return p.Name
		}
	}
	return fmt.Sprintf("Seat %d", seat)
}

// seatOrder is turn order when the game runs, else ascending seats.
func seatOrder(st *engine.State) []int {
	if st != nil && len(st.Order) > 0 {
		return st.Order
	}
	seats := make([]int, 0, len(st.Seats))
	for s := range st.Seats {
		seats = append(seats, s)
	}
	sort.Ints(seats)
	return seats
}

func sortedCounterNames(m map[string]int) []string {
	names := make([]string, 0, len(m))
	for name, n := range m {
		if n != 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// countWord spells a count the prompt can say: the number when known,
// an honest "?" when not — unknown is a value, never zero.
func countWord(c engine.Count) string {
	if c.Known {
		return fmt.Sprintf("%d", c.N)
	}
	return "?"
}
