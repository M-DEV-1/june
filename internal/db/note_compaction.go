package db

import (
	"context"
	"fmt"
)

// minNotesToConsolidate is the floor below which the notes table is small enough that periodic consolidation isn't worth an LLM call.
const minNotesToConsolidate = 20

// NoteConsolidator collapses the full notes set into a deduplicated, durable-only canonical list.
// It's the curation counterpart to per-flush ReconcileNotes: reconciliation guards the front door, consolidation cleans the whole room.
type NoteConsolidator interface {
	ConsolidateNotes(ctx context.Context, notes []string) ([]string, error)
}

// NoteCompactor periodically rewrites the notes table into a smaller canonical set, merging near-duplicates and dropping ephemeral task config that leaked in as "facts."
// This is the defence against notes-layer context rot — per-flush reconciliation under-merges over time, so the whole set gets curated.
type NoteCompactor struct {
	llm   NoteConsolidator
	store *Store
}

// NewNoteCompactor builds a NoteCompactor. Input: the model that consolidates note text into a canonical set, and the store whose notes of kind "fact" it reads and rewrites. Output: a ready NoteCompactor.
func NewNoteCompactor(llm NoteConsolidator, store *Store) *NoteCompactor {
	return &NoteCompactor{llm: llm, store: store}
}

// Compact loads every note, asks the model for the canonical set, and rewrites the table — but only when doing so is safe and useful. Guards:
//   - below minNotesToConsolidate: skip (table is small).
//   - empty model result: skip (never wipe the table on a bad response).
//   - no reduction in count: skip (not worth churning rows).
func (nc *NoteCompactor) Compact(ctx context.Context) error {
	existing, err := nc.store.ExistingNotes(ctx)
	if err != nil {
		return fmt.Errorf("note consolidation: load notes: %w", err)
	}
	if len(existing) < minNotesToConsolidate {
		return nil
	}

	contents := make([]string, len(existing))
	for i, n := range existing {
		contents[i] = n.Content
	}

	merged, err := nc.llm.ConsolidateNotes(ctx, contents)
	if err != nil {
		return fmt.Errorf("note consolidation: llm call: %w", err)
	}

	// Safety: never wipe the table on an empty/garbage response, and don't churn rows when the model failed to actually reduce the set.
	if len(merged) == 0 || len(merged) >= len(existing) {
		return nil
	}

	return nc.store.ReplaceAllNotes(ctx, merged)
}
