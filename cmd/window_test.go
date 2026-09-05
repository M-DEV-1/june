package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeExecutable puts an executable file at path so windowBinary has something real to find.
func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// ORA_WINDOW names the window to run, so a developer can point the daemon at any build.
func TestWindowBinary_PrefersTheEnvironmentOverride(t *testing.T) {
	want := writeExecutable(t, filepath.Join(t.TempDir(), "some-build", "ora"))
	t.Setenv("ORA_WINDOW", want)
	got, err := windowBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("windowBinary = %q, want %q", got, want)
	}
}

// A path that names something unusable is passed over rather than launched: a directory, a file with no execute bit, and a name for nothing at all.
func TestWindowBinary_SkipsWhatCannotBeRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "adirectory"), 0o755); err != nil {
		t.Fatal(err)
	}
	notExecutable := filepath.Join(dir, "plain")
	if err := os.WriteFile(notExecutable, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"a directory":         filepath.Join(dir, "adirectory"),
		"no execute bit":      notExecutable,
		"nothing of the name": filepath.Join(dir, "missing"),
	} {
		t.Setenv("ORA_WINDOW", path)
		if got, err := windowBinary(); err == nil && got == path {
			t.Errorf("%s: windowBinary returned %q, which cannot be run", name, got)
		}
	}
}

// The daemon binary is named "ora" as well, so a candidate that resolves to this very process is passed over; launching it would fork daemons without end.
func TestWindowBinary_NeverReturnsTheDaemonItself(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORA_WINDOW", exe)
	got, err := windowBinary()
	if err == nil && got == exe {
		t.Fatalf("windowBinary returned the daemon's own binary %q", got)
	}
}

// A machine with no window built runs the daemon alone rather than failing to start.
func TestWindowBinary_ReportsWhenThereIsNoneToRun(t *testing.T) {
	t.Setenv("ORA_WINDOW", filepath.Join(t.TempDir(), "not-here"))
	if _, err := windowBinary(); err == nil {
		t.Error("expected an error when there is no window binary to run")
	}
}

// Quitting from the tray exits the window cleanly, and starting it again would make that menu item do nothing.
func TestShouldRestartWindow_ACleanExitIsLeftAlone(t *testing.T) {
	wait, giveUp := shouldRestartWindow(1, 5*time.Minute, true)
	if !giveUp || wait != 0 {
		t.Errorf("shouldRestartWindow(clean) = %v, %v; want no wait and give up", wait, giveUp)
	}
}

// A crashing window is always started again, because the tray icon and the ring overlay vanish with it and a log line is the only other sign. The pause doubles so a window failing at once does not spin, and stops at a minute so one that recovers is picked up soon after.
func TestShouldRestartWindow_BacksOffButNeverGivesUp(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		failures := i + 1
		wait, giveUp := shouldRestartWindow(failures, 20*time.Millisecond, false)
		if giveUp {
			t.Fatalf("failure %d: gave up on a crashing window", failures)
		}
		if wait != w {
			t.Errorf("failure %d: wait = %v, want %v", failures, wait, w)
		}
	}
	// However long it has been failing, the pause never grows past the cap and never overflows into something negative or instant.
	for _, failures := range []int{40, 64, 65, 200} {
		wait, giveUp := shouldRestartWindow(failures, 0, false)
		if giveUp || wait != windowRetryMax {
			t.Errorf("failure %d: wait = %v, giveUp = %v; want the cap and no giving up", failures, wait, giveUp)
		}
	}
}
