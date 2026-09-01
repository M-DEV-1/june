package db

import "testing"

// minMaxNormalize is reached only indirectly, through RankedEpisodes, where a wrong scaling shifts result order subtly instead of failing loudly. These cases pin the scaling itself so the arithmetic cannot drift unnoticed.
func TestMinMaxNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		want []float64
	}{
		// The span, not the magnitude, is what sets the scale: dividing by max alone would give [0.333, 0.667, 1] here.
		{"nonzero minimum stretches to the full range", []float64{10, 20, 30}, []float64{0, 0.5, 1}},
		// Negative values must still land inside [0,1], which only holds when the minimum is subtracted first.
		{"negatives normalize into range", []float64{-10, 0, 10}, []float64{0, 0.5, 1}},
		// Documented special case: an all-equal term contributes its full weight rather than collapsing the score to zero.
		{"all equal values normalize to one", []float64{5, 5, 5}, []float64{1, 1, 1}},
		{"single value normalizes to one", []float64{42}, []float64{1}},
		{"empty input returns empty", []float64{}, []float64{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := minMaxNormalize(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("minMaxNormalize(%v) returned %d values, want %d", tc.in, len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("minMaxNormalize(%v)[%d] = %v, want %v", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}
