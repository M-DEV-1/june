package db_test

import (
	"context"
	"ora/internal/db"
	"testing"
)

// t param is test controller. object to provide methods to control the flow of the test + reporting
func TestStore_ActivityLifeCycle(t *testing.T) {
	ctx := context.Background()

	// 1. be able to create new store in memory for testing
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store :%v", err)
	}
	defer store.Close()

	// 2. be able to log an activity with normalization
	err = store.LogActivity(ctx, "Antigravity", "main.go - ora")
	if err != nil {
		t.Errorf("Failed to log activity: %v", err)
	}

	// 3. we want to get context back
	context, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Errorf("Failed to get context: %v", err)
	}
	if len(context) == 0 {
		t.Error("Expected context nodes, got none")
	}
}
