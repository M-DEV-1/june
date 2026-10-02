package embed

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
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

// The local text server is the card's largest tenant — 1853 MiB of a 4096 MiB RTX 3050 on 2026-09-15, against the embedding server's 653 MiB — and it was the one tenant nothing could evict. The embedding server had both halves of the protocol (whisper asks it to leave, and a gate keeps it off the card for the whole decode) and this engine, added later, had neither. A meeting that should have transcribed in 15 minutes took 50, because whisper found ~788 MiB free, died out of device memory and redid the whole decode on the CPU.
func TestTextEngine_YieldsTheCardAndStaysOffItForTheWholeDecode(t *testing.T) {
	e := newTestTextEngine(t, time.Hour)
	busy := false
	e.SetGPUGate(func() bool { return busy })

	if _, err := e.Generate(context.Background(), "hello"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !e.running() {
		t.Fatal("the server should be up before the decode starts")
	}

	// whisper claims the card and asks its tenants to leave.
	busy = true
	if !e.StopIfIdle() {
		t.Fatal("the text server did not yield the card to a transcription")
	}
	if e.running() {
		t.Error("the text server is still holding its GPU memory after yielding")
	}

	// The working-state derive ticks every five minutes, so something asks again mid-decode. It must not climb back onto the card.
	if _, err := e.Generate(context.Background(), "the derive tick"); err == nil {
		t.Error("a generate during a decode started the server again, putting a second model back on the card")
	}
	if e.running() {
		t.Error("the text server climbed back onto the card mid-decode")
	}

	// Once the decode is done the next derive brings it back.
	busy = false
	if _, err := e.Generate(context.Background(), "after the decode"); err != nil {
		t.Errorf("the text server did not come back after the decode: %v", err)
	}
}
