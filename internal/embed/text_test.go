package embed

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"ora/internal/config"
)

// newTestTextEngine builds a TextEngine that spawns the TestEngineHelperProcess stand-in instead of llama-server, with the timers wound down so idle-shutdown tests finish in milliseconds. It reuses the same re-exec trick and helper process the Engine tests use, since the helper now also serves /v1/chat/completions.
func newTestTextEngine(t *testing.T, idle time.Duration) *TextEngine {
	t.Helper()
	port := freePort(t)
	e := newTextEngine(os.Args[0], []string{"-test.run=TestEngineHelperProcess", "embed-helper", fmt.Sprint(port)},
		fmt.Sprintf("http://127.0.0.1:%d", port), 5, idle)
	e.startupTimeout = 15 * time.Second
	e.pollInterval = 20 * time.Millisecond
	t.Cleanup(func() { e.Close() })
	return e
}

// A nil *TextEngine stands for "no local text model configured." The daemon holds one without nil-checking it on every call, so every method must be safe to call on it.
func TestNilTextEngineIsSafe(t *testing.T) {
	var e *TextEngine
	if _, err := e.Generate(context.Background(), "hello"); err == nil {
		t.Fatal("Generate on a nil TextEngine should return an error, not resurrect a server")
	}
	if !e.StopIfIdle() {
		t.Fatal("StopIfIdle on a nil TextEngine should report the server down")
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close on a nil TextEngine should be a no-op, got %v", err)
	}
}

func TestNewTextEngine_NilWithoutAModelConfigured(t *testing.T) {
	if e := NewTextEngine(config.OraConfig{}); e != nil {
		t.Fatal("NewTextEngine should return nil when neither LocalText nor Dream names a model")
	}
}

func TestNewTextEngine_FallsBackToDreamModelPath(t *testing.T) {
	cfg := config.OraConfig{Dream: config.DreamConfig{ModelPath: "dream.gguf"}}
	if e := NewTextEngine(cfg); e == nil {
		t.Fatal("NewTextEngine should fall back to Dream.ModelPath and return a non-nil engine")
	}
}

func TestTextEngineSpawnsLazilyOnFirstGenerate(t *testing.T) {
	e := newTestTextEngine(t, time.Hour)

	if e.running() {
		t.Fatal("engine spawned the child before any Generate call")
	}

	reply, err := e.Generate(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if reply != "the local reply" {
		t.Fatalf("reply = %q, want %q", reply, "the local reply")
	}
	if !e.running() {
		t.Fatal("engine should be running after a Generate")
	}
}

// Two concurrent Generate calls arriving before the server is up must join the same spawn rather than each starting their own, or the second would fail binding the already-claimed port.
func TestTextEngineConcurrentGenerateSpawnsOnlyOneChild(t *testing.T) {
	e := newTestTextEngine(t, time.Hour)

	var wg sync.WaitGroup
	pids := make([]int, 4)
	for i := range pids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := e.Generate(context.Background(), "hello"); err != nil {
				t.Errorf("Generate: %v", err)
				return
			}
			pids[i] = e.pid()
		}(i)
	}
	wg.Wait()

	for i, p := range pids {
		if p == 0 || p != pids[0] {
			t.Fatalf("pid[%d] = %d, want all four concurrent Generate calls to share one pid (got %v)", i, p, pids)
		}
	}
}

func TestTextEngineIdleShutdownAndRespawn(t *testing.T) {
	e := newTestTextEngine(t, 150*time.Millisecond)

	if _, err := e.Generate(context.Background(), "hello"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	firstPID := e.pid()
	waitFor(t, 5*time.Second, "the idle child to be reaped", func() bool { return !e.running() })

	reply, err := e.Generate(context.Background(), "hello again")
	if err != nil {
		t.Fatalf("Generate after reap: %v", err)
	}
	if reply != "the local reply" {
		t.Fatalf("reply = %q, want %q", reply, "the local reply")
	}
	if !e.running() {
		t.Fatal("engine did not respawn the child")
	}
	if e.pid() == firstPID {
		t.Fatal("engine reported the same PID after a respawn")
	}
}
