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
