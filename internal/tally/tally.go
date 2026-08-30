// Package tally is ORA's self-accounting seam: a decorator that counts brain
// calls per provider per day, and a weekly render of everything the daemon
// tracks about its own behavior (brain usage, vector-search contribution,
// dreaming nights, diary entries) — so the machine can be asked how it spent
// its week the same way it can be asked how the user spent theirs.
package tally

import (
	"context"
	"log/slog"
	"time"

	"ora/internal/brain"
)

// Recorder is what Wrap needs to record one brain call's outcome. Satisfied structurally by *ora/internal/db.Store's RecordTally, so a Store can be handed to Wrap with no adapter.
type Recorder interface {
	RecordTally(provider string, ok bool, ms time.Duration) error
}

// Wrap decorates b with call/failure/latency counting under name, recorded through rec keyed by today's local calendar day. The wrapped Brain behaves identically to b — same prompt in, same reply and error out — Wrap only ever adds a measurement side effect after the call returns. Recording is best-effort: a Recorder failure is logged and never returned, so a broken counter can never break a brain call.
func Wrap(name string, b brain.Brain, rec Recorder) brain.Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		start := time.Now()
		reply, err := b(ctx, prompt)
		if recErr := rec.RecordTally(name, err == nil, time.Since(start)); recErr != nil {
			slog.Warn("tally: recording brain call failed", "provider", name, "error", recErr)
		}
		return reply, err
	}
}
