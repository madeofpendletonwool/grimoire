package encounter

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

// The regression harness the statblock issue calls for. The corpus
// (testdata/srd_corpus.json) is a snapshot of the Open5e SRD bestiary — the
// same records Catalog.Sync mirrors — so the parser and the DMG calculator
// are proven against every creature the mirror can serve, offline, with the
// printed CRs as ground truth.
//
// It is committed on purpose: a change to the maths
// shows up as a diff in the distribution — every creature shifting together —
// rather than as one creature quietly drifting. Regenerate deliberately:
//
//	go test ./internal/encounter -run TestCorpusCRDistribution -args -update-golden
//
// and say why in the commit. Where the DMG's own procedure disagrees with
// the printed rating, the disagreement is admitted here and in
// docs/development/statblock.md, not tuned away.

const corpusPath = "testdata/srd_corpus.json"
const goldenPath = "testdata/cr_golden.json"

// updateGolden is the deliberate-regeneration switch:
//
//	go test ./internal/encounter -run TestCorpusCRDistribution -update-golden
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/cr_golden.json from the current corpus run")

type corpusFile struct {
	Count     int              `json:"count"`
	Creatures []creatureRecord `json:"creatures"`
}

// corpusCreatures decodes the snapshot through the production wire type and
// flattens it with the production toCreature, so the harness exercises
// exactly the path a sync takes.
func corpusCreatures(t *testing.T) []Creature {
	t.Helper()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var file corpusFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if len(file.Creatures) == 0 {
		t.Fatal("corpus is empty; run internal/encounter/testdata/fetch_corpus.py")
	}
	out := make([]Creature, 0, len(file.Creatures))
	for _, rec := range file.Creatures {
		c, ok := rec.toCreature()
		if !ok {
			t.Errorf("%s: corpus record has no usable challenge rating", rec.Name)
			continue
		}
		out = append(out, c)
	}
	return out
}

type parseStats struct {
	Creatures    int     `json:"creatures"`
	Actions      int     `json:"actions"`
	Parsed       int     `json:"parsed"`
	Unparsed     int     `json:"unparsed"`
	UnparsedRate float64 `json:"unparsed_rate"`
	FullyParsed  int     `json:"fully_parsed_creatures"`
	Spellcasters int     `json:"spellcasters"`
}

type accuracyStats struct {
	Within1    float64        `json:"within_1"`
	Within2    float64        `json:"within_2"`
	Exact      float64        `json:"exact"`
	MeanDelta  float64        `json:"mean_delta"`
	Delta      map[string]int `json:"delta"`
	Confidence map[string]int `json:"confidence"`
}

type goldenFile struct {
	Creatures int           `json:"creatures"`
	Parse     parseStats    `json:"parse"`
	Accuracy  accuracyStats `json:"accuracy"`
}

// roundKey renders a CR delta as a stable histogram key: snapped CR minus
// printed CR, in printed-CR steps (a printed 1/2 against a computed 1 is +1).
func deltaSteps(computed, printed float64) int {
	// Walk the printed CR table: the distance between two challenge ratings
	// is counted in table steps, the granularity the books print at.
	from := crStepIndex(printed)
	to := crStepIndex(computed)
	return to - from
}

// crStepIndex maps a CR value onto the statblock package's table ladder so a
// delta is expressible in whole printed steps. Unknown values snap nearest.
func crStepIndex(cr float64) int {
	ladder := []float64{0, 0.125, 0.25, 0.5, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10,
		11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30}
	best := 0
	bestDist := math.Inf(1)
	for i, v := range ladder {
		if d := math.Abs(cr - v); d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
}

func computeCorpusStats(t *testing.T, creatures []Creature) (parseStats, accuracyStats, map[string]statblock.Rating) {
	var ps parseStats
	var as accuracyStats
	as.Delta = map[string]int{}
	as.Confidence = map[string]int{}
	ratings := make(map[string]statblock.Rating, len(creatures))

	for _, c := range creatures {
		s := c.Statblock()
		r := statblock.ComputeCR(s)
		if _, dup := ratings[c.Name]; dup {
			t.Errorf("corpus holds two creatures named %s", c.Name)
		}
		ratings[c.Name] = r

		ps.Creatures++
		ps.Actions += len(c.Actions)
		ps.Parsed += len(c.Attacks)
		ps.Unparsed += len(c.Unparsed)
		if len(c.Unparsed) == 0 {
			ps.FullyParsed++
		}
		if c.Spellcasting {
			ps.Spellcasters++
		}
		if len(c.Actions) > 0 && len(c.Unparsed) > 0 {
			// Round-trip invariant: every action is accounted for.
			if len(c.Attacks)+len(c.Unparsed) != len(c.Actions) {
				t.Errorf("%s: %d actions parsed into %d attacks + %d unparsed",
					c.Name, len(c.Actions), len(c.Attacks), len(c.Unparsed))
			}
		}

		// A creature whose parse was incomplete never rates high.
		if len(c.Unparsed) > 0 && r.Confidence == statblock.ConfidenceHigh {
			t.Errorf("%s: %d unparsed action(s) yet confidence %q — a partial parse must never rate high",
				c.Name, len(c.Unparsed), r.Confidence)
		}

		as.Confidence[string(r.Confidence)]++
		steps := deltaSteps(r.CR, c.CRNum)
		as.Delta[fmt.Sprint(steps)]++
		if steps == 0 {
			as.Exact++
		}
		if math.Abs(float64(steps)) <= 1 {
			as.Within1++
		}
		if math.Abs(float64(steps)) <= 2 {
			as.Within2++
		}
		as.MeanDelta += float64(steps)
	}
	as.MeanDelta /= math.Max(1, float64(ps.Creatures))
	as.Exact /= math.Max(1, float64(ps.Creatures))
	as.Within1 /= math.Max(1, float64(ps.Creatures))
	as.Within2 /= math.Max(1, float64(ps.Creatures))
	ps.UnparsedRate = float64(ps.Unparsed) / math.Max(1, float64(ps.Actions))
	return ps, as, ratings
}

func readGolden(t *testing.T) goldenFile {
	t.Helper()
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	return g
}

func writeGolden(t *testing.T, g goldenFile) {
	t.Helper()
	raw, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenPath, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCorpusParsesEveryAction is the round-trip acceptance: every action in
// the mirrored SRD ends up in a structured Attack or is explicitly marked
// unparsed — never half-parsed — and the unparsed rate never rises above
// what the golden file recorded.
func TestCorpusParsesEveryAction(t *testing.T) {
	creatures := corpusCreatures(t)
	ps, _, _ := computeCorpusStats(t, creatures)

	if ps.Actions != ps.Parsed+ps.Unparsed {
		t.Fatalf("round-trip broken: %d actions vs %d parsed + %d unparsed",
			ps.Actions, ps.Parsed, ps.Unparsed)
	}
	g := readGolden(t)
	if ps.UnparsedRate > g.Parse.UnparsedRate+1e-9 {
		t.Errorf("unparsed rate %.4f (%d of %d actions) rose above the golden %.4f — a new action shape is not parsing; fix the parser or, deliberately, regenerate the golden and say why",
			ps.UnparsedRate, ps.Unparsed, ps.Actions, g.Parse.UnparsedRate)
	}
	t.Logf("parse: %d/%d actions parsed (%.1f%%), %d/%d creatures fully parsed",
		ps.Parsed, ps.Actions, 100*(1-ps.UnparsedRate), ps.FullyParsed, ps.Creatures)
}

// TestCorpusCRDistribution is the regression harness: compute CR over the
// whole bestiary, compare the distribution of computed − printed against the
// golden file, and hold the accuracy floor the issue agreed on.
func TestCorpusCRDistribution(t *testing.T) {
	creatures := corpusCreatures(t)
	ps, as, _ := computeCorpusStats(t, creatures)

	if *updateGolden {
		writeGolden(t, goldenFile{Creatures: ps.Creatures, Parse: ps, Accuracy: as})
		t.Logf("golden updated: %d creatures", ps.Creatures)
		return
	}

	g := readGolden(t)
	want, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(goldenFile{Creatures: ps.Creatures, Parse: ps, Accuracy: as}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Fatalf(`CR distribution drifted from the golden file.
This is expected when the parser or the DMG arithmetic changed — the whole
distribution moves together, which is what the golden file is for.
Regenerate deliberately and describe the shift in the commit:

    go test ./internal/encounter -run TestCorpusCRDistribution -update-golden

--- want (golden)
%s
--- got (computed)
%s`, want, got)
	}

	// The accuracy floor, asserted rather than described: better than six in
	// ten creatures land within one printed step of their printed CR, and
	// better than nine in ten within two. The DMG's own procedure does not
	// reproduce every printed CR — the misses are documented, not tuned.
	if as.Within1 < 0.6 {
		t.Errorf("within-one-step accuracy %.2f fell below the agreed 0.60 floor", as.Within1)
	}
	if as.Within2 < 0.9 {
		t.Errorf("within-two-steps accuracy %.2f fell below the agreed 0.90 floor", as.Within2)
	}
}

// TestCorpusDeterministic runs the whole bestiary through the calculator
// twice and requires byte-identical ratings — ComputeCR is pure, and this is
// where that promise is proven at scale.
func TestCorpusDeterministic(t *testing.T) {
	creatures := corpusCreatures(t)
	first := make([]statblock.Rating, 0, len(creatures))
	second := make([]statblock.Rating, 0, len(creatures))
	for _, c := range creatures {
		first = append(first, statblock.ComputeCR(c.Statblock()))
	}
	for _, c := range creatures {
		second = append(second, statblock.ComputeCR(c.Statblock()))
	}
	for i := range first {
		a, _ := json.Marshal(first[i])
		b, _ := json.Marshal(second[i])
		if string(a) != string(b) {
			t.Fatalf("%s: rating differs between runs:\n%s\n%s", creatures[i].Name, a, b)
		}
	}
}

// TestCorpusHardCases pins the creatures the DMG's own procedure treats
// distinctively, individually, with the reason. Some reproduce their printed
// CR, some do not — the misses are a property of the source procedure, and
// if the arithmetic changes under any of them, they fail loudly.
func TestCorpusHardCases(t *testing.T) {
	cases := []struct {
		name    string
		wantCR  float64 // what the procedure computes
		printed float64 // what the book prints
		why     string
	}{
		{"Goblin", 0.5, 0.25,
			"The assumed-table half-step on attack bonus lifts the mundane humanoid one printed step; the DMG procedure prices a goblin's shortbow like a soldier's."},
		{"Werewolf", 3, 3,
			"Reproduces the printed CR: the ×2 immunity pricing on nonmagical b/p/s plus the resolved two-attack multiattack land exactly on 3 — the one place the immunity multiplier is confirmed end to end."},
		{"Troll", 6, 7,
			"Regeneration priced at three rounds (30 HP) is not enough for the printed rating; the books lean on the troll's horrific resilience in play."},
		{"Ghost", 3, 4,
			"Defense lands low (a hair of hit points even with the resistance multiplier) while Withering Touch carries all the offense; Horrifying Visage and incapacitation are invisible to damage-only pricing."},
		{"Iron Golem", 17, 16,
			"Overshoots by one: the adamantine exception in the golem's immunity is invisible to the b/p/s clause test, so the full ×2 lands, and the choice multiattack prices two strongest-option attacks."},
		{"Adult Red Dragon", 17, 17,
			"Reproduces the printed CR even with the conservative recharge reading and four unparsed actions (spellcasting, three legendary options) — the legendary-action pricing carries the defense's shortfall."},
		{"Lich", 20, 21,
			"Comes within a step despite pricing no spell damage at all: the choice multiattack (three Eldritch Bursts) carries the offense, and the printed CR's spellbook edge is nearly absorbed."},
	}
	creatures := corpusCreatures(t)
	byName := map[string]Creature{}
	for _, c := range creatures {
		byName[c.Name] = c
	}
	for _, tc := range cases {
		c, ok := byName[tc.name]
		if !ok {
			t.Errorf("corpus has no %q; re-pin the hard cases against the corpus", tc.name)
			continue
		}
		got := statblock.ComputeCR(c.Statblock())
		if math.Abs(got.CR-tc.wantCR) > 0.001 {
			t.Errorf("%s: computed CR %v (%s), want the pinned %v — the arithmetic moved under a documented case: %s",
				tc.name, got.CR, got.Label, tc.wantCR, tc.why)
		}
		if d := deltaSteps(got.CR, tc.printed); d == 0 {
			t.Logf("%s: now matches its printed CR %.6g (%s)", tc.name, tc.printed, tc.why)
		} else {
			t.Logf("%s: computed %s vs printed %s — %s", tc.name, got.Label, statblock.Label(tc.printed), tc.why)
		}
	}
}
