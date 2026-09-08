package stats

// The fold's unit tests (MAD-428): determinism is the acceptance —
// identical log in, identical stats out — pinned by folding the same
// inputs twice and by shuffling the order the rows arrive in (the
// aggregates must not care). The math tests pin the party ratios, the
// luck thresholds and the tie-breaks the renderings rely on.

import (
	"encoding/json"
	"math/rand"
	"testing"
)

func foldFixture() Inputs {
	return Inputs{
		Rolls: []RollInput{
			{CharacterID: "v", CharacterName: "Velren", TargetID: "g", TargetName: "Goblin", Context: "attack", D20s: []int{20, 8}},
			{CharacterID: "v", CharacterName: "Velren", TargetID: "g", TargetName: "Goblin", Context: "attack", D20s: []int{14}},
			{CharacterID: "n", CharacterName: "Nyx", TargetID: "g", TargetName: "Goblin", Context: "attack", D20s: []int{1}},
			{CharacterID: "v", CharacterName: "Velren", Context: "check", D20s: []int{7}},
			{CharacterID: "n", CharacterName: "Nyx", Context: "save", Secret: true, D20s: []int{11}},
		},
		Damage: []DamageInput{
			// Velren takes 31 of the party's 46; Nyx takes 15.
			{TargetEntity: "v", TargetName: "Velren", TargetKind: "pc", SourceEntity: "", SourceName: "", Amount: 10},
			{TargetEntity: "v", TargetName: "Velren", TargetKind: "pc", SourceEntity: "n", SourceName: "Nyx", Amount: 6},
			{TargetEntity: "n", TargetName: "Nyx", TargetKind: "pc", SourceEntity: "v", SourceName: "Velren", Amount: 15},
			// A hit the party dealt to the other side, attributed.
			{TargetEntity: "", TargetName: "Goblin", TargetKind: "monster", SourceEntity: "v", SourceName: "Velren", Amount: 22},
		},
		Inspired: 2,
	}
}

func TestFoldDeterministicBytes(t *testing.T) {
	in := foldFixture()
	first, err := json.Marshal(Fold(in))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	again, err := json.Marshal(Fold(in))
	if err != nil {
		t.Fatalf("marshal again: %v", err)
	}
	if string(first) != string(again) {
		t.Fatal("identical inputs produced different bytes")
	}

	// The order rows arrive must not matter: shuffle the rolls and hits,
	// fold, and the JSON stays byte-identical.
	shuffled := foldFixture()
	rng := rand.New(rand.NewSource(1))
	rng.Shuffle(len(shuffled.Rolls), func(i, j int) { shuffled.Rolls[i], shuffled.Rolls[j] = shuffled.Rolls[j], shuffled.Rolls[i] })
	rng.Shuffle(len(shuffled.Damage), func(i, j int) { shuffled.Damage[i], shuffled.Damage[j] = shuffled.Damage[j], shuffled.Damage[i] })
	mixed, err := json.Marshal(Fold(shuffled))
	if err != nil {
		t.Fatalf("marshal shuffled: %v", err)
	}
	if string(mixed) != string(first) {
		t.Fatalf("shuffled inputs produced different bytes:\n%s\n%s", mixed, first)
	}
}

func TestFoldMath(t *testing.T) {
	st := Fold(foldFixture())

	if st.Rolls.Total != 5 || st.Rolls.ByContext["attack"] != 3 || st.Rolls.D20s != 6 {
		t.Fatalf("roll counts: %+v", st.Rolls)
	}
	// kept d20s: 20, 8, 14, 1, 7, 11 → sum 61, mean 10.2.
	if st.Rolls.MeanD20 != 10.2 {
		t.Fatalf("mean d20 = %v, want 10.2", st.Rolls.MeanD20)
	}
	if want := []int{1, 2, 2, 1}; !equalInts(st.Rolls.Buckets, want) {
		t.Fatalf("buckets = %v, want %v", st.Rolls.Buckets, want)
	}
	// Six dice is under the claim minimum: no luck line.
	if st.Rolls.Luck != "" {
		t.Fatalf("luck claimed on %d dice: %q", st.Rolls.D20s, st.Rolls.Luck)
	}
	if st.Rolls.Crits != 1 || st.Rolls.Natural1s != 1 {
		t.Fatalf("crits %d nat1s %d, want 1/1", st.Rolls.Crits, st.Rolls.Natural1s)
	}

	// The party: Velren took 16 (10 + 6), Nyx took 15 → party 31.
	// Dealt: Nyx 6, Velren 15 + 22 = 37.
	if st.PartyDamageTaken != 31 || st.PartyDamageDealt != 43 {
		t.Fatalf("party totals: taken %d dealt %d, want 31/43", st.PartyDamageTaken, st.PartyDamageDealt)
	}
	if len(st.Characters) != 2 {
		t.Fatalf("characters: %+v", st.Characters)
	}
	nyx, velren := st.Characters[0], st.Characters[1] // name order: Nyx first
	if velren.Name != "Velren" || velren.Taken != 16 || velren.Dealt != 37 || velren.Share != 51.6 {
		t.Fatalf("velren: %+v", velren)
	}
	if nyx.Name != "Nyx" || nyx.Taken != 15 || nyx.Dealt != 6 || nyx.Share != 48.4 {
		t.Fatalf("nyx: %+v", nyx)
	}

	if len(st.Targets) != 1 || st.Targets[0].Name != "Goblin" || st.Targets[0].Rolls != 3 {
		t.Fatalf("targets: %+v", st.Targets)
	}
	if st.InspirationSpends != 2 {
		t.Fatalf("inspiration spends: %d", st.InspirationSpends)
	}
	if st.Empty() {
		t.Fatal("a full session is not empty")
	}
}

func TestFoldLuckClaimsNeedSample(t *testing.T) {
	hot := Inputs{Rolls: []RollInput{{Context: "check", D20s: []int{20, 19, 18, 17, 20, 19, 18, 17, 20, 19}}}}
	if st := Fold(hot); st.Rolls.Luck != "hot" {
		t.Fatalf("ten hot dice mean %.1f → %q, want hot", st.Rolls.MeanD20, st.Rolls.Luck)
	}
	cool := Inputs{Rolls: []RollInput{{Context: "check", D20s: []int{2, 3, 4, 2, 3, 4, 2, 3, 4, 5}}}}
	if st := Fold(cool); st.Rolls.Luck != "cold" {
		t.Fatalf("ten cold dice mean %.1f → %q, want cold", st.Rolls.MeanD20, st.Rolls.Luck)
	}
	true1 := Inputs{Rolls: []RollInput{{Context: "check", D20s: []int{6, 7, 8, 9, 10, 11, 12, 13, 14, 15}}}}
	if st := Fold(true1); st.Rolls.Luck != "true" {
		t.Fatalf("ten fair dice mean %.1f → %q, want true", st.Rolls.MeanD20, st.Rolls.Luck)
	}
	// Nine dice, however volcanic, say nothing.
	nine := Inputs{Rolls: []RollInput{{Context: "check", D20s: []int{20, 20, 20, 20, 20, 20, 20, 20, 20}}}}
	if st := Fold(nine); st.Rolls.Luck != "" {
		t.Fatalf("nine dice claimed luck: %q", st.Rolls.Luck)
	}
}

func TestFoldTargetsCapAndTieBreak(t *testing.T) {
	in := Inputs{Rolls: []RollInput{
		{Context: "attack", TargetName: "Zombie", D20s: []int{10}},
		{Context: "attack", TargetName: "Zombie", D20s: []int{10}},
		{Context: "attack", TargetName: "Alpha", D20s: []int{10}},
		{Context: "attack", TargetName: "Alpha", D20s: []int{10}},
		{Context: "attack", TargetName: "Bravo", D20s: []int{10}},
		{Context: "attack", TargetName: "Charlie", D20s: []int{10}},
		{Context: "check", TargetName: "Charlie", D20s: []int{10}}, // checks do not target
	}}
	st := Fold(in)
	// Two at two rolls tie by name: Alpha before Zombie; the single-roll
	// targets fall off the podium after Charlie? No — cap is three:
	// Alpha, Zombie, then the 1-roll group tie-broken by name: Bravo.
	if len(st.Targets) != 3 {
		t.Fatalf("target cap: %+v", st.Targets)
	}
	if st.Targets[0].Name != "Alpha" || st.Targets[1].Name != "Zombie" || st.Targets[2].Name != "Bravo" {
		t.Fatalf("target order: %+v", st.Targets)
	}
}

func TestMarkdownRendersThePartyShot(t *testing.T) {
	st := Fold(foldFixture())
	want := `## The numbers

_5 rolls · 6 d20s averaging 10.2 · 1 crit · 1 natural 1 · 2 inspiration spent_

The party took 31 damage:

- **Nyx** took 48% of it (15) and dealt 6
- **Velren** took 52% of it (16) and dealt 37

**Most targeted:** Goblin (3 rolls)
`
	if got := st.Markdown(); got != want {
		t.Fatalf("markdown:\n%s\nwant:\n%s", got, want)
	}

	empty := Fold(Inputs{})
	if empty.Markdown() != "" {
		t.Fatalf("empty stats rendered %q", empty.Markdown())
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
