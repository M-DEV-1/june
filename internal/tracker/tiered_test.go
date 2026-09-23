package tracker

import (
	"testing"
	"time"
)

func TestShouldUseVision(t *testing.T) {
	cases := []struct {
		name            string
		textLen         int
		visionEnabled   bool
		mediaActive     bool
		sinceLastVision time.Duration
		want            bool
	}{
		{"rich accessibility text -> no vision", 5000, true, false, time.Hour, false},
		{"thin text, vision ready -> vision", 10, true, false, time.Hour, true},
		{"empty text, vision ready -> vision", 0, true, false, time.Hour, true},
		{"thin text but vision disabled -> no", 10, false, false, time.Hour, false},
		{"thin text but rate-limited -> no", 10, true, false, time.Second, false},
		{"exactly at threshold -> no vision", thinTextThreshold, true, false, time.Hour, false},
		{"just below threshold -> vision", thinTextThreshold - 1, true, false, time.Hour, true},

		// mediaActive (MPRIS "Playing") bypasses the text-length gate: a browser tab playing video/a call returns lots of chrome text but describes nothing about what's on screen.
		{"media playing, rich chrome text, ready -> vision fires anyway", 5000, true, true, time.Hour, true},
		{"media not playing, rich text -> still no vision", 5000, true, false, time.Hour, false},
		{"media playing, thin text, ready -> vision", 10, true, true, time.Hour, true},
		{"media playing but vision disabled -> no", 5000, false, true, time.Hour, false},
		{"media playing but rate-limited -> no", 5000, true, true, time.Second, false},
	}
	for _, c := range cases {
		if got := shouldUseVision(c.textLen, c.visionEnabled, c.mediaActive, c.sinceLastVision); got != c.want {
			t.Errorf("%s: shouldUseVision=%v want %v", c.name, got, c.want)
		}
	}
}
