package odds

// The hypergeometric maths against hand-computed values (MAD-336's
// mandatory test): the fractions below were reduced by hand from the
// binomial coefficients, so a regression in the arithmetic shows up as
// a wrong rational, not just a drift in the ninth decimal.

import (
	"math/big"
	"testing"
)

func wantRat(t *testing.T, got *big.Rat, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RatString() != want {
		t.Fatalf("got %s, want %s", got.RatString(), want)
	}
}

func TestAtLeastOneCopyOfFourInSeven(t *testing.T) {
	// The classic: a 60-card deck, a playset, an opening seven.
	// P(≥1) = 1 − C(56,7)/C(60,7) = 1 − (50·51·52·53)/(57·58·59·60)
	//       = 1 − 7027800/11703240 = 38962/97527 ≈ 0.3995.
	got, err := AtLeast(60, 4, 7, 1)
	wantRat(t, got, err, "38962/97527")
	if f, _ := got.Float64(); f < 0.39949 || f > 0.39951 {
		t.Fatalf("float = %v, want ≈0.3995", f)
	}
}

func TestAtLeastSingleCopyInCommander(t *testing.T) {
	// One copy of a card in 99, opening seven: P(≥1) = 7/99.
	got, err := AtLeast(99, 1, 7, 1)
	wantRat(t, got, err, "7/99")
}

func TestPMFHandComputed(t *testing.T) {
	// 20 cards, 6 hits, 5 draws, exactly 2 hits:
	// C(6,2)·C(14,3)/C(20,5) = 15·364/15504 = 5460/15504 = 455/1292.
	got, err := PMF(20, 6, 5, 2)
	wantRat(t, got, err, "455/1292")
}

func TestAtLeastTwoHandComputed(t *testing.T) {
	// 20 cards, 6 hits, 5 draws, at least 2:
	// 1 − [C(14,5) + 6·C(14,4)]/C(20,5) = 1 − 8008/15504 = 937/1938.
	got, err := AtLeast(20, 6, 5, 2)
	wantRat(t, got, err, "937/1938")
}

func TestHyperSupportEdges(t *testing.T) {
	// Outside the support the answer is zero, never an error: three
	// hits from two copies cannot happen.
	got, err := AtLeast(20, 2, 5, 3)
	wantRat(t, got, err, "0")
	// k ≤ 0 is the certain event.
	got, err = AtLeast(20, 2, 5, 0)
	wantRat(t, got, err, "1")
	// k beyond the draw count is zero.
	got, err = AtLeast(40, 10, 3, 4)
	wantRat(t, got, err, "0")
	// Everything drawn: n = N makes the outcome certain.
	got, err = AtLeast(10, 4, 10, 4)
	wantRat(t, got, err, "1")
	// PMF over the full support sums to exactly one.
	sum := new(big.Rat)
	for k := 0; k <= 5; k++ {
		p, err := PMF(20, 6, 5, k)
		if err != nil {
			t.Fatalf("pmf k=%d: %v", k, err)
		}
		sum.Add(sum, p)
	}
	if sum.RatString() != "1" {
		t.Fatalf("pmf sum = %s, want 1", sum.RatString())
	}
}

func TestHyperRejectsImpossiblePopulations(t *testing.T) {
	if _, err := AtLeast(10, 11, 5, 1); err == nil {
		t.Fatal("K > N must error")
	}
	if _, err := AtLeast(10, 5, 11, 1); err == nil {
		t.Fatal("n > N must error")
	}
	if _, err := AtLeast(-1, 0, 0, 0); err == nil {
		t.Fatal("negative N must error")
	}
	if _, err := PMF(10, 5, 3, -1); err == nil {
		t.Fatal("negative k must error")
	}
}
