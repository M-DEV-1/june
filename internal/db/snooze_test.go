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
