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
