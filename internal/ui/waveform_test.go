package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestWaveform_RisesFast(t *testing.T) {
	w := NewWaveform(10)
	w.Update(1.0)
	if w.smoothed < 0.5 {
		t.Errorf("expected fast rise toward 1.0, got %f", w.smoothed)
	}
}

func TestWaveform_FallingEdgeIsSmoothed(t *testing.T) {
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
}

func TestWaveform_VariationNonZero(t *testing.T) {
	w := NewWaveform(20)
	for i, v := range w.variation {
		if v <= 0 || v > 1.0 {
			t.Errorf("variation[%d]=%f out of (0,1] range", i, v)
		}
	}
}

func TestWaveform_SetWidth(t *testing.T) {
	w := NewWaveform(20)
	w.SetWidth(40)
	if w.width != 40 || len(w.variation) != 40 {
		t.Errorf("expected width=40, got width=%d variation=%d", w.width, len(w.variation))
	}
	w.SetWidth(0) // invalid — no-op
	if w.width != 40 {
		t.Errorf("SetWidth(0) should be no-op, got %d", w.width)
	}
}

func TestWaveform_RenderContainsLabel(t *testing.T) {
	w := NewWaveform(20)
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("#AF87FF"))
	out := w.Render(style, "MICROPHONE")
	if !strings.Contains(out, "MICROPHONE") {
		t.Error("Render missing label")
	}
}
