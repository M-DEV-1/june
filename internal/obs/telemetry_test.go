package obs_test

import (
	"context"
	"ora/internal/obs"
	"os"
	"path/filepath"
	"testing"
)

func TestInitTelemetry(t *testing.T) {
	ctx := context.Background()

	// initialize telemetry hub
	shutdown, err := obs.InitTelemetry(ctx, true)
	if err != nil {
		t.Fatalf("Failed to initialize telemetry: %+v", err)
	}

	//
	if err := shutdown(ctx); err != nil {
		t.Errorf("Shutdown failed: %+v", err)
	}

	t.Log("Telemetry Hub initialized and shut down successfully.")
}

// TestInitTelemetry_WritesLogIntoDataDir verifies the log file lands in config.DataDir() rather than a working-directory-relative "ora-db", so the daemon and a terminal-launched client write to the same log no matter where each was started from.
func TestInitTelemetry_WritesLogIntoDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)

	shutdown, err := obs.InitTelemetry(context.Background(), true)
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	defer shutdown(context.Background())

	if _, err := os.Stat(filepath.Join(dir, "ora.log")); err != nil {
		t.Errorf("expected the log at %s/ora.log, got: %v", dir, err)
	}
	if _, err := os.Stat("ora-db"); err == nil {
		t.Errorf("InitTelemetry created a cwd-relative ora-db directory instead of using the data dir")
	}
}
