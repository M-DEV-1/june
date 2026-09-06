package obs_test

import (
	"context"
	"ora/internal/obs"
	"os"
	"path/filepath"
	"testing"
)

// TestInitTelemetry_WritesLogIntoDataDir verifies the log file lands in config.DataDir() rather than a working-directory-relative "ora-db", so the daemon and a terminal-launched client write to the same log no matter where each was started from.
func TestInitTelemetry_WritesLogIntoDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)

	shutdown, err := obs.InitTelemetry(context.Background(), true)
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	if _, err := os.Stat(filepath.Join(dir, "ora.log")); err != nil {
		t.Errorf("expected the log at %s/ora.log, got: %v", dir, err)
	}
	if _, err := os.Stat("ora-db"); err == nil {
		t.Errorf("InitTelemetry created a cwd-relative ora-db directory instead of using the data dir")
	}
}

// TestInitTelemetry_LogFileIsPrivateToTheUser pins the log file's mode. Every tool result's first 160 characters is written into this file, and for observe_screen that is the title and contents of whatever window was in front — a password manager, an inbox — so the file must not be readable by anyone else with an account on the machine.
func TestInitTelemetry_LogFileIsPrivateToTheUser(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)

	shutdown, err := obs.InitTelemetry(context.Background(), true)
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	info, err := os.Stat(filepath.Join(dir, "ora.log"))
	if err != nil {
		t.Fatalf("stat the log: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("log file mode = %#o, want 0600", got)
	}
}

// TestInitTelemetry_TightensAnAlreadyWorldReadableLog covers the machines that have been running the old code: the log is already there at 0644, and opening an existing file does not change its mode, so start-up has to tighten it.
func TestInitTelemetry_TightensAnAlreadyWorldReadableLog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)
	path := filepath.Join(dir, "ora.log")
	if err := os.WriteFile(path, []byte("{\"msg\":\"an old line\"}\n"), 0644); err != nil {
		t.Fatalf("write the old log: %v", err)
	}

	shutdown, err := obs.InitTelemetry(context.Background(), true)
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the log: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("log file mode = %#o, want the existing log tightened to 0600", got)
	}
}

// ora.log is appended to at Debug for the life of every daemon and nothing ever truncated it, so on a long-running machine it grows without bound. A log already past the cap is rolled aside on open and a fresh one started, keeping at most the current log and one previous.
func TestInitTelemetry_RotatesALogPastTheSizeCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)

	logPath := filepath.Join(dir, "ora.log")
	if err := os.WriteFile(logPath, []byte("an old line\n"), 0600); err != nil {
		t.Fatalf("write the old log: %v", err)
	}
	// Sparse, so the test costs no disk: Stat still reports the full size, which is all the cap is compared against. 50 MB is maxLogBytes in telemetry.go.
	if err := os.Truncate(logPath, 50<<20+1); err != nil {
		t.Fatalf("grow the old log past the cap: %v", err)
	}

	shutdown, err := obs.InitTelemetry(context.Background(), true)
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	defer shutdown(context.Background())

	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Errorf("expected the oversized log to be rolled to ora.log.1, got: %v", err)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat the fresh log: %v", err)
	}
	if info.Size() > 1<<20 {
		t.Errorf("expected a fresh log, got one of %d bytes", info.Size())
	}
}

// A log still under the cap is appended to, not rotated: rotating on every start would throw away the record of the session before this one.
func TestInitTelemetry_KeepsALogUnderTheSizeCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)

	logPath := filepath.Join(dir, "ora.log")
	if err := os.WriteFile(logPath, []byte("an old line\n"), 0600); err != nil {
		t.Fatalf("write the old log: %v", err)
	}

	shutdown, err := obs.InitTelemetry(context.Background(), true)
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	defer shutdown(context.Background())

	if _, err := os.Stat(logPath + ".1"); err == nil {
		t.Error("a log under the cap was rotated")
	}
}

// The data directory holds the store, the IPC token and the log, so it is the user's alone. InitTelemetry is usually the first thing to create it, and it used to create it 0755, which is what left the live directory world-readable.
func TestInitTelemetry_CreatesTheDataDirPrivateToTheUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ora")
	t.Setenv("ORA_DATA_DIR", dir)

	shutdown, err := obs.InitTelemetry(context.Background(), true)
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	defer shutdown(context.Background())

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the data dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Errorf("expected the data dir to be 0700, got %o", got)
	}
}
