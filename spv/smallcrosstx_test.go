package spv

import "testing"

// TestMaxArbitersSign pins the threshold formula.
//
// maxArbitersSign is a verbatim copy of pgp-chain/smallcrosstx.GetMaxArbitersSign
// (smallcrosstx/small_cross_tx.go:130). Once that package is ported this copy,
// and this test, must be deleted and the call switched back to the original —
// nothing else in this repo can validate the formula.
func TestMaxArbitersSign(t *testing.T) {
	cases := []struct {
		total int
		want  int
	}{
		{1, 1},
		{2, 2},
		{3, 3},
		{4, 3},
		{7, 5},
		{12, 9},
	}
	for _, c := range cases {
		if got := maxArbitersSign(c.total); got != c.want {
			t.Errorf("maxArbitersSign(%d) = %d, want %d", c.total, got, c.want)
		}
	}
}
