package db_test

// Tests for the writes the derivation jobs make: which day and which task a summary is filed under, how a day's digest is written and dated, how many summaries one compaction run is handed, and what happens to a thread id the model made up.

import (
	"context"
	"testing"
	"time"

	"june/internal/db"
	"june/internal/memory"
)

// dayOfSummary walks summary → task → session → day and returns the day node's content for the summary whose text is the given one.
func dayOfSummary(t *testing.T, store *db.Store, summaryText string) string {
	t.Helper()
	var day string
	err := store.DB().QueryRow(`
		SELECT day.content
		FROM nodes sm
		JOIN nodes task ON sm.parent_id = task.id AND task.type = 'task'
		JOIN nodes sess ON task.parent_id = sess.id AND sess.type = 'session'
		JOIN nodes day  ON sess.parent_id = day.id  AND day.type = 'day'
		WHERE sm.type = 'summary' AND sm.content LIKE ?
	`, "%"+summaryText+"%").Scan(&day)
	if err != nil {
		t.Fatalf("find the day node above summary %q: %v", summaryText, err)
	}
	return day
}

// taskOfSummary returns the content of the task node the summary whose text is the given one hangs off.
func taskOfSummary(t *testing.T, store *db.Store, summaryText string) string {
	t.Helper()
	var task string
	err := store.DB().QueryRow(`
		SELECT task.content FROM nodes sm
		JOIN nodes task ON sm.parent_id = task.id AND task.type = 'task'
		WHERE sm.type = 'summary' AND sm.content LIKE ?
	`, "%"+summaryText+"%").Scan(&task)
	if err != nil {
		t.Fatalf("find the task node above summary %q: %v", summaryText, err)
	}
	return task
}

// A daemon left running past local midnight used to file every later summary under the day node it started on, because the day and session were resolved once at open. Each write resolves its own day.
func TestLogSemanticNode_FilesEachWriteUnderTheDayItHappened(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	evening := time.Date(2026, 3, 10, 23, 30, 0, 0, time.Local)
	store.SetClock(func() time.Time { return evening })
	if err := store.LogSemanticNode(ctx, memory.TaskSummary{TaskName: "evening work", Summary: "wrote the release notes"}); err != nil {
		t.Fatalf("LogSemanticNode (before midnight): %v", err)
	}

	afterMidnight := evening.Add(time.Hour)
	store.SetClock(func() time.Time { return afterMidnight })
	if err := store.LogSemanticNode(ctx, memory.TaskSummary{TaskName: "late night work", Summary: "chased the flaky test"}); err != nil {
		t.Fatalf("LogSemanticNode (after midnight): %v", err)
	}

	if got := dayOfSummary(t, store, "wrote the release notes"); got != "2026-03-10" {
		t.Errorf("the 23:30 summary is filed under day %q, want 2026-03-10", got)
	}
	if got := dayOfSummary(t, store, "chased the flaky test"); got != "2026-03-11" {
		t.Errorf("the 00:30 summary is filed under day %q, want 2026-03-11", got)
	}
}

// SameTask says the caller believes it is continuing the thread it named. With two threads interleaved — watching a show while coding, the case the attribution call exists for — trusting it alone filed each summary under whichever task was written last, so the show's summary landed under the coding task. The task node comes from the summary's own name.
func TestLogSemanticNode_InterleavedThreadsKeepTheirOwnTasks(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	writes := []memory.TaskSummary{
		{SameTask: false, TaskName: "watching suits", Summary: "started season one"},
		{SameTask: true, TaskName: "june compiler", Summary: "fixed the flush floor"},
		{SameTask: true, TaskName: "watching suits", Summary: "got to episode four"},
		{SameTask: true, TaskName: "june compiler", Summary: "wrote the test for it"},
	}
	for _, w := range writes {
		if err := store.LogSemanticNode(ctx, w); err != nil {
			t.Fatalf("LogSemanticNode(%q): %v", w.TaskName, err)
		}
	}

	for _, w := range writes {
		if got := taskOfSummary(t, store, w.Summary); got != w.TaskName {
			t.Errorf("summary %q is filed under task %q, want %q", w.Summary, got, w.TaskName)
		}
	}
}

// UpsertThread used to return whatever id it was handed, so a thread id the model invented or one since deleted had this flush's episodes linked to it and its summary filed against nothing. An id that matches no row falls through to the insert-by-subject path.
func TestUpsertThread_UnknownIDFallsThroughToInsertBySubject(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	id, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		ID: 4242, Subject: "reading the compiler", Kind: "work", State: "in the flush path",
	})
	if err != nil {
		t.Fatalf("UpsertThread with an unknown id: %v", err)
	}
	if id == 4242 {
		t.Fatal("UpsertThread returned the id it was handed, but no thread carries it")
	}

	var subject, state string
	if err := store.DB().QueryRowContext(ctx, `SELECT subject, state FROM threads WHERE id = ?`, id).Scan(&subject, &state); err != nil {
		t.Fatalf("read the thread that was created instead: %v", err)
	}
	if subject != "reading the compiler" || state != "in the flush path" {
		t.Errorf("created thread = (%q, %q), want the subject and state from the update", subject, state)
	}
}
