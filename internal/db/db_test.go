package db_test

// tests are first class citizens

import (
	"context"
	"ora/internal/db"
	"strings"
	"testing"
)

// t param is test controller. object to provide methods to control the flow of the test + reporting
func TestStore_ActivityLifeCycle(t *testing.T) {
	ctx := context.Background()

	// 1. be able to create new store in memory for testing
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store : %v", err)
	}
	defer store.Close()

	// 2. be able to log an activity with normalization
	err = store.LogActivity(ctx, "VSCode", "main.go - ora")
	if err != nil {
		t.Errorf("Failed to log activity: %v", err)
	}

	// 3. we want to get context back
	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Errorf("Failed to get context: %v", err)
	}

	// 4. we verify the tree structure
	if len(branch) < 3 {
		t.Fatalf("Expected at least 3 nodes (User, Day, Activity), got %d: %+v", len(branch), branch)
	}

	// check nodes ordering, expected ROOT to LEAF
	if !strings.Contains(branch[0], "default_user") {
		t.Errorf("Expected root node to contain user, got: %s", branch[0])
	}
	if !strings.Contains(branch[len(branch)-1], "VSCode") {
		t.Errorf("Expected leaf node to contain activity, got %s", branch[len(branch)-1])
	}

	t.Logf("Successfully retrieved branch: %+v", branch)
}
