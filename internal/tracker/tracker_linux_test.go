//go:build linux

package tracker_test

import (
	"os"
	"testing"

	"ora/internal/tracker"
)

func TestLinuxTracker_New(t *testing.T) {
	tr, err := tracker.New()
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	if tr == nil {
		t.Fatal("New() returned nil tracker")
	}
}

func TestLinuxTracker_X11(t *testing.T) {
	if os.Getenv("DISPLAY") == "" {
		t.Skip("DISPLAY not set — skipping X11 test")
	}

	tr, err := tracker.New()
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}

	act, err := tr.GetActiveWindow()
	if err != nil {
		t.Fatalf("GetActiveWindow() returned error: %v", err)
	}
	if act.App == "" || act.Title == "" {
		t.Errorf("expected non-empty App and Title, got App=%q Title=%q", act.App, act.Title)
	}
	t.Logf("x11: App=%q Title=%q", act.App, act.Title)
}

func TestLinuxTracker_Sway(t *testing.T) {
	if os.Getenv("SWAYSOCK") == "" {
		t.Skip("SWAYSOCK not set — skipping sway test")
	}

	tr, err := tracker.New()
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}

	act, err := tr.GetActiveWindow()
	if err != nil {
		t.Fatalf("GetActiveWindow() returned error: %v", err)
	}
	if act.App == "" || act.Title == "" {
		t.Errorf("expected non-empty App and Title, got App=%q Title=%q", act.App, act.Title)
	}
	t.Logf("sway: App=%q Title=%q", act.App, act.Title)
}

func TestLinuxTracker_Hyprland(t *testing.T) {
	if os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") == "" {
		t.Skip("HYPRLAND_INSTANCE_SIGNATURE not set — skipping hyprland test")
	}

	tr, err := tracker.New()
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}

	act, err := tr.GetActiveWindow()
	if err != nil {
		t.Fatalf("GetActiveWindow() returned error: %v", err)
	}
	if act.App == "" || act.Title == "" {
		t.Errorf("expected non-empty App and Title, got App=%q Title=%q", act.App, act.Title)
	}
	t.Logf("hyprland: App=%q Title=%q", act.App, act.Title)
}

func TestLinuxTracker_Generic(t *testing.T) {
	// Generic fallback fires when no compositor env vars are set — test it indirectly by calling New() and checking GetActiveWindow doesn't panic.
	tr, err := tracker.New()
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	act, err := tr.GetActiveWindow()
	if err != nil {
		t.Fatalf("GetActiveWindow() panicked or errored: %v", err)
	}
	if act == nil {
		t.Fatal("GetActiveWindow() returned nil activity")
	}
	t.Logf("generic: App=%q Title=%q", act.App, act.Title)
}
