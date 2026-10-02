package cmd

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestSecureEnvFile_RestrictsPermissions verifies .env (which holds the Gemini API key) gets locked down to 0600 — it commonly defaults to 0644 (world-readable) on a multi-user machine.
func TestSecureEnvFile_RestrictsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("secureEnvFile does nothing on Windows, which has no POSIX permission bits")
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

// fakeDaemon stands a real HTTP server in for the daemon on DaemonPort (showWindow and its helpers build their URLs from that package var directly, not from an injectable client), so this is what "the daemon ping faked" means for this file: a real listener on the port the code under test actually dials, restored once the test ends. Input: the mux to serve. Output: none — DaemonPort is left pointed at it until t's cleanup runs.
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
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
}

// A daemon this process just spawned may not have its window listening yet, so showWindow retries the show instruction rather than sending it once and hoping.
func TestShowWindow_FreshDaemon_RetriesTheShowInstruction(t *testing.T) {
	isolateConfig(t)
	binary := writeExecutable(t, filepath.Join(t.TempDir(), "june-window"))
	t.Setenv("JUNE_WINDOW", binary)

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

	if code := showWindow(true); code != 0 {
		t.Fatalf("showWindow should exit 0 when a window binary exists, got %d", code)
	}
	if got := atomic.LoadInt32(&opens); got != freshDaemonOpenAttempts {
		t.Errorf("expected %d retries for a freshly spawned daemon, got %d", freshDaemonOpenAttempts, got)
	}
}
