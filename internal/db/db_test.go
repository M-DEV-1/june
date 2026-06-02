package db_test

// tests are first class citizens

import (
	"context"
	"ora/internal/db"
	"ora/internal/memory"
	"os"
	"strings"
	"testing"
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
