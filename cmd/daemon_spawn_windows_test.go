package cmd_test

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const daemonTestPort = "6942"

var testPingClient = &http.Client{Timeout: 1 * time.Second}

func isDaemonUp() bool {
	resp, err := testPingClient.Get("http://127.0.0.1:" + daemonTestPort + "/ping")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// port blocked by smth else or nahs
func isPortBound() bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+daemonTestPort, 150*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// finds PID of blocker process and kills it, could be zombie daemons from prev runs
func killPortOwner(t *testing.T) {
	t.Helper()
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, ":"+daemonTestPort) && strings.Contains(line, "LISTENING") {
			fields := strings.Fields(line)
			if len(fields) >= 5 {
				pid := strings.TrimSpace(fields[len(fields)-1])
				t.Logf("killing PID %s holding port %s", pid, daemonTestPort)
				exec.Command("taskkill", "/F", "/PID", pid).Run()
				// wait for OS to release the port
				time.Sleep(300 * time.Millisecond)
			}
		}
	}
}

// buildOraBinary compiles the module into a temp binary and returns its path.
// Test working dir is cmd/, so ".." is the module root.
func buildOraBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "ora_test.exe")

	cmd := exec.Command("go", "build", "-o", bin, "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	return bin
}

func netstatPort(t *testing.T, label string) {
	t.Helper()
	out, err := exec.Command("netstat", "-ano").Output()
	if err != nil {
		t.Logf("[%s] netstat error: %v", label, err)
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, daemonTestPort) {
			t.Logf("[%s] netstat: %s", label, strings.TrimSpace(line))
		}
	}
}

func TestDaemonSpawnsAndResponds(t *testing.T) {
	netstatPort(t, "initial")

	if isDaemonUp() {
		t.Skip("responsive daemon already on port 69420 — skipping to avoid conflict")
	}

	if isPortBound() {
		t.Log("port 69420 is bound but not serving — killing port owner")
		killPortOwner(t)
		netstatPort(t, "after-kill")
		if isPortBound() {
			t.Skip("could not free port 69420 — kill any running ora.exe manually and retry")
		}
	}

	bin := buildOraBinary(t)

	// give the daemon a clean working dir so ora-db/ doesn't pollute the repo
	workDir := t.TempDir()

	cmd := exec.Command(bin, "--daemon")
	cmd.Dir = workDir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	// output for diagnostics if the test fails.
	outFile, err := os.CreateTemp("", "ora-daemon-*.log")
	if err == nil {
		cmd.Stdout = outFile
		cmd.Stderr = outFile
		defer outFile.Close()
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start daemon process: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		// daemon stdout/stderr (pre-slog-redirect lines)
		if outFile != nil {
			outFile.Seek(0, 0)
			buf := make([]byte, 4096)
			n, _ := outFile.Read(buf)
			if n > 0 {
				t.Logf("daemon stderr/stdout:\n%s", buf[:n])
			}
		}

		if logData, err := os.ReadFile(filepath.Join(workDir, "ora-db", "ora.log")); err == nil {
			t.Logf("daemon ora.log:\n%s", logData)
		}
	}()

	// poll up to 15s, each attempt has its own 300ms timeout so we never hang
	deadline := time.Now().Add(15 * time.Second)
	attempts := 0
	for time.Now().Before(deadline) {
		attempts++
		if isDaemonUp() {
			t.Logf("daemon /ping succeeded after %d attempts (~%dms)", attempts, attempts*100)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	netstatPort(t, "after-timeout")
	t.Fatalf("daemon did not respond to /ping within 15s after %d attempts", attempts)
}

func TestDaemonPingEndpoint(t *testing.T) {
	if !isDaemonUp() {
		t.Skip("no daemon running — run TestDaemonSpawnsAndResponds first or start daemon manually")
	}

	resp, err := testPingClient.Get(fmt.Sprintf("http://127.0.0.1:%s/ping", daemonTestPort))
	if err != nil {
		t.Fatalf("ping failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
