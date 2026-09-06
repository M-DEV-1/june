package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestWaveform is a table over Waveform's distinct behaviours: a loud sample rises fast toward
// 1.0, a single quiet sample after a run of loud ones decays smoothly rather than dropping
// straight to zero, every entry in the variation profile stays in the (0,1] range, SetWidth
// resizes the variation slice and rejects a zero width as a no-op, and Render includes the given
// label in its output.
func TestWaveform(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"rises fast", func(t *testing.T) {
			w := NewWaveform(10)
			w.Update(1.0)
			if w.smoothed < 0.5 {
				t.Errorf("expected fast rise toward 1.0, got %f", w.smoothed)
			}
		}},
		{"falling edge is smoothed", func(t *testing.T) {
			w := NewWaveform(10)
			// drive up
			for range 5 {
				w.Update(1.0)
			}
			high := w.smoothed
			if high < 0.5 {
				t.Fatalf("expected high amplitude after repeated Update(1.0), got %f", high)
			}
			// single zero tick should not drop to zero
			w.Update(0.0)
			if w.smoothed == 0 {
				t.Error("expected smoothed fall, not instant drop to 0")
			}
			if w.smoothed >= high {
				t.Errorf("expected decay below peak %f, got %f", high, w.smoothed)
			}
		}},
		{"variation is non-zero", func(t *testing.T) {
			w := NewWaveform(20)
			for i, v := range w.variation {
				if v <= 0 || v > 1.0 {
					t.Errorf("variation[%d]=%f out of (0,1] range", i, v)
				}
			}
		}},
		{"SetWidth resizes, zero is a no-op", func(t *testing.T) {
			w := NewWaveform(20)
			w.SetWidth(40)
			if w.width != 40 || len(w.variation) != 40 {
				t.Errorf("expected width=40, got width=%d variation=%d", w.width, len(w.variation))
			}
			w.SetWidth(0) // invalid — no-op
			if w.width != 40 {
				t.Errorf("SetWidth(0) should be no-op, got %d", w.width)
			}
		}},
		{"Render contains the label", func(t *testing.T) {
			w := NewWaveform(20)
			style := lipgloss.NewStyle().Foreground(lipgloss.Color("#AF87FF"))
			out := w.Render(style, "MICROPHONE")
			if !strings.Contains(out, "MICROPHONE") {
				t.Error("Render missing label")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}
