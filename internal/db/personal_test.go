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

	if err := store.SetPersonalContext(ctx, "identity", "The user is Mahadevan KS."); err != nil {
		t.Fatalf("SetPersonalContext: %v", err)
	}
	if err := store.SetPersonalContext(ctx, "identity", "The user is Mahadevan KS, git handle M-DEV-1."); err != nil {
		t.Fatalf("SetPersonalContext second write: %v", err)
	}

	entries, err := store.PersonalContext(ctx)
	if err != nil {
		t.Fatalf("PersonalContext: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry after two writes to the same subject, got %d: %+v", len(entries), entries)
	}
	if entries[0].Content != "The user is Mahadevan KS, git handle M-DEV-1." {
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
		{"trupti-hosmani", "Trupti Hosmani is the user's colleague."},
		{"identity", "The user is Mahadevan KS."},
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
	want := []string{"identity", "preferences-communication", "trupti-hosmani"}
	if strings.Join(subjects, ",") != strings.Join(want, ",") {
		t.Errorf("want subjects ordered %v, got %v", want, subjects)
	}

	if err := store.DeletePersonalContext(ctx, "trupti-hosmani"); err != nil {
		t.Fatalf("DeletePersonalContext: %v", err)
	}
	entries, err = store.PersonalContext(ctx)
	if err != nil {
		t.Fatalf("PersonalContext after delete: %v", err)
	}
	for _, e := range entries {
		if e.Subject == "trupti-hosmani" {
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

	if err := store.SetPersonalContext(ctx, "identity", "The user is Mahadevan KS."); err != nil {
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
		if strings.Contains(n.Content, "Mahadevan KS") {
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
	if len(entries) != 1 || entries[0].Content != "The user is Mahadevan KS." {
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

// TestPersonalContext_MigratesIdentityNote covers the one-time move of the identity fact out of notes and into personal context, which happens when the schema is created on an existing database.
func TestPersonalContext_MigratesIdentityNote(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ora.db")

	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	if _, err := store.LogNote(ctx, "Mahadevan KS is the owner of this computer and goes by Mahadevan.", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	if _, err := store.LogNote(ctx, "the user was reading about sqlite", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	store.Close()

	store, err = db.New(path)
	if err != nil {
		t.Fatalf("db.New reopen: %v", err)
	}
	defer store.Close()

	entries, err := store.PersonalContext(ctx)
	if err != nil {
		t.Fatalf("PersonalContext: %v", err)
	}
	if len(entries) != 1 || entries[0].Subject != "identity" {
		t.Fatalf("want one entry under \"identity\", got %+v", entries)
	}
	if !strings.Contains(entries[0].Content, "Mahadevan KS") || !strings.Contains(entries[0].Content, "M-DEV-1") {
		t.Errorf("identity entry lost its content: %q", entries[0].Content)
	}

	notes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	for _, n := range notes {
		if strings.Contains(n.Content, "owner of this computer") {
			t.Errorf("the identity note is still in notes: %q", n.Content)
		}
	}
	if len(notes) != 1 {
		t.Errorf("the migration touched unrelated notes: %+v", notes)
	}

	// Idempotent: opening again neither re-adds the note nor overwrites an entry the user has since edited.
	if err := store.SetPersonalContext(ctx, "identity", "Edited by the user."); err != nil {
		t.Fatalf("SetPersonalContext: %v", err)
	}
	store.Close()
	store, err = db.New(path)
	if err != nil {
		t.Fatalf("db.New third open: %v", err)
	}
	entries, err = store.PersonalContext(ctx)
	if err != nil {
		t.Fatalf("PersonalContext: %v", err)
	}
	if len(entries) != 1 || entries[0].Content != "Edited by the user." {
		t.Errorf("the migration ran twice or overwrote a user edit: %+v", entries)
	}
}
