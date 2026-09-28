package db_test

// Tests for the writes the derivation jobs make: which day and which task a summary is filed under, how a day's digest is written and dated, how many summaries one compaction run is handed, and what happens to a thread id the model made up.

import (
	"context"
	"fmt"
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

// The domain a summary is tagged with is a majority vote over the episodes behind it. Anchoring that vote only on the task node's created_at let a task that is days old vote over days of unrelated activity; Since, the start of the flush the summary came from, is the floor.
func TestLogSemanticNode_DomainVoteIsBoundedByTheFlushWindow(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)
	raw := store.DB()

	if err := store.LogSemanticNode(ctx, memory.TaskSummary{TaskName: "long running thread", Summary: "opened the thread"}); err != nil {
		t.Fatalf("LogSemanticNode (create task): %v", err)
	}
	// A thread that has been open for two days, with two days of personal activity under it.
	if _, err := raw.ExecContext(ctx, `UPDATE nodes SET created_at = datetime('now','-2 days') WHERE type='task'`); err != nil {
		t.Fatalf("backdate the task node: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.LogEpisode(ctx, "Netflix", fmt.Sprintf("some show %d", i), "watching"); err != nil {
			t.Fatalf("LogEpisode (personal): %v", err)
		}
	}
	if _, err := raw.ExecContext(ctx, `UPDATE episodes SET created_at = datetime('now','-2 days')`); err != nil {
		t.Fatalf("backdate the personal episodes: %v", err)
	}
	// This flush's own episode: one work capture, minutes old.
	if _, err := store.LogEpisode(ctx, "Slack", "standup", "talking to the team"); err != nil {
		t.Fatalf("LogEpisode (work): %v", err)
	}

	domainOfLatestSummary := func() string {
		t.Helper()
		var domain string
		if err := raw.QueryRowContext(ctx, `SELECT domain FROM nodes WHERE type='summary' ORDER BY id DESC LIMIT 1`).Scan(&domain); err != nil {
			t.Fatalf("read the summary's domain: %v", err)
		}
		return domain
	}

	if err := store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: true, TaskName: "long running thread", Summary: "bounded by the flush window",
		Since: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("LogSemanticNode (bounded): %v", err)
	}
	if got := domainOfLatestSummary(); got != "work" {
		t.Errorf("domain = %q, want work — only the one episode inside the flush window should vote", got)
	}

	if err := store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: true, TaskName: "long running thread", Summary: "unbounded, votes over the whole task",
	}); err != nil {
		t.Fatalf("LogSemanticNode (unbounded): %v", err)
	}
	if got := domainOfLatestSummary(); got != "personal" {
		t.Errorf("domain = %q, want personal — with no floor the two days of episodes under the task all vote", got)
	}
}

// seedDayWithSummaries inserts a day → session → task → summaries chain, dated daysAgo days back, and returns the day node's id and the summary ids.
func seedDayWithSummaries(t *testing.T, ctx context.Context, store *db.Store, day string, daysAgo int, summaries []string) (int64, []int64) {
	t.Helper()
	raw := store.DB()
	back := fmt.Sprintf("-%d days", daysAgo)

	var userID int64
	if err := raw.QueryRowContext(ctx, `SELECT id FROM nodes WHERE type='user' LIMIT 1`).Scan(&userID); err != nil {
		t.Fatalf("find user node: %v", err)
	}
	// Get-or-create, so a test can seed the same day twice — the shape a day whose summaries cross the age cutoff over two runs has.
	ensure := func(parent int64, nodeType, content string) int64 {
		t.Helper()
		if _, err := raw.ExecContext(ctx, `INSERT OR IGNORE INTO nodes (parent_id, type, content, created_at) VALUES (?,?,?,datetime('now',?))`, parent, nodeType, content, back); err != nil {
			t.Fatalf("insert %s node %q: %v", nodeType, content, err)
		}
		var id int64
		if err := raw.QueryRowContext(ctx, `SELECT id FROM nodes WHERE parent_id=? AND type=? AND content=?`, parent, nodeType, content).Scan(&id); err != nil {
			t.Fatalf("find %s node %q: %v", nodeType, content, err)
		}
		return id
	}
	dayID := ensure(userID, "day", day)
	sessID := ensure(dayID, "session", "Old Session")
	taskID := ensure(sessID, "task", "Old Task")
	var ids []int64
	for _, c := range summaries {
		var id int64
		if err := raw.QueryRowContext(ctx, `INSERT INTO nodes (parent_id, type, content, created_at) VALUES (?,'summary',?,datetime('now',?)) RETURNING id`, taskID, c, back).Scan(&id); err != nil {
			t.Fatalf("insert summary %q: %v", c, err)
		}
		ids = append(ids, id)
	}
	return dayID, ids
}

// TestStore_ReplaceSummariesWithDigest covers three properties of one call: the summaries it rolls up survive reparented under the digest (not deleted) and both stay searchable via FTS, the digest is dated by the day it covers rather than the moment compaction ran, and a second pass over the same day rewrites the digest already there instead of creating a second one.
func TestStore_ReplaceSummariesWithDigest(t *testing.T) {
	t.Run("reparents summaries under the digest and keeps FTS in sync", func(t *testing.T) {
		ctx := context.Background()
		store := memStore(t)

		dayID := seedOldTree(t, ctx, store, "2026-05-10", []string{
			"user debugged the xgb tracker uniquetoken1",
			"user reviewed PR for audio pipeline uniquetoken2",
		})

		// grab the summary IDs just inserted
		rows, err := store.DB().QueryContext(ctx,
			`SELECT id FROM nodes WHERE type='summary' ORDER BY id ASC`)
		if err != nil {
			t.Fatalf("query summaries: %v", err)
		}
		var summaryIDs []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan id: %v", err)
			}
			summaryIDs = append(summaryIDs, id)
		}
		rows.Close()

		if len(summaryIDs) != 2 {
			t.Fatalf("expected 2 summary IDs before replace, got %d", len(summaryIDs))
		}

		digest := "user spent the day debugging the tracker and reviewing the audio PR uniquetoken3"
		if err := store.ReplaceSummariesWithDigest(ctx, dayID, summaryIDs, digest); err != nil {
			t.Fatalf("ReplaceSummariesWithDigest: %v", err)
		}

		// digest must exist under dayID
		var digestID int64
		var digestContent string
		if err := store.DB().QueryRowContext(ctx,
			`SELECT id, content FROM nodes WHERE type='digest' AND parent_id=?`, dayID).Scan(&digestID, &digestContent); err != nil {
			t.Fatalf("find digest node: %v", err)
		}
		if digestContent != digest {
			t.Errorf("digest content mismatch: got %q", digestContent)
		}

		// summaries must survive, reparented under the digest rather than deleted
		var sumCount int
		if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE type='summary'`).Scan(&sumCount); err != nil {
			t.Fatalf("count summaries: %v", err)
		}
		if sumCount != 2 {
			t.Errorf("expected 2 summaries to survive replace, got %d", sumCount)
		}
		for _, id := range summaryIDs {
			var parentID int64
			if err := store.DB().QueryRowContext(ctx, `SELECT parent_id FROM nodes WHERE id=?`, id).Scan(&parentID); err != nil {
				t.Fatalf("find summary %d: %v", id, err)
			}
			if parentID != digestID {
				t.Errorf("summary %d: expected parent_id=%d, got %d", id, digestID, parentID)
			}
		}

		// FTS: digest term must be searchable
		hits, err := store.SearchMemory(ctx, "uniquetoken3")
		if err != nil {
			t.Fatalf("SearchMemory for digest term: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("FTS returned no hits for digest term — digest insert trigger not firing")
		}
		if hits[0].Source != "digest" {
			t.Errorf("expected source=digest, got %s", hits[0].Source)
		}

		// FTS: terms unique to the surviving summaries must still be searchable — they were reparented, not deleted.
		oldHits1, err := store.SearchMemory(ctx, "uniquetoken1")
		if err != nil {
			t.Fatalf("SearchMemory for surviving summary term: %v", err)
		}
		if len(oldHits1) == 0 {
			t.Error("FTS lost surviving summary term 'uniquetoken1' — summary should not have been deleted")
		}

		oldHits2, err := store.SearchMemory(ctx, "uniquetoken2")
		if err != nil {
			t.Fatalf("SearchMemory for surviving summary term: %v", err)
		}
		if len(oldHits2) == 0 {
			t.Error("FTS lost surviving summary term 'uniquetoken2' — summary should not have been deleted")
		}
	})

	// A digest dated the moment compaction ran makes every reader that selects by created_at — the timeline, the evening close, the dream — narrate last week's work as today's. It carries the date of the day it covers.
	t.Run("dates the digest by the day it covers", func(t *testing.T) {
		ctx := context.Background()
		store := memStore(t)

		dayID, summaryIDs := seedDayWithSummaries(t, ctx, store, "2026-08-20", 12, []string{
			"reviewed the audio PR", "chased a flaky test",
		})

		if err := store.ReplaceSummariesWithDigest(ctx, dayID, summaryIDs, "a day of reviews and flaky tests"); err != nil {
			t.Fatalf("ReplaceSummariesWithDigest: %v", err)
		}

		// Both read as the raw stored text, since MAX() comes back as a string rather than a converted DATETIME.
		var digestAt, newestSummaryAt string
		if err := store.DB().QueryRowContext(ctx, `SELECT CAST(created_at AS TEXT) FROM nodes WHERE type='digest' AND parent_id=?`, dayID).Scan(&digestAt); err != nil {
			t.Fatalf("read digest created_at: %v", err)
		}
		if err := store.DB().QueryRowContext(ctx, `SELECT MAX(created_at) FROM nodes WHERE id IN (?,?)`, summaryIDs[0], summaryIDs[1]).Scan(&newestSummaryAt); err != nil {
			t.Fatalf("read the newest summary's created_at: %v", err)
		}

		if digestAt != newestSummaryAt {
			t.Errorf("digest created_at = %s, want the newest constituent summary's %s", digestAt, newestSummaryAt)
		}
		at, err := time.Parse("2006-01-02 15:04:05", digestAt)
		if err != nil {
			t.Fatalf("parse digest created_at %q: %v", digestAt, err)
		}
		if time.Since(at) < 24*time.Hour {
			t.Errorf("digest created_at = %s, which reads as today — a digest of a day twelve days back must not land in today's timeline", digestAt)
		}
	})

	// A day's summaries cross the age cutoff over more than one run, so a day gets digested again with the rest of its material. The reused digest node used to keep its first text and the newly generated paragraph was dropped, which froze the day's digest at whatever the first partial run said.
	t.Run("rewrites the reused digest on a second pass", func(t *testing.T) {
		ctx := context.Background()
		store := memStore(t)

		dayID, firstIDs := seedDayWithSummaries(t, ctx, store, "2026-08-21", 12, []string{
			"morning: reviewed the audio PR uniquefirsthalf",
		})
		if err := store.ReplaceSummariesWithDigest(ctx, dayID, firstIDs, "the morning, in one paragraph uniquemorning"); err != nil {
			t.Fatalf("ReplaceSummariesWithDigest (first pass): %v", err)
		}

		_, secondIDs := seedDayWithSummaries(t, ctx, store, "2026-08-21", 12, []string{
			"afternoon: shipped the fix uniquesecondhalf",
		})
		if err := store.ReplaceSummariesWithDigest(ctx, dayID, secondIDs, "the whole day, in one paragraph uniquewholeday"); err != nil {
			t.Fatalf("ReplaceSummariesWithDigest (second pass): %v", err)
		}

		var digestCount int
		if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE type='digest' AND parent_id=?`, dayID).Scan(&digestCount); err != nil {
			t.Fatalf("count digests: %v", err)
		}
		if digestCount != 1 {
			t.Fatalf("got %d digests for the day, want exactly 1", digestCount)
		}

		var content string
		if err := store.DB().QueryRowContext(ctx, `SELECT content FROM nodes WHERE type='digest' AND parent_id=?`, dayID).Scan(&content); err != nil {
			t.Fatalf("read digest content: %v", err)
		}
		if content != "the whole day, in one paragraph uniquewholeday" {
			t.Errorf("digest content = %q, want the text the second pass generated", content)
		}

		// The rewritten text has to be searchable, and the text it replaced must not still be.
		hits, err := store.SearchMemory(ctx, "uniquewholeday")
		if err != nil {
			t.Fatalf("SearchMemory for the rewritten digest: %v", err)
		}
		if len(hits) == 0 {
			t.Error("the rewritten digest is not in FTS")
		}
		stale, err := store.SearchMemory(ctx, "uniquemorning")
		if err != nil {
			t.Fatalf("SearchMemory for the replaced digest text: %v", err)
		}
		if len(stale) != 0 {
			t.Errorf("the replaced digest text is still in FTS: %+v", stale)
		}
	})
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
