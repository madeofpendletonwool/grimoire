package analysis

// The coach's other two halves: the missed-trigger diff and the
// prompt. Both are pure over the same scoped log Summarize folds.
//
// The missed-trigger diff is the issue's own derivation: "missed
// triggers derived from the 5c registry against what actually fired."
// The registry (MAD-335) fires TRIGGER_FIRED rows at Submit time over
// the structural events a batch produces — so the honest post-game
// read is to replay those batches with **today's** registry and diff
// against the rows that landed. A match that fired nowhere is a missed
// trigger, attributed to the batch's most informative row and phrased
// against today's knowledge, because that is exactly what the
// derivation knows — a registration that arrived after the game
// explains the gap without excusing it.
//
// The digest is the log as the model reads it: turn-anchored lines,
// ordinals cited, capped. The prompt hands the model the deterministic
// report plus that digest and nothing else — it interprets recorded
// facts and never invents state.

import (
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// digestCap bounds the digest — a long Commander log's lines are
// worth every token, but the honest ceiling is a coach's attention
// span, not the log's length.
const digestCap = 14000

// missedTriggers replays the log's batches with today's registry and
// reports, for the seat, every registry match that fired nowhere.
func missedTriggers(evs []engine.Event, reg engine.TriggerRegistry, seat int) []MissedTrigger {
	if len(reg) == 0 {
		return nil
	}
	names := engine.Fold(evs) // spelling only: identities the whole log holds
	base := engine.NewState()
	var out []MissedTrigger
	for _, batch := range batches(evs) {
		expected := engine.ExpectedTriggerFires(base, batch, reg)
		base.FoldInto(batch)
		if len(expected) == 0 {
			continue
		}
		// What actually fired in this batch, by card and controller.
		fired := map[string]int{}
		for _, e := range batch {
			if e.Kind == engine.EventTriggerFired {
				fired[fireKey(e.Card, e.Controller)]++
			}
		}
		for _, exp := range expected {
			if exp.Controller != seat {
				continue // the coach reads one seat's report
			}
			key := fireKey(exp.Card, exp.Controller)
			if fired[key] > 0 {
				fired[key]--
				continue
			}
			mt := MissedTrigger{Card: exp.Card, Effect: exp.Effect}
			if row, text := happening(batch, names); row != nil {
				mt.At = row.Ord
				mt.Happening = text
			}
			out = append(out, mt)
		}
	}
	return out
}

// fireKey identifies one firing by card and controller — the pair the
// actual and expected rows share.
func fireKey(card string, controller int) string {
	return fmt.Sprintf("%s\x00%d", card, controller)
}

// batches splits the log into Submit batches — contiguous rows sharing
// the writer's stamp. Rows without a stamp (an in-memory log, or rows
// from before the stamp existed) are their own batches.
func batches(evs []engine.Event) [][]engine.Event {
	var out [][]engine.Event
	start := 0
	for i := 1; i <= len(evs); i++ {
		if i == len(evs) || evs[i].Batch == "" || evs[i].Batch != evs[start].Batch {
			out = append(out, evs[start:i])
			start = i
		}
	}
	return out
}

// happening picks the batch's most informative row — the one a table
// would name when asked what set the trigger off — and spells it.
func happening(batch []engine.Event, names *engine.State) (*engine.Event, string) {
	rank := func(e engine.Event) int {
		switch e.Kind {
		case engine.EventCast:
			return 1
		case engine.EventLandPlayed:
			return 2
		case engine.EventDied:
			return 3
		case engine.EventZoneChanged:
			return 4 // a spoken sacrifice or destroy
		case engine.EventAttackersDeclared:
			return 5
		case engine.EventCreatureETB:
			return 6
		case engine.EventStepEntered:
			return 7 // an upkeep, an end step
		}
		return 99
	}
	best := -1
	for i := range batch {
		if rank(batch[i]) < 99 && (best < 0 || rank(batch[i]) < rank(batch[best])) {
			best = i
		}
	}
	if best < 0 {
		return nil, ""
	}
	row := &batch[best]
	return row, spellEvent(*row, names)
}

/* ---------- the digest ---------- */

// Digest renders the log the way the coach reads it: one line per
// turn-anchored happening, ordinals cited so every claim in the
// interpretation traces to a row.
func Digest(evs []engine.Event, seat int) string {
	st := engine.Fold(evs)
	var b strings.Builder
	turn := 0
	for _, e := range evs {
		switch e.Kind {
		case engine.EventTurnStarted:
			turn = e.Turn
			fmt.Fprintf(&b, "T%d %s:\n", turn, nameOf(st, e.TurnSeat))
		case engine.EventCast,
			engine.EventLandPlayed,
			engine.EventCardDrawn,
			engine.EventCardRevealed,
			engine.EventAttackersDeclared,
			engine.EventDamageDealt,
			engine.EventDied,
			engine.EventPlayerLeft,
			engine.EventTriggerFired,
			engine.EventGameEnded:
			line := digestLine(e, st, seat)
			if line == "" {
				continue
			}
			fmt.Fprintf(&b, "  %s (#%d)\n", line, e.Ord)
		}
		if b.Len() > digestCap {
			fmt.Fprintf(&b, "  … the log is truncated here — #%d is the last row cited\n", e.Ord)
			break
		}
	}
	return b.String()
}

// digestLine spells one row; the seat's own draws ride their scoped
// stream, everything listed is a row the viewer holds.
func digestLine(e engine.Event, st *engine.State, seat int) string {
	who := nameOf(st, e.ActorSeat)
	switch e.Kind {
	case engine.EventCast:
		from := ""
		if e.From != "" && e.From != "hand" {
			from = fmt.Sprintf(" from %s", e.From)
		}
		return fmt.Sprintf("%s casts %s%s", who, e.Card, from)
	case engine.EventLandPlayed:
		return fmt.Sprintf("%s plays %s", who, e.Card)
	case engine.EventCardDrawn:
		if e.TargetSeat != seat {
			return "" // other seats' draws are counts in the facts, not lines here
		}
		n := e.Count
		if n == 0 {
			n = 1
		}
		return fmt.Sprintf("%s draws %d", nameOf(st, e.TargetSeat), n)
	case engine.EventCardRevealed:
		return fmt.Sprintf("%s reveals %s", nameOf(st, e.TargetSeat), strings.Join(e.Cards, ", "))
	case engine.EventAttackersDeclared:
		return fmt.Sprintf("%s attacks with %d creature(s)", who, len(e.Attackers))
	case engine.EventDamageDealt:
		target := nameOf(st, e.TargetSeat)
		src := ""
		if e.SourceCard != "" {
			src = fmt.Sprintf(" from %s", e.SourceCard)
		}
		combat := ""
		if e.Combat {
			combat = " in combat"
		}
		return fmt.Sprintf("%s takes %d%s%s", target, e.Amount, src, combat)
	case engine.EventDied:
		return fmt.Sprintf("%s dies", objectLabel(st, e.Object))
	case engine.EventPlayerLeft:
		return fmt.Sprintf("%s leaves the game (%s)", nameOf(st, e.TargetSeat), e.Cause)
	case engine.EventTriggerFired:
		effect := ""
		if e.Effect != "" {
			effect = fmt.Sprintf(" — %s", e.Effect)
		}
		return fmt.Sprintf("%s triggers%s", e.Card, effect)
	case engine.EventGameEnded:
		if e.Reason == "" {
			return "the game ends"
		}
		return fmt.Sprintf("the game ends — %s", e.Reason)
	}
	return ""
}

// spellEvent renders a row for the happening attribution — the same
// voice the digest uses, so the two can never disagree.
func spellEvent(e engine.Event, st *engine.State) string {
	if line := digestLine(e, st, 0); line != "" {
		return line
	}
	switch e.Kind {
	case engine.EventZoneChanged:
		return fmt.Sprintf("%s's %s happens to %s", nameOf(st, e.ActorSeat), e.Cause, objectLabel(st, e.Object))
	case engine.EventStepEntered:
		return fmt.Sprintf("%s's %s %s begins", nameOf(st, st.TurnSeat), e.Phase, e.Step)
	}
	return string(e.Kind)
}

func nameOf(st *engine.State, seat int) string {
	if p, ok := st.Seats[seat]; ok && p.Name != "" {
		return p.Name
	}
	if seat == 0 {
		return ""
	}
	return fmt.Sprintf("Seat %d", seat)
}

func objectLabel(st *engine.State, id int64) string {
	if o, ok := st.Objects[id]; ok {
		if o.Identity.Card != "" {
			return o.Identity.Card
		}
		if o.Identity.Token != nil && o.Identity.Token.Name != "" {
			return o.Identity.Token.Name
		}
	}
	return fmt.Sprintf("object %d", id)
}

/* ---------- the prompt ---------- */

// Prompt assembles the coach's ask: the system side fixes the contract
// — interpret recorded facts, cite ordinals, never invent state — and
// the user side carries the deterministic report and the digest,
// nothing else.
func Prompt(sum *Summary, digest string) (system, user string) {
	system = strings.Join([]string{
		"You are the post-game coach for a Magic: the Gathering game that Grimoire recorded as a deterministic event log.",
		"You are handed recorded facts derived from that log and a digest of the log itself. Interpret them. Never invent state: if the facts do not say it, do not claim it, and say plainly when the log could not know something (unknown hand counts, unidentified cards, library order).",
		"Every claim you make must trace to a fact or a digest line; cite ordinals like #412 where they support you.",
		"Address the player directly. Structure the answer under these headings: The story of the game; The turning point; Biggest mistakes; Best play; Resource usage; Missed triggers.",
		"Missed triggers are derived from today's trigger registry replayed against the log — a registration that arrived after the game explains a gap; say so rather than blaming the player when that is plausible.",
		"Be concrete and brief: a coach's debrief, not an essay. If the facts are thin (a short log, nothing declared), say the read is thin rather than padding it.",
	}, "\n")
	user = RenderFacts(sum) + "\n\nThe log, digested:\n\n" + digest
	return system, user
}

// RenderFacts spells the deterministic report — the same text the
// prompt carries, so the surface and the model can never disagree
// about what was derived.
func RenderFacts(sum *Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GAME: %s", sum.Game.Status)
	if sum.Game.Format != "" {
		fmt.Fprintf(&b, " (%s)", sum.Game.Format)
	}
	fmt.Fprintf(&b, ", %d turns, log head #%d", sum.Game.Turns, sum.Game.HeadOrd)
	if sum.Game.Reason != "" {
		fmt.Fprintf(&b, ", ended: %s", sum.Game.Reason)
	}
	b.WriteString("\nTABLE:")
	for _, s := range sum.Game.Seats {
		state := fmt.Sprintf("alive at %d life", s.Life)
		if !s.Alive {
			state = "out"
			if s.LeftCause != "" {
				state = fmt.Sprintf("out (%s)", s.LeftCause)
			}
		}
		fmt.Fprintf(&b, " %s=%s", s.Name, state)
	}
	b.WriteString("\n")
	f := sum.Seat
	if f.Seat == 0 {
		// The table's public read (an observer's ask): no seat's private
		// zones are in a public stream, so the seat half stays thin and
		// the digest carries the game.
		b.WriteString("SEAT: the table itself — the public read; no seat's private zones are in it\n")
		return b.String()
	}
	fmt.Fprintf(&b, "SEAT: %s — %d turns played, %d casts, %d lands, %d cards drawn\n",
		f.Name, f.TurnsPlayed, f.Casts, f.LandsPlayed, f.Drawn)
	hand := "?"
	if f.HandKnownAtEnd {
		hand = fmt.Sprintf("%d", f.HandAtEnd)
	}
	fmt.Fprintf(&b, "AT GAME END: %s cards in hand (%s)\n", hand, f.Name)
	if len(f.HandKnown) > 0 {
		fmt.Fprintf(&b, "CARDS THEY HAD SEEN: %s\n", strings.Join(f.HandKnown, ", "))
	}
	fmt.Fprintf(&b, "DAMAGE: dealt %d, taken %d; life lost %d, gained %d\n",
		f.DamageDealt, f.DamageTaken, f.LifeLost, f.LifeGained)
	snap := f.FinalTurn
	fmt.Fprintf(&b, "FINAL TURN: T%d", snap.Turn)
	if !snap.EndedTurn {
		b.WriteString(" (the game ended during it)")
	}
	lands := "unknown"
	if snap.LandsKnown {
		lands = fmt.Sprintf("%d", snap.Lands)
	}
	fmt.Fprintf(&b, " — unspent mana sources at its end: %s, %d land drop(s) that turn, %d cards in hand\n",
		lands, snap.LandDrops, snap.Hand)
	b.WriteString("(honesty: " + sum.ManaNote + ")\n")
	if len(f.MissedTriggers) == 0 {
		b.WriteString("MISSED TRIGGERS: none derived — every match today's registry makes over this log fired\n")
	} else {
		b.WriteString("MISSED TRIGGERS (today's registry, replayed over the log):\n")
		for _, mt := range f.MissedTriggers {
			fmt.Fprintf(&b, "  - %s (%s) — %s at #%d\n", mt.Card, mt.Effect, mt.Happening, mt.At)
		}
	}
	return b.String()
}
