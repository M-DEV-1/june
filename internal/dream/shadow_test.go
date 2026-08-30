package dream

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestShadowHelperProcess is not a real test — it is the child process the tests below spawn, standing in for llama-server. It serves /health (refusing readiness for healthDelayMs, standing in for a model load) and runs until killed, so the wait/kill assertions exercise a real process instead of a mock.
func TestShadowHelperProcess(t *testing.T) {
	port := ""
	healthDelay := time.Duration(0)
	for i, a := range os.Args {
		if a == "shadow-helper" && i+1 < len(os.Args) {
			port = os.Args[i+1]
			if i+2 < len(os.Args) {
				if ms, err := strconv.Atoi(os.Args[i+2]); err == nil {
					healthDelay = time.Duration(ms) * time.Millisecond
				}
			}
		}
	}
	if port == "" {
		t.Skip("not running as the spawned helper")
	}
	started := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(started) < healthDelay {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.ListenAndServe("127.0.0.1:"+port, mux)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// spawnHelper starts the helper process above, standing in for llama-server, and returns the command and its exited channel.
func spawnHelper(t *testing.T, port int, healthDelay time.Duration) (*exec.Cmd, chan struct{}) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestShadowHelperProcess", "shadow-helper", fmt.Sprint(port), fmt.Sprint(healthDelay.Milliseconds()))
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawning the helper: %v", err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	return cmd, exited
}

// waitHealthy returns once a server that is already answering /health, without waiting out its timeout.
func TestWaitHealthy_ReturnsAsSoonAsHealthy(t *testing.T) {
	port := freePort(t)
	cmd, exited := spawnHelper(t, port, 0)
	defer killShadowChild(cmd, exited)

	err := waitHealthy(context.Background(), fmt.Sprintf("http://127.0.0.1:%d", port), exited, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A server that is slow to answer /health (standing in for a model load) is still waited for, up to the timeout.
func TestWaitHealthy_WaitsOutASlowLoad(t *testing.T) {
	port := freePort(t)
	cmd, exited := spawnHelper(t, port, 200*time.Millisecond)
	defer killShadowChild(cmd, exited)

	err := waitHealthy(context.Background(), fmt.Sprintf("http://127.0.0.1:%d", port), exited, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A server that never becomes healthy within the timeout is reported as failed, not waited on forever.
func TestWaitHealthy_TimesOut(t *testing.T) {
	port := freePort(t)
	cmd, exited := spawnHelper(t, port, time.Hour)
	defer killShadowChild(cmd, exited)

	err := waitHealthy(context.Background(), fmt.Sprintf("http://127.0.0.1:%d", port), exited, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

// A cancelled context stops the wait immediately rather than riding out the full timeout.
func TestWaitHealthy_RespectsContextCancellation(t *testing.T) {
	port := freePort(t)
	cmd, exited := spawnHelper(t, port, time.Hour)
	defer killShadowChild(cmd, exited)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := waitHealthy(ctx, fmt.Sprintf("http://127.0.0.1:%d", port), exited, time.Minute)
	if err == nil {
		t.Fatal("expected the cancelled context to fail the wait")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("waitHealthy took %s after cancellation, want it to return promptly", time.Since(start))
	}
}

// killShadowChild actually terminates the process — the real assertion the fixed timeout in cli.go's runCLI-style wait can't make on its own.
func TestKillShadowChild_TerminatesTheProcess(t *testing.T) {
	port := freePort(t)
	cmd, exited := spawnHelper(t, port, 0)
	if err := waitHealthy(context.Background(), fmt.Sprintf("http://127.0.0.1:%d", port), exited, 5*time.Second); err != nil {
		t.Fatalf("helper never became healthy: %v", err)
	}

	killShadowChild(cmd, exited)

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the child was still alive after killShadowChild returned")
	}
}

// A nil cmd is a no-op, which is what a ShadowLifecycle.Stop called before any Start ever ran hits.
func TestKillShadowChild_NilIsNoop(t *testing.T) {
	killShadowChild(nil, nil)
}

// An empty device leaves the choice to llama-server: no -dev flag at all.
func TestShadowArgs_NoDeviceOmitsDevFlag(t *testing.T) {
	args := shadowArgs("/models/model.gguf", 6944, "")
	for i, a := range args {
		if a == "-dev" {
			t.Fatalf("unexpected -dev flag at args[%d]: %v", i, args)
		}
	}
}

// A configured device is passed through as llama-server's -dev flag, so a machine with more than one Vulkan
// device (e.g. an Intel iGPU and a discrete NVIDIA card) can be pinned to the fast one instead of whichever
// llama-server picks by default.
func TestShadowArgs_DeviceSetAppendsDevFlag(t *testing.T) {
	args := shadowArgs("/models/model.gguf", 6944, "Vulkan1")
	for i, a := range args {
		if a == "-dev" {
			if i+1 >= len(args) || args[i+1] != "Vulkan1" {
				t.Fatalf("-dev not followed by Vulkan1: %v", args)
			}
			return
		}
	}
	t.Fatalf("expected -dev flag in args: %v", args)
}
