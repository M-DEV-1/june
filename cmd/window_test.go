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

// The daemon binary is named "june" as well, so a candidate that resolves to this very process is passed over; launching it would fork daemons without end.
func TestWindowBinary_NeverReturnsTheDaemonItself(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUNE_WINDOW", exe)
	got, _, err := windowBinary()
	if err == nil && got == exe {
		t.Fatalf("windowBinary returned the daemon's own binary %q", got)
	}
}

// A clean exit (Quit from the tray) is left alone. A crashing window is always started again, because the tray icon and the ring overlay vanish with it; the pause doubles from a second and stops at windowRetryMax without overflowing however long the crashes go on.
func TestShouldRestartWindow(t *testing.T) {
	if wait, giveUp := shouldRestartWindow(1, 5*time.Minute, true); !giveUp || wait != 0 {
		t.Errorf("clean exit: wait = %v, giveUp = %v; want no wait and give up", wait, giveUp)
	}
	for failures, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 200: windowRetryMax} {
		if wait, giveUp := shouldRestartWindow(failures, 0, false); giveUp || wait != want {
			t.Errorf("failure %d: wait = %v, giveUp = %v; want %v and no giving up", failures, wait, giveUp, want)
		}
	}
}
