package obs_test

import (
	"context"
	"june/internal/obs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The log lands in config.DataDir(), which InitTelemetry creates 0700, and the log itself is 0600: every tool result's first characters are written there, and for observe_screen that is whatever window was in front, so nobody else with an account on the machine may read it.
func TestInitTelemetry_WritesAPrivateLogIntoAPrivateDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "june")
	t.Setenv("JUNE_DATA_DIR", dir)

	shutdown, err := obs.InitTelemetry(context.Background(), true)
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	t.Cleanup(func() { shutdown(context.Background()) })

	logInfo, err := os.Stat(filepath.Join(dir, "june.log"))
	if err != nil {
		t.Fatalf("expected the log at %s/june.log, got: %v", dir, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the data dir: %v", err)
	}
	if got := logInfo.Mode().Perm(); got != 0600 {
		t.Errorf("log file mode = %#o, want 0600", got)
	}
	if got := dirInfo.Mode().Perm(); got != 0700 {
		t.Errorf("data dir mode = %#o, want 0700", got)
	}
}
