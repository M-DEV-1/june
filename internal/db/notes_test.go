package db

import (
	"testing"
	"time"
)

// TestNotesSince checks the window GET /today reads its notes through: a note created at or after the bound is returned, one older is not, and one created before the bound but updated after it is returned too — an action item is closed long after it was written, and the day it was closed is the day it belongs on.
func TestNotesSince(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()
	ctx := t.Context()

	fresh, err := store.LogNote(ctx, "the venue is booked", "fact")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	old, err := store.LogNote(ctx, "the old venue was cancelled", "fact")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	closedToday, err := store.LogNote(ctx, "send the deposit", "action")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE notes SET created_at = datetime('now','-10 days'), updated_at = datetime('now','-10 days') WHERE id = ?`, old); err != nil {
		t.Fatalf("age the old note: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE notes SET created_at = datetime('now','-10 days') WHERE id = ?`, closedToday); err != nil {
		t.Fatalf("age the closed note: %v", err)
	}

	notes, err := store.NotesSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("NotesSince: %v", err)
	}
	got := map[int64]bool{}
	for _, n := range notes {
		got[n.ID] = true
	}
	if !got[fresh] {
		t.Errorf("a note created inside the window is missing")
	}
	if !got[closedToday] {
		t.Errorf("a note created before the window but updated inside it is missing")
	}
	if got[old] {
		t.Errorf("a note untouched since before the window came back")
	}
}
