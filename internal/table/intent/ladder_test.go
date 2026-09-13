package intent

// The confirmation ladder as a table test: every disposition is a pure
// function of kind and confidence, and the bars are the grammar's and
// universe's published bands.

import (
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/grammar"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

func TestLadderAssign(t *testing.T) {
	cases := []struct {
		name string
		kind engine.ActionKind
		conf float64
		want Disposition
	}{
		// The auto tier: the interaction doc's list, at the bands the
		// deterministic layers publish.
		{"life certain", engine.ActionChangeLife, grammar.ConfCertain, Auto},
		{"life referenced", engine.ActionChangeLife, grammar.ConfReference, Auto},
		{"damage derived", engine.ActionDealDamage, grammar.ConfDerived, Auto},
		{"tap certain", engine.ActionTap, grammar.ConfCertain, Auto},
		{"untap certain", engine.ActionUntap, grammar.ConfCertain, Auto},
		{"draw certain", engine.ActionDraw, grammar.ConfCertain, Auto},
		{"land drop exact", engine.ActionPlayLand, universe.ConfExact, Auto},
		{"pass certain", engine.ActionPassPriority, grammar.ConfCertain, Auto},
		{"advance certain", engine.ActionAdvance, grammar.ConfCertain, Auto},
		// The model's ceiling: nothing the fallback emits reaches the
		// auto bar, whatever it claims (the gate does the capping —
		// 1.0 here is only reachable from the deterministic layers).
		{"model life capped", engine.ActionChangeLife, universe.ConfLLM, Confirm},
		{"life at certainty", engine.ActionChangeLife, 1.0, Auto},
		{"model draw capped", engine.ActionDraw, universe.ConfLLM, Confirm},
		// Confirm: applied optimistically even at full confidence —
		// these kinds are watchable by default.
		{"cast exact", engine.ActionCast, universe.ConfExact, Confirm},
		{"cast deck fuzzy", engine.ActionCast, universe.ConfOwnFuzzy, Confirm},
		{"cast ambiguous own", engine.ActionCast, universe.ConfOwnFuzzy - universe.ConfAmbiguousDelta, Confirm},
		{"referent tap", engine.ActionTap, grammar.ConfReferent, Confirm},
		{"token certain", engine.ActionCreateToken, grammar.ConfCertain, Confirm},
		{"counter certain", engine.ActionAdjustCounters, grammar.ConfCertain, Confirm},
		{"sac referenced", engine.ActionMoveZone, grammar.ConfReference, Confirm},
		// Ask: genuinely ambiguous, never applied.
		{"cast unresolved", engine.ActionCast, grammar.ConfUnresolved, Ask},
		{"other spelling ambiguous", engine.ActionCast, universe.ConfOtherSpelling - universe.ConfAmbiguousDelta, Ask},
		{"global fuzzy ambiguous", engine.ActionCast, universe.ConfGlobalFuzzy - universe.ConfAmbiguousDelta, Ask},
		{"unstamped confidence", engine.ActionChangeLife, 0, Ask},
	}
	for _, tc := range cases {
		got := Assign(engine.Action{Kind: tc.kind, Confidence: tc.conf})
		if got != tc.want {
			t.Errorf("%s: Assign(%s, %.2f) = %s, want %s", tc.name, tc.kind, tc.conf, got, tc.want)
		}
	}
}
