package embed

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"ora/internal/brain"
	"ora/internal/config"
)

// TextEngine owns the local text-generation llama-server as a child process, the same way Engine owns the embedding one, so ORA's unattended text jobs answer from a local model instead of spending metered API quota.
// Lifecycle: spawned on the first Generate, killed once idle past idle with no Generate, respawned on the next Generate if it died or was reaped, and killed unconditionally on Close. Unlike Engine there is no client-presence concept — only the idle timer decides when the server goes down.
type TextEngine struct {
	*serverProcess
	call brain.Brain
	// gpuBusy reports whether a heavier job holds the card. See SetGPUGate.
	gpuBusy func() bool
}

// SetGPUGate wires in the question "is something heavier using the card right now". Input: a function reporting whether another job holds the GPU (recorder.GPUBusy in the daemon, nil in tests that do not care). Output: none.
// This engine is the card's largest tenant: 1853 MiB of a 4096 MiB RTX 3050 on 2026-09-15, against the embedding server's 653 MiB. It had neither half of the protocol Engine has, so a whisper decode found nothing it could evict, ran out of device memory and redid the whole meeting on the CPU — 50 minutes for a decode that takes 15 on the card.
// Safe on a nil *TextEngine, which is what the daemon holds when no local text model is configured — the common case on a fresh install, and a panic at startup before this.
func (e *TextEngine) SetGPUGate(busy func() bool) {
	if e == nil {
		return
	}
	e.gpuBusy = busy
}

// gpuTaken reports whether another job holds the GPU. False when no gate was wired in.
func (e *TextEngine) gpuTaken() bool { return e != nil && e.gpuBusy != nil && e.gpuBusy() }

// errGPUHeld is what a generate gets while a whisper decode holds the card, so the text server is not spawned back onto it mid-decode.
var errGPUHeld = errors.New("text engine: the GPU is held by a transcription")

// StopIfIdle kills the child now so its GPU memory can go to a heavier job. Unlike Engine there is no client presence to weigh against it: nobody waits on a working-state derive, so a transcription always wins. Not final — the next Generate after the decode spawns it again. Reports whether the server is down when it returns. Safe on a nil *TextEngine, which has no card to give up.
func (e *TextEngine) StopIfIdle() bool {
	if e == nil {
		return true
	}
	return e.stopNow()
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
		serverProcess: newServerProcess("text engine", "local text server", binary, args, baseURL, idle, 120*time.Second, 100*time.Millisecond),
		call:          brain.LlamaServer(baseURL, timeoutSeconds),
	}
}

// Generate answers one prompt on the local model, spawning the server first if it is not already up. Input: the prompt. Output: the model's reply, or an error when the server could not be started or the call failed. Safe to call on a nil *TextEngine, which returns an error rather than panicking.
func (e *TextEngine) Generate(ctx context.Context, prompt string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("text engine: no local text model configured")
	}
	// Asking once before the decode is not enough on its own: the working-state derive ticks every five minutes, so one lands inside any decode longer than that and would spawn the server straight back onto the card. The daemon installs this behind a fallback to the routed cloud brain, so a derive refused here is answered there instead of being lost.
	if e.gpuTaken() {
		return "", errGPUHeld
	}
	if err := e.ensureUp(ctx); err != nil {
		return "", err
	}
	e.touchUse()
	return e.call(ctx, prompt)
}

// Close kills the child for good and puts the TextEngine into a state where further generations fail rather than resurrecting it. Safe to call on a nil *TextEngine, which has nothing to kill.
func (e *TextEngine) Close() error {
	if e == nil {
		return nil
	}
	return e.serverProcess.Close()
}
