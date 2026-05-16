package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// NOTE
// faced a lot of "bleeding" colors, and holes in the ui, not sure why, since I don't know much Bubbletea, but well, ig it works now.

func TestAppFrameBackground(t *testing.T) {
	s := DefaultStyles()
	bg := s.AppFrame.GetBackground()

	// We expect the AppFrame to have NO background color (transparent)
	// so it doesn't bleed into children.
	if bg != (lipgloss.NoColor{}) {
		t.Errorf("Expected AppFrame to have no background color, but got %v", bg)
	}
}
