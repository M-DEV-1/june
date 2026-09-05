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
	"sync/atomic"
	"time"

	"ora/internal/config"
)

// Engine is a LocalEmbedder that owns the embedding server as a child process. The daemon holds exactly one of these; nothing else may spawn the server, because two copies would fight over the same port.
// Lifecycle: spawned on the first embed (or on the first authenticated client request), kept alive as long as a TUI client has been seen inside presenceWindow, otherwise killed once idle for the configured timeout, respawned on the next embed if it died or was reaped, and killed unconditionally on Close.
type Engine struct {
	binary  string
	args    []string
	baseURL string
	inner   *LocalEmbedder

	// idle is how long the server may sit unused before it is killed, and presenceWindow how recently a client must have been seen for the server to be pinned alive instead. startupTimeout bounds the readiness wait after a spawn and pollInterval is how often readiness is retried; both are fields rather than constants so the tests can wind them down.
	idle           time.Duration
	presenceWindow time.Duration
	startupTimeout time.Duration
	pollInterval   time.Duration

	mu      sync.Mutex
	cmd     *exec.Cmd
	exited  chan struct{}
	start   *startState
	lastUse time.Time
	closed  bool

	// lastClient is atomic rather than guarded by mu because MarkClientPresence runs on every authenticated IPC request, including ones with a 300ms budget, and must never wait behind a spawn, a model load, or a child being killed.
	lastClient atomic.Int64
}

// startState is one in-flight spawn-and-load. err is written before done is closed, so every waiter that observes the close sees it.
type startState struct {
	done chan struct{}
	err  error
}

// defaultPresenceWindow is how long after a client's last authenticated IPC request the embedding server stays pinned in memory. Three minutes: a user pausing mid-conversation should not pay a cold start for their next question.
const defaultPresenceWindow = 3 * time.Minute

// NewEngine builds the daemon-owned embedding engine from cfg, or returns nil when no local embedder is configured — the caller reads nil as "no semantic half at all". Nothing is spawned until the first Embed or client request.
// The server binds loopback only. -c 8192 across --parallel 4 gives each slot 2048 tokens, matching EmbeddingGemma's training context, and the batch sizes must reach that same 2048 or the server rejects a full-length document instead of processing it.
// -ngl 99 offloads every layer to the GPU, which puts the warm model in VRAM instead of system RAM. A build without GPU support ignores the flag and runs on the CPU, so the same arguments work either way.
func NewEngine(cfg config.EmbedConfig) *Engine {
	if !cfg.LocalEnabled() {
		return nil
	}
	args := []string{
		"--embedding",
		"-m", cfg.ModelPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(cfg.Port),
		"-c", "8192",
		"--parallel", "4",
		"-b", "2048",
		"-ub", "2048",
		"-ngl", "99",
	}
	if cfg.Device != "" {
		args = append(args, "--device", cfg.Device)
	}
	return newEngine(cfg.LlamaServer, args, cfg.BaseURL(), config.LocalEmbedModel, cfg.IdleTimeout*time.Millisecond)
}

// newEngine wires an Engine to the server binary and the arguments that make it listen at baseURL, with model sent in each embed request and idle as the no-client shutdown timeout. NewEngine is the production entry point; this exists separately so the tests can spawn a stand-in process with their own arguments.
func newEngine(binary string, args []string, baseURL, model string, idle time.Duration) *Engine {
	return &Engine{
		binary:  binary,
		args:    args,
		baseURL: baseURL,
		inner:   NewLocalEmbedder(baseURL, model),

		idle:           idle,
		presenceWindow: defaultPresenceWindow,
		// EmbeddingGemma-300M loads in a second or two from SSD, but a cold page cache or a busy machine can stretch that, and a spurious timeout here costs an embed.
		startupTimeout: 90 * time.Second,
		pollInterval:   100 * time.Millisecond,
	}
}

// MarkClientPresence records that an authenticated client request just arrived, and starts the server in the background if it is not already up. This is what pins the server in memory while a TUI is running, and what makes the user's first question meet an already-loaded model instead of paying the cold start. The daemon's IPC auth wrapper calls it on every authenticated request, so it never waits on a spawn or a model load — the timestamp write is lock-free and the start runs on its own goroutine.
// Input: a context whose lifetime is the daemon's, used only for the background start. Output: none.
func (e *Engine) MarkClientPresence(ctx context.Context) {
	e.lastClient.Store(time.Now().UnixNano())
	if e.running() {
		return
	}
	go func() {
		if err := e.ensureUp(ctx); err != nil {
			slog.Warn("failed to warm the embedding server for a client", "error", err)
		}
	}()
}

// sinceLastClient is how long ago the last authenticated client request arrived, or a very long time when there has never been one.
func (e *Engine) sinceLastClient() time.Duration {
	ns := e.lastClient.Load()
	if ns == 0 {
		return time.Duration(1<<62 - 1)
	}
	return time.Since(time.Unix(0, ns))
}

// Embed starts the server if needed and then embeds text through it, applying the same EmbeddingGemma prefixes LocalEmbedder does.
func (e *Engine) Embed(ctx context.Context, task TaskType, text string) ([]float32, error) {
	if err := e.ensureUp(ctx); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.lastUse = time.Now()
	e.mu.Unlock()
	return e.inner.Embed(ctx, task, text)
}

// running reports whether the child process is up right now.
func (e *Engine) running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.aliveLocked()
}

// StopIfIdle kills the child now so its GPU memory can go to a heavier job, unless a client has been seen inside the presence window — someone mid-conversation keeps their fast embeds. Unlike Close this is not final: the next embed just spawns the server again. Reports whether the server is down when it returns.
func (e *Engine) StopIfIdle() bool {
	if e.sinceLastClient() < e.presenceWindow {
		return !e.running()
	}
	e.mu.Lock()
	cmd, exited := e.detachLocked()
	e.mu.Unlock()
	if cmd != nil {
		slog.Info("stopping the embedding server to free its GPU memory")
	}
	killChild(cmd, exited)
	return true
}

// Close kills the child and puts the Engine into a state where further embeds fail rather than resurrecting it. Called from the daemon's shutdown path so the server never outlives the daemon. It waits for the child to actually die, but does that outside e.mu so nothing else blocks on it.
func (e *Engine) Close() error {
	e.mu.Lock()
	e.closed = true
	cmd, exited := e.detachLocked()
	e.mu.Unlock()
	killChild(cmd, exited)
	return nil
}

// aliveLocked reports whether a child has been started and has not exited. Caller holds e.mu.
func (e *Engine) aliveLocked() bool {
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

// ensureUp makes sure a loaded server is up, and returns only once one is (or the attempt failed, or ctx ran out). The spawn and the readiness wait happen on their own goroutine under a background context, so a caller giving up — its request context carries the client's few-second retrieve budget, far shorter than a model load — takes nothing down with it: the child keeps loading and the next caller joins the same wait.
func (e *Engine) ensureUp(ctx context.Context) error {
	start, err := e.beginStart()
	if err != nil || start == nil {
		return err
	}
	select {
	case <-start.done:
		return start.err
	case <-ctx.Done():
		return fmt.Errorf("embed engine: waiting for server: %w", ctx.Err())
	}
}

// beginStart returns the in-flight (or newly launched) start to wait on, or nil when a loaded server is already up. Input: nothing. Output: the start to wait on, or an error if the Engine is closed or the child could not be spawned at all. e.mu is held only for the spawn itself, never for the readiness wait.
func (e *Engine) beginStart() (*startState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return nil, fmt.Errorf("embed engine: closed")
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
		return nil, fmt.Errorf("embed engine: start %s: %w", e.binary, err)
	}

	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	e.cmd, e.exited = cmd, exited
	e.lastUse = time.Now()
	slog.Info("embedding server started", "pid", cmd.Process.Pid, "url", e.baseURL)

	start := &startState{done: make(chan struct{})}
	e.start = start
	go e.finishStart(start, exited)
	return start, nil
}

// finishStart waits for the freshly spawned child to answer /health and publishes the outcome to everyone waiting on start. A child that never became ready is killed here — that is the only cancellation that may kill it, never a caller's.
func (e *Engine) finishStart(start *startState, exited chan struct{}) {
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
func (e *Engine) waitReady(exited chan struct{}) error {
	ctx := context.Background()
	deadline := time.Now().Add(e.startupTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-exited:
			return fmt.Errorf("embed engine: server exited during startup")
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/health", nil)
		if err != nil {
			return fmt.Errorf("embed engine: build health request: %w", err)
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("embed engine: server did not become ready at %s within %v", e.baseURL, e.startupTimeout)
		}
		time.Sleep(e.pollInterval)
	}
}

// reap watches one child and kills it once it has been idle past e.idle with no client seen inside e.presenceWindow. Returns as soon as that child is gone, so there is exactly one of these per spawn.
func (e *Engine) reap(exited chan struct{}) {
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
		// A client seen recently pins the server: the user is in a session and must not pay a cold start mid-conversation.
		if e.sinceLastClient() < e.presenceWindow || time.Since(e.lastUse) < e.idle {
			e.mu.Unlock()
			continue
		}
		slog.Info("embedding server idle, shutting it down", "idle_for", time.Since(e.lastUse).Round(time.Second))
		cmd, dead := e.detachLocked()
		e.mu.Unlock()
		killChild(cmd, dead)
		return
	}
}

// detachLocked hands the running child over to the caller and forgets it, so the Engine immediately reports as not running while the process is still dying. Caller holds e.mu. Returns nil when nothing is running.
func (e *Engine) detachLocked() (*exec.Cmd, chan struct{}) {
	if !e.aliveLocked() {
		e.cmd = nil
		return nil, nil
	}
	cmd, exited := e.cmd, e.exited
	e.cmd = nil
	return cmd, exited
}

// killChild SIGTERMs a detached child and waits for it to go, escalating to SIGKILL after five seconds. Called outside e.mu — nothing else may block on a dying process. A nil cmd is a no-op.
func killChild(cmd *exec.Cmd, exited chan struct{}) {
	if cmd == nil {
		return
	}
	cmd.Process.Signal(os.Interrupt)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-exited
	}
}

// pid returns the running child's process id, or 0 when nothing is running. Used by the tests to prove a respawn produced a genuinely new process.
func (e *Engine) pid() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.aliveLocked() {
		return 0
	}
	return e.cmd.Process.Pid
}

// killChildForTest kills the child out from under the Engine, standing in for a server that crashed on its own.
func (e *Engine) killChildForTest() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.aliveLocked() {
		e.cmd.Process.Kill()
	}
}
