package intent

// The confirmation ladder (MAD-331, stage 4 of MAD-321), exactly as
// docs/table/interaction.md states it: every proposed action carries a
// disposition assigned from the resolution confidence, and the
// disposition decides the UI.
//
//	auto    high confidence, cheap to undo — applied immediately, one-tap
//	        undo in the log. Life, tap/untap, draws, land drops, damage,
//	        and the pure flow shapes.
//	confirm applied, worth a look — applied optimistically, highlighted
//	        in the current-action pane with accept / fix. Card
//	        identification below the certainty bar.
//	ask     genuinely ambiguous — NOT applied. One question with
//	        tappable answers, riding beside the log.
//
// The bars are deliberately anchored to the grammar's and universe's
// published confidence bands rather than invented here: the auto bar is
// ConfDerived (0.90) — a deterministic layer earned its answer from the
// state — and the confirm bar is ConfReferent (0.70), the weakest thing
// the deterministic layers ever optimistically assert. Below that lies
// the damped-ambiguity and unresolved territory, which is a question,
// never a write. The ladder is a pure function: same action, same
// disposition, unit-testable, no model in it.

import "github.com/madeofpendletonwool/grimoire/internal/table/engine"

// Disposition is the ladder's verdict on one candidate action.
type Disposition string

const (
	// Auto: applied immediately, one-tap undo.
	Auto Disposition = "auto"
	// Confirm: applied optimistically, highlighted with accept / fix.
	Confirm Disposition = "confirm"
	// Ask: not applied; one question with tappable answers.
	Ask Disposition = "ask"
)

// The bars, in the grammar's and universe's published bands.
const (
	// AutoConf matches grammar.ConfDerived: a deterministic resolution
	// earned from the current state. Anything the model emits is capped
	// below this (universe.ConfLLM), so the auto tier is the
	// deterministic layers' alone — the LLM is the escape hatch, not the
	// front door.
	AutoConf = 0.90
	// ConfirmConf matches grammar.ConfReferent: the weakest confidence
	// the deterministic layers ever apply optimistically.
	ConfirmConf = 0.70
)

// autoKinds are the kinds cheap enough to undo that high confidence
// buys immediate application: the interaction doc's list — life,
// tap/untap, draws, land drops, damage — plus the flow shapes that
// carry no reference at all ("pass", "next step"). Everything else,
// however confident, is applied optimistically at most: a CAST is
// watchable by default because card identification is where the
// certainty bar actually bites.
var autoKinds = map[engine.ActionKind]bool{
	engine.ActionChangeLife:   true,
	engine.ActionDealDamage:   true,
	engine.ActionTap:          true,
	engine.ActionUntap:        true,
	engine.ActionDraw:         true,
	engine.ActionPlayLand:     true,
	engine.ActionPassPriority: true,
	engine.ActionAdvance:      true,
}

// Assign is the ladder: one action in, one disposition out.
func Assign(a engine.Action) Disposition {
	switch {
	case a.Confidence >= AutoConf && autoKinds[a.Kind]:
		return Auto
	case a.Confidence >= ConfirmConf:
		return Confirm
	default:
		return Ask
	}
}
