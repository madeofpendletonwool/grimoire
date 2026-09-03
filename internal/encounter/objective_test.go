package encounter

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The vocabulary is exhaustive in exactly one place. Every kind must have a
// label, a default round budget, and an ending; anything outside the slice
// must be refused.
func TestObjectiveKindsExhaustive(t *testing.T) {
	if len(Kinds) != 7 {
		t.Fatalf("Kinds = %v, want the seven declared kinds", Kinds)
	}
	for _, k := range Kinds {
		if KindLabels[k] == "" {
			t.Errorf("kind %q has no label", k)
		}
		if _, ok := defaultRounds[k]; !ok {
			t.Errorf("kind %q has no default round budget", k)
		}
		if err := (Objective{Kind: k}).Validate(); err != nil {
			t.Errorf("kind %q refused: %v", k, err)
		}
		e := Objective{Kind: k}.Ending()
		if e.Success == "" || e.Failure == "" {
			t.Errorf("kind %q has an incomplete ending: %+v", k, e)
		}
	}
	for _, bad := range []string{"escort", "Defeat", "raid", "kill"} {
		if err := (Objective{Kind: Kind(bad)}).Validate(); err == nil {
			t.Errorf("unknown kind %q accepted — objectives are rejected, never defaulted", bad)
		}
	}
	if err := (Objective{}).Validate(); err != nil {
		t.Errorf("an empty kind is absence, not an error: %v", err)
	}
}

// Normalized fills every kind's defaults, so a client that sends only a kind
// still gets a real clock and a readable focus.
func TestObjectiveNormalized(t *testing.T) {
	o := Objective{Kind: Survive}.Normalized()
	if o.Rounds != 6 {
		t.Errorf("survive rounds = %d, want the default 6", o.Rounds)
	}
	o = Objective{Kind: Protect}.Normalized()
	if o.Rounds != 5 || o.Focus == "" {
		t.Errorf("protect = %+v, want a default clock and focus", o)
	}
	o = Objective{Kind: Stop, Focus: "  the summoning circle  "}.Normalized()
	if o.Focus != "the summoning circle" {
		t.Errorf("focus not trimmed: %q", o.Focus)
	}
	if got := (Objective{Kind: Defeat}).Normalized().RoundBudget(); got != 0 {
		t.Errorf("defeat round budget = %d, want 0 — defeat has no clock", got)
	}
}

// The arithmetic each objective kind applies, pinned kind by kind for the
// DMG's sample-size party (four 3rd-level characters) at Medium. The
// thresholds are Easy 300 / Medium 600 / Hard 900 / Deadly 1600; the aim
// ladder walks those, then 2400 (half again Deadly).
//
// The numbers are the whole point: the adjustments are deterministic,
// exposed in the readout, and every value below is the readout's own claim.
func TestPlanAdjustmentsPinnedPerKind(t *testing.T) {
	party := []int{3, 3, 3, 3}
	packMult := func(b Budget) float64 {
		for _, s := range b.Shapes {
			if s.Key == "pack" {
				return s.Multiplier
			}
		}
		return 0
	}

	t.Run("defeat", func(t *testing.T) {
		b := Plan(party, BandMedium, Objective{Kind: Defeat})
		if b.TargetXP != 600 || b.CeilingXP != 900 {
			t.Fatalf("aim = %d past %d, want the Medium threshold 600 past 900", b.TargetXP, b.CeilingXP)
		}
		if b.Objective != nil || b.Adjustments != nil || b.Terrain != nil || b.Waves != 0 {
			t.Fatalf("defeat carries no objective layer: %+v", b)
		}
		if packMult(b) != 2 {
			t.Errorf("pack multiplier = %v, want the plain DMG rung", packMult(b))
		}
	})

	t.Run("survive", func(t *testing.T) {
		b := Plan(party, BandMedium, Objective{Kind: Survive, Rounds: 6})
		if b.Waves != 2 {
			t.Fatalf("waves = %d, want 2 for a six-round hold", b.Waves)
		}
		if b.TargetXP != 600 || b.CeilingXP != 900 {
			t.Errorf("aim = %d past %d, want the unshifted 600 past 900", b.TargetXP, b.CeilingXP)
		}
		for _, s := range b.Shapes {
			// Each wave is priced at its own multiplier; the shape's raw XP
			// is the whole roster across every wave.
			if got := int(float64(s.WaveXP)*s.Multiplier + 0.5); got != b.TargetXP/b.Waves && got != b.TargetXP/b.Waves+1 {
				t.Errorf("%s: wave adjusted = %d, want ~%d per wave", s.Key, got, b.TargetXP/b.Waves)
			}
			if s.Waves != 2 || s.WaveXP*2 != s.RawXP {
				t.Errorf("%s: wave split wrong: %+v", s.Key, s)
			}
		}
		if len(b.Adjustments) == 0 || b.Adjustments[0].Rule != "waves" {
			t.Errorf("the wave rule must be in the readout: %+v", b.Adjustments)
		}
	})

	t.Run("reach", func(t *testing.T) {
		b := Plan(party, BandMedium, Objective{Kind: Reach})
		// One rung up concentrates the budget, and the rockfall hazard moves
		// the aim a step: 600 -> 900.
		if b.TargetXP != 900 || b.CeilingXP != 1600 {
			t.Fatalf("aim = %d past %d, want 900 past 1600", b.TargetXP, b.CeilingXP)
		}
		if packMult(b) != 2.5 {
			t.Errorf("pack multiplier = %v, want one rung up from 2", packMult(b))
		}
		rules := adjustmentRules(b)
		if !reflect.DeepEqual(rules, []string{"rung", "hazard"}) {
			t.Errorf("rules = %v, want rung then hazard", rules)
		}
	})

	t.Run("protect", func(t *testing.T) {
		b := Plan(party, BandMedium, Objective{Kind: Protect})
		// One rung down buys a bigger board — the monsters split fire with
		// what they came for — and the burning ground moves the aim: 900.
		if b.TargetXP != 900 || b.CeilingXP != 1600 {
			t.Fatalf("aim = %d past %d, want 900 past 1600", b.TargetXP, b.CeilingXP)
		}
		if packMult(b) != 1.5 {
			t.Errorf("pack multiplier = %v, want one rung down from 2", packMult(b))
		}
	})

	t.Run("stop", func(t *testing.T) {
		b := Plan(party, BandMedium, Objective{Kind: Stop})
		if b.TargetXP != 900 {
			t.Fatalf("aim = %d, want 900", b.TargetXP)
		}
		if packMult(b) != 2.5 {
			t.Errorf("pack multiplier = %v, want one rung up", packMult(b))
		}
		if b.Waves != 0 {
			t.Errorf("waves = %d, want 0 — stop is one board against a clock", b.Waves)
		}
	})

	t.Run("retrieve", func(t *testing.T) {
		b := Plan(party, BandMedium, Objective{Kind: Retrieve})
		if b.TargetXP != 900 {
			t.Fatalf("aim = %d, want 900", b.TargetXP)
		}
		if packMult(b) != 1.5 {
			t.Errorf("pack multiplier = %v, want one rung down", packMult(b))
		}
	})

	t.Run("escape", func(t *testing.T) {
		b := Plan(party, BandMedium, Objective{Kind: Escape})
		// Priced above the party deliberately: the aim moves to the next
		// band's threshold, and escape's terrain carries no hazard on top.
		if b.TargetXP != 900 || b.CeilingXP != 1600 {
			t.Fatalf("aim = %d past %d, want the Hard threshold past Deadly", b.TargetXP, b.CeilingXP)
		}
		if packMult(b) != 2 {
			t.Errorf("pack multiplier = %v, want the plain rung", packMult(b))
		}
		if len(b.Adjustments) != 1 || b.Adjustments[0].Rule != "aim" {
			t.Errorf("rules = %v, want exactly the aim move", adjustmentRules(b))
		}
		// Deadly asks for the derived rung: half again the threshold.
		d := Plan(party, BandDeadly, Objective{Kind: Escape})
		if d.TargetXP != 2400 || d.CeilingXP != 3200 {
			t.Errorf("deadly escape aim = %d past %d, want 2400 past 3200", d.TargetXP, d.CeilingXP)
		}
	})

	t.Run("the readout explains every rule", func(t *testing.T) {
		for _, k := range Kinds[1:] {
			b := Plan(party, BandMedium, Objective{Kind: k})
			if len(b.Adjustments) == 0 {
				t.Errorf("%s: no adjustments in the readout", k)
			}
			for _, a := range b.Adjustments {
				if a.Detail == "" || a.Target <= 0 {
					t.Errorf("%s: adjustment %q unexplained: %+v", k, a.Rule, a)
				}
			}
		}
	})
}

func adjustmentRules(b Budget) []string {
	var out []string
	for _, a := range b.Adjustments {
		out = append(out, a.Rule)
	}
	return out
}

// WavesFor: one wave roughly every three rounds, floored at two — a one-wave
// survive is defeat wearing a label — and capped at four.
func TestWavesFor(t *testing.T) {
	for _, c := range []struct{ rounds, want int }{
		{1, 2}, {3, 2}, {4, 2}, {6, 2}, {7, 3}, {9, 3}, {12, 4}, {20, 4},
	} {
		if got := WavesFor(c.rounds); got != c.want {
			t.Errorf("WavesFor(%d) = %d, want %d", c.rounds, got, c.want)
		}
	}
}

// A survive encounter's verdict prices each wave at its own multiplier and
// checks the total across waves — the first wave alone must never read as
// the whole fight.
func TestEvaluateWavesChecksAllWaves(t *testing.T) {
	party := []int{3, 3, 3, 3}
	waves := [][]Monster{
		{{Name: "Goblin", CR: "1/4", XP: 50, Count: 4}},
		{{Name: "Bugbear", CR: "1", XP: 200, Count: 1}, {Name: "Goblin", CR: "1/4", XP: 50, Count: 2}},
	}
	v := EvaluateWaves(party, waves)

	if len(v.Waves) != 2 {
		t.Fatalf("waves = %d, want 2", len(v.Waves))
	}
	// Wave 1: 4 monsters ×2 = 400. Wave 2: 3 monsters ×2 = 600.
	if v.Waves[0].AdjustedXP != 400 || v.Waves[1].AdjustedXP != 600 {
		t.Fatalf("per-wave adjusted = %d + %d, want 400 + 600", v.Waves[0].AdjustedXP, v.Waves[1].AdjustedXP)
	}
	if v.TotalXP != 500 || v.AdjustedXP != 1000 {
		t.Fatalf("totals = %d raw / %d adjusted, want 500 / 1000 across waves", v.TotalXP, v.AdjustedXP)
	}
	if v.Difficulty != BandHard {
		t.Errorf("difficulty = %q, want Hard at 1000 adjusted", v.Difficulty)
	}
	// The comparison the acceptance asks for: not the first wave alone.
	first := Evaluate(party, waves[0])
	if first.AdjustedXP == v.AdjustedXP {
		t.Errorf("the waves verdict equals the first wave's: %d", v.AdjustedXP)
	}
}

// An encounter with objective = defeat and no terrain is byte-identical to
// what the builder produces before objectives existed: the adjustment layer
// serialises to nothing at all.
func TestDefeatObjectiveChangesNothing(t *testing.T) {
	party := []int{3, 3, 3, 2}
	a, err := json.Marshal(Plan(party, BandMedium, Objective{}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Plan(party, BandMedium, Objective{Kind: Defeat}))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("an empty and an explicit defeat objective disagree:\n%s\n%s", a, b)
	}
	var raw map[string]any
	if err := json.Unmarshal(a, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"objective", "adjustments", "terrain", "waves"} {
		if _, ok := raw[key]; ok {
			t.Errorf("defeat budget carries %q in its JSON — the MAD-299 surface regressed", key)
		}
	}
	for _, s := range raw["shapes"].([]any) {
		shape := s.(map[string]any)
		if _, ok := shape["waves"]; ok {
			t.Errorf("defeat shape carries wave fields: %v", shape)
		}
		if _, ok := shape["wave_xp"]; ok {
			t.Errorf("defeat shape carries wave fields: %v", shape)
		}
	}
}
