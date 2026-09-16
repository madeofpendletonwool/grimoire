package odds

// The mulligan advisor's tests (MAD-336): deterministic verdicts over
// hands whose land counts sit in each rule band, every reason carrying
// its arithmetic, and the percentile computed by the same
// hypergeometric the draw questions use.

import (
	"math/big"
	"strings"
	"testing"
)

// exactCDF recomputes P(X ≤ k) independently of the advisor, from the
// same exported PMF — the test's own arithmetic, not a re-run of the
// code under test.
func exactCDF(N, K, n, k int) float64 {
	sum := new(big.Rat)
	for j := 0; j <= k; j++ {
		if p, err := PMF(N, K, n, j); err == nil {
			sum.Add(sum, p)
		}
	}
	f, _ := sum.Float64()
	return f
}

// monoGreenDeck is a 39-card slice of a deck: 15 lands, ramp, draw,
// interaction, wipes — enough shape for every rule band.
func monoGreenDeck() map[string]int {
	return map[string]int{
		"Forest":        15,
		"Wrath of God":  2,
		"Counterspell":  2,
		"Beast Within":  2,
		"Naturalize":    2,
		"Sol Ring":      2,
		"Cultivate":     4,
		"Harmonize":     4,
		"Rhystic Study": 6,
	}
}

func TestMulliganOneLandIsAMulligan(t *testing.T) {
	hand := []string{"Forest", "Rhystic Study", "Harmonize", "Cultivate", "Beast Within", "Wrath of God", "Sol Ring"}
	a, err := Advise(hand, monoGreenDeck(), fixtureLookup)
	if err != nil {
		t.Fatal(err)
	}
	if a.Verdict != "mulligan" || a.Lands != 1 {
		t.Fatalf("verdict = %s lands = %d, want mulligan/1", a.Verdict, a.Lands)
	}
	if a.LandRational == "" || len(a.Reasons) == 0 {
		t.Fatalf("advice must carry its arithmetic: %+v", a)
	}
	if !strings.Contains(strings.Join(a.Reasons, " "), "15-land deck") {
		t.Fatalf("reasons should cite the deck's land count: %v", a.Reasons)
	}
}

func TestMulliganZeroAndFloodedHands(t *testing.T) {
	deck := monoGreenDeck()
	if a, _ := Advise([]string{"Rhystic Study", "Harmonize", "Cultivate", "Beast Within", "Wrath of God", "Sol Ring", "Counterspell"}, deck, fixtureLookup); a.Verdict != "mulligan" {
		t.Fatalf("zero lands = %s, want mulligan", a.Verdict)
	}
	flooded := []string{"Forest", "Forest", "Forest", "Forest", "Forest", "Forest", "Cultivate"}
	if a, _ := Advise(flooded, deck, fixtureLookup); a.Verdict != "mulligan" {
		t.Fatalf("six lands = %s, want mulligan", a.Verdict)
	}
}

func TestMulliganTwoLandsKeepsWithRamp(t *testing.T) {
	hand := []string{"Forest", "Forest", "Sol Ring", "Cultivate", "Harmonize", "Beast Within", "Rhystic Study"}
	a, err := Advise(hand, monoGreenDeck(), fixtureLookup)
	if err != nil {
		t.Fatal(err)
	}
	if a.Verdict != "keep" {
		t.Fatalf("verdict = %s, want keep (ramp saves two lands)", a.Verdict)
	}
	if !strings.Contains(strings.Join(a.Reasons, " "), "ramp") {
		t.Fatalf("the reason should name the ramp: %v", a.Reasons)
	}
}

func TestMulliganTwoLandsWithoutRampMulligans(t *testing.T) {
	hand := []string{"Forest", "Forest", "Rhystic Study", "Harmonize", "Beast Within", "Wrath of God", "Counterspell"}
	a, err := Advise(hand, monoGreenDeck(), fixtureLookup)
	if err != nil {
		t.Fatal(err)
	}
	if a.Verdict != "mulligan" {
		t.Fatalf("verdict = %s, want mulligan (no ramp, 40%% deck)", a.Verdict)
	}
}

func TestMulliganMidbandKeeps(t *testing.T) {
	hand := []string{"Forest", "Forest", "Forest", "Cultivate", "Harmonize", "Beast Within", "Rhystic Study"}
	a, err := Advise(hand, monoGreenDeck(), fixtureLookup)
	if err != nil {
		t.Fatal(err)
	}
	if a.Verdict != "keep" {
		t.Fatalf("verdict = %s, want keep", a.Verdict)
	}
	// The roles ride along — internal/deck's classification, not a
	// second scheme.
	if a.HandRoles["Cultivate"] != "ramp" || a.HandRoles["Beast Within"] != "interaction" {
		t.Fatalf("roles = %v", a.HandRoles)
	}
}

func TestMulliganKeepBandWithNothingCastableMulligans(t *testing.T) {
	// Four lands is mid-band, but every spell needs colors the hand's
	// basics do not make (blue) — the hand cannot play Magic.
	hand := []string{"Forest", "Forest", "Forest", "Forest", "Counterspell", "Rhystic Study", "Harmonize"}
	// Harmonize is {2}{G} — castable; so make the uncastable case
	// clean by using only blue spells.
	hand = []string{"Forest", "Forest", "Forest", "Forest", "Counterspell", "Rhystic Study", "Counterspell"}
	a, err := Advise(hand, monoGreenDeck(), fixtureLookup)
	if err != nil {
		t.Fatal(err)
	}
	if a.Verdict != "mulligan" {
		t.Fatalf("verdict = %s, want mulligan (nothing castable early)", a.Verdict)
	}
}

func TestMulliganDeterministic(t *testing.T) {
	hand := []string{"Forest", "Forest", "Sol Ring", "Cultivate", "Harmonize", "Beast Within", "Rhystic Study"}
	a1, _ := Advise(hand, monoGreenDeck(), fixtureLookup)
	a2, _ := Advise(hand, monoGreenDeck(), fixtureLookup)
	if a1.Verdict != a2.Verdict || a1.LandPercentile != a2.LandPercentile {
		t.Fatal("advice must be deterministic")
	}
	// The percentile is the hypergeometric's: for 16 lands in 40 cards
	// over 7, P(X ≤ 2) can be recomputed independently.
	lib := Library{Known: true, Exact: true, N: 39, Counts: monoGreenDeck()}
	_ = lib
	p := exactCDF(39, 15, 7, 2)
	if p != a1.LandPercentile {
		t.Fatalf("percentile %v != recomputed %v", a1.LandPercentile, p)
	}
}

func TestMulliganNeedsHandAndDeck(t *testing.T) {
	if _, err := Advise(nil, monoGreenDeck(), fixtureLookup); err == nil {
		t.Fatal("no hand must error")
	}
	if _, err := Advise([]string{"Forest"}, nil, fixtureLookup); err == nil {
		t.Fatal("no decklist must error")
	}
}
