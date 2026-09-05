package main

import "testing"

// The scorecard's header line names the tracks the run selected, so every track the runner can actually run has to be in the list ranTracks walks: a run of 8, 9 or 10 alone reported "Tracks run: none", which reads as a run that measured nothing.
func TestRanTracks_NamesEveryTrackTheRunnerCanRun(t *testing.T) {
	cases := []struct {
		name string
		sel  map[string]bool
		want string
	}{
		{"the computer-use track alone", map[string]bool{"10": true}, "10"},
		{"the gold set alone", map[string]bool{"9": true}, "9"},
		{"context vs capacity alone", map[string]bool{"8": true}, "8"},
		{"every track the -tracks flag accepts", map[string]bool{"1": true, "2": true, "3": true, "5": true, "6": true, "7": true, "8": true, "9": true, "10": true}, "1, 2, 3, 5, 6, 7, 8, 9, 10"},
		{"two tracks stay in track order", map[string]bool{"10": true, "2": true}, "2, 10"},
		{"nothing selected", map[string]bool{}, "none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ranTracks(c.sel); got != c.want {
				t.Errorf("ranTracks(%v) = %q, want %q", c.sel, got, c.want)
			}
		})
	}
}
