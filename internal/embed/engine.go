package embed

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Engine is a LocalEmbedder that owns the embedding server as a child process. The daemon holds exactly one of these; nothing else may spawn the server, because two copies would fight over the same port.
// Lifecycle: spawned on the first embed (or on Warm), kept alive as long as a TUI client has been seen inside presenceWindow, otherwise killed once idle for the configured timeout, respawned on the next embed if it died or was reaped, and killed unconditionally on Close.
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

	mu         sync.Mutex
	cmd        *exec.Cmd
	exited     chan struct{}
	lastUse    time.Time
	lastClient time.Time
	closed     bool
}

// defaultPresenceWindow is how long after a client's last authenticated IPC request the embedding server stays pinned in memory. Three minutes: a user pausing mid-conversation should not pay a cold start for their next question.
const defaultPresenceWindow = 3 * time.Minute

// NewEngine wires an Engine to the server binary and the arguments that make it listen at baseURL, with model sent in each embed request and idle as the no-client shutdown timeout. Nothing is spawned until the first Embed or Warm.
func NewEngine(binary string, args []string, baseURL, model string, idle time.Duration) *Engine {
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

// MarkClientPresence records that an authenticated client request just arrived. This is what pins the server in memory while a TUI is running; it is called from the daemon's IPC auth wrapper, so every client request counts.
func (e *Engine) MarkClientPresence() {
	e.mu.Lock()
	e.lastClient = time.Now()
	e.mu.Unlock()
}

// Warm starts the server if it is not already up, without embedding anything — used when a client first appears, so the user's first question does not pay the cold start.
func (e *Engine) Warm(ctx context.Context) error {
	return e.ensureUp(ctx)
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

// Running reports whether the child process is up right now.
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.aliveLocked()
}

// Close kills the child and puts the Engine into a state where further embeds fail rather than resurrecting it. Called from the daemon's shutdown path so the server never outlives the daemon.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	e.stopLocked()
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

// ensureUp spawns the child if it is not running and blocks until the server answers /health. Concurrent callers serialize on e.mu, so only one spawn ever happens.
func (e *Engine) ensureUp(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return fmt.Errorf("embed engine: closed")
	}
	if e.aliveLocked() {
		return nil
	}

	cmd := exec.Command(e.binary, e.args...)
	// The server's own logs go to the daemon's stderr, which is already redirected to ora.log — a silently failing model load is otherwise invisible.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = childProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("embed engine: start %s: %w", e.binary, err)
	}

	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	e.cmd, e.exited = cmd, exited
	e.lastUse = time.Now()
	slog.Info("embedding server started", "pid", cmd.Process.Pid, "url", e.baseURL)

	if err := e.waitReady(ctx, exited); err != nil {
		e.stopLocked()
		return err
	}

	go e.reap(exited)
	return nil
}

// waitReady polls the server's /health until it answers, the child exits, the caller's context is cancelled, or startupTimeout passes.
func (e *Engine) waitReady(ctx context.Context, exited chan struct{}) error {
	deadline := time.Now().Add(e.startupTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-exited:
			return fmt.Errorf("embed engine: server exited during startup")
		case <-ctx.Done():
			return fmt.Errorf("embed engine: waiting for server: %w", ctx.Err())
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
		if time.Since(e.lastClient) < e.presenceWindow || time.Since(e.lastUse) < e.idle {
			e.mu.Unlock()
			continue
		}
		slog.Info("embedding server idle, shutting it down", "idle_for", time.Since(e.lastUse).Round(time.Second))
		e.stopLocked()
		e.mu.Unlock()
		return
	}
}

// stopLocked SIGTERMs the child and waits briefly for it to go, escalating to SIGKILL. Caller holds e.mu. A no-op when nothing is running.
func (e *Engine) stopLocked() {
	if !e.aliveLocked() {
		e.cmd = nil
		return
	}
	cmd, exited := e.cmd, e.exited
	cmd.Process.Signal(os.Interrupt)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-exited
	}
	e.cmd = nil
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
