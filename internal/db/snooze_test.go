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
	if _, err := store.AddSnooze(ctx, "routine", "7", "Routine", "Vexil replied", now.Add(time.Hour)); err != nil {
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

// TestAddSnooze_ReplacesPendingForSameNotice checks that snoozing a notice a second time pushes the reminder rather than leaving the first snooze to fire on its own alongside the second.
func TestAddSnooze_ReplacesPendingForSameNotice(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Now()

	first, err := store.AddSnooze(ctx, "task", "42", "Still open", "Send the invoice", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("AddSnooze(first): %v", err)
	}
	second, err := store.AddSnooze(ctx, "task", "42", "Still open", "Send the invoice", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("AddSnooze(second): %v", err)
	}

	due, err := store.DueSnoozes(ctx, now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("DueSnoozes: %v", err)
	}
	if len(due) != 1 || due[0].ID != second {
		t.Fatalf("DueSnoozes = %+v, want only the second snooze (id %d), first (id %d) should have been replaced", due, second, first)
	}
}

// TestAddSnooze_LeavesOtherNoticesAlone checks that replacing a pending snooze only touches the notice it was made for, never another notice's own snooze.
func TestAddSnooze_LeavesOtherNoticesAlone(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Now()

	other, err := store.AddSnooze(ctx, "task", "43", "Other task", "Call the client", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("AddSnooze(other): %v", err)
	}
	if _, err := store.AddSnooze(ctx, "task", "42", "Still open", "Send the invoice", now.Add(time.Hour)); err != nil {
		t.Fatalf("AddSnooze(42): %v", err)
	}
	if _, err := store.AddSnooze(ctx, "task", "42", "Still open", "Send the invoice", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("AddSnooze(42 again): %v", err)
	}

	due, err := store.DueSnoozes(ctx, now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("DueSnoozes: %v", err)
	}
	var otherStillThere bool
	for _, sn := range due {
		if sn.ID == other {
			otherStillThere = true
		}
	}
	if !otherStillThere {
		t.Errorf("DueSnoozes = %+v, want notice 43's own snooze (id %d) untouched", due, other)
	}
}

// TestAddSnooze_FailedReplaceLeavesThePendingSnooze checks that cancelling the old snooze and inserting the new one happen in one transaction: when the insert side of an AddSnooze call fails, the earlier pending snooze is left exactly as it was, not half-cancelled.
func TestAddSnooze_FailedReplaceLeavesThePendingSnooze(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Now()

	first, err := store.AddSnooze(ctx, "task", "42", "Still open", "Send the invoice", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("AddSnooze(first): %v", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.AddSnooze(canceled, "task", "42", "Still open", "Send the invoice", now.Add(2*time.Hour)); err == nil {
		t.Fatal("AddSnooze with an already-canceled context = nil error, want one")
	}

	due, err := store.DueSnoozes(ctx, now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("DueSnoozes: %v", err)
	}
	if len(due) != 1 || due[0].ID != first || due[0].Due.Unix() != now.Add(time.Hour).Unix() {
		t.Fatalf("DueSnoozes after a failed replace = %+v, want the original snooze (id %d) untouched", due, first)
	}
}

// TestCancelSnoozes_MarksPendingFired checks that CancelSnoozes stamps every unfired row for one notice as fired and reports how many, while leaving an already-fired row and another notice's row alone. Two unfired rows for the same notice are inserted directly, past AddSnooze's own dedup, so the count covers the defensive case of more than one pending row.
func TestCancelSnoozes_MarksPendingFired(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Now()

	insert := func(kind, noticeID string, due time.Time) int64 {
		t.Helper()
		res, err := store.db.ExecContext(ctx, `INSERT INTO snoozes (notice_kind, notice_id, title, body, due_at) VALUES (?, ?, 'Still open', 'Send the invoice', ?)`, kind, noticeID, sqliteUTC(due))
		if err != nil {
			t.Fatalf("insert snooze: %v", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("insert snooze id: %v", err)
		}
		return id
	}

	insert("task", "42", now.Add(time.Hour))
	insert("task", "42", now.Add(2*time.Hour))
	alreadyFired := insert("task", "42", now.Add(-time.Hour))
	if err := store.MarkSnoozeFired(ctx, alreadyFired); err != nil {
		t.Fatalf("MarkSnoozeFired: %v", err)
	}
	other := insert("task", "43", now.Add(time.Hour))

	n, err := store.CancelSnoozes(ctx, "task", "42")
	if err != nil {
		t.Fatalf("CancelSnoozes: %v", err)
	}
	if n != 2 {
		t.Errorf("CancelSnoozes cancelled %d rows, want 2 (the already-fired row does not count again)", n)
	}

	due, err := store.DueSnoozes(ctx, now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("DueSnoozes: %v", err)
	}
	if len(due) != 1 || due[0].ID != other {
		t.Fatalf("DueSnoozes after CancelSnoozes = %+v, want only notice 43's own snooze (id %d)", due, other)
	}
}

// TestCancelSnoozes_NoPendingReturnsZero checks that cancelling a notice with nothing pending is a no-op that reports zero rather than an error.
func TestCancelSnoozes_NoPendingReturnsZero(t *testing.T) {
	n, err := newStore(t).CancelSnoozes(context.Background(), "task", "no-such-notice")
	if err != nil {
		t.Fatalf("CancelSnoozes: %v", err)
	}
	if n != 0 {
		t.Errorf("CancelSnoozes = %d, want 0", n)
	}
}
