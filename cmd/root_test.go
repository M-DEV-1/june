package cmd

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"ora/internal/config"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// TestFileIdentity_DiffersAfterRebuild verifies calling fileIdentity twice on the same unchanged file returns the same non-"unknown" string — the daemon and a freshly-relaunched client built from the same binary must agree when nothing has actually changed — that it changes when the file is overwritten (a rebuild), which is the whole point: an old daemon process holds an identity captured at ITS startup, while a freshly-relaunched client reads whatever's on disk now — and that a stat failure on a missing file degrades to "unknown" instead of panicking or erroring out, since this is a diagnostic, not something that should ever block startup.
func TestFileIdentity_DiffersAfterRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ora-bin")
	if err := os.WriteFile(path, []byte("old build"), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before := fileIdentity(path)
	if before != fileIdentity(path) {
		t.Errorf("expected a stable identity for an unchanged file")
	}
	if before == "unknown" {
		t.Errorf("expected a real identity for a file that exists, got %q", before)
	}

	if err := os.WriteFile(path, []byte("a new, longer build of ora"), 0755); err != nil {
		t.Fatalf("WriteFile (rebuild): %v", err)
	}
	after := fileIdentity(path)
	if before == after {
		t.Errorf("expected a different identity after the file was rewritten, got the same %q both times", before)
	}

	if got := fileIdentity(filepath.Join(t.TempDir(), "does-not-exist")); got != "unknown" {
		t.Errorf(`expected "unknown" for a missing file, got %q`, got)
	}
}

// TestFormatHotkey verifies the GNOME accelerator gsettings reports turns into the plain text a terminal hint prints.
func TestFormatHotkey(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"<Control><Alt>space", "Ctrl+Alt+Space"},
		{"<Super>a", "Super+A"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := formatHotkey(tc.raw); got != tc.want {
			t.Errorf("formatHotkey(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns everything fn printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = old
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// fakeDaemon stands a real HTTP server in for the daemon on DaemonPort (offerWindow and its helpers build their URLs from that package var directly, not from an injectable client), so this is what "the daemon ping faked" means for this file: a real listener on the port the code under test actually dials, restored once the test ends. Input: the mux to serve. Output: none — DaemonPort is left pointed at it until t's cleanup runs.
func fakeDaemon(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	oldPort := DaemonPort
	DaemonPort = strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	t.Cleanup(func() {
		srv.Close()
		DaemonPort = oldPort
	})
}

// isolateConfig points config.DataDir (and so ConfigPath) at a fresh temp directory, so LoadConfig/SaveConfig in a test never touch the real machine's config file.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("ORA_DATA_DIR", t.TempDir())
}

// A machine with no window built is told exactly where ora looked and how to fix it, and offerWindow leaves the terminal UI as the caller's job.
func TestOfferWindow_NoWindowBinary_PrintsHintAndDefersToTUI(t *testing.T) {
	isolateConfig(t)
	t.Setenv("ORA_WINDOW", filepath.Join(t.TempDir(), "not-here"))

	var took bool
	out := captureStdout(t, func() { took = offerWindow(false) })

	if took {
		t.Error("offerWindow should leave startup to the terminal UI when nothing is built")
	}
	if !strings.Contains(out, "ORA_WINDOW") {
		t.Errorf("expected the hint to mention ORA_WINDOW, got %q", out)
	}
	if !strings.Contains(out, "not-here") {
		t.Errorf("expected the hint to name the path it tried, got %q", out)
	}
}

// A config with the window turned off runs the terminal UI without printing anything about a window at all — this is the user's own choice, not a missing binary.
func TestOfferWindow_WindowTurnedOff_SilentlyDefersToTUI(t *testing.T) {
	isolateConfig(t)
	if err := config.SaveConfig(config.OraConfig{Window: false}); err != nil {
		t.Fatal(err)
	}

	var took bool
	out := captureStdout(t, func() { took = offerWindow(false) })

	if took {
		t.Error("offerWindow should defer to the terminal UI when the config has the window off")
	}
	if out != "" {
		t.Errorf("expected no output when the window is turned off by config, got %q", out)
	}
}

// A window binary that exists is shown rather than the terminal UI opened, and the printed line names the binary and the accelerator that brings a hidden window back.
func TestOfferWindow_WindowFound_ShowsItAndReportsTheHotkey(t *testing.T) {
	isolateConfig(t)
	binary := writeExecutable(t, filepath.Join(t.TempDir(), "ora-window"))
	t.Setenv("ORA_WINDOW", binary)

	var opens int32
	mux := http.NewServeMux()
	mux.HandleFunc("/window", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&opens, 1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/settings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"hotkey":"<Control><Alt>space"}`))
	})
	fakeDaemon(t, mux)

	var took bool
	out := captureStdout(t, func() { took = offerWindow(false) })

	if !took {
		t.Fatal("offerWindow should take over startup when a window binary exists and the config wants one")
	}
	if !strings.Contains(out, binary) {
		t.Errorf("expected the line to name the binary %q, got %q", binary, out)
	}
	if !strings.Contains(out, "Ctrl+Alt+Space") {
		t.Errorf("expected the line to report the hotkey, got %q", out)
	}
	if !strings.Contains(out, "ora --tui") {
		t.Errorf("expected the line to mention the --tui fallback, got %q", out)
	}
	if got := atomic.LoadInt32(&opens); got != 1 {
		t.Errorf("expected exactly one show request for an already-running daemon, got %d", got)
	}
}

// A daemon this process just spawned may not have its window listening yet, so offerWindow retries the show instruction rather than sending it once and hoping.
func TestOfferWindow_FreshDaemon_RetriesTheShowInstruction(t *testing.T) {
	isolateConfig(t)
	binary := writeExecutable(t, filepath.Join(t.TempDir(), "ora-window"))
	t.Setenv("ORA_WINDOW", binary)

	oldInterval := openRetryInterval
	openRetryInterval = time.Millisecond
	t.Cleanup(func() { openRetryInterval = oldInterval })

	var opens int32
	mux := http.NewServeMux()
	mux.HandleFunc("/window", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&opens, 1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/settings", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"hotkey":""}`))
	})
	fakeDaemon(t, mux)

	if took := offerWindow(true); !took {
		t.Fatal("offerWindow should take over startup when a window binary exists")
	}
	if got := atomic.LoadInt32(&opens); got != freshDaemonOpenAttempts {
		t.Errorf("expected %d retries for a freshly spawned daemon, got %d", freshDaemonOpenAttempts, got)
	}
}

// fetchWindowHotkey degrades to "" rather than erroring when the daemon can't be reached at all — offerWindow falls back to a generic hint in that case, and startup must never block or fail on this.
func TestFetchWindowHotkey_UnreachableDaemon_ReturnsEmpty(t *testing.T) {
	oldPort := DaemonPort
	DaemonPort = "1" // nothing listens on a privileged low port in a test sandbox
	t.Cleanup(func() { DaemonPort = oldPort })

	if got := fetchWindowHotkey(); got != "" {
		t.Errorf("fetchWindowHotkey() = %q, want empty when the daemon is unreachable", got)
	}
}
