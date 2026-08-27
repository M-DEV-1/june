package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCheckDaemonBuildMismatch_SameIdentity_ReturnsEmpty verifies a daemon reporting this process's own buildIdentity produces no warning — the common case, daemon and client from the same build.
func TestCheckDaemonBuildMismatch_SameIdentity_ReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(buildIdentity))
	}))
	defer srv.Close()

	if got := checkDaemonBuildMismatch(http.DefaultClient, srv.URL); got != "" {
		t.Errorf("expected no warning for a matching identity, got %q", got)
	}
}

// TestCheckDaemonBuildMismatch_DifferentIdentity_ReturnsWarning verifies a daemon reporting a different identity than this process's own produces a non-empty warning mentioning the daemon is stale.
func TestCheckDaemonBuildMismatch_DifferentIdentity_ReturnsWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("some-other-build-identity"))
	}))
	defer srv.Close()

	got := checkDaemonBuildMismatch(http.DefaultClient, srv.URL)
	if got == "" {
		t.Fatal("expected a non-empty warning for a mismatched identity")
	}
	if !strings.Contains(got, "older build") {
		t.Errorf("expected the warning to mention an older build, got %q", got)
	}
}

// TestCheckDaemonBuildMismatch_EmptyBody_ReturnsEmpty verifies an empty response body (e.g. an old daemon binary predating this whole feature, which still answers /ping with an empty 200 rather than 404) degrades to no warning instead of a false-positive mismatch.
func TestCheckDaemonBuildMismatch_EmptyBody_ReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if got := checkDaemonBuildMismatch(http.DefaultClient, srv.URL); got != "" {
		t.Errorf("expected no warning for an empty body, got %q", got)
	}
}

// TestCheckDaemonBuildMismatch_Unreachable_ReturnsEmpty verifies a failed request (daemon gone, network error) fails open — this is a diagnostic, not something that should ever block or warn incorrectly on its own failure.
func TestCheckDaemonBuildMismatch_Unreachable_ReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now unreachable

	if got := checkDaemonBuildMismatch(http.DefaultClient, url); got != "" {
		t.Errorf("expected no warning when the daemon is unreachable, got %q", got)
	}
}

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

// TestFileIdentity_StableForSameFile verifies calling fileIdentity twice on the same unchanged file returns the same string — the daemon and a freshly-relaunched client built from the same binary must agree when nothing has actually changed.
func TestFileIdentity_StableForSameFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ora-bin")
	if err := os.WriteFile(path, []byte("a build of ora"), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	id1 := fileIdentity(path)
	id2 := fileIdentity(path)

	if id1 != id2 {
		t.Errorf("expected a stable identity for an unchanged file, got %q then %q", id1, id2)
	}
	if id1 == "unknown" {
		t.Errorf("expected a real identity for a file that exists, got %q", id1)
	}
}

// TestFileIdentity_DiffersAfterRebuild verifies fileIdentity changes when the file at path is overwritten (a rebuild) — this is the whole point: an old daemon process holds an identity captured at ITS startup, while a freshly-relaunched client reads whatever's on disk now.
func TestFileIdentity_DiffersAfterRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ora-bin")
	if err := os.WriteFile(path, []byte("old build"), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before := fileIdentity(path)

	if err := os.WriteFile(path, []byte("a new, longer build of ora"), 0755); err != nil {
		t.Fatalf("WriteFile (rebuild): %v", err)
	}
	after := fileIdentity(path)

	if before == after {
		t.Errorf("expected a different identity after the file was rewritten, got the same %q both times", before)
	}
}

// TestFileIdentity_MissingFile_ReturnsUnknown verifies a stat failure degrades to "unknown" instead of panicking or erroring out — this is a diagnostic, not something that should ever block startup.
func TestFileIdentity_MissingFile_ReturnsUnknown(t *testing.T) {
	got := fileIdentity(filepath.Join(t.TempDir(), "does-not-exist"))
	if got != "unknown" {
		t.Errorf(`expected "unknown" for a missing file, got %q`, got)
	}
}
