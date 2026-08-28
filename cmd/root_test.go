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

// checkDaemonBuildMismatch warns only when the running daemon reports an identity different from this process's own. It fails open everywhere else: an empty body (an old daemon binary predating the feature, which still answers /ping with an empty 200) and an unreachable daemon both mean no warning, since this is a diagnostic and must never warn incorrectly on its own failure.
func TestCheckDaemonBuildMismatch(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		closeServer bool
		wantWarning bool
	}{
		{name: "same identity", body: buildIdentity},
		{name: "different identity", body: "some-other-build-identity", wantWarning: true},
		{name: "empty body"},
		{name: "unreachable daemon", closeServer: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.body))
			}))
			if tc.closeServer {
				srv.Close()
			} else {
				defer srv.Close()
			}

			got := checkDaemonBuildMismatch(http.DefaultClient, srv.URL)
			if (got != "") != tc.wantWarning {
				t.Fatalf("checkDaemonBuildMismatch = %q, want a warning: %v", got, tc.wantWarning)
			}
			if tc.wantWarning && !strings.Contains(got, "older build") {
				t.Errorf("expected the warning to mention an older build, got %q", got)
			}
		})
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
