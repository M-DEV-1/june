package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"ora/internal/memory"
)

func TestLatestMemoryTime_UsesNewestRowNotWallClock(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	got, err := store.LatestMemoryTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Fatalf("empty store should have zero clock, got %v", got)
	}

	if _, err := store.LogEpisode(ctx, "Brave", "Opal", "climate risk"); err != nil {
		t.Fatal(err)
	}
	got, err = store.LatestMemoryTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.IsZero() {
		t.Fatal("expected a clock after logging an episode")
	}
	if time.Since(got) > time.Minute {
		t.Fatalf("clock %v is older than a minute", got)
	}
}

func TestMemoryAsOf_NoteThreadEpisodeWorkingState(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	noteID, err := store.LogNote(ctx, "User prefers TDD.", "fact")
	if err != nil {
		t.Fatal(err)
	}
	threadID, err := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "Suits", Kind: "entertainment", State: "Season 7 Episode 15 Tiny Violin"})
	if err != nil {
		t.Fatal(err)
	}
	epID, err := store.LogEpisode(ctx, "Brave", "Opal", "slides")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetWorkingState(ctx, "Climate Risk Statement Builder ASRS"); err != nil {
		t.Fatal(err)
	}

	for _, source := range []string{
		"working_state",
		fmt.Sprintf("note:%d", noteID),
		fmt.Sprintf("thread:%d", threadID),
		fmt.Sprintf("episode:%d", epID),
		"episode:recent",
	} {
		ts, err := store.MemoryAsOf(ctx, source)
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		if ts.IsZero() {
			t.Fatalf("%s: expected a timestamp", source)
		}
	}

	missing, err := store.MemoryAsOf(ctx, "thread:99999")
	if err != nil {
		t.Fatal(err)
	}
	if !missing.IsZero() {
		t.Fatalf("missing thread should be zero, got %v", missing)
	}
}
