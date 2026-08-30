package tally

import (
	"context"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
)

// testStore opens a throwaway in-memory store that is closed when the test ends.
func testStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestRenderWeeklyLog_IncludesAllSectionsWithFixtureRows seeds one week's worth of brain-call, vector-contribution, dream-run, and diary fixture rows, and asserts the rendered text names every section and reflects the fixture numbers — the render is the whole point of the weekly log, so every input this package can produce must show up somewhere in the output.
func TestRenderWeeklyLog_IncludesAllSectionsWithFixtureRows(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	now := time.Now()

	// Brain calls: three claude-cli calls, one of which failed.
	if err := store.RecordTally("claude-cli", true, 100*time.Millisecond); err != nil {
		t.Fatalf("RecordTally: %v", err)
	}
	if err := store.RecordTally("claude-cli", true, 300*time.Millisecond); err != nil {
		t.Fatalf("RecordTally: %v", err)
	}
	if err := store.RecordTally("claude-cli", false, 200*time.Millisecond); err != nil {
		t.Fatalf("RecordTally: %v", err)
	}

	// Vector contribution: HybridSearch is the real writer (see internal/db/hybrid_test.go), but the weekly render only reads the tally table, so fixture rows written directly are equivalent input.
	if _, err := store.DB().Exec(`INSERT INTO tally (day, provider, calls, failures, total_ms) VALUES (?, 'vector-queries', 4, 0, 0)`, now.Format(weeklyDayFormat)); err != nil {
		t.Fatalf("seed vector-queries: %v", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO tally (day, provider, calls, failures, total_ms) VALUES (?, 'vector-hits', 3, 0, 7)`, now.Format(weeklyDayFormat)); err != nil {
		t.Fatalf("seed vector-hits: %v", err)
	}

	// Dreaming loop: one finished night with two stages done, one still in progress.
	if err := store.StartDreamRun(ctx, now.Format(weeklyDayFormat)); err != nil {
		t.Fatalf("StartDreamRun: %v", err)
	}
	if err := store.FinishDreamRun(ctx, now.Format(weeklyDayFormat), "quiet night", "quiet night"); err != nil {
		t.Fatalf("FinishDreamRun: %v", err)
	}
	yesterday := now.AddDate(0, 0, -1).Format(weeklyDayFormat)
	if err := store.StartDreamRun(ctx, yesterday); err != nil {
		t.Fatalf("StartDreamRun: %v", err)
	}

	// Diary entries: a day close and a morning brief.
	if err := store.SetDiaryEntry(ctx, now.Format(weeklyDayFormat), "day", "Today was quiet."); err != nil {
		t.Fatalf("SetDiaryEntry(day): %v", err)
	}
	if err := store.SetDiaryEntry(ctx, now.Format(weeklyDayFormat), "brief", "Nothing urgent."); err != nil {
		t.Fatalf("SetDiaryEntry(brief): %v", err)
	}

	text, err := RenderWeeklyLog(ctx, store, now)
	if err != nil {
		t.Fatalf("RenderWeeklyLog: %v", err)
	}

	for _, want := range []string{
		"Brain calls:",
		"claude-cli: 3 calls, 1 failures, 200ms mean latency",
		"Vector search contribution:",
		"3/4 queries (75%) had a surviving vector hit, 7 hits total",
		"Dreaming loop:",
		"2 nights ran, 1 finished, stages seen:",
		"Diary entries written:",
		"brief: 1",
		"day: 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered weekly log missing %q, got:\n%s", want, text)
		}
	}
}

// TestRenderWeeklyLog_EmptyStore_RendersNoneForEverySection verifies a brand-new store with no activity at all renders every section with an honest "no data" line rather than an error or a blank section — the daemon's very first Sunday should still produce a readable log.
func TestRenderWeeklyLog_EmptyStore_RendersNoneForEverySection(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)

	text, err := RenderWeeklyLog(ctx, store, time.Now())
	if err != nil {
		t.Fatalf("RenderWeeklyLog: %v", err)
	}
	for _, want := range []string{"(none)", "(no retrieval queries this week)", "(no nights ran)"} {
		if !strings.Contains(text, want) {
			t.Errorf("empty-store render missing %q, got:\n%s", want, text)
		}
	}
}

// TestRenderWeeklyLog_OnlyLooksAtLastSevenDays verifies a tally row older than the window is excluded from the render — otherwise the "weekly" log would grow to cover the store's entire lifetime.
func TestRenderWeeklyLog_OnlyLooksAtLastSevenDays(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	now := time.Now()

	oldDay := now.AddDate(0, 0, -30).Format(weeklyDayFormat)
	if _, err := store.DB().Exec(`INSERT INTO tally (day, provider, calls, failures, total_ms) VALUES (?, 'grok-cli', 5, 0, 500)`, oldDay); err != nil {
		t.Fatalf("seed old tally row: %v", err)
	}

	text, err := RenderWeeklyLog(ctx, store, now)
	if err != nil {
		t.Fatalf("RenderWeeklyLog: %v", err)
	}
	if strings.Contains(text, "grok-cli") {
		t.Errorf("render included a tally row from 30 days ago, want it excluded from the 7-day window:\n%s", text)
	}
}
