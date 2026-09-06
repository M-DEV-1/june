package db_test

import (
	"context"
	"ora/internal/db"
	"path/filepath"
	"strings"
	"testing"
)

// TestPersonalContext_UpsertNotAppend is the defining property of the store: writing the same subject twice edits the one row instead of adding a second, so the table cannot grow by restatement the way notes do.
func TestPersonalContext_UpsertNotAppend(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if err := store.SetPersonalContext(ctx, "identity", "The user is Alex Rivera."); err != nil {
		t.Fatalf("SetPersonalContext: %v", err)
	}
	if err := store.SetPersonalContext(ctx, "identity", "The user is Alex Rivera, git handle M-DEV-1."); err != nil {
		t.Fatalf("SetPersonalContext second write: %v", err)
	}

	entries, err := store.PersonalContext(ctx)
	if err != nil {
		t.Fatalf("PersonalContext: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry after two writes to the same subject, got %d: %+v", len(entries), entries)
	}
	if entries[0].Content != "The user is Alex Rivera, git handle M-DEV-1." {
		t.Errorf("want the newest content, got %q", entries[0].Content)
	}
	if entries[0].UpdatedAt.IsZero() {
		t.Error("updated_at was never set")
	}
}

// TestPersonalContext_OrderedBySubjectAndDeletable covers the two remaining store operations: a stable read order and removal by subject.
func TestPersonalContext_OrderedBySubjectAndDeletable(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	for _, e := range [][2]string{
		{"priya-shah", "Priya Shah is the user's colleague."},
		{"identity", "The user is Alex Rivera."},
		{"preferences-communication", "The user wants short answers."},
	} {
		if err := store.SetPersonalContext(ctx, e[0], e[1]); err != nil {
			t.Fatalf("SetPersonalContext %s: %v", e[0], err)
		}
	}

	entries, err := store.PersonalContext(ctx)
	if err != nil {
		t.Fatalf("PersonalContext: %v", err)
	}
	var subjects []string
	for _, e := range entries {
		subjects = append(subjects, e.Subject)
	}
	want := []string{"identity", "preferences-communication", "priya-shah"}
	if strings.Join(subjects, ",") != strings.Join(want, ",") {
		t.Errorf("want subjects ordered %v, got %v", want, subjects)
	}

	if err := store.DeletePersonalContext(ctx, "priya-shah"); err != nil {
		t.Fatalf("DeletePersonalContext: %v", err)
	}
	entries, err = store.PersonalContext(ctx)
	if err != nil {
		t.Fatalf("PersonalContext after delete: %v", err)
	}
	for _, e := range entries {
		if e.Subject == "priya-shah" {
			t.Error("deleted subject is still there")
		}
	}
	if len(entries) != 2 {
		t.Errorf("want 2 entries left, got %d", len(entries))
	}
}

// TestPersonalContext_ConsolidatorCannotSeeIt is the structural guard: the note compactor reads ExistingNotes and rewrites everything it gets back through ReplaceAllNotes, so personal context has to be invisible to the first and untouched by the second.
func TestPersonalContext_ConsolidatorCannotSeeIt(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if err := store.SetPersonalContext(ctx, "identity", "The user is Alex Rivera."); err != nil {
		t.Fatalf("SetPersonalContext: %v", err)
	}
	if _, err := store.LogNote(ctx, "the user was reading about sqlite", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	existing, err := store.ExistingNotes(ctx)
	if err != nil {
		t.Fatalf("ExistingNotes: %v", err)
	}
	for _, n := range existing {
		if strings.Contains(n.Content, "Alex Rivera") {
			t.Fatalf("the consolidator can see personal context: %q", n.Content)
		}
	}

	// What consolidation actually does: replace every fact with its own curated set.
	if err := store.ReplaceAllNotes(ctx, []string{"the user reads about databases"}); err != nil {
		t.Fatalf("ReplaceAllNotes: %v", err)
	}

	entries, err := store.PersonalContext(ctx)
	if err != nil {
		t.Fatalf("PersonalContext: %v", err)
	}
	if len(entries) != 1 || entries[0].Content != "The user is Alex Rivera." {
		t.Errorf("consolidation changed personal context: %+v", entries)
	}
}

// TestPersonalContext_RejectsEmptySubjectOrContent keeps the table from collecting blank rows, since nothing downstream can use one.
func TestPersonalContext_RejectsEmptySubjectOrContent(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if err := store.SetPersonalContext(ctx, "  ", "something"); err == nil {
		t.Error("an empty subject was accepted")
	}
	if err := store.SetPersonalContext(ctx, "identity", " \n "); err == nil {
		t.Error("empty content was accepted")
	}
}

// TestPersonalContext_NoteNamingTheUserSurvivesRestart checks that opening the store leaves the notes table alone. The one-time identity migration ran inside createSchema on every open and unconditionally deleted every kind='fact' note naming both the user and their computer, so any later note the compiler or save_note wrote was destroyed on the next daemon start.
func TestPersonalContext_NoteNamingTheUserSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ora.db")

	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	const kept = "Alex Rivera, owner of this computer, wants the daemon to start at login"
	if _, err := store.LogNote(ctx, kept, "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	store.Close()

	store, err = db.New(path)
	if err != nil {
		t.Fatalf("db.New reopen: %v", err)
	}
	defer store.Close()

	notes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	if len(notes) != 1 || notes[0].Content != kept {
		t.Errorf("reopening the store destroyed the note: %+v", notes)
	}
}
