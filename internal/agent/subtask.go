package agent

import (
	"context"
	"fmt"
	"log/slog"
)

// subtaskAllowedTools is the read-only, stateless tool subset a background side call (branch's web search fallback, ask's own flow) may reach — shared here since both need the same safe subset.
var subtaskAllowedTools = map[string]bool{
	"query_memory": true,
	"recall":       true,
}

// surfacePendingFolds fetches every branch() result that missed its original live session (see runToolCall's dead-session fallback in connect.go) and returns them as context lines for the next handshake, marking each consumed so it surfaces exactly once.
// Fetch errors are logged and degrade to no lines rather than failing the handshake — a missed fold surfacing late is better than a broken connect.
func (a *Agent) surfacePendingFolds(ctx context.Context) []string {
	folds, err := a.brain.UnconsumedFolds(ctx)
	if err != nil {
		slog.Warn("failed to fetch pending folds", "error", err)
		return nil
	}
	var lines []string
	for _, f := range folds {
		lines = append(lines, fmt.Sprintf("  [while you were away] %s: %s", f.Task, f.Result))
		if err := a.brain.ConsumeFold(ctx, f.ID); err != nil {
			slog.Warn("failed to mark fold consumed", "fold_id", f.ID, "error", err)
		}
	}
	return lines
}
