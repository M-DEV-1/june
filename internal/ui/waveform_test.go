package ui

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestWaveform_Logic(t *testing.T) {
	width := 10
	w := NewWaveform(width)

	if len(w.history) != width {
		t.Errorf("expected width %d, got %d", width, len(w.history))
	}

	for i := 0; i < width; i++ {
		w.Update(float64(i) / 10.0)
	}

	if w.history[width-1] != 0.9 {
		t.Errorf("expected latest value 0.9, got %f", w.history[width-1])
	}

	// update once more and check scroll
	w.Update(1.0)
	if w.history[0] != 0.1 {
		t.Errorf("expected scroll: first value should be 0.1, got %f", w.history[0])
	}
}

func TestWaveform_VisualDemo(t *testing.T) {
	/*
		NOTE
		This test is intended for manual visual verification
		Run with: go test -v ./internal/ui
	*/
	width := 60
	w := NewWaveform(width)
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("#AF87FF"))

	fmt.Println("\n--- Waveform Visual Test (Sine Wave) ---")

	// a simple sine wave pattern
	for i := 0; i < width; i++ {
		amp := 0.5 + 0.5*math.Sin(float64(i)*0.3)
		w.Update(amp)
	}

	output := w.Render(style, "SINE TEST")
	fmt.Println(output)
	fmt.Println("----------------------------------------")

	if !strings.Contains(output, "SINE TEST") {
		t.Error("Render output missing label")
	}
}

func TestWaveform_Resize(t *testing.T) {
	/*
		NOTE
		This test is intended for manual visual verification
		Run with: go test -v ./internal/ui
	*/

	style := lipgloss.NewStyle().Foreground(lipgloss.Color("#7D7D7D"))

	fmt.Println("\n--- Waveform Resize Test ---")

	w := NewWaveform(30)
	// ramp up
	for i := 0; i < 30; i++ {
		w.Update(float64(i) / 30.0)
	}
	fmt.Println("Original (Width 30):")
	fmt.Println(w.Render(style, "WIDTH 30"))

	// expand the pattern
	w.SetWidth(60)
	fmt.Println("\nExpanded (Width 60 - Pattern should be on the right):")
	fmt.Println(w.Render(style, "WIDTH 60"))

	if len(w.history) != 60 {
		t.Errorf("expected width 60, got %d", len(w.history))
	}
	if w.history[59] < 0.9 {
		t.Error("Resize (expand) lost history data at the end")
	}

	// shrink the pattern
	w.SetWidth(15)
	fmt.Println("\nShrunk (Width 15 - Only the latest ramp should be visible):")
	fmt.Println(w.Render(style, "WIDTH 15"))

	if len(w.history) != 15 {
		t.Errorf("expected width 15, got %d", len(w.history))
	}
	if w.history[14] < 0.9 {
		t.Error("Resize (shrink) lost history data at the end")
	}
	fmt.Println("----------------------------")
}
