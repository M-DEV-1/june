package db_test

import (
	"context"
	"testing"
)

// TestStore_SaveFold_AppearsInUnconsumedFolds is the tracer bullet for fold persistence: a saved fold must be retrievable via UnconsumedFolds, since that's the only path a branch result has back to the user once its live session has died.
func TestStore_SaveFold_AppearsInUnconsumedFolds(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	id, err := store.SaveFold(ctx, "catch me up on Riddler", "Riddler kicked off last week, blocked on X")
	if err != nil {
		t.Fatalf("SaveFold: %v", err)
	}
	if id == 0 {
		t.Fatal("SaveFold returned id 0")
	}

	folds, err := store.UnconsumedFolds(ctx)
	if err != nil {
		t.Fatalf("UnconsumedFolds: %v", err)
	}
	if len(folds) != 1 {
		t.Fatalf("UnconsumedFolds returned %d folds, want 1", len(folds))
	}
	if folds[0].Task != "catch me up on Riddler" || folds[0].Result != "Riddler kicked off last week, blocked on X" {
		t.Errorf("unexpected fold contents: %+v", folds[0])
	}
}

// TestStore_ConsumeFold_RemovesItFromUnconsumedFolds verifies a consumed fold doesn't repeat at the next session's handshake — it's surfaced once.
func TestStore_ConsumeFold_RemovesItFromUnconsumedFolds(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	id, err := store.SaveFold(ctx, "task", "result")
	if err != nil {
		t.Fatalf("SaveFold: %v", err)
	}

	if err := store.ConsumeFold(ctx, id); err != nil {
		t.Fatalf("ConsumeFold: %v", err)
	}

	folds, err := store.UnconsumedFolds(ctx)
	if err != nil {
		t.Fatalf("UnconsumedFolds: %v", err)
	}
	if len(folds) != 0 {
		t.Errorf("UnconsumedFolds after ConsumeFold = %+v, want empty", folds)
	}
}
