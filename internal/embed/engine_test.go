package embed

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"os"
	"strconv"
	"testing"
	"time"
)

// TestEngineHelperProcess is not a real test — it is the child process Engine spawns in the tests below, standing in for llama-server. It serves the same two endpoints Engine depends on (/health for readiness, /v1/embeddings for the embed itself) and runs until it is killed, which is what makes the spawn/reap/respawn assertions real rather than mocked.
// It exits immediately unless invoked with the "embed-helper <port> [<healthDelayMs>]" arguments the tests pass, so `go test` running it as a normal test is a no-op. healthDelayMs stands in for a model load: /health answers 503 until that many milliseconds have passed since the process started.
func TestEngineHelperProcess(t *testing.T) {
	port := ""
	healthDelay := time.Duration(0)
	for i, a := range os.Args {
		if a == "embed-helper" && i+1 < len(os.Args) {
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
	// llama-server refuses to start on a device it cannot see, which is what a named GPU does once its driver stops loading.
	for _, a := range os.Args {
		if a == "--device" {
			os.Exit(1)
		}
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
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.5, 0.5}, "index": 0}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "the local reply"}}}})
	})
	http.ListenAndServe("127.0.0.1:"+port, mux)
}

// freePort asks the kernel for an unused loopback port so parallel test runs never collide on a fixed one.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// newTestEngine builds an Engine that spawns the helper process above instead of llama-server, with the timers wound down so idle-shutdown tests finish in milliseconds.
func newTestEngine(t *testing.T, idle, presence time.Duration) *Engine {
	return newTestEngineWithHealthDelay(t, idle, presence, 0)
}

// newTestEngineWithHealthDelay is newTestEngine with a child that refuses readiness for healthDelay after it starts, standing in for llama-server loading a model.
func newTestEngineWithHealthDelay(t *testing.T, idle, presence, healthDelay time.Duration) *Engine {
	t.Helper()
	port := freePort(t)
	e := newEngine(os.Args[0], []string{"-test.run=TestEngineHelperProcess", "embed-helper", fmt.Sprint(port), fmt.Sprint(healthDelay.Milliseconds())},
		fmt.Sprintf("http://127.0.0.1:%d", port), "test-model", idle)
	e.presenceWindow = presence
	e.startupTimeout = 15 * time.Second
	e.pollInterval = 20 * time.Millisecond
	t.Cleanup(func() { e.Close() })
	return e
}

// waitFor polls cond until it holds or the deadline passes, so the tests never depend on a fixed sleep.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

func TestEngineSpawnsLazilyOnFirstEmbed(t *testing.T) {
	e := newTestEngine(t, time.Hour, time.Hour)

	if e.running() {
		t.Fatal("engine spawned the child before any embed request")
	}

	vec, err := e.Embed(context.Background(), TaskRetrievalQuery, "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 2 {
		t.Fatalf("got %d dimensions, want 2 from the helper", len(vec))
	}
	if !e.running() {
		t.Fatal("engine should be running after an embed")
	}
}

func TestEngineIdleShutdown(t *testing.T) {
	e := newTestEngine(t, 150*time.Millisecond, 0)

	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	waitFor(t, 5*time.Second, "the idle child to be reaped", func() bool { return !e.running() })
}

func TestEngineClientPresencePinsTheChild(t *testing.T) {
	e := newTestEngine(t, 100*time.Millisecond, 10*time.Second)
	e.MarkClientPresence(context.Background())

	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello"); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	// Well past the idle timeout: a client seen inside the presence window must keep the child alive anyway.
	time.Sleep(600 * time.Millisecond)
	if !e.running() {
		t.Fatal("child was reaped while a client was present")
	}
}

func TestEngineRespawnsAfterChildDies(t *testing.T) {
	e := newTestEngine(t, time.Hour, time.Hour)

	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello"); err != nil {
		t.Fatalf("first Embed: %v", err)
	}
	firstPID := e.pid()

	e.killChildForTest()
	waitFor(t, 5*time.Second, "the child to be seen as gone", func() bool { return !e.running() })

	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello again"); err != nil {
		t.Fatalf("Embed after the child died: %v", err)
	}
	if !e.running() {
		t.Fatal("engine did not respawn the child")
	}
	if e.pid() == firstPID {
		t.Fatal("engine reported the same PID after a respawn")
	}
}

func TestEngineCloseKillsTheChildAndRefusesFurtherEmbeds(t *testing.T) {
	e := newTestEngine(t, time.Hour, time.Hour)

	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if e.running() {
		t.Fatal("child still running after Close")
	}
	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello"); err == nil {
		t.Fatal("Embed after Close should fail rather than resurrect the child")
	}
}

// TestEngineColdStartSurvivesACallerDeadline is the /embed-under-a-3s-retrieve-budget case: the daemon passes the client's request context into Embed, so a cold start that takes longer than that budget must return the caller its deadline error while the child keeps loading — not kill the child, which turns every retry into another spawn-wait-kill cycle and never reaches a loaded model.
func TestEngineColdStartSurvivesACallerDeadline(t *testing.T) {
	e := newTestEngineWithHealthDelay(t, time.Hour, time.Hour, 1500*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := e.ensureUp(ctx); err == nil {
		t.Fatal("Warm should have returned the caller's deadline error")
	}

	if !e.running() {
		t.Fatal("the loading child was killed by the caller's cancelled context")
	}
	pidWhileLoading := e.pid()

	if err := e.ensureUp(context.Background()); err != nil {
		t.Fatalf("second Warm, with a patient context: %v", err)
	}
	if e.pid() != pidWhileLoading {
		t.Fatalf("engine respawned instead of adopting the still-loading child: %d then %d", pidWhileLoading, e.pid())
	}
}

// TestEngineColdStartDoesNotBlockPresenceOrRunning covers the IPC-wide stall: MarkClientPresence and Running are called by the daemon's auth wrapper on every authenticated request, including /buffer with its 300ms client budget, so neither may wait on an in-flight spawn and model load.
func TestEngineColdStartDoesNotBlockPresenceOrRunning(t *testing.T) {
	e := newTestEngineWithHealthDelay(t, time.Hour, time.Hour, 2*time.Second)

	go e.ensureUp(context.Background())
	// Polled over HTTP rather than through e.pid(), which takes the same lock this test is about and would hide the stall.
	waitFor(t, 5*time.Second, "the child's HTTP server to answer at all", func() bool {
		resp, err := http.Get(e.baseURL + "/health")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	})

	start := time.Now()
	e.MarkClientPresence(context.Background())
	e.running()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("MarkClientPresence+Running blocked for %v during a cold start; every authenticated IPC request pays that", elapsed)
	}
}

// StopIfIdle frees the GPU for a heavier job: with no client inside the presence window the child dies now and the next embed just respawns it; with a client pinned it stays up.
func TestEngineStopIfIdle(t *testing.T) {
	e := newTestEngine(t, time.Hour, time.Hour)

	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello"); err != nil {
		t.Fatalf("first Embed: %v", err)
	}
	if !e.StopIfIdle() {
		t.Fatal("an engine no client has touched must stop when asked")
	}
	waitFor(t, 5*time.Second, "the child to be gone", func() bool { return !e.running() })

	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello again"); err != nil {
		t.Fatalf("Embed after StopIfIdle: %v", err)
	}
	e.MarkClientPresence(context.Background())
	if e.StopIfIdle() {
		t.Fatal("an engine pinned by a recent client must refuse to stop")
	}
	if !e.running() {
		t.Fatal("the pinned engine was stopped anyway")
	}
}

// On 2026-09-23 a driver upgrade left the RTX 3050 out of Vulkan until a reboot, llama-server refused "--device Vulkan1" on every start, and every embedding failed. A device that is not there is dropped so the server starts on whatever it can find, and embeddings keep working.
func TestEngineStartsWithoutADeviceThatIsNotThere(t *testing.T) {
	port := freePort(t)
	e := newEngine(os.Args[0], []string{"-test.run=TestEngineHelperProcess", "embed-helper", fmt.Sprint(port), "0", "--device", "Vulkan1"},
		fmt.Sprintf("http://127.0.0.1:%d", port), "test-model", time.Hour)
	e.startupTimeout = 15 * time.Second
	e.pollInterval = 20 * time.Millisecond
	t.Cleanup(func() { e.Close() })

	e.Embed(context.Background(), TaskRetrievalQuery, "hello")
	if _, err := e.Embed(context.Background(), TaskRetrievalQuery, "hello"); err != nil {
		t.Fatalf("Embed after the device refused = %v, want the server started without it", err)
	}
}
