package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeConsolidator is a NoteConsolidator double that never calls a real model. Input: nothing until called. Output: the canned out/err this test set, and it remembers whether it was called and what it was asked to consolidate.
type fakeConsolidator struct {
	out    []string
	err    error
	gotIn  []string
	called bool
}

func (f *fakeConsolidator) ConsolidateNotes(ctx context.Context, notes []string) ([]string, error) {
	f.called = true
	f.gotIn = notes
	return f.out, f.err
}

// seedFacts writes n fact notes to store and fails the test if any write errors. Input: the store and how many facts to write. Output: none.
func seedFacts(t *testing.T, store *Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := store.LogNote(context.Background(), fmt.Sprintf("the user knows fact number %d", i), "fact"); err != nil {
			t.Fatalf("LogNote: %v", err)
		}
	}
}

func TestNoteCompactor_BelowThresholdSkips(t *testing.T) {
	store := newFileStore(t)
	seedFacts(t, store, minNotesToConsolidate-1)
	llm := &fakeConsolidator{}
	nc := NewNoteCompactor(llm, store)

	if err := nc.Compact(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if llm.called {
		t.Error("consolidator should not be called below threshold")
	}
}

func TestNoteCompactor_ReducesAndReplaces(t *testing.T) {
	store := newFileStore(t)
	seedFacts(t, store, minNotesToConsolidate)
	llm := &fakeConsolidator{out: []string{"a", "b", "c"}}
	nc := NewNoteCompactor(llm, store)

	if err := nc.Compact(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	notes, err := store.GetNotes(context.Background())
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	if len(notes) != 3 {
		t.Errorf("expected 3 merged notes, got %d", len(notes))
	}
}

func TestNoteCompactor_NeverWipesOnEmptyResult(t *testing.T) {
	store := newFileStore(t)
	seedFacts(t, store, minNotesToConsolidate)
	llm := &fakeConsolidator{out: []string{}}
	nc := NewNoteCompactor(llm, store)

	if err := nc.Compact(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	notes, err := store.GetNotes(context.Background())
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	if len(notes) != minNotesToConsolidate {
		t.Errorf("must never wipe notes when model returns an empty set, got %d notes", len(notes))
	}
}

func TestNoteCompactor_SkipsWhenNoReduction(t *testing.T) {
	store := newFileStore(t)
	seedFacts(t, store, minNotesToConsolidate)
	out := make([]string, minNotesToConsolidate)
	for i := range out {
		out[i] = "x"
	}
	llm := &fakeConsolidator{out: out}
	nc := NewNoteCompactor(llm, store)

	if err := nc.Compact(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	notes, err := store.GetNotes(context.Background())
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	if len(notes) != minNotesToConsolidate {
		t.Errorf("should not rewrite when consolidation did not reduce the set, got %d notes", len(notes))
	}
	for _, n := range notes {
		if n.Content == "x" {
			t.Fatal("notes were rewritten to the model's non-reducing output")
		}
	}
}

func TestNoteCompactor_LLMErrorDoesNotReplace(t *testing.T) {
	store := newFileStore(t)
	seedFacts(t, store, minNotesToConsolidate)
	llm := &fakeConsolidator{err: errors.New("boom")}
	nc := NewNoteCompactor(llm, store)

	if err := nc.Compact(context.Background()); err == nil {
		t.Fatal("expected error to propagate")
	}
	notes, err := store.GetNotes(context.Background())
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	if len(notes) != minNotesToConsolidate {
		t.Errorf("must not rewrite notes when the model call failed, got %d notes", len(notes))
	}
}
