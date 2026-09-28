//go:build linux

package tracker_test

import (
	"testing"

	"june/internal/tracker"
)

// New picks a backend from the session env (sway/hyprland/AT-SPI/X11/none) and GetActiveWindow must return an Activity through whichever one it picked, never an error, even on a bare session where the app and title are unknown.
func TestLinuxTracker_GetActiveWindow(t *testing.T) {
	tr, err := tracker.New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	act, err := tr.GetActiveWindow()
	if err != nil {
		t.Fatalf("GetActiveWindow(): %v", err)
	}
	if act == nil {
		t.Fatal("GetActiveWindow() returned nil activity")
	}
	t.Logf("App=%q Title=%q", act.App, act.Title)
}
