package encounter

// The linter's false-positive fixture (MAD-385). The acceptance is
// explicit that it matters more than the true-positive one: the
// recharge-cycle check must stay silent on the SRD's own recharging
// abilities, and the vocabulary rules must stay silent on the whole
// bestiary. The corpus snapshot is exactly the shelf the mirror serves,
// so this runs every official statblock through the linter's structural
// checks and requires silence — official content is never the finding
// (that is MAD-379's golden file, for the cases the calculator itself
// disagrees with, which are arithmetic, not structure).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/homebrew"
)

func TestSRDCorpusStaysStructurallyLintClean(t *testing.T) {
	creatures := corpusCreatures(t)
	e := &homebrew.Engine{}
	// Upstream data artifacts the vocabulary rules are not allowed to
	// paper over: the 2024 octopus record mirrors Charisma -3 and no
	// Constitution at all. That is a mirror data bug — pinned here, by
	// creature and check, rather than tuned out of the checker, where
	// loosening the rule would weaken it for every homebrew statblock.
	allowed := map[string]string{
		"Octopus": homebrew.CheckMonsterScores,
	}
	type offender struct {
		name    string
		check   string
		subject string
		message string
	}
	var bad []offender
	shown := map[string]bool{}
	for _, c := range creatures {
		rep := e.LintMonster(context.Background(),
			homebrew.MonsterInput{Statblock: c.Statblock()})
		for _, f := range rep.Findings {
			if f.Basis.Origin != homebrew.OriginStructural {
				continue
			}
			key := f.Subject + ": " + f.Message
			if allowed[c.Name] == f.Check {
				continue
			}
			bad = append(bad, offender{c.Name, f.Check, f.Subject, f.Message})
			if !shown[key] && len(shown) < 20 {
				shown[key] = true
				t.Errorf("%s: %s/%s on %s: %s", c.Name, f.Severity, f.Check, f.Subject, f.Message)
			}
		}
	}
	if len(bad) > 0 {
		t.Fatalf("%d structural finding(s) across %d official statblocks — the SRD is not the finding; tighten the check's scope or fix the checker",
			len(bad), len(creatures))
	}
}

// TestSRDCorpusCycleSilence pins the specific published mechanics the
// cycle check was scoped around, so the reason each stays silent is
// written next to the fixture: the recharge roll is a bounded usage, the
// regeneration trait is out of scope, and the vampire's bite is
// damage-coupled.
func TestSRDCorpusCycleSilence(t *testing.T) {
	creatures := corpusCreatures(t)
	byName := map[string]Creature{}
	for _, c := range creatures {
		byName[c.Name] = c
	}
	cases := []struct {
		name string
		why  string
	}{
		{"Adult Red Dragon", "Fire Breath is (Recharge 5-6) — a declared usage cost"},
		{"Unicorn", "Healing Touch is 3/day — a declared per-day budget"},
		{"Troll", "Regeneration is a trait — the check reads actions only"},
		{"Vampire", "Bite's restoration is equal to the damage dealt — damage-coupled, bounded by the enemy"},
		{"Lich", "Legendary actions are budgeted per round — out of the check's scope"},
	}
	e := &homebrew.Engine{}
	for _, tc := range cases {
		c, ok := byName[tc.name]
		if !ok {
			t.Errorf("corpus has no %q; re-pin the cycle fixtures against the corpus", tc.name)
			continue
		}
		rep := e.LintMonster(context.Background(),
			homebrew.MonsterInput{Statblock: c.Statblock()})
		for _, f := range rep.Findings {
			if f.Check == homebrew.CheckMonsterCycle {
				t.Errorf("%s tripped the cycle check (%s): %s", tc.name, tc.why, f.Message)
			}
		}
	}
}

// TestSRDCorpusLintJSONStable lints the corpus twice and requires
// byte-identical findings — the structural checks are pure, and this is
// where that promise is proven at scale, the way the CR harness proves
// ComputeCR's.
func TestSRDCorpusLintJSONStable(t *testing.T) {
	creatures := corpusCreatures(t)
	e := &homebrew.Engine{}
	for _, c := range creatures {
		in := homebrew.MonsterInput{Statblock: c.Statblock()}
		a, err := json.Marshal(e.LintMonster(context.Background(), in).Findings)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(e.LintMonster(context.Background(), in).Findings)
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("%s: lint findings differ between runs", c.Name)
		}
	}
}
