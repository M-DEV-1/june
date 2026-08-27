package embed

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestEngineHelperProcess is not a real test — it is the child process Engine spawns in the tests below, standing in for llama-server. It serves the same two endpoints Engine depends on (/health for readiness, /v1/embeddings for the embed itself) and runs until it is killed, which is what makes the spawn/reap/respawn assertions real rather than mocked.
// It exits immediately unless invoked with the "embed-helper <port>" arguments the tests pass, so `go test` running it as a normal test is a no-op.
func TestEngineHelperProcess(t *testing.T) {
	port := ""
	for i, a := range os.Args {
		if a == "embed-helper" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	if port == "" {
		t.Skip("not running as the spawned helper")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.5, 0.5}, "index": 0}},
		})
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
	t.Helper()
	port := freePort(t)
	e := NewEngine(os.Args[0], []string{"-test.run=TestEngineHelperProcess", "embed-helper", fmt.Sprint(port)},
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

	if e.Running() {
		t.Fatal("engine spawned the child before any embed request")
	}

	vec, err := e.Embed(context.Background(), TaskRetrievalQuery, "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 2 {
		t.Fatalf("got %d dimensions, want 2 from the helper", len(vec))
	}
	if !e.Running() {
		t.Fatal("engine should be running after an embed")
	}
}

func TestEngineIdleShutdown(t *testing.T) {
	e := newTestEngine(t, 150*time.Millisecond, 0)

	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	waitFor(t, 5*time.Second, "the idle child to be reaped", func() bool { return !e.Running() })
}

func TestEngineClientPresencePinsTheChild(t *testing.T) {
	e := newTestEngine(t, 100*time.Millisecond, 10*time.Second)
	e.MarkClientPresence()

	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello"); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	// Well past the idle timeout: a client seen inside the presence window must keep the child alive anyway.
	time.Sleep(600 * time.Millisecond)
	if !e.Running() {
		t.Fatal("child was reaped while a client was present")
	}
}

func TestEngineWarmSpawnsWithoutAnEmbed(t *testing.T) {
	e := newTestEngine(t, time.Hour, time.Hour)
	e.MarkClientPresence()

	if err := e.Warm(context.Background()); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if !e.Running() {
		t.Fatal("Warm should have spawned the child")
	}
}

func TestEngineRespawnsAfterChildDies(t *testing.T) {
	e := newTestEngine(t, time.Hour, time.Hour)

	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello"); err != nil {
		t.Fatalf("first Embed: %v", err)
	}
	firstPID := e.pid()

	e.killChildForTest()
	waitFor(t, 5*time.Second, "the child to be seen as gone", func() bool { return !e.Running() })

	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello again"); err != nil {
		t.Fatalf("Embed after the child died: %v", err)
	}
	if !e.Running() {
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
	if e.Running() {
		t.Fatal("child still running after Close")
	}
	if _, err := e.Embed(context.Background(), TaskRetrievalDocument, "hello"); err == nil {
		t.Fatal("Embed after Close should fail rather than resurrect the child")
	}
}

func TestEngineSatisfiesEmbedder(t *testing.T) {
	var _ Embedder = NewEngine("/bin/true", nil, "http://127.0.0.1:6943", "m", time.Minute)
}
