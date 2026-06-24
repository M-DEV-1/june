package db_test

// tests are first class citizens

import (
	"context"
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

	id, _ := store.LogNote(ctx, "user works at Acme ESG", "fact")

	// re-logging same content + kind is a no-op (idempotent)
	id2, err := store.LogNote(ctx, "user works at Acme ESG", "fact")
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

func TestStore_GetImplicitContext_IncludesNotes(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	_, _ = store.LogNote(ctx, "user is a Go developer", "fact")
	_, _ = store.LogNote(ctx, "user prefers terse responses", "preference")

	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext: %v", err)
	}

	var foundFact, foundPref bool
	for _, b := range branch {
		if strings.Contains(b, "user is a Go developer") {
			foundFact = true
		}
		if strings.Contains(b, "user prefers terse responses") {
			foundPref = true
		}
	}
	if !foundFact || !foundPref {
		t.Errorf("notes missing from implicit context: %+v", branch)
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
	_, _ = store.LogNote(ctx, "user works at Acme ESG as an intern", "fact")

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
	noteHits, err := store.SearchMemory(ctx, "Acme")
	if err != nil {
		t.Fatalf("SearchMemory notes: %v", err)
	}
	if len(noteHits) == 0 {
		t.Fatal("FTS5 returned no hits for 'Acme'")
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

func TestStore_QueryMemory(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// semantic summaries
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Debugging UI",
		Summary:  "Fixing lipgloss layout issues",
	})
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Research",
		Summary:  "Reading StackOverflow about websockets",
	})

	// Search for 'StackOverflow'
	results, err := store.QueryMemory(ctx, "StackOverflow")
	if err != nil {
		t.Fatalf("QueryMemory failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}
	if !strings.Contains(results[0], "StackOverflow") {
		t.Errorf("Expected result to contain 'StackOverflow', got: %s", results[0])
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

	// seed a summary so fallback has something to return
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Baseline Task",
		Summary:  "Writing baseline summary for context test",
	})
	_, _ = store.LogNote(ctx, "user is a Go developer", "fact")

	// without working state → fallback returns raw summary nodes; note must still appear
	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext (no state): %v", err)
	}

	var hasNote bool
	for _, line := range branch {
		if strings.Contains(line, "user is a Go developer") {
			hasNote = true
		}
		if strings.Contains(line, "[state]") {
			t.Errorf("did not expect [state] line before working state is set: %s", line)
		}
	}
	if !hasNote {
		t.Errorf("note missing from context without working state: %+v", branch)
	}

	// now set working state
	const state = "user is actively debugging the Linux audio pipeline and writing TDD tests"
	if err := store.SetWorkingState(ctx, state); err != nil {
		t.Fatalf("SetWorkingState: %v", err)
	}

	branch2, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext (with state): %v", err)
	}

	var foundNote, foundState bool
	var hasRawSummary bool
	for _, line := range branch2 {
		if strings.Contains(line, "user is a Go developer") {
			foundNote = true
		}
		if strings.Contains(line, "[state]") && strings.Contains(line, state) {
			foundState = true
		}
		if strings.Contains(line, "Writing baseline summary") {
			hasRawSummary = true
		}
	}

	if !foundNote {
		t.Errorf("note missing from context with working state: %+v", branch2)
	}
	if !foundState {
		t.Errorf("[state] line missing from context: %+v", branch2)
	}
	if hasRawSummary {
		t.Errorf("raw summary content must NOT appear when working state is set: %+v", branch2)
	}

	// note must come before [state] line
	var noteIdx, stateIdx int = -1, -1
	for i, line := range branch2 {
		if strings.Contains(line, "user is a Go developer") {
			noteIdx = i
		}
		if strings.Contains(line, "[state]") {
			stateIdx = i
		}
	}
	if noteIdx == -1 || stateIdx == -1 {
		t.Fatalf("could not find note (%d) or state (%d) in branch: %+v", noteIdx, stateIdx, branch2)
	}
	if noteIdx >= stateIdx {
		t.Errorf("expected note (idx %d) to appear before [state] (idx %d)", noteIdx, stateIdx)
	}
}

func TestStore_GetImplicitContext_FallbackWhenNoState(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed summaries and a note
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Fallback Task",
		Summary:  "Checking fallback behavior works correctly",
	})
	_, _ = store.LogNote(ctx, "user prefers terse responses", "preference")

	// no working state set → fallback path: should include summaries + note
	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext fallback: %v", err)
	}

	var hasSummary, hasNote bool
	for _, line := range branch {
		if strings.Contains(line, "Checking fallback behavior") {
			hasSummary = true
		}
		if strings.Contains(line, "user prefers terse responses") {
			hasNote = true
		}
	}
	if !hasSummary {
		t.Errorf("expected summary in fallback context: %+v", branch)
	}
	if !hasNote {
		t.Errorf("expected note in fallback context: %+v", branch)
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
