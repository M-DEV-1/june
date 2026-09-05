package db_test

import (
	"context"
	"testing"

	"ora/internal/memory"
)

// TestSearchMemory_HitsCarryCreatedAt is a regression test for recency shaping: a hit with a zero CreatedAt can never be pushed up or down by age, so every source searchMemoryWindow can return (note, summary, thread) must carry its row's real timestamp.
func TestSearchMemory_HitsCarryCreatedAt(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if _, err := store.LogNote(ctx, "note about a unique term zylophonic1.", "fact"); err != nil {
		t.Fatal(err)
	}
	if err := store.LogSemanticNode(ctx, memory.TaskSummary{
		TaskName: "zylophonic2 task",
		Summary:  "a summary mentioning zylophonic2 for search",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "zylophonic3 thread", Kind: "topic", State: "ongoing"}); err != nil {
		t.Fatal(err)
	}

	for _, term := range []string{"zylophonic1", "zylophonic2", "zylophonic3"} {
		hits, err := store.SearchMemory(ctx, term)
		if err != nil {
			t.Fatalf("%s: %v", term, err)
		}
		if len(hits) == 0 {
			t.Fatalf("%s: no hits", term)
		}
		if hits[0].CreatedAt.IsZero() {
			t.Fatalf("%s: hit %+v has zero CreatedAt", term, hits[0])
		}
	}
}
