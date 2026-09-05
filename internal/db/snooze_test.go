package db

import (
	"context"
	"testing"
	"time"
)

// TestSnoozeRoundTrip stores two snoozes, one already due and one still ahead, and checks that DueSnoozes returns only the due one and that MarkSnoozeFired stops it coming back.
func TestSnoozeRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Now()

	past, err := store.AddSnooze(ctx, "task", "42", "Still open", "Send the invoice", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("AddSnooze(past): %v", err)
	}
	if _, err := store.AddSnooze(ctx, "routine", "7", "Routine", "Priya replied", now.Add(time.Hour)); err != nil {
		t.Fatalf("AddSnooze(future): %v", err)
	}

	due, err := store.DueSnoozes(ctx, now)
	if err != nil {
		t.Fatalf("DueSnoozes: %v", err)
	}
	if len(due) != 1 || due[0].ID != past {
		t.Fatalf("DueSnoozes = %+v, want only the snooze with id %d", due, past)
	}
	got := due[0]
	if got.Kind != "task" || got.NoticeID != "42" || got.Title != "Still open" || got.Body != "Send the invoice" {
		t.Errorf("due snooze = %+v, want the notice it was made from", got)
	}
	if got.Due.Unix() != now.Add(-time.Minute).Unix() {
		t.Errorf("due at = %v, want %v", got.Due, now.Add(-time.Minute))
	}

	if err := store.MarkSnoozeFired(ctx, past); err != nil {
		t.Fatalf("MarkSnoozeFired: %v", err)
	}
	due, err = store.DueSnoozes(ctx, now)
	if err != nil {
		t.Fatalf("DueSnoozes after firing: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("DueSnoozes after firing = %+v, want none", due)
	}
}

// TestMarkSnoozeFiredUnknownID checks that firing an id that matches nothing is an error rather than a silent no-op, so a bug in the scheduler cannot quietly re-fire the same snooze every minute.
func TestMarkSnoozeFiredUnknownID(t *testing.T) {
	if err := newStore(t).MarkSnoozeFired(context.Background(), 999); err == nil {
		t.Error("MarkSnoozeFired(999) = nil, want an error")
	}
}
