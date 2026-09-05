package embed

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"ora/internal/brain"
	"ora/internal/config"
)

// TextEngine owns the local text-generation llama-server as a child process, the same way Engine owns the embedding one, so ORA's unattended text jobs answer from a local model instead of spending metered API quota.
// Lifecycle: spawned on the first Generate, killed once idle past idle with no Generate, respawned on the next Generate if it died or was reaped, and killed unconditionally on Close. Unlike Engine there is no client-presence concept — only the idle timer decides when the server goes down.
type TextEngine struct {
	binary  string
	args    []string
	baseURL string
	call    brain.Brain

	// idle is how long the server may sit unused before it is killed. startupTimeout bounds the readiness wait after a spawn and pollInterval is how often readiness is retried; both are fields rather than constants so the tests can wind them down.
	idle           time.Duration
	startupTimeout time.Duration
	pollInterval   time.Duration

	mu      sync.Mutex
	cmd     *exec.Cmd
	exited  chan struct{}
	start   *startState
	lastUse time.Time
	closed  bool
}

// NewTextEngine returns the engine for the local text model cfg names, or nil when no local text model is configured (LocalText.Enabled is false). Input: the whole app config, for the fallbacks LocalTextConfig resolves against. Output: the engine, or nil. Nothing is spawned until the first Generate.
func NewTextEngine(cfg config.OraConfig) *TextEngine {
	if !cfg.LocalText.Enabled(cfg) {
		return nil
	}
	port := cfg.LocalText.LocalTextPort()
	args := []string{
		"-m", cfg.LocalText.ResolvedModelPath(cfg),
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		// Measured rather than guessed: one real working-state prompt built from this store — five stable notes and eight recent summaries — came to 29,293 characters, which llama-server tokenised as 6,757 tokens. The production prompt is larger again (ten summaries, ten notes and the live threads), so 8192 overflows and the server answers 400. Thirty-two thousand leaves room for the largest prompt the job can build and for the reply after it.
		"-c", "32768",
		"-ngl", "99",
	}
	if dev := cfg.LocalText.ResolvedDevice(cfg); dev != "" {
		args = append(args, "-dev", dev)
	}
	// IdleTimeout is a count of milliseconds stored in a time.Duration, exactly like EmbedConfig.IdleTimeout, so it is converted to a real duration by multiplying by time.Millisecond.
	return newTextEngine(cfg.LocalText.ResolvedBinary(cfg), args, cfg.LocalText.BaseURL(), cfg.LocalText.Timeout(), cfg.LocalText.Idle()*time.Millisecond)
}

// newTextEngine wires a TextEngine to the server binary and the arguments that make it listen at baseURL, with timeoutSeconds bounding each Generate call through brain.LlamaServer and idle as the no-use shutdown timeout. NewTextEngine is the production entry point; this exists separately so the tests can spawn a stand-in process with their own arguments.
func newTextEngine(binary string, args []string, baseURL string, timeoutSeconds int, idle time.Duration) *TextEngine {
	return &TextEngine{
		binary:  binary,
		args:    args,
		baseURL: baseURL,
		call:    brain.LlamaServer(baseURL, timeoutSeconds),

		idle: idle,
		// A 3.3 GB instruction-tuned GGUF can take a while to load from disk, longer than the embedding model's startup budget.
		startupTimeout: 120 * time.Second,
		pollInterval:   100 * time.Millisecond,
	}
}

// Generate answers one prompt on the local model, spawning the server first if it is not already up. Input: the prompt. Output: the model's reply, or an error when the server could not be started or the call failed. Safe to call on a nil *TextEngine, which returns an error rather than panicking.
func (e *TextEngine) Generate(ctx context.Context, prompt string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("text engine: no local text model configured")
	}
	if err := e.ensureUp(ctx); err != nil {
		return "", err
	}
	e.mu.Lock()
	e.lastUse = time.Now()
	e.mu.Unlock()
	return e.call(ctx, prompt)
}

// running reports whether the child process is up right now.
func (e *TextEngine) running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.aliveLocked()
}

// StopIfIdle kills the child now so its GPU memory can go to a heavier job, and reports whether the server is down when it returns. Not final: the next Generate spawns it again. Safe to call on a nil *TextEngine, which has nothing running and so reports true.
func (e *TextEngine) StopIfIdle() bool {
	if e == nil {
		return true
	}
	e.mu.Lock()
	cmd, exited := e.detachLocked()
	e.mu.Unlock()
	if cmd != nil {
		slog.Info("stopping the local text server to free its GPU memory")
	}
	killChild(cmd, exited)
	return true
}

// Close kills the child for good and puts the TextEngine into a state where further generations fail rather than resurrecting it. Safe to call on a nil *TextEngine, which has nothing to kill.
func (e *TextEngine) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	e.closed = true
	cmd, exited := e.detachLocked()
	e.mu.Unlock()
	killChild(cmd, exited)
	return nil
}

// aliveLocked reports whether a child has been started and has not exited. Caller holds e.mu.
func (e *TextEngine) aliveLocked() bool {
	if e.cmd == nil {
		return false
	}
	select {
	case <-e.exited:
		return false
	default:
		return true
	}
}

// ensureUp makes sure a loaded server is up, and returns only once one is (or the attempt failed, or ctx ran out). The spawn and the readiness wait happen on their own goroutine under a background context, so a caller giving up takes nothing down with it: the child keeps loading and the next caller joins the same wait.
func (e *TextEngine) ensureUp(ctx context.Context) error {
	start, err := e.beginStart()
	if err != nil || start == nil {
		return err
	}
	select {
	case <-start.done:
		return start.err
	case <-ctx.Done():
		return fmt.Errorf("text engine: waiting for server: %w", ctx.Err())
	}
}

// beginStart returns the in-flight (or newly launched) start to wait on, or nil when a loaded server is already up. Input: nothing. Output: the start to wait on, or an error if the TextEngine is closed or the child could not be spawned at all. e.mu is held only for the spawn itself, never for the readiness wait.
func (e *TextEngine) beginStart() (*startState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return nil, fmt.Errorf("text engine: closed")
	}
	if e.start != nil {
		return e.start, nil
	}
	if e.aliveLocked() {
		return nil, nil
	}

	cmd := exec.Command(e.binary, e.args...)
	// The server's own logs go to the daemon's stderr, which is already redirected to ora.log — a silently failing model load is otherwise invisible.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = childProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("text engine: start %s: %w", e.binary, err)
	}

	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	e.cmd, e.exited = cmd, exited
	e.lastUse = time.Now()
	slog.Info("local text server started", "pid", cmd.Process.Pid, "url", e.baseURL)

	start := &startState{done: make(chan struct{})}
	e.start = start
	go e.finishStart(start, exited)
	return start, nil
}

// finishStart waits for the freshly spawned child to answer /health and publishes the outcome to everyone waiting on start. A child that never became ready is killed here — that is the only cancellation that may kill it, never a caller's.
func (e *TextEngine) finishStart(start *startState, exited chan struct{}) {
	err := e.waitReady(exited)

	e.mu.Lock()
	var cmd *exec.Cmd
	var dead chan struct{}
	if err != nil {
		cmd, dead = e.detachLocked()
	}
	e.start = nil
	e.mu.Unlock()

	start.err = err
	close(start.done)

	if err != nil {
		killChild(cmd, dead)
		return
	}
	go e.reap(exited)
}

// waitReady polls the server's /health until it answers, the child exits, or startupTimeout passes. It deliberately takes no caller context: a model load outlives any single request's budget.
func (e *TextEngine) waitReady(exited chan struct{}) error {
	ctx := context.Background()
	deadline := time.Now().Add(e.startupTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-exited:
			return fmt.Errorf("text engine: server exited during startup")
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/health", nil)
		if err != nil {
			return fmt.Errorf("text engine: build health request: %w", err)
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("text engine: server did not become ready at %s within %v", e.baseURL, e.startupTimeout)
		}
		time.Sleep(e.pollInterval)
	}
}

// reap watches one child and kills it once it has been idle past e.idle with no Generate call. Returns as soon as that child is gone, so there is exactly one of these per spawn.
func (e *TextEngine) reap(exited chan struct{}) {
	interval := e.idle / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-exited:
			return
		case <-tick.C:
		}

		e.mu.Lock()
		if e.closed || e.exited != exited || !e.aliveLocked() {
			e.mu.Unlock()
			return
		}
		if time.Since(e.lastUse) < e.idle {
			e.mu.Unlock()
			continue
		}
		slog.Info("local text server idle, shutting it down", "idle_for", time.Since(e.lastUse).Round(time.Second))
		cmd, dead := e.detachLocked()
		e.mu.Unlock()
		killChild(cmd, dead)
		return
	}
}

// detachLocked hands the running child over to the caller and forgets it, so the TextEngine immediately reports as not running while the process is still dying. Caller holds e.mu. Returns nil when nothing is running.
func (e *TextEngine) detachLocked() (*exec.Cmd, chan struct{}) {
	if !e.aliveLocked() {
		e.cmd = nil
		return nil, nil
	}
	cmd, exited := e.cmd, e.exited
	e.cmd = nil
	return cmd, exited
}

// pid returns the running child's process id, or 0 when nothing is running. Used by the tests to prove a respawn produced a genuinely new process.
func (e *TextEngine) pid() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.aliveLocked() {
		return 0
	}
	return e.cmd.Process.Pid
}
