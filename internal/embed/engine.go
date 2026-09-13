package embed

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"ora/internal/config"
)

// Engine is a LocalEmbedder that owns the embedding server as a child process. The daemon holds exactly one of these; nothing else may spawn the server, because two copies would fight over the same port.
// Lifecycle: spawned on the first embed (or on the first authenticated client request), kept alive as long as a TUI client has been seen inside presenceWindow, otherwise killed once idle for the configured timeout, respawned on the next embed if it died or was reaped, and killed unconditionally on Close.
type Engine struct {
	*serverProcess
	inner          *LocalEmbedder
	presenceWindow time.Duration

	// lastClient is atomic rather than guarded by mu because MarkClientPresence runs on every authenticated IPC request, including ones with a 300ms budget, and must never wait behind a spawn, a model load, or a child being killed.
	lastClient atomic.Int64

	// gpuBusy reports whether a heavier job (a whisper decode) holds the GPU. Set once at wiring time by SetGPUGate and only read after, so it needs no lock.
	gpuBusy func() bool
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
	e := &Engine{
		inner:          NewLocalEmbedder(baseURL, model),
		presenceWindow: defaultPresenceWindow,
	}
	e.serverProcess = newServerProcess("embed engine", "embedding server", binary, args, baseURL, idle, 90*time.Second, 100*time.Millisecond)
	e.serverProcess.shouldKeepAlive = func() bool {
		return !e.gpuTaken() && e.sinceLastClient() < e.presenceWindow
	}
	return e
}

// SetGPUGate wires in the question "is something heavier using the card right now". Input: a function reporting whether another job holds the GPU (recorder.GPUBusy in the daemon, nil in tests that do not care). Output: none.
// A transcription outranks client presence. The presence window exists so someone mid-conversation does not pay a cold start, but it is refreshed by every authenticated IPC request and the desktop window polls continuously, so in practice the server was pinned whenever Ora was open and StopIfIdle never actually yielded. whisper then decoded anyway, on a 4 GB laptop card already carrying 2.5 GB of Ora's own models, and died out of device memory 37 times in six days.
func (e *Engine) SetGPUGate(busy func() bool) { e.gpuBusy = busy }

// gpuTaken reports whether another job holds the GPU. False when no gate was wired in.
func (e *Engine) gpuTaken() bool { return e.gpuBusy != nil && e.gpuBusy() }

// MarkClientPresence records that an authenticated client request just arrived, and starts the server in the background if it is not already up. This is what pins the server in memory while a TUI is running, and what makes the user's first question meet an already-loaded model instead of paying the cold start. The daemon's IPC auth wrapper calls it on every authenticated request, so it never waits on a spawn or a model load — the timestamp write is lock-free and the start runs on its own goroutine.
// Input: a context whose lifetime is the daemon's, used only for the background start. Output: none.
func (e *Engine) MarkClientPresence(ctx context.Context) {
	e.lastClient.Store(time.Now().UnixNano())
	if e.running() || e.gpuTaken() {
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
	if e.gpuTaken() {
		return nil, errGPUHeld
	}
	if err := e.ensureUp(ctx); err != nil {
		return nil, err
	}
	e.touchUse()
	return e.inner.Embed(ctx, task, text)
}

// StopIfIdle kills the child now so its GPU memory can go to a heavier job, unless a client has been seen inside the presence window — someone mid-conversation keeps their fast embeds. Unlike Close this is not final: the next embed just spawns the server again. Reports whether the server is down when it returns.
func (e *Engine) StopIfIdle() bool {
	if !e.gpuTaken() && e.sinceLastClient() < e.presenceWindow {
		return !e.running()
	}
	return e.stopNow()
}

// errGPUHeld is what an embed gets while a heavier job holds the card. Callers already degrade without the semantic half — hybrid search falls back to lexical only, and the background reconcile sweep stops and picks the work up next time — which is the right trade against putting a second model on a card a transcription is decoding on.
var errGPUHeld = errors.New("embed engine: the GPU is held by a transcription")
