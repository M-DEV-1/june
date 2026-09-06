package db_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ora/internal/db"
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

// TestStore_DiaryEntry_MissingIsEmptyNotError pins the contract the scheduler's condition checks rely on: no row for (day, kind) is an ordinary "" result, since a missing entry is exactly what "the close hasn't run yet" looks like.
func TestStore_DiaryEntry_MissingIsEmptyNotError(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	got, err := store.DiaryEntry(ctx, "2026-08-29", "day")
	if err != nil {
		t.Fatalf("DiaryEntry on empty table: %v", err)
	}
	if got != "" {
		t.Errorf("DiaryEntry on empty table = %q, want empty", got)
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

// TestStore_RecentDiaryEntries_NewestFirstDaysOnly verifies ordering and that the understanding doc and brief markers never leak into the day listing.
func TestStore_RecentDiaryEntries_NewestFirstDaysOnly(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	for _, e := range []struct{ day, kind, content string }{
		{"2026-08-27", "day", "wednesday"},
		{"2026-08-28", "day", "thursday"},
		{"2026-08-29", "day", "friday"},
		{"", "understanding", "the standing model"},
		{"2026-08-29", "brief", "the morning brief"},
	} {
		if err := store.SetDiaryEntry(ctx, e.day, e.kind, e.content); err != nil {
			t.Fatalf("SetDiaryEntry(%s, %s): %v", e.day, e.kind, err)
		}
	}

	entries, err := store.RecentDiaryEntries(ctx, 2)
	if err != nil {
		t.Fatalf("RecentDiaryEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("RecentDiaryEntries(2) returned %d rows, want 2", len(entries))
	}
	if entries[0].Day != "2026-08-29" || entries[1].Day != "2026-08-28" {
		t.Errorf("RecentDiaryEntries order = %s, %s; want 2026-08-29, 2026-08-28", entries[0].Day, entries[1].Day)
	}
	if entries[0].Content != "friday" {
		t.Errorf("newest entry content = %q, want %q", entries[0].Content, "friday")
	}
}

// TestStore_NotesOfKindSince_FiltersKindAndTime verifies the proactive seams' minutes query: only the asked-for kind comes back, and only rows created inside the window.
func TestStore_NotesOfKindSince_FiltersKindAndTime(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if _, err := store.LogNote(ctx, "Minutes: agreed to ship Friday", "meeting"); err != nil {
		t.Fatalf("LogNote(meeting): %v", err)
	}
	if _, err := store.LogNote(ctx, "user prefers dark roast", "fact"); err != nil {
		t.Fatalf("LogNote(fact): %v", err)
	}

	notes, err := store.NotesOfKindSince(ctx, "meeting", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("NotesOfKindSince: %v", err)
	}
	if len(notes) != 1 || notes[0].Content != "Minutes: agreed to ship Friday" {
		t.Fatalf("NotesOfKindSince(meeting, -1h) = %+v, want just the minutes", notes)
	}

	notes, err = store.NotesOfKindSince(ctx, "meeting", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("NotesOfKindSince (future window): %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("NotesOfKindSince with a future since = %+v, want none", notes)
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

// TestNew_ClearsAWatermarkAlreadyInTheIndex pins the migration for a store written before the watermark was kept out of the index: its rows are still mirrored, and opening the store again drops them.
func TestNew_ClearsAWatermarkAlreadyInTheIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ora.db")
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	if err := store.SetDiaryEntry(context.Background(), "", db.TaskNoticeWatermarkKind, "42"); err != nil {
		t.Fatal(err)
	}
	// What the old triggers left behind: the watermark's content mirrored under its own row id.
	var id int64
	if err := store.DB().QueryRow(`SELECT id FROM diary WHERE kind = ?`, db.TaskNoticeWatermarkKind).Scan(&id); err != nil {
		t.Fatalf("read the watermark row: %v", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO memory_fts(content, source, ref_id) VALUES ('42', 'diary', ?)`, id); err != nil {
		t.Fatalf("mirror the watermark the way the old triggers did: %v", err)
	}
	store.Close()

	reopened, err := db.New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	if got := diaryFTSCount(t, reopened); got != 0 {
		t.Errorf("%d watermark rows are still mirrored into memory_fts after reopening, want 0", got)
	}
}

// TestSetDiaryEntry_JobMarkerIsNeverSearchable pins that a background job's last-run marker stays out of the search index. Like the task-notice watermark it is a diary row only because the diary is where the daemon keeps a keyed marker, and its content is a bare RFC 3339 timestamp the metered job rewrites every time it runs; mirrored into memory_fts it is a searchable "memory" saying nothing but a date.
func TestSetDiaryEntry_JobMarkerIsNeverSearchable(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if err := store.SetDiaryEntry(ctx, "2026-09-06", "day", "A day of moving the store around."); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDiaryEntry(ctx, "", db.JobMarkerKindPrefix+"compact", "2026-09-06T04:00:00+05:30"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDiaryEntry(ctx, "", db.JobMarkerKindPrefix+"compact", "2026-09-06T05:00:00+05:30"); err != nil {
		t.Fatal(err)
	}

	if got := diaryFTSCount(t, store); got != 1 {
		t.Errorf("%d diary rows are mirrored into memory_fts, want 1 — the day page alone", got)
	}
}

// TestNew_ClearsAJobMarkerAlreadyInTheIndex pins the migration for a store written before the job markers were kept out of the index: their timestamps are still mirrored, and opening the store again drops them.
func TestNew_ClearsAJobMarkerAlreadyInTheIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ora.db")
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	kind := db.JobMarkerKindPrefix + "compact"
	if err := store.SetDiaryEntry(context.Background(), "", kind, "2026-09-06T04:00:00+05:30"); err != nil {
		t.Fatal(err)
	}
	// What the old triggers left behind: the marker's timestamp mirrored under its own row id.
	var id int64
	if err := store.DB().QueryRow(`SELECT id FROM diary WHERE kind = ?`, kind).Scan(&id); err != nil {
		t.Fatalf("read the marker row: %v", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO memory_fts(content, source, ref_id) VALUES ('2026-09-06T04:00:00+05:30', 'diary', ?)`, id); err != nil {
		t.Fatalf("mirror the marker the way the old triggers did: %v", err)
	}
	store.Close()

	reopened, err := db.New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	if got := diaryFTSCount(t, reopened); got != 0 {
		t.Errorf("%d diary rows are still mirrored after reopening, want 0", got)
	}
}
