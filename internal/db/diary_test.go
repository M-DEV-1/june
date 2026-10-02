package db_test

import (
	"context"
	"testing"

	"june/internal/db"
)

// TestStore_SetDiaryEntry_ReplacesByDayAndKind verifies upsert semantics: writing the same (day, kind) twice yields one row holding the second content, which is what lets the understanding doc be rewritten in place.
func TestStore_SetDiaryEntry_ReplacesByDayAndKind(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if err := store.SetDiaryEntry(ctx, "2026-08-29", "day", "first draft"); err != nil {
		t.Fatalf("SetDiaryEntry: %v", err)
	}
	if err := store.SetDiaryEntry(ctx, "2026-08-29", "day", "final entry"); err != nil {
		t.Fatalf("SetDiaryEntry (upsert): %v", err)
	}

	got, err := store.DiaryEntry(ctx, "2026-08-29", "day")
	if err != nil {
		t.Fatalf("DiaryEntry: %v", err)
	}
	if got != "final entry" {
		t.Errorf("DiaryEntry = %q, want %q", got, "final entry")
	}

	entries, err := store.RecentDiaryEntries(ctx, 10)
	if err != nil {
		t.Fatalf("RecentDiaryEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("RecentDiaryEntries returned %d rows, want 1 (upsert must not add a second)", len(entries))
	}
}

// TestStore_DiaryEntry_IndexedInMemoryFTS verifies the diary triggers mirror content into memory_fts, so entries surface through the existing SearchMemory/query_memory path — and that an in-place rewrite replaces the indexed text rather than leaving the old version searchable.
func TestStore_DiaryEntry_IndexedInMemoryFTS(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if err := store.SetDiaryEntry(ctx, "2026-08-29", "day", "the user spent the evening on the zeppelin project"); err != nil {
		t.Fatalf("SetDiaryEntry: %v", err)
	}

	hits, err := store.SearchMemory(ctx, "zeppelin")
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) != 1 || hits[0].Source != "diary" {
		t.Fatalf("SearchMemory(zeppelin) = %+v, want one diary hit", hits)
	}

	if err := store.SetDiaryEntry(ctx, "2026-08-29", "day", "the user spent the evening reading"); err != nil {
		t.Fatalf("SetDiaryEntry (rewrite): %v", err)
	}
	hits, err = store.SearchMemory(ctx, "zeppelin")
	if err != nil {
		t.Fatalf("SearchMemory after rewrite: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("SearchMemory(zeppelin) after rewrite = %+v, want none (stale index text)", hits)
	}
}

// diaryFTSCount reports how many memory_fts rows mirror the diary table.
func diaryFTSCount(t *testing.T, store *db.Store) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM memory_fts WHERE source = 'diary'`).Scan(&n); err != nil {
		t.Fatalf("count mirrored diary rows: %v", err)
	}
	return n
}

// TestSetDiaryEntry_WatermarkIsNeverSearchable pins that the task-notice watermark stays out of the search index. It is a diary row only because the diary is where a keyed marker could be kept, and its content is a bare note id that the proactive loop rewrites on most ticks; mirrored into memory_fts it is a searchable "memory" saying nothing but a number.
func TestSetDiaryEntry_WatermarkIsNeverSearchable(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if err := store.SetDiaryEntry(ctx, "2026-09-06", "day", "A day of moving the store around."); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDiaryEntry(ctx, "", db.TaskNoticeWatermarkKind, "41"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDiaryEntry(ctx, "", db.TaskNoticeWatermarkKind, "42"); err != nil {
		t.Fatal(err)
	}

	if got := diaryFTSCount(t, store); got != 1 {
		t.Errorf("%d diary rows are mirrored into memory_fts, want 1 — the day page alone", got)
	}
}
