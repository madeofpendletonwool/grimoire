package encounter

import (
	"reflect"
	"strings"
	"testing"
)

// The DMG's own sample party — three 3rd-level and one 2nd-level character —
// is the worked example the threshold table was pinned against, so the budget
// has to reproduce it exactly.
func TestPlanMatchesDMGSampleParty(t *testing.T) {
	b := Plan([]int{3, 3, 3, 2}, BandMedium, Objective{})

	want := map[string]int{
		BandEasy:   75*3 + 50,
		BandMedium: 150*3 + 100,
		BandHard:   225*3 + 150,
		BandDeadly: 400*3 + 200,
	}
	if !reflect.DeepEqual(b.Thresholds, want) {
		t.Fatalf("thresholds = %v, want %v", b.Thresholds, want)
	}
	if b.TargetXP != want[BandMedium] {
		t.Errorf("target = %d, want the Medium threshold %d", b.TargetXP, want[BandMedium])
	}
	if b.CeilingXP != want[BandHard] {
		t.Errorf("ceiling = %d, want the Hard threshold %d", b.CeilingXP, want[BandHard])
	}
	if b.PartySize != 4 || b.AvgLevel != 2.75 {
		t.Errorf("party = %d at avg %v, want 4 at 2.75", b.PartySize, b.AvgLevel)
	}
}

// A missing party and a missing band both have to produce a real budget:
// "just give me an encounter" is the request the designer exists for.
func TestPlanFillsDefaults(t *testing.T) {
	b := Plan(nil, "", Objective{})
	if b.Band != BandMedium {
		t.Errorf("band = %q, want Medium", b.Band)
	}
	if !reflect.DeepEqual(b.Party, DefaultParty) {
		t.Errorf("party = %v, want the default table %v", b.Party, DefaultParty)
	}
	if b.TargetXP <= 0 || len(b.Shapes) == 0 {
		t.Fatalf("default plan produced nothing usable: %+v", b)
	}
	if got := Plan(nil, "nonsense band", Objective{}).Band; got != BandMedium {
		t.Errorf("unknown band = %q, want Medium", got)
	}
}

// Deadly has no band above it in the table, so its ceiling is derived rather
// than looked up — and it must still sit above its own threshold.
func TestPlanDeadlyCeiling(t *testing.T) {
	b := Plan([]int{5, 5, 5, 5}, "deadly", Objective{})
	if b.CeilingXP <= b.TargetXP {
		t.Fatalf("deadly ceiling %d must exceed the threshold %d", b.CeilingXP, b.TargetXP)
	}
}

// Every shape has to spend the same adjusted budget: raw XP times the DMG
// multiplier lands back on the target, which is the whole reason the model is
// handed shapes rather than a single number.
func TestShapesSpendTheSameAdjustedBudget(t *testing.T) {
	b := Plan([]int{5, 5, 5, 5}, BandHard, Objective{})
	if len(b.Shapes) < 3 {
		t.Fatalf("expected several shapes, got %+v", b.Shapes)
	}
	for _, s := range b.Shapes {
		adjusted := float64(s.RawXP) * s.Multiplier
		if diff := adjusted - float64(b.TargetXP); diff > 2 || diff < -2 {
			t.Errorf("%s: %d raw XP × %v = %.0f adjusted, want ~%d",
				s.Key, s.RawXP, s.Multiplier, adjusted, b.TargetXP)
		}
		if s.EachXP <= 0 || s.EachCR == "" {
			t.Errorf("%s: no per-monster guidance: %+v", s.Key, s)
		}
	}
}

// A 1st-level party cannot pay for nine monsters; that shape must drop out
// rather than suggest a horde of things below CR 0.
func TestShapesDropUnaffordableGroups(t *testing.T) {
	b := Plan([]int{1}, BandEasy, Objective{})
	for _, s := range b.Shapes {
		if s.EachXP < 10 {
			t.Fatalf("shape %q is unaffordable but was offered: %+v", s.Key, s)
		}
	}
}

func TestReadIdea(t *testing.T) {
	h := ReadIdea("something creepy and undead in a flooded crypt")
	if !contains(h.Types, "undead") {
		t.Errorf("types = %v, want undead", h.Types)
	}
	for _, want := range []string{"aquatic", "frightening", "undead-caller"} {
		if !contains(h.Tags, want) {
			t.Errorf("tags = %v, want %q among them", h.Tags, want)
		}
	}
	if !contains(h.Terms, "crypt") {
		t.Errorf("terms = %v, want crypt", h.Terms)
	}
	// Filler must not become a search term — it outranks the real idea in
	// every statblock match.
	for _, junk := range []string{"something", "party", "encounter"} {
		if contains(h.Terms, junk) {
			t.Errorf("stopword %q leaked into terms %v", junk, h.Terms)
		}
	}
	if h := ReadIdea(""); len(h.Types)+len(h.Tags)+len(h.Terms) != 0 {
		t.Errorf("empty idea produced hints: %+v", h)
	}
}

// The catalog the parser resolves against, built without touching the network.
func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	c := &Catalog{byKey: map[string]int{}}
	c.replace([]Creature{
		{Name: "Goblin", CR: "1/4", XP: 50, Type: "Humanoid"},
		{Name: "Goblin Boss", CR: "1", XP: 200, Type: "Humanoid"},
		{Name: "Owlbear", CR: "3", XP: 700, Type: "Monstrosity"},
	}, timeZero())
	return c
}

func TestParseDesign(t *testing.T) {
	cat := testCatalog(t)
	md := strings.Join([]string{
		"# The Ledge Above the Trail",
		"",
		"## The pitch",
		"Scouts with the high ground and no patience.",
		"",
		"## Roster",
		"6 × Goblin",
		"1 × Goblin Boss",
		"2 Owlbear",
		"1 × Shadow Wyrm of Made-Up Peak",
		"",
		"## Tactics",
		"The boss stays back.",
	}, "\n")

	d := ParseDesign(cat, md)
	if d.Name != "The Ledge Above the Trail" {
		t.Errorf("name = %q", d.Name)
	}
	want := []Monster{
		{Name: "Goblin", CR: "1/4", XP: 50, Count: 6},
		{Name: "Goblin Boss", CR: "1", XP: 200, Count: 1},
		{Name: "Owlbear", CR: "3", XP: 700, Count: 2},
	}
	if !reflect.DeepEqual(d.Monsters, want) {
		t.Errorf("monsters = %+v, want %+v", d.Monsters, want)
	}
	if len(d.Unverified) != 1 || !strings.Contains(d.Unverified[0], "Made-Up") {
		t.Errorf("unverified = %v, want the invented monster", d.Unverified)
	}
	if d.Prose != md {
		t.Error("the whole reply must survive for the reader")
	}
}

// The roster block is the only machine-read part, so it has to tolerate the
// several ways a model writes a count without ever reading prose as a monster.
func TestParseDesignRosterVariants(t *testing.T) {
	cat := testCatalog(t)
	md := strings.Join([]string{
		"# Variants",
		"## Roster",
		"- 2x Goblin — lookouts on the ledge",
		"Owlbear ×3",
		"Goblin Boss",
		"## Tactics",
		"4 × Goblin appear here but must not be counted.",
	}, "\n")

	d := ParseDesign(cat, md)
	got := map[string]int{}
	for _, m := range d.Monsters {
		got[m.Name] = m.Count
	}
	want := map[string]int{"Goblin": 2, "Owlbear": 3, "Goblin Boss": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roster = %v, want %v (prose must not be parsed)", got, want)
	}
}

// A model that leads with a section heading instead of a title must not have
// "Roster" become the encounter's name.
func TestParseDesignSkipsSectionHeadingsAsTitles(t *testing.T) {
	cat := testCatalog(t)
	d := ParseDesign(cat, "## The pitch\nA fight.\n\n## Roster\n1 × Owlbear\n")
	if d.Name != "" {
		t.Errorf("name = %q, want empty rather than a section label", d.Name)
	}
	if len(d.Monsters) != 1 || d.Monsters[0].Name != "Owlbear" {
		t.Errorf("monsters = %+v", d.Monsters)
	}
}

// Counts outside what the store accepts are clamped rather than rejected: a
// model that writes "500 × Goblin" should still produce a saveable encounter.
func TestParseDesignClampsCounts(t *testing.T) {
	cat := testCatalog(t)
	d := ParseDesign(cat, "# Swarm\n## Roster\n500 × Goblin\n")
	if len(d.Monsters) != 1 || d.Monsters[0].Count != 200 {
		t.Fatalf("monsters = %+v, want a single entry clamped to 200", d.Monsters)
	}
}

// Stray prose inside the roster block must be ignored, not reported to the DM
// as a monster the SRD is missing.
func TestParseDesignIgnoresProseInTheRoster(t *testing.T) {
	cat := testCatalog(t)
	d := ParseDesign(cat, strings.Join([]string{
		"# Noise",
		"## Roster",
		"2 × Goblin",
		"Total: 100 XP",
		"That is the whole fight, and it should not be read as a creature name.",
	}, "\n"))
	if len(d.Monsters) != 1 || d.Monsters[0].Name != "Goblin" {
		t.Fatalf("monsters = %+v", d.Monsters)
	}
	for _, u := range d.Unverified {
		if strings.Contains(u, "whole fight") {
			t.Errorf("prose reported as an unverified monster: %v", d.Unverified)
		}
	}
}

// A survive roster arrives in waves: each wave parses on its own, the
// flattened roster is every wave, and a single-wave roster degrades to a
// plain one rather than pretending there is structure.
func TestParseDesignWaves(t *testing.T) {
	cat := testCatalog(t)
	md := strings.Join([]string{
		"# Hold the Bridge",
		"## Roster",
		"Wave 1: 4 × Goblin",
		"**Wave 2** — 1 × Goblin Boss, 2 × Goblin",
		"- Wave 3: 1 × Owlbear",
		"",
		"## Tactics",
		"4 × Goblin would be prose if it were not a wave line.",
	}, "\n")

	d := ParseDesign(cat, md)
	if len(d.Waves) != 3 {
		t.Fatalf("waves = %d, want 3: %+v", len(d.Waves), d.Waves)
	}
	if len(d.Waves[0]) != 1 || d.Waves[0][0].Count != 4 {
		t.Errorf("wave 1 = %+v, want 4 goblins", d.Waves[0])
	}
	if len(d.Waves[1]) != 2 {
		t.Errorf("wave 2 = %+v, want the boss and two goblins", d.Waves[1])
	}
	if d.Waves[2][0].Name != "Owlbear" {
		t.Errorf("wave 3 = %+v, want the owlbear", d.Waves[2])
	}
	// The flattened roster is the whole fight, in wave order — what the
	// roster display and a plain save carry.
	if len(d.Monsters) != 4 {
		t.Fatalf("roster = %+v, want every wave flattened", d.Monsters)
	}
	if d.Monsters[0].Name != "Goblin" || d.Monsters[0].Count != 4 {
		t.Errorf("first roster entry = %+v, want wave 1's goblins", d.Monsters[0])
	}
}

// One wave marker is defeat wearing a survive label: the design keeps the
// roster but no wave structure, so the verdict prices it the ordinary way.
func TestParseDesignSingleWaveIsNoWaves(t *testing.T) {
	cat := testCatalog(t)
	d := ParseDesign(cat, "# Not really waves\n## Roster\nWave 1: 4 × Goblin\n")
	if d.Waves != nil {
		t.Errorf("waves = %+v, want none for a single-wave roster", d.Waves)
	}
	if len(d.Monsters) != 1 || d.Monsters[0].Count != 4 {
		t.Fatalf("roster = %+v, want the wave's monsters as a plain roster", d.Monsters)
	}
}

func TestDescribe(t *testing.T) {
	got := Describe([]Monster{{Name: "Goblin", CR: "1/4", Count: 6}})
	if !strings.Contains(got, "6 × Goblin (CR 1/4)") {
		t.Errorf("Describe = %q", got)
	}
	if Describe(nil) != "(empty)" {
		t.Errorf("empty roster = %q", Describe(nil))
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
