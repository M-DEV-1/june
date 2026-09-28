package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"june/internal/db"
	"june/internal/memory"
)

// TestDayStart_UTCInstantMapsToLocalDay checks that DayStart uses the local calendar day, not the UTC one, when the daemon's zone is set to IST — a UTC instant whose IST wall-clock time has already rolled past midnight into the next day must land on that later IST day.
func TestDayStart_UTCInstantMapsToLocalDay(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip("no tzdata available: " + err.Error())
	}
	old := time.Local
	time.Local = ist
	defer func() { time.Local = old }()

	// 2026-09-05 20:00 UTC is 2026-09-06 01:30 IST: past midnight, so the IST day is Sep 6 even though the UTC day is still Sep 5.
	instant := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 6, 0, 0, 0, 0, ist)
	if got := db.DayStart(instant); !got.Equal(want) {
		t.Fatalf("DayStart(%v) = %v, want %v", instant, got, want)
	}
}

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

	if _, err := store.LogEpisode(ctx, "Brave", "Opal", "route planning"); err != nil {
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
	if err := store.SetWorkingState(ctx, "Brightpath Statement Builder VRDS"); err != nil {
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
