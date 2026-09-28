// This file is the pruning stage: once a night the store's two retention passes run, empty conversations first and act runs second, so the two tables nothing ever deleted from stop growing. It is the last stage of the night on purpose — the procedures stage just before it writes the "How I did X" notes that make the store keep an act run for good, so pruning ahead of it could take a run that was about to be written up.
package dream

import (
	"context"
	"time"

	"june/internal/config"
	"june/internal/db"
)

// pruneReport is what the pruning stage hands the night's log: how many rows each pass removed, and how many rows the policy held back, split by the rule that held them. Every number is a count of rows in the store, so a person reading the log sees the policy working instead of guessing at it.
type pruneReport struct {
	// conversationsRemoved is how many empty conversations older than db.EmptyConversationAge were deleted; conversationsProtected is how many conversations that old were kept anyway, because something was said in them or one of the user's tasks still points at them.
	conversationsRemoved   int64
	conversationsProtected int64
	// runsRemoved is how many act runs the count cap took; runsKeptForNotes is how many were exempt because a nightly note was written from them, and runsKeptFailedYoung how many were exempt because they failed and are still inside the grace.
	runsRemoved         int64
	runsKeptForNotes    int64
	runsKeptFailedYoung int64
	// toolCallsRemoved is how many tool_calls rows older than failedGrace were deleted — the same window PruneActRuns gives a failed run before the count cap can take it.
	toolCallsRemoved int64
}

// pruneStage runs the store's two retention passes in order, conversations then act runs, counting what each one held back as well as what it removed. Input: ctx. Output: the report, or the store's own error the moment one of the four calls fails.
// A failed call stops the stage there: the pass after it does not run, nothing is counted, and the caller commits no token, so a store that cannot be reached is retried on the next wake. That is the whole reason the errors are returned rather than swallowed — an unreachable store answering nothing must never be read as a store with nothing to prune.
func (r *Runner) pruneStage(ctx context.Context) (pruneReport, error) {
	keep, failedGrace := r.retention()

	protectedConversations, err := r.store.ProtectedConversations(ctx, db.EmptyConversationAge)
	if err != nil {
		return pruneReport{}, err
	}
	removedConversations, err := r.store.PruneEmptyConversations(ctx, db.EmptyConversationAge)
	if err != nil {
		return pruneReport{}, err
	}

	keptForNotes, keptFailedYoung, err := r.store.ProtectedActRuns(ctx, failedGrace)
	if err != nil {
		return pruneReport{}, err
	}
	removedRuns, err := r.store.PruneActRuns(ctx, keep, failedGrace)
	if err != nil {
		return pruneReport{}, err
	}

	removedToolCalls, err := r.store.PruneToolCalls(ctx, failedGrace)
	if err != nil {
		return pruneReport{}, err
	}

	return pruneReport{
		conversationsRemoved:   removedConversations,
		conversationsProtected: protectedConversations,
		runsRemoved:            removedRuns,
		runsKeptForNotes:       keptForNotes,
		runsKeptFailedYoung:    keptFailedYoung,
		toolCallsRemoved:       removedToolCalls,
	}, nil
}

// configRetention reads the two act run retention numbers out of the config file: how many ordinary runs to keep, and how long a failed run is kept regardless of that cap. Read fresh each night so editing the config takes effect on the next night rather than on the next restart.
func configRetention() (keep int, failedGrace time.Duration) {
	cfg := config.LoadConfig()
	return cfg.ActRunsKept(), time.Duration(cfg.FailedActRunsKeptDays()) * 24 * time.Hour
}
