package cmd

import (
	"os"
	"path/filepath"
	"strings"
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
		if got, _, err := windowBinary(); err == nil && got == path {
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
	got, _, err := windowBinary()
	if err == nil && got == exe {
		t.Fatalf("windowBinary returned the daemon's own binary %q", got)
	}
}

// The hint printed when nothing is found needs every path windowBinary actually tried, so it must come back even when none of them panned out — and it must include a candidate built from the working directory, since `go build -o ora . && ./ora` from a checkout is the path that never has anything beside the executable.
func TestWindowBinary_ReturnsCandidatesTriedEvenOnFailure(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORA_WINDOW", "")

	_, tried, err := windowBinary()
	if err == nil {
		t.Fatal("expected no window binary to be found in an empty directory")
	}
	if len(tried) == 0 {
		t.Fatal("expected windowBinary to report the candidates it tried")
	}
	wantSuffix := filepath.Join("app", "src-tauri", "target", "release", "ora")
	found := false
	for _, c := range tried {
		if strings.HasSuffix(c, wantSuffix) && strings.HasPrefix(c, dir) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a working-directory candidate ending in %q, got %v", wantSuffix, tried)
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

func TestWindowLog_AppendsInTheDataDir(t *testing.T) {
	dir := t.TempDir()
	for _, line := range []string{"first\n", "second\n"} {
		f, err := windowLog(dir)
		if err != nil {
			t.Fatalf("windowLog: %v", err)
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	got, err := os.ReadFile(filepath.Join(dir, "window.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\nsecond\n" {
		t.Errorf("window.log = %q; want both lines kept", got)
	}
	info, _ := os.Stat(filepath.Join(dir, "window.log"))
	if info.Mode().Perm() != 0600 {
		t.Errorf("mode = %v; want 0600 like ora.log", info.Mode().Perm())
	}
}

// The restart line must carry the reason, not just "exit status 1". Read against the machine's real window.log shape: a tao panic whose message is the GTK failure.
func TestLastWindowWords_NamesThePanicNotTheBacktrace(t *testing.T) {
	dir := t.TempDir()
	body := "event stream: ready\nevent stream: tick\n" +
		"thread 'main' (12345) panicked at /tao-0.35.3/src/event_loop.rs:217:53:\n" +
		"Failed to initialize gtk backend!: BoolError { message: \"Failed to initialize GTK\" }\n" +
		"note: run with `RUST_BACKTRACE=1`\n"
	if err := os.WriteFile(filepath.Join(dir, "window.log"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	got := lastWindowWords(dir)
	if !strings.Contains(got, "panicked at") {
		t.Errorf("lastWindowWords = %q, want the panic line", got)
	}
	if lastWindowWords(t.TempDir()) != "" {
		t.Error("a missing window.log must say nothing rather than fail")
	}
}
