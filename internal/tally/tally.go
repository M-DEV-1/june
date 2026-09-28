// Package tally is June's self-accounting seam: a decorator that counts brain
// calls per provider per day, and a weekly render of everything the daemon
// tracks about its own behavior (brain usage, vector-search contribution,
// dreaming nights, diary entries) — so the machine can be asked how it spent
// its week the same way it can be asked how the user spent theirs.
package tally

import (
	"context"
	"log/slog"
	"time"

	"june/internal/brain"
)

// Recorder is what Wrap needs to record one brain call's outcome. Satisfied structurally by *june/internal/db.Store's RecordUsage, so a Store can be handed to Wrap with no adapter.
// It takes characters rather than tokens on purpose. The Brain seam is func(ctx, prompt) (string, error) and carries no usage metadata, and the CLI providers do not report token counts at all — so characters are the one quantity measurable on every provider. Converting to tokens is done at read time with a stated divisor, which keeps the stored numbers facts rather than estimates.
type Recorder interface {
	RecordUsage(provider string, ok bool, ms time.Duration, promptChars, replyChars int) error
}

// Wrap decorates b with call/failure/latency counting under name, recorded through rec keyed by today's local calendar day. The wrapped Brain behaves identically to b — same prompt in, same reply and error out — Wrap only ever adds a measurement side effect after the call returns. Recording is best-effort: a Recorder failure is logged and never returned, so a broken counter can never break a brain call.
func Wrap(name string, b brain.Brain, rec Recorder) brain.Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		start := time.Now()
		reply, err := b(ctx, prompt)
		// The prompt is counted even when the call failed: it was still sent, and it still spent the quota.
		if recErr := rec.RecordUsage(name, err == nil, time.Since(start), len([]rune(prompt)), len([]rune(reply))); recErr != nil {
			slog.Warn("tally: recording brain call failed", "provider", name, "error", recErr)
		}
		return reply, err
	}
}
