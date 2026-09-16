// Package odds is the Magic table's deck-aware play layer (MAD-336,
// stage 5 of MAD-321): exact probability maths over a known library
// composition, outs search against a board object, and mulligan advice
// over an opening hand. No model calls, no estimates — every answer is
// a hypergeometric over the remaining library the fold derived, so it
// is exact and reproducible, and it says so when the composition is
// only bounded.
//
// The library is a composition, never an order. Order-dependent
// questions — "what's my next card", "when will I draw a Wrath" — are
// refused out loud (universe.ErrLibraryOrder), the same refusal
// docs/table/interaction.md pins: a tracker that pretends to know
// order is untrustworthy the moment someone checks.
package odds

import (
	"fmt"
	"math/big"
)

/* ---------- the hypergeometric distribution, exactly ---------- */

// PMF is P(X = k) for X ~ Hypergeometric(N, K, n): the chance that n
// draws from a population of N with K hits contains exactly k hits.
// Exact rational arithmetic — binomial coefficients over math/big — so
// every answer is reproducible to the last bit once converted to float.
func PMF(N, K, n, k int) (*big.Rat, error) {
	if err := validHyper(N, K, n); err != nil {
		return nil, err
	}
	if k < 0 {
		return nil, fmt.Errorf("odds: k must not be negative")
	}
	// Outside the support the probability is zero, not an error: asking
	// for three hits out of two copies is a real question with answer 0.
	if k > K || k > n || n-k > N-K {
		return new(big.Rat), nil
	}
	num := new(big.Int).Mul(binom(K, k), binom(N-K, n-k))
	return new(big.Rat).SetFrac(num, binom(N, n)), nil
}

// AtLeast is P(X ≥ k) for X ~ Hypergeometric(N, K, n): the chance n
// draws find at least k of the K hits. The shape every table question
// actually takes — "a land in the next three", "a board wipe by turn
// nine" — computed as 1 − P(X ≤ k−1) over exact rationals.
func AtLeast(N, K, n, k int) (*big.Rat, error) {
	if err := validHyper(N, K, n); err != nil {
		return nil, err
	}
	if k <= 0 {
		return new(big.Rat).SetInt64(1), nil
	}
	if k > K || k > n {
		return new(big.Rat), nil
	}
	// P(X ≥ k) = 1 − P(X ≤ k−1): sum the PMF below k and subtract
	// from one. The skipped-tail form 1 − C(N−K, n)/C(N, n) only covers
	// k = 1; the sum covers every k with the same exactness.
	cdf := new(big.Rat)
	for j := 0; j < k; j++ {
		p, err := PMF(N, K, n, j)
		if err != nil {
			return nil, err
		}
		cdf.Add(cdf, p)
	}
	return new(big.Rat).Sub(new(big.Rat).SetInt64(1), cdf), nil
}

// validHyper rejects the populations that have no answer at all.
func validHyper(N, K, n int) error {
	if N < 0 || K < 0 || n < 0 {
		return fmt.Errorf("odds: N, K and n must not be negative (N=%d K=%d n=%d)", N, K, n)
	}
	if K > N {
		return fmt.Errorf("odds: K=%d hits in a population of N=%d", K, N)
	}
	if n > N {
		return fmt.Errorf("odds: n=%d draws from a population of N=%d", n, N)
	}
	return nil
}

// binom is the binomial coefficient C(n, k), exact.
func binom(n, k int) *big.Int {
	if k < 0 || k > n {
		return big.NewInt(0)
	}
	// C(n, k) = C(n, n−k): walk the shorter side.
	if k > n-k {
		k = n - k
	}
	out := big.NewInt(1)
	for i := 1; i <= k; i++ {
		out.Mul(out, big.NewInt(int64(n-i+1)))
		out.Div(out, big.NewInt(int64(i)))
	}
	return out
}

// ratString renders an exact rational in lowest terms — the
// reproducibility receipt every answer carries alongside its float.
func ratString(r *big.Rat) string { return r.RatString() }
