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
