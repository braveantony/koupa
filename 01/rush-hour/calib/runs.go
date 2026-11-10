package calib

import "math"

// RunsTest is the Wald-Wolfowitz runs test on a boolean sequence, using the
// normal approximation. It returns the z statistic and the two-sided p-value.
//
// The test is undefined when the sequence has fewer than 2 elements, contains
// only one value, or the variance of the run count is zero. In those cases it
// returns z = NaN and p = NaN. Callers must treat NaN as "cannot test", which
// means the independence model is NOT used (see Estimate).
func RunsTest(hits []bool) (z float64, p float64) {
	n := len(hits)
	if n < 2 {
		return math.NaN(), math.NaN()
	}
	var n1 int
	for _, h := range hits {
		if h {
			n1++
		}
	}
	n2 := n - n1
	if n1 == 0 || n2 == 0 {
		return math.NaN(), math.NaN()
	}
	runs := 1
	for i := 1; i < n; i++ {
		if hits[i] != hits[i-1] {
			runs++
		}
	}
	fn, f1, f2 := float64(n), float64(n1), float64(n2)
	mu := 2*f1*f2/fn + 1
	variance := (mu - 1) * (mu - 2) / (fn - 1)
	if variance <= 0 {
		return math.NaN(), math.NaN()
	}
	z = (float64(runs) - mu) / math.Sqrt(variance)
	return z, math.Erfc(math.Abs(z) / math.Sqrt2)
}

// twoProportionP returns the two-sided p-value of the pooled two-proportion
// z-test. ok is false when the test cannot be computed (an empty group). When
// the pooled proportion is 0 or 1 both groups are identical, so p = 1.
func twoProportionP(h1, n1, h2, n2 int) (p float64, ok bool) {
	if n1 == 0 || n2 == 0 {
		return 0, false
	}
	pool := float64(h1+h2) / float64(n1+n2)
	se := math.Sqrt(pool * (1 - pool) * (1/float64(n1) + 1/float64(n2)))
	if se == 0 {
		return 1, true
	}
	z := (float64(h1)/float64(n1) - float64(h2)/float64(n2)) / se
	return math.Erfc(math.Abs(z) / math.Sqrt2), true
}
