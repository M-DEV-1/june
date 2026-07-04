package db_test

// tests are first class citizens

import (
	"context"
	"fmt"
	"ora/internal/db"
	"ora/internal/memory"
	"os"
	"strings"
	"testing"
	"time"
)

// t param is test controller. object to provide methods to control the flow of the test + reporting
func TestStore_ActivityLifeCycle(t *testing.T) {
	ctx := context.Background()

	// 1. be able to create new store in memory for testing
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store : %+v", err)
	}
	defer store.Close()

	// 2. be able to log a semantic node (simulating the compiler)
	err = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "TDD Testing",
		Summary:  "Writing tests for SQLite CTE",
	})
	if err != nil {
		t.Errorf("Failed to log semantic node: %+v", err)
	}

	// 3. we want to get context back
	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Errorf("Failed to get context: %+v", err)
	}

	// 4. we verify the tree structure (User, Day, Session, Task, Summary)
	if len(branch) < 5 {
		t.Fatalf("Expected at least 5 nodes, got %d: %+v", len(branch), branch)
	}

	// check nodes ordering, expected ROOT to LEAF
	if !strings.Contains(branch[0], "default_user") {
		t.Errorf("Expected root node to contain user, got: %s", branch[0])
	}
	if !strings.Contains(branch[2], "session") {
		t.Errorf("Expected node to contain a session, got %s", branch[2])
	}
	if !strings.Contains(branch[len(branch)-1], "Writing tests") {
		t.Errorf("Expected leaf node to contain summary, got %s", branch[len(branch)-1])
	}

	t.Logf("Successfully retrieved branch: %+v", branch)
}

func TestStore_Notes_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	id, err := store.LogNote(ctx, "user prefers coffee over tea", "preference")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero note id")
	}

	notes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}

	if len(notes) != 1 {
		t.Fatalf("want 1 note, got %d", len(notes))
	}
	if notes[0].Content != "user prefers coffee over tea" {
		t.Errorf("unexpected content: %s", notes[0].Content)
	}
	if notes[0].Kind != "preference" {
		t.Errorf("unexpected kind: %s", notes[0].Kind)
	}
}

func TestStore_Notes_DeleteAndDedupe(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	id, _ := store.LogNote(ctx, "user works at Credibl ESG", "fact")

	// re-logging same content + kind is a no-op (idempotent)
	id2, err := store.LogNote(ctx, "user works at Credibl ESG", "fact")
	if err != nil {
		t.Fatalf("LogNote dedupe: %v", err)
	}
	if id2 != id {
		t.Errorf("expected idempotent insert to return same id, got %d != %d", id2, id)
	}

	if err := store.DeleteNote(ctx, id); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}

	notes, _ := store.GetNotes(ctx)
	if len(notes) != 0 {
		t.Fatalf("want 0 notes after delete, got %d", len(notes))
	}
}

// TestStore_GetImplicitContext_GatesIrrelevantNotes pins the Slice-0 behavior:
// implicit context must NOT dump identity notes unconditionally. A note unrelated
// to what the user is doing now stays out; the live thread that matches the
// current focus is what surfaces. This is the fix for "bombarding notes with no
// point → agent spews bullshit with no context."
func TestStore_GetImplicitContext_GatesIrrelevantNotes(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// current focus: debugging the ESG portal
	if err := store.SetWorkingState(ctx, "debugging the ESG Benchmarking Portal backend"); err != nil {
		t.Fatalf("SetWorkingState: %v", err)
	}
	// a live thread that matches what the user is doing now
	if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "ESG Benchmarking Portal",
		Kind:    "work",
		State:   "Monitoring CRD dashboard while debugging backend",
	}); err != nil {
		t.Fatalf("UpsertThread: %v", err)
	}
	// a durable identity note with nothing to do with the current focus
	if _, err := store.LogNote(ctx, "user has an interest in Pune real estate", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext: %v", err)
	}
	joined := strings.Join(branch, "\n")

	// the live thread must surface — that's the useful recall
	if !strings.Contains(joined, "ESG Benchmarking Portal") {
		t.Errorf("expected live thread in context, got: %+v", branch)
	}
	// the irrelevant identity note must NOT be dumped in unconditionally
	if strings.Contains(joined, "Pune real estate") {
		t.Errorf("irrelevant note leaked into context (unconditional note dump): %+v", branch)
	}
}

func TestStore_InitCreatesDirectory(t *testing.T) {
	path := "test_dir/test.db"
	defer os.RemoveAll("test_dir")

	store, err := db.New(path)
	if err != nil {
		t.Fatalf("Failed to create store in new directory: %+v", err)
	}
	defer store.Close()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("Database file was not created at %s", path)
	}
}

func TestStore_SearchMemory_FTS5(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed a summary and a note
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Voice Pipeline",
		Summary:  "Debugging WebSocket reconnect loop in Gemini Live session",
	})
	_, _ = store.LogNote(ctx, "user works at Credibl ESG as an intern", "fact")

	// FTS5 should find the summary by a tokenized word
	hits, err := store.SearchMemory(ctx, "WebSocket")
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("FTS5 returned no hits for 'WebSocket'")
	}
	if !strings.Contains(hits[0].Content, "WebSocket") {
		t.Errorf("expected hit to mention WebSocket: %s", hits[0].Content)
	}
	if hits[0].Source != "summary" {
		t.Errorf("expected source=summary, got %s", hits[0].Source)
	}

	// FTS5 should also surface notes
	noteHits, err := store.SearchMemory(ctx, "Credibl")
	if err != nil {
		t.Fatalf("SearchMemory notes: %v", err)
	}
	if len(noteHits) == 0 {
		t.Fatal("FTS5 returned no hits for 'Credibl'")
	}
	if noteHits[0].Source != "note" {
		t.Errorf("expected source=note, got %s", noteHits[0].Source)
	}
}

func TestStore_CullRawActivities(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed a summary so we can confirm it survives the cull
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Cull Test Task",
		Summary:  "Summary that must survive",
	})

	// log 3 recent activities (created_at = now)
	_ = store.LogActivity(ctx, "App1", "Title1")
	_ = store.LogActivity(ctx, "App2", "Title2")
	_ = store.LogActivity(ctx, "App3", "Title3")

	// direct-insert one activity backdated 100 hours
	_, err = store.DB().ExecContext(ctx,
		`INSERT INTO nodes (parent_id, type, content, created_at)
		 VALUES ((SELECT id FROM nodes WHERE type = 'session' LIMIT 1),
		         'activity', 'old-activity', datetime('now', '-100 hours'))`,
	)
	if err != nil {
		t.Fatalf("backdated insert: %v", err)
	}

	// cull anything older than 72 hours — only the backdated row qualifies
	culled, err := store.CullRawActivities(ctx, 72*time.Hour)
	if err != nil {
		t.Fatalf("CullRawActivities: %v", err)
	}
	if culled != 1 {
		t.Errorf("expected 1 row culled, got %d", culled)
	}

	// the 3 recent activities must still exist
	var actCount int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM nodes WHERE type = 'activity'`).Scan(&actCount); err != nil {
		t.Fatalf("count activities: %v", err)
	}
	if actCount != 3 {
		t.Errorf("expected 3 recent activities to remain, got %d", actCount)
	}

	// summary must be untouched
	var sumCount int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM nodes WHERE type = 'summary'`).Scan(&sumCount); err != nil {
		t.Fatalf("count summaries: %v", err)
	}
	if sumCount == 0 {
		t.Error("summary was deleted; cull must only touch 'activity' nodes")
	}
}

func TestStore_UpdateNote_AndFTSSync(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	id, err := store.LogNote(ctx, "user prefers terse responses", "fact")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	// old content must be searchable before update
	hits, err := store.SearchMemory(ctx, "terse")
	if err != nil {
		t.Fatalf("SearchMemory pre-update: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected FTS hit for old content before update")
	}

	if err := store.UpdateNote(ctx, id, "user prefers terse and concise responses"); err != nil {
		t.Fatalf("UpdateNote: %v", err)
	}

	// new content must be searchable
	hits, err = store.SearchMemory(ctx, "concise")
	if err != nil {
		t.Fatalf("SearchMemory post-update new term: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("FTS did not index new content after UpdateNote")
	}
	if !strings.Contains(hits[0].Content, "concise") {
		t.Errorf("unexpected FTS hit content: %s", hits[0].Content)
	}

	// notes table itself must reflect the new content
	notes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	if len(notes) != 1 || notes[0].Content != "user prefers terse and concise responses" {
		t.Errorf("GetNotes returned unexpected content: %+v", notes)
	}
}

func TestStore_ExistingNotes(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	id1, _ := store.LogNote(ctx, "user is a Go developer", "fact")
	id2, _ := store.LogNote(ctx, "user prefers dark mode", "preference")

	refs, err := store.ExistingNotes(ctx)
	if err != nil {
		t.Fatalf("ExistingNotes: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	// ordered by id ASC
	if refs[0].ID != id1 || refs[0].Content != "user is a Go developer" {
		t.Errorf("unexpected first ref: %+v", refs[0])
	}
	if refs[1].ID != id2 || refs[1].Content != "user prefers dark mode" {
		t.Errorf("unexpected second ref: %+v", refs[1])
	}
}

// seedOldTree inserts user→day→session→task→summary nodes using explicit old
// timestamps so OldSummaryGroups can find them. Returns the day node id.
func seedOldTree(t *testing.T, ctx context.Context, store *db.Store, dayContent string, summaries []string) int64 {
	t.Helper()
	raw := store.DB()

	var userID int64
	if err := raw.QueryRowContext(ctx, `SELECT id FROM nodes WHERE type='user' LIMIT 1`).Scan(&userID); err != nil {
		t.Fatalf("find user node: %v", err)
	}

	var dayID int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content, created_at) VALUES (?,?,?,datetime('now','-30 days')) RETURNING id`,
		userID, "day", dayContent).Scan(&dayID); err != nil {
		t.Fatalf("insert day node: %v", err)
	}

	var sessID int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content, created_at) VALUES (?,?,?,datetime('now','-30 days')) RETURNING id`,
		dayID, "session", "Old Session").Scan(&sessID); err != nil {
		t.Fatalf("insert session node: %v", err)
	}

	var taskID int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content, created_at) VALUES (?,?,?,datetime('now','-30 days')) RETURNING id`,
		sessID, "task", "Old Task").Scan(&taskID); err != nil {
		t.Fatalf("insert task node: %v", err)
	}

	for _, s := range summaries {
		if _, err := raw.ExecContext(ctx,
			`INSERT INTO nodes (parent_id, type, content, created_at) VALUES (?,?,?,datetime('now','-30 days'))`,
			taskID, "summary", s); err != nil {
			t.Fatalf("insert summary node: %v", err)
		}
	}

	return dayID
}

func TestStore_OldSummaryGroups_ReturnsGroupedByDay(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed two separate old days with summaries
	dayID1 := seedOldTree(t, ctx, store, "2026-05-01", []string{"summary-alpha", "summary-beta"})
	dayID2 := seedOldTree(t, ctx, store, "2026-05-02", []string{"summary-gamma"})

	// a fresh summary (created now) must NOT appear
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Current Task",
		Summary:  "very recent summary should not appear",
	})

	groups, err := store.OldSummaryGroups(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("OldSummaryGroups: %v", err)
	}

	// build a map by dayID for assertion order independence
	byDay := make(map[int64]memory.SummaryGroup)
	for _, g := range groups {
		byDay[g.DayID] = g
	}

	g1, ok := byDay[dayID1]
	if !ok {
		t.Fatalf("group for dayID %d not found; got groups: %+v", dayID1, groups)
	}
	if len(g1.Summaries) != 2 {
		t.Errorf("expected 2 summaries in day1 group, got %d", len(g1.Summaries))
	}

	g2, ok := byDay[dayID2]
	if !ok {
		t.Fatalf("group for dayID %d not found", dayID2)
	}
	if len(g2.Summaries) != 1 {
		t.Errorf("expected 1 summary in day2 group, got %d", len(g2.Summaries))
	}
}

func TestStore_WorkingState_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// empty before any set
	got, err := store.GetWorkingState(ctx)
	if err != nil {
		t.Fatalf("GetWorkingState (empty): %v", err)
	}
	if got != "" {
		t.Errorf("expected empty working state, got %q", got)
	}

	const first = "user is debugging the audio pipeline on Linux"
	if err := store.SetWorkingState(ctx, first); err != nil {
		t.Fatalf("SetWorkingState (first): %v", err)
	}

	got, err = store.GetWorkingState(ctx)
	if err != nil {
		t.Fatalf("GetWorkingState (after first set): %v", err)
	}
	if got != first {
		t.Errorf("expected %q, got %q", first, got)
	}

	// second Set must overwrite — single row
	const second = "user is now writing tests for the memory compiler"
	if err := store.SetWorkingState(ctx, second); err != nil {
		t.Fatalf("SetWorkingState (second): %v", err)
	}

	got, err = store.GetWorkingState(ctx)
	if err != nil {
		t.Fatalf("GetWorkingState (after second set): %v", err)
	}
	if got != second {
		t.Errorf("expected %q, got %q", second, got)
	}

	// verify only one row exists
	var rowCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM working_state`).Scan(&rowCount); err != nil {
		t.Fatalf("count working_state rows: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("expected exactly 1 working_state row, got %d", rowCount)
	}
}

func TestStore_GetImplicitContext_WithWorkingState(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed a summary so the tree has raw material
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Baseline Task",
		Summary:  "Writing baseline summary for context test",
	})

	// before working state is set, there must be no [now] line
	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext (no state): %v", err)
	}
	for _, line := range branch {
		if strings.Contains(line, "[now]") {
			t.Errorf("did not expect [now] line before working state is set: %s", line)
		}
	}

	// once set, working state appears as [now] and REPLACES the raw summary dump
	const state = "user is actively debugging the Linux audio pipeline and writing TDD tests"
	if err := store.SetWorkingState(ctx, state); err != nil {
		t.Fatalf("SetWorkingState: %v", err)
	}

	branch2, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext (with state): %v", err)
	}

	var foundState, hasRawSummary bool
	for _, line := range branch2 {
		if strings.Contains(line, "[now]") && strings.Contains(line, state) {
			foundState = true
		}
		if strings.Contains(line, "Writing baseline summary") {
			hasRawSummary = true
		}
	}
	if !foundState {
		t.Errorf("[now] line missing from context: %+v", branch2)
	}
	if hasRawSummary {
		t.Errorf("raw summary content must NOT appear when working state is set: %+v", branch2)
	}
}

func TestStore_GetImplicitContext_FallbackWhenNoState(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed a summary only. with no identity notes, no live threads, and no
	// working state, GetImplicitContext falls back to the recursive summary walk.
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Fallback Task",
		Summary:  "Checking fallback behavior works correctly",
	})

	// cold start: nothing synthesized → fallback path returns the summary tree
	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext fallback: %v", err)
	}

	var hasSummary bool
	for _, line := range branch {
		if strings.Contains(line, "Checking fallback behavior") {
			hasSummary = true
		}
	}
	if !hasSummary {
		t.Errorf("expected summary in fallback context: %+v", branch)
	}
}

func TestStore_RecentSummaries(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed 3 summaries
	for _, name := range []string{"first-summary-alpha", "second-summary-beta", "third-summary-gamma"} {
		_ = store.LogSemanticNode(ctx, memory.TaskSummary{
			SameTask: false,
			TaskName: name,
			Summary:  name + " content",
		})
	}

	// seed a digest via direct insert (should also appear)
	_, err = store.DB().ExecContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (
			(SELECT id FROM nodes WHERE type='day' LIMIT 1),
			'digest', 'digest-delta-content'
		)`)
	if err != nil {
		t.Fatalf("insert digest: %v", err)
	}

	recent, err := store.RecentSummaries(ctx, 10)
	if err != nil {
		t.Fatalf("RecentSummaries: %v", err)
	}

	// must include both summary and digest rows
	if len(recent) < 4 {
		t.Fatalf("expected at least 4 rows (3 summaries + 1 digest), got %d: %+v", len(recent), recent)
	}

	// newest-first: digest was inserted last, so it must appear first
	if !strings.Contains(recent[0], "digest-delta-content") {
		t.Errorf("expected digest to be first (newest), got: %s", recent[0])
	}

	// limit should be respected
	limited, err := store.RecentSummaries(ctx, 2)
	if err != nil {
		t.Fatalf("RecentSummaries (limited): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("expected exactly 2 rows with limit=2, got %d", len(limited))
	}
}

func TestStore_CountSummariesSince(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	before := time.Now()

	// seed 2 summaries AFTER the mark
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "New Task 1",
		Summary:  "new work alpha",
	})
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: true,
		TaskName: "New Task 1",
		Summary:  "new work beta",
	})

	// seed 1 old summary via direct insert
	_, err = store.DB().ExecContext(ctx,
		`INSERT INTO nodes (parent_id, type, content, created_at) VALUES (
			(SELECT id FROM nodes WHERE type='session' LIMIT 1),
			'summary', 'old-summary-content', datetime('now', '-2 hours')
		)`)
	if err != nil {
		t.Fatalf("insert old summary: %v", err)
	}

	count, err := store.CountSummariesSince(ctx, before)
	if err != nil {
		t.Fatalf("CountSummariesSince: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 summaries after 'before', got %d", count)
	}

	// counting from the future should return 0
	future := time.Now().Add(time.Hour)
	count2, err := store.CountSummariesSince(ctx, future)
	if err != nil {
		t.Fatalf("CountSummariesSince (future): %v", err)
	}
	if count2 != 0 {
		t.Errorf("expected 0 summaries in the future, got %d", count2)
	}
}

func TestStore_ReplaceSummariesWithDigest_TransactionAndFTS(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

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

	// summaries must be gone
	var sumCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE type='summary'`).Scan(&sumCount); err != nil {
		t.Fatalf("count summaries: %v", err)
	}
	if sumCount != 0 {
		t.Errorf("expected 0 summaries after replace, got %d", sumCount)
	}

	// digest must exist under dayID
	var digestContent string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT content FROM nodes WHERE type='digest' AND parent_id=?`, dayID).Scan(&digestContent); err != nil {
		t.Fatalf("find digest node: %v", err)
	}
	if digestContent != digest {
		t.Errorf("digest content mismatch: got %q", digestContent)
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

	// FTS: terms unique to deleted summaries must be gone
	oldHits1, err := store.SearchMemory(ctx, "uniquetoken1")
	if err != nil {
		t.Fatalf("SearchMemory for deleted summary term: %v", err)
	}
	if len(oldHits1) != 0 {
		t.Errorf("FTS still returns deleted summary term 'uniquetoken1' — delete trigger not firing")
	}

	oldHits2, err := store.SearchMemory(ctx, "uniquetoken2")
	if err != nil {
		t.Fatalf("SearchMemory for deleted summary term: %v", err)
	}
	if len(oldHits2) != 0 {
		t.Errorf("FTS still returns deleted summary term 'uniquetoken2' — delete trigger not firing")
	}
}

// ─── Thread tests ─────────────────────────────────────────────────────────────

// TestStore_UpsertThread_NewThread verifies that a zero-ID upsert creates a new
// row with the right initial salience (0.5 when Novel=false, 0.6 when Novel=true),
// times_seen=1, and status='active'.
func TestStore_UpsertThread_NewThread(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	cases := []struct {
		novel        bool
		wantSalience float64
	}{
		{novel: false, wantSalience: 0.5},
		{novel: true, wantSalience: 0.6},
	}

	for _, tc := range cases {
		subject := fmt.Sprintf("project-novel-%v", tc.novel)
		id, err := store.UpsertThread(ctx, memory.ThreadUpdate{
			Subject: subject,
			Kind:    "work",
			State:   "working on it",
			Novel:   tc.novel,
		})
		if err != nil {
			t.Fatalf("UpsertThread (novel=%v): %v", tc.novel, err)
		}
		if id == 0 {
			t.Fatalf("novel=%v: expected non-zero id", tc.novel)
		}

		var sal float64
		var timesSeen int
		var status string
		if err := store.DB().QueryRowContext(ctx,
			`SELECT salience, times_seen, status FROM threads WHERE id = ?`, id).
			Scan(&sal, &timesSeen, &status); err != nil {
			t.Fatalf("query thread (novel=%v): %v", tc.novel, err)
		}
		if sal != tc.wantSalience {
			t.Errorf("novel=%v: want salience %v, got %v", tc.novel, tc.wantSalience, sal)
		}
		if timesSeen != 1 {
			t.Errorf("novel=%v: want times_seen=1, got %d", tc.novel, timesSeen)
		}
		if status != "active" {
			t.Errorf("novel=%v: want status='active', got %q", tc.novel, status)
		}
	}
}

// TestStore_UpsertThread_Conflict verifies that a second zero-ID upsert with the
// same (subject, kind) updates state, bumps times_seen to 2, raises salience by
// ~0.05 (capped at ≤1.0), and returns the same id with exactly one row.
func TestStore_UpsertThread_Conflict(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	id1, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "ORA project",
		Kind:    "work",
		State:   "initial state",
		Novel:   false,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	var sal1 float64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT salience FROM threads WHERE id = ?`, id1).Scan(&sal1); err != nil {
		t.Fatalf("query salience before conflict: %v", err)
	}

	// second upsert with same subject+kind, different state
	id2, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "ORA project",
		Kind:    "work",
		State:   "writing more tests",
		Novel:   false,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id2 != id1 {
		t.Errorf("conflict must return same id: got %d, want %d", id2, id1)
	}

	var sal2 float64
	var timesSeen int
	var state string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT salience, times_seen, state FROM threads WHERE id = ?`, id1).
		Scan(&sal2, &timesSeen, &state); err != nil {
		t.Fatalf("query after conflict: %v", err)
	}
	if sal2 <= sal1 {
		t.Errorf("salience should increase on conflict: before=%v after=%v", sal1, sal2)
	}
	if sal2 > 1.0 {
		t.Errorf("salience must not exceed 1.0, got %v", sal2)
	}
	if timesSeen != 2 {
		t.Errorf("times_seen should be 2 after conflict, got %d", timesSeen)
	}
	if state != "writing more tests" {
		t.Errorf("state should be updated to new value, got %q", state)
	}

	// must stay at exactly one row
	var count int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM threads WHERE subject = 'ORA project' AND kind = 'work'`).Scan(&count); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if count != 1 {
		t.Errorf("conflict must keep single row, got %d rows", count)
	}
}

// TestStore_UpsertThread_SalienceCap verifies that salience never exceeds 1.0
// regardless of how many times the same thread is upserted.
func TestStore_UpsertThread_SalienceCap(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	for i := 0; i < 30; i++ {
		if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{
			Subject: "recurring thread",
			Kind:    "work",
			State:   fmt.Sprintf("iteration %d", i),
			Novel:   true,
		}); err != nil {
			t.Fatalf("upsert iter %d: %v", i, err)
		}
	}

	var sal float64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT salience FROM threads WHERE subject = 'recurring thread' AND kind = 'work'`).Scan(&sal); err != nil {
		t.Fatalf("query salience: %v", err)
	}
	if sal > 1.0 {
		t.Errorf("salience must not exceed 1.0 after many upserts, got %v", sal)
	}
}

// TestStore_UpsertThread_ExplicitID verifies the ID>0 update path: state is
// replaced, times_seen is incremented, and the same id is returned.
func TestStore_UpsertThread_ExplicitID(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	id, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "ORA project",
		Kind:    "work",
		State:   "initial",
		Novel:   false,
	})
	if err != nil {
		t.Fatalf("initial insert: %v", err)
	}

	var timesSeen1 int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT times_seen FROM threads WHERE id = ?`, id).Scan(&timesSeen1); err != nil {
		t.Fatalf("query times_seen before explicit upsert: %v", err)
	}

	returnedID, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		ID:    id,
		State: "explicit-state-update",
	})
	if err != nil {
		t.Fatalf("explicit-id upsert: %v", err)
	}
	if returnedID != id {
		t.Errorf("explicit-id upsert must return same id: got %d, want %d", returnedID, id)
	}

	var state string
	var timesSeen2 int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT state, times_seen FROM threads WHERE id = ?`, id).
		Scan(&state, &timesSeen2); err != nil {
		t.Fatalf("query after explicit upsert: %v", err)
	}
	if state != "explicit-state-update" {
		t.Errorf("state should be replaced: got %q", state)
	}
	if timesSeen2 != timesSeen1+1 {
		t.Errorf("times_seen should increment: got %d, want %d", timesSeen2, timesSeen1+1)
	}
}

// TestStore_GetLiveThreads_RecencyWindow verifies the 2-day cutoff: a thread
// last_seen within 2 days is returned; one older than 2 days is not. Also checks
// newest-first ordering and that the limit parameter is honored.
func TestStore_GetLiveThreads_RecencyWindow(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	// recent thread (last_seen = now)
	recentID, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "recent-work",
		Kind:    "work",
		State:   "in progress",
	})
	if err != nil {
		t.Fatalf("insert recent thread: %v", err)
	}

	// day-old thread (still within 2-day window)
	dayOldID, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "day-old-work",
		Kind:    "work",
		State:   "ongoing",
	})
	if err != nil {
		t.Fatalf("insert day-old thread: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE threads SET last_seen_at = datetime('now', '-1 day') WHERE id = ?`, dayOldID); err != nil {
		t.Fatalf("backdate day-old thread: %v", err)
	}

	// thread older than 2 days (must NOT appear)
	oldID, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "old-work",
		Kind:    "work",
		State:   "stale",
	})
	if err != nil {
		t.Fatalf("insert old thread: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE threads SET last_seen_at = datetime('now', '-3 days') WHERE id = ?`, oldID); err != nil {
		t.Fatalf("backdate old thread: %v", err)
	}

	threads, err := store.GetLiveThreads(ctx, 10)
	if err != nil {
		t.Fatalf("GetLiveThreads: %v", err)
	}

	byID := make(map[int64]int) // id → index in result
	for i, th := range threads {
		byID[th.ID] = i
	}
	if _, ok := byID[recentID]; !ok {
		t.Error("recent thread should appear in live threads")
	}
	if _, ok := byID[dayOldID]; !ok {
		t.Error("1-day-old thread should appear in live threads (within 2-day window)")
	}
	if _, ok := byID[oldID]; ok {
		t.Error("3-day-old thread must NOT appear in live threads")
	}

	// ordering: newest-first; recent must precede day-old
	recentIdx, dayOldIdx := byID[recentID], byID[dayOldID]
	if recentIdx > dayOldIdx {
		t.Errorf("recent thread (idx %d) should come before day-old thread (idx %d)", recentIdx, dayOldIdx)
	}

	// limit is honored
	limited, err := store.GetLiveThreads(ctx, 1)
	if err != nil {
		t.Fatalf("GetLiveThreads (limit=1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit=1 should return exactly 1 thread, got %d", len(limited))
	}
}

// TestStore_ThreadsForAttribution_Window verifies the 14-day cutoff. Threads
// touched 3 days ago appear here (but not in GetLiveThreads); threads touched
// 15 days ago appear in neither.
func TestStore_ThreadsForAttribution_Window(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	recentID, _ := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "recent", Kind: "work", State: "s"})

	mid3dID, _ := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "mid-3d", Kind: "work", State: "s"})
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE threads SET last_seen_at = datetime('now', '-3 days') WHERE id = ?`, mid3dID); err != nil {
		t.Fatalf("backdate 3d thread: %v", err)
	}

	tooOldID, _ := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "too-old", Kind: "work", State: "s"})
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE threads SET last_seen_at = datetime('now', '-15 days') WHERE id = ?`, tooOldID); err != nil {
		t.Fatalf("backdate 15d thread: %v", err)
	}

	attrThreads, err := store.ThreadsForAttribution(ctx, 50)
	if err != nil {
		t.Fatalf("ThreadsForAttribution: %v", err)
	}

	attrByID := make(map[int64]bool)
	for _, th := range attrThreads {
		attrByID[th.ID] = true
	}
	if !attrByID[recentID] {
		t.Error("recent thread should appear in 14-day attribution window")
	}
	if !attrByID[mid3dID] {
		t.Error("3-day-old thread should appear in 14-day attribution window")
	}
	if attrByID[tooOldID] {
		t.Error("15-day-old thread must NOT appear in 14-day attribution window")
	}

	// confirm 3-day-old is outside the 2-day live window
	live, err := store.GetLiveThreads(ctx, 50)
	if err != nil {
		t.Fatalf("GetLiveThreads: %v", err)
	}
	liveByID := make(map[int64]bool)
	for _, th := range live {
		liveByID[th.ID] = true
	}
	if liveByID[mid3dID] {
		t.Error("3-day-old thread must NOT appear in 2-day live window")
	}
}

// TestStore_GetImplicitContext_ThreadFormat verifies that GetImplicitContext
// emits [about] lines for identity notes (capped at 8), [thread:kind] subject — state
// for threads with a state, [thread:kind] subject for threads without a state,
// and [now] for working_state.
func TestStore_GetImplicitContext_ThreadFormat(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	// seed 10 notes — none should be dumped as [about] lines anymore (relevance-gated)
	for i := 0; i < 10; i++ {
		_, _ = store.LogNote(ctx, fmt.Sprintf("identity fact %d", i), "fact")
	}

	// thread with state
	_, _ = store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "Suits",
		Kind:    "entertainment",
		State:   "season 1 episode 3",
	})

	// thread without state (empty string)
	_, _ = store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "ORA project",
		Kind:    "work",
		State:   "",
	})

	_ = store.SetWorkingState(ctx, "debugging the audio pipeline")

	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext: %v", err)
	}

	var aboutCount int
	var foundThreadWithState, foundThreadNoState, foundNow bool
	for _, line := range branch {
		if strings.HasPrefix(line, "[about] ") {
			aboutCount++
		}
		// [thread:entertainment] Suits — season 1 episode 3
		if line == "[thread:entertainment] Suits — season 1 episode 3" {
			foundThreadWithState = true
		}
		// [thread:work] ORA project (no state → no em dash suffix)
		if line == "[thread:work] ORA project" {
			foundThreadNoState = true
		}
		if strings.HasPrefix(line, "[now] ") {
			foundNow = true
		}
	}

	if aboutCount != 0 {
		t.Errorf("identity notes must no longer be dumped as [about] lines (relevance-gated now), got %d", aboutCount)
	}
	if !foundThreadWithState {
		t.Errorf("[thread:entertainment] Suits — state line not found in: %v", branch)
	}
	if !foundThreadNoState {
		t.Errorf("[thread:work] ORA project (no-state) line not found in: %v", branch)
	}
	if !foundNow {
		t.Errorf("[now] line not found in: %v", branch)
	}
}

// TestStore_SearchMemory_FindsThread verifies that after UpsertThread, SearchMemory
// returns a hit whose Source is "thread".
func TestStore_SearchMemory_FindsThread(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	_, err = store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "quuxzap project",
		Kind:    "work",
		State:   "writing integration tests",
	})
	if err != nil {
		t.Fatalf("UpsertThread: %v", err)
	}

	hits, err := store.SearchMemory(ctx, "quuxzap")
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchMemory returned no hits for thread subject word 'quuxzap'")
	}
	if hits[0].Source != "thread" {
		t.Errorf("expected source='thread', got %q", hits[0].Source)
	}
}

// ─── Relevance retrieval tests (B2) ───────────────────────────────────────────

func TestStore_RetrieveRelevant_Basic(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed a note and a summary that will match via FTS
	_, _ = store.LogNote(ctx, "user prefers dark mode for coding", "preference")
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "UI work",
		Summary:  "Fixing layout in dark mode editor",
	})

	// basic call with focus should surface matching items
	results, err := store.RetrieveRelevant(ctx, "dark mode", 10)
	if err != nil {
		t.Fatalf("RetrieveRelevant: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected some relevant results for 'dark mode'")
	}
	foundNote, foundSummary := false, false
	for _, r := range results {
		if strings.Contains(r, "dark mode") {
			if strings.Contains(r, "[note]") {
				foundNote = true
			} else if strings.Contains(r, "[summary]") {
				foundSummary = true
			}
		}
	}
	if !foundNote {
		t.Errorf("expected note in RetrieveRelevant results: %+v", results)
	}
	if !foundSummary {
		t.Errorf("expected summary in RetrieveRelevant results: %+v", results)
	}
}

// TestStore_GetImplicitContext_WiresRelevanceRetrieval verifies that the
// working-state focus drives the relevance-retrieval layer, surfacing a matching
// item as a [note] line (distinct from the always-on [about] identity dump).
// Exclusion of unrelated items is covered by the dedicated RetrieveRelevant tests.
func TestStore_GetImplicitContext_WiresRelevanceRetrieval(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// a relevant note; no summaries/tasks so the focus signal stays clean
	_, _ = store.LogNote(ctx, "debugging Linux audio pipeline crackle", "fact")

	const state = "debugging Linux audio"
	if err := store.SetWorkingState(ctx, state); err != nil {
		t.Fatalf("SetWorkingState: %v", err)
	}

	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext: %v", err)
	}

	var hasRelevanceLine bool
	for _, b := range branch {
		if strings.HasPrefix(b, "[note]") && strings.Contains(b, "Linux audio pipeline") {
			hasRelevanceLine = true
		}
	}
	if !hasRelevanceLine {
		t.Errorf("expected a [note] relevance line driven by working-state focus: %+v", branch)
	}
}

func TestStore_RetrieverInterface(t *testing.T) {
	var _ db.Retriever = (*db.Store)(nil)
}

func TestStore_RetrieveRelevant_FocusAffectsResults(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	_, _ = store.LogNote(ctx, "user works with Go and SQLite", "fact")
	_, _ = store.LogNote(ctx, "user likes hiking in mountains", "fact")

	// focus on Go should return Go note
	goResults, _ := store.RetrieveRelevant(ctx, "Go and SQLite", 5)
	foundGo := false
	for _, r := range goResults {
		if strings.Contains(r, "Go and SQLite") {
			foundGo = true
		}
	}
	if !foundGo {
		t.Errorf("focus 'Go' should surface Go note, got: %+v", goResults)
	}

	// different focus should not surface unrelated
	hikeResults, _ := store.RetrieveRelevant(ctx, "hiking in mountains", 5)
	foundHikeInGoFocus := false
	for _, r := range goResults {
		if strings.Contains(r, "hiking") {
			foundHikeInGoFocus = true
		}
	}
	if foundHikeInGoFocus {
		t.Errorf("focus on Go should not surface hike note: %+v", goResults)
	}
	// check that different focus returns different sets
	if len(goResults) > 0 && len(hikeResults) > 0 && goResults[0] == hikeResults[0] {
		t.Errorf("different focus should return different result sets, got: %+v vs %+v", goResults, hikeResults)
	}
}

// ─── Episode tests (Cycle 1: append-only episode storage) ────────────────────

// TestStore_LogEpisode_AppendOnly_NoDedupe verifies episodes are NOT deduped
// like nodes/notes are: logging the same app+title twice with different
// screen_text must persist as two distinct rows. This is the whole point of a
// dedicated episodes table instead of reusing the nodes tree (whose unique
// index on (parent_id,type,content) would wrongly collapse repeat visits).
// It also verifies SearchEpisodes finds a distinctive word via FTS5.
func TestStore_LogEpisode_AppendOnly_NoDedupe(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	id1, err := store.LogEpisode(ctx, "Firefox", "Batman Wiki", "Reading about the Riddler's origin story")
	if err != nil {
		t.Fatalf("LogEpisode (1): %v", err)
	}
	if id1 == 0 {
		t.Fatal("expected non-zero episode id")
	}

	id2, err := store.LogEpisode(ctx, "Firefox", "Batman Wiki", "Now reading about Two-Face instead")
	if err != nil {
		t.Fatalf("LogEpisode (2): %v", err)
	}
	if id2 == 0 {
		t.Fatal("expected non-zero episode id")
	}
	if id2 == id1 {
		t.Errorf("expected distinct ids for repeat app+title visits, got same id %d twice", id1)
	}

	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM episodes`).Scan(&count); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 distinct episode rows (no dedupe), got %d", count)
	}

	hits, err := store.SearchEpisodes(ctx, "Riddler")
	if err != nil {
		t.Fatalf("SearchEpisodes: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchEpisodes returned no hits for 'Riddler'")
	}
	if !strings.Contains(hits[0].Content, "Riddler") {
		t.Errorf("expected hit content to contain 'Riddler', got: %s", hits[0].Content)
	}
	if hits[0].Source != "episode" {
		t.Errorf("expected source='episode', got %q", hits[0].Source)
	}
}

// ─── Episode tests (Cycle 2: retrieval surfaces episodes) ────────────────────

// TestStore_RetrieveRelevant_IncludesEpisodes verifies that RetrieveRelevant
// merges episode hits alongside note/summary/thread hits, formatted as
// "[episode] <screen_text excerpt>", and that those hits are ordered by the
// RankedEpisodes weighted score (recency+importance+relevance) rather than
// plain FTS rank: a recent, important episode must be surfaced before a
// stale, trivial-importance one that matches the same focus term.
func TestStore_RetrieveRelevant_IncludesEpisodes(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	richID, err := store.LogEpisode(ctx, "Firefox", "Gotham News", "Breaking: Commissioner Gordon holds press conference about the Riddler's latest scheme downtown")
	if err != nil {
		t.Fatalf("LogEpisode (rich/recent): %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE episodes SET importance = 0.95, created_at = datetime('now') WHERE id = ?`, richID); err != nil {
		t.Fatalf("backdate rich episode: %v", err)
	}

	staleID, err := store.LogEpisode(ctx, "Notes", "old memo", "Riddler Riddler Riddler mentioned once in a stale note")
	if err != nil {
		t.Fatalf("LogEpisode (stale/trivial): %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE episodes SET importance = 0.05, created_at = datetime('now', '-720 hours') WHERE id = ?`, staleID); err != nil {
		t.Fatalf("backdate stale episode: %v", err)
	}

	results, err := store.RetrieveRelevant(ctx, "Riddler", 10)
	if err != nil {
		t.Fatalf("RetrieveRelevant: %v", err)
	}

	var foundEpisode bool
	richIdx, staleIdx := -1, -1
	for i, r := range results {
		if strings.HasPrefix(r, "[episode] ") && strings.Contains(r, "Riddler") {
			foundEpisode = true
		}
		if strings.Contains(r, "press conference") {
			richIdx = i
		}
		if strings.Contains(r, "stale note") {
			staleIdx = i
		}
	}
	if !foundEpisode {
		t.Errorf("expected a [episode] line matching focus in RetrieveRelevant results: %+v", results)
	}
	if richIdx == -1 {
		t.Fatalf("expected rich/recent episode in results: %+v", results)
	}
	if staleIdx == -1 {
		t.Fatalf("expected stale/trivial episode in results: %+v", results)
	}
	if richIdx > staleIdx {
		t.Errorf("expected recent+important episode (idx %d) to rank before stale low-importance one (idx %d), i.e. RetrieveRelevant should use RankedEpisodes not plain FTS: %+v", richIdx, staleIdx, results)
	}
}

// ─── Episode tests (Cycle 3: importance heuristic) ────────────────────────────

// TestStore_LogEpisode_ImportanceHeuristic verifies the stored importance score
// reflects both signals: richer screen_text and revisitation (a prior episode
// with the same app+title) should score higher than a sparse, first-visit one.
func TestStore_LogEpisode_ImportanceHeuristic(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// first-visit, trivial screen_text -> low importance
	trivialID, err := store.LogEpisode(ctx, "Notepad", "untitled.txt", "hi")
	if err != nil {
		t.Fatalf("LogEpisode (trivial): %v", err)
	}

	// prior visit to the same app+title, so the next visit counts as a revisit
	if _, err := store.LogEpisode(ctx, "VSCode", "main.go — ora", "package main\n\nfunc main() {}"); err != nil {
		t.Fatalf("LogEpisode (seed revisit): %v", err)
	}
	richID, err := store.LogEpisode(ctx, "VSCode", "main.go — ora", strings.Repeat("word ", 300))
	if err != nil {
		t.Fatalf("LogEpisode (rich revisit): %v", err)
	}

	var trivialImportance, richImportance float64
	if err := store.DB().QueryRowContext(ctx, `SELECT importance FROM episodes WHERE id = ?`, trivialID).Scan(&trivialImportance); err != nil {
		t.Fatalf("query trivial importance: %v", err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT importance FROM episodes WHERE id = ?`, richID).Scan(&richImportance); err != nil {
		t.Fatalf("query rich importance: %v", err)
	}

	if richImportance <= trivialImportance {
		t.Errorf("expected rich+revisited episode importance (%v) > trivial first-visit importance (%v)", richImportance, trivialImportance)
	}
	if richImportance < 0 || richImportance > 1 {
		t.Errorf("importance must be in [0,1], got %v", richImportance)
	}
	if trivialImportance < 0 || trivialImportance > 1 {
		t.Errorf("importance must be in [0,1], got %v", trivialImportance)
	}
}

// ─── Episode tests (Cycle 4: ranking) ─────────────────────────────────────────

// TestStore_RankedEpisodes_WeightedOrdering constructs three episodes where
// recency/importance/relevance pull in different directions and asserts that
// a slightly-less-relevant but far-more-important+recent episode outranks a
// stale, barely-relevant one, per the documented weighted formula.
func TestStore_RankedEpisodes_WeightedOrdering(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	raw := store.DB()

	// Winner: recent, high importance, decent (but not perfect) relevance.
	winnerID, err := store.LogEpisode(ctx, "VSCode", "compiler.go", "refactoring the memory compiler ranking logic today")
	if err != nil {
		t.Fatalf("LogEpisode (winner): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.95, created_at = datetime('now') WHERE id = ?`, winnerID); err != nil {
		t.Fatalf("backdate winner: %v", err)
	}

	// Loser: stale (30 days old), low importance, but a slightly more literal
	// relevance match on the focus term.
	loserID, err := store.LogEpisode(ctx, "Notes", "old memo", "ranking ranking ranking notes from a month ago")
	if err != nil {
		t.Fatalf("LogEpisode (loser): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.05, created_at = datetime('now', '-720 hours') WHERE id = ?`, loserID); err != nil {
		t.Fatalf("backdate loser: %v", err)
	}

	results, err := store.RankedEpisodes(ctx, "ranking", 10)
	if err != nil {
		t.Fatalf("RankedEpisodes: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 ranked episodes, got %d: %+v", len(results), results)
	}

	winnerIdx, loserIdx := -1, -1
	for i, r := range results {
		if strings.Contains(r.Content, "refactoring the memory compiler") {
			winnerIdx = i
		}
		if strings.Contains(r.Content, "old memo") || strings.Contains(r.Content, "month ago") {
			loserIdx = i
		}
	}
	if winnerIdx == -1 {
		t.Fatalf("winner episode not found in results: %+v", results)
	}
	if loserIdx == -1 {
		t.Fatalf("loser episode not found in results: %+v", results)
	}
	if winnerIdx > loserIdx {
		t.Errorf("expected recent+important episode (idx %d) to outrank stale low-importance episode (idx %d): %+v", winnerIdx, loserIdx, results)
	}

	// limit is honored
	limited, err := store.RankedEpisodes(ctx, "ranking", 1)
	if err != nil {
		t.Fatalf("RankedEpisodes (limit=1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("expected exactly 1 result with limit=1, got %d", len(limited))
	}
}

// ─── Episode tests (Cycle 5: culling / aging) ─────────────────────────────────

// TestStore_AgeEpisodes_DropsOnlyOldLowImportance seeds an old low-importance
// episode, an old high-importance episode, and a recent one, and verifies
// AgeEpisodes empties screen_text only for the old low-importance row while
// keeping the row itself (ts/app/title/importance intact) — the other two
// keep their screen_text untouched.
func TestStore_AgeEpisodes_DropsOnlyOldLowImportance(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()
	raw := store.DB()

	oldLowID, err := store.LogEpisode(ctx, "Notes", "old low", "trivial old content")
	if err != nil {
		t.Fatalf("LogEpisode (old low): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.1, created_at = datetime('now', '-720 hours') WHERE id = ?`, oldLowID); err != nil {
		t.Fatalf("backdate old low: %v", err)
	}

	oldHighID, err := store.LogEpisode(ctx, "VSCode", "old high", "important old content about the core architecture")
	if err != nil {
		t.Fatalf("LogEpisode (old high): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.9, created_at = datetime('now', '-720 hours') WHERE id = ?`, oldHighID); err != nil {
		t.Fatalf("backdate old high: %v", err)
	}

	recentID, err := store.LogEpisode(ctx, "Notes", "recent low", "trivial recent content")
	if err != nil {
		t.Fatalf("LogEpisode (recent): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.1 WHERE id = ?`, recentID); err != nil {
		t.Fatalf("set recent importance: %v", err)
	}

	aged, err := store.AgeEpisodes(ctx, 168*time.Hour, 0.5)
	if err != nil {
		t.Fatalf("AgeEpisodes: %v", err)
	}
	if aged != 1 {
		t.Errorf("expected exactly 1 episode aged, got %d", aged)
	}

	var oldLowText, oldHighText, recentText string
	if err := raw.QueryRowContext(ctx, `SELECT screen_text FROM episodes WHERE id = ?`, oldLowID).Scan(&oldLowText); err != nil {
		t.Fatalf("query old low text: %v", err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT screen_text FROM episodes WHERE id = ?`, oldHighID).Scan(&oldHighText); err != nil {
		t.Fatalf("query old high text: %v", err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT screen_text FROM episodes WHERE id = ?`, recentID).Scan(&recentText); err != nil {
		t.Fatalf("query recent text: %v", err)
	}

	if oldLowText != "" {
		t.Errorf("expected old low-importance episode's screen_text to be emptied, got %q", oldLowText)
	}
	if oldHighText != "important old content about the core architecture" {
		t.Errorf("old high-importance episode's screen_text must survive, got %q", oldHighText)
	}
	if recentText != "trivial recent content" {
		t.Errorf("recent episode's screen_text must survive, got %q", recentText)
	}

	// rows must still exist (never deleted)
	var count int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM episodes`).Scan(&count); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if count != 3 {
		t.Errorf("expected all 3 episode rows to survive aging, got %d", count)
	}

	// FTS mirror must not keep surfacing the cleared content (external-content
	// fts5 requires the update trigger to purge the stale index entry).
	staleHits, err := store.SearchEpisodes(ctx, "trivial old content")
	if err != nil {
		t.Fatalf("SearchEpisodes after aging: %v", err)
	}
	for _, h := range staleHits {
		if strings.Contains(h.Content, "trivial old content") {
			t.Errorf("aged episode's old content still searchable via FTS: %+v", staleHits)
		}
	}
}

// ─── Consolidation retrieval (Cycle 1: temporal walk) ─────────────────────────

// TestStore_EpisodesInWindow_ChronologicalAndBounded seeds episodes at
// controlled timestamps spanning a day, plus one episode clearly outside the
// window, and verifies EpisodesInWindow returns only the in-window rows,
// ordered oldest-first (chronological, i.e. the "day arc"), and honors limit.
func TestStore_EpisodesInWindow_ChronologicalAndBounded(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()
	raw := store.DB()

	morningID, err := store.LogEpisode(ctx, "Mail", "Inbox", "reading morning emails")
	if err != nil {
		t.Fatalf("LogEpisode (morning): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET created_at = '2026-07-04 08:00:00' WHERE id = ?`, morningID); err != nil {
		t.Fatalf("backdate morning: %v", err)
	}

	noonID, err := store.LogEpisode(ctx, "VSCode", "main.go", "writing the consolidation layer")
	if err != nil {
		t.Fatalf("LogEpisode (noon): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET created_at = '2026-07-04 12:00:00' WHERE id = ?`, noonID); err != nil {
		t.Fatalf("backdate noon: %v", err)
	}

	eveningID, err := store.LogEpisode(ctx, "Firefox", "News", "reading the evening news")
	if err != nil {
		t.Fatalf("LogEpisode (evening): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET created_at = '2026-07-04 20:00:00' WHERE id = ?`, eveningID); err != nil {
		t.Fatalf("backdate evening: %v", err)
	}

	// clearly outside the window: the day before
	outsideID, err := store.LogEpisode(ctx, "Notes", "old memo", "yesterday's note")
	if err != nil {
		t.Fatalf("LogEpisode (outside): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET created_at = '2026-07-03 20:00:00' WHERE id = ?`, outsideID); err != nil {
		t.Fatalf("backdate outside: %v", err)
	}

	since := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 4, 23, 59, 59, 0, time.UTC)

	episodes, err := store.EpisodesInWindow(ctx, since, until, 10)
	if err != nil {
		t.Fatalf("EpisodesInWindow: %v", err)
	}
	if len(episodes) != 3 {
		t.Fatalf("expected 3 in-window episodes, got %d: %+v", len(episodes), episodes)
	}

	// chronological order: morning, noon, evening
	if episodes[0].ID != morningID || episodes[1].ID != noonID || episodes[2].ID != eveningID {
		t.Errorf("expected chronological order [morning,noon,evening], got ids [%d,%d,%d]",
			episodes[0].ID, episodes[1].ID, episodes[2].ID)
	}

	for _, e := range episodes {
		if e.ID == outsideID {
			t.Errorf("episode outside window must be excluded, got: %+v", e)
		}
	}

	// limit is honored
	limited, err := store.EpisodesInWindow(ctx, since, until, 2)
	if err != nil {
		t.Fatalf("EpisodesInWindow (limit=2): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("expected exactly 2 episodes with limit=2, got %d", len(limited))
	}
	if limited[0].ID != morningID || limited[1].ID != noonID {
		t.Errorf("expected limit to keep the earliest 2 in chronological order, got ids [%d,%d]", limited[0].ID, limited[1].ID)
	}
}

// ─── Consolidation retrieval (Cycle 2: MMR diversity) ─────────────────────────

// TestStore_DiverseEpisodes_AvoidsNearDuplicateCluster seeds 5 near-identical
// episodes (same app+title, near-identical screen_text, all matching the
// focus term) plus 2 clearly-distinct episodes that also match the focus. A
// plain top-N (RankedEpisodes) would return ~3 near-duplicates since they all
// score similarly high; DiverseEpisodes must instead spread across the
// distinct content via MMR, returning at most 1-2 from the duplicate cluster
// and at least one of the distinct episodes.
func TestStore_DiverseEpisodes_AvoidsNearDuplicateCluster(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	dupTexts := []string{
		"reviewing the DeepSeek post-training paper section on RLHF",
		"reviewing the DeepSeek post-training paper section on RLHF again",
		"reviewing the DeepSeek post-training paper section on RLHF once more",
		"still reviewing the DeepSeek post-training paper section on RLHF",
		"reviewing the DeepSeek post-training paper section on RLHF one more time",
	}
	dupIDs := make(map[int64]bool)
	for _, text := range dupTexts {
		id, err := store.LogEpisode(ctx, "Firefox", "DeepSeek Paper", text)
		if err != nil {
			t.Fatalf("LogEpisode (dup): %v", err)
		}
		dupIDs[id] = true
	}

	distinct1ID, err := store.LogEpisode(ctx, "Terminal", "training run", "kicking off a DeepSeek fine-tune job on the cluster")
	if err != nil {
		t.Fatalf("LogEpisode (distinct1): %v", err)
	}
	distinct2ID, err := store.LogEpisode(ctx, "Slack", "#research", "discussing DeepSeek benchmark results with the team")
	if err != nil {
		t.Fatalf("LogEpisode (distinct2): %v", err)
	}

	results, err := store.DiverseEpisodes(ctx, "DeepSeek", 3)
	if err != nil {
		t.Fatalf("DiverseEpisodes: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected some diverse episode results")
	}

	dupCount := 0
	foundDistinct := false
	for _, r := range results {
		if dupIDs[r.RefID] {
			dupCount++
		}
		if r.RefID == distinct1ID || r.RefID == distinct2ID {
			foundDistinct = true
		}
	}
	if dupCount > 2 {
		t.Errorf("expected at most 2 results from the near-duplicate cluster, got %d: %+v", dupCount, results)
	}
	if !foundDistinct {
		t.Errorf("expected at least one distinct episode in diverse results: %+v", results)
	}
}

// ─── Consolidation retrieval (Cycle 3: thread fusion) ─────────────────────────

// TestStore_RecallSubject_FusesThreadAndEpisodes seeds a live thread for
// subject "DeepSeek" plus several DeepSeek episodes, and verifies
// RecallSubject returns the thread's arc as a "[thread] ..." line followed by
// episode specifics as "[episode] ..." lines — the arc first, then the
// details, so a caller can narrate "you've been doing X, specifically Y, Z".
func TestStore_RecallSubject_FusesThreadAndEpisodes(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "DeepSeek",
		Kind:    "learning",
		State:   "studying post-training",
	}); err != nil {
		t.Fatalf("UpsertThread: %v", err)
	}

	episodeTexts := []string{
		"reading the DeepSeek post-training paper introduction",
		"skimming the DeepSeek RLHF section",
		"taking notes on the DeepSeek reward model design",
	}
	for _, text := range episodeTexts {
		if _, err := store.LogEpisode(ctx, "Firefox", "DeepSeek Paper", text); err != nil {
			t.Fatalf("LogEpisode: %v", err)
		}
	}

	lines, err := store.RecallSubject(ctx, "DeepSeek", 4)
	if err != nil {
		t.Fatalf("RecallSubject: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("expected some lines from RecallSubject")
	}

	var foundThread, foundEpisode bool
	threadIdx, episodeIdx := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "[thread] ") && strings.Contains(l, "DeepSeek") {
			foundThread = true
			if threadIdx == -1 {
				threadIdx = i
			}
		}
		if strings.HasPrefix(l, "[episode] ") {
			foundEpisode = true
			if episodeIdx == -1 {
				episodeIdx = i
			}
		}
	}
	if !foundThread {
		t.Errorf("expected a [thread] line in RecallSubject results: %+v", lines)
	}
	if !foundEpisode {
		t.Errorf("expected [episode] lines in RecallSubject results: %+v", lines)
	}
	if foundThread && foundEpisode && threadIdx > episodeIdx {
		t.Errorf("expected thread (arc) before episodes (specifics), got thread at %d, episode at %d: %+v", threadIdx, episodeIdx, lines)
	}
}
