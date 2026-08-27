package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSecureEnvFile_RestrictsPermissions verifies .env (which holds the Gemini API key) gets locked down to 0600 — it commonly defaults to 0644 (world-readable) on a multi-user machine.
func TestSecureEnvFile_RestrictsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply on Windows")
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("GEMINI_API_KEY=x"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	secureEnvFile(path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("permissions = %o, want 0600", got)
	}
}

// TestSecureEnvFile_MissingFile_DoesNotPanic verifies a missing .env (the common case — env vars set directly) is a silent no-op.
func TestSecureEnvFile_MissingFile_DoesNotPanic(t *testing.T) {
	secureEnvFile(filepath.Join(t.TempDir(), "does-not-exist"))
}
