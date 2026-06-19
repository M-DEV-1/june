package memory

import (
	"context"
	"errors"
	"testing"
)

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

type fakeNoteStore struct {
	notes      []NoteRef
	replaced   []string
	replaceErr error
	didReplace bool
}

func (f *fakeNoteStore) ExistingNotes(ctx context.Context) ([]NoteRef, error) {
	return f.notes, nil
}

func (f *fakeNoteStore) ReplaceAllNotes(ctx context.Context, contents []string) error {
	f.didReplace = true
	f.replaced = contents
	return f.replaceErr
}

func notes(n int) []NoteRef {
	out := make([]NoteRef, n)
	for i := range out {
		out[i] = NoteRef{ID: int64(i + 1), Content: "note"}
	}
	return out
}

func TestNoteCompactor_BelowThresholdSkips(t *testing.T) {
	llm := &fakeConsolidator{}
	store := &fakeNoteStore{notes: notes(minNotesToConsolidate - 1)}
	nc := NewNoteCompactor(llm, store)

	if err := nc.Compact(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if llm.called {
		t.Error("consolidator should not be called below threshold")
	}
	if store.didReplace {
		t.Error("store should not be rewritten below threshold")
	}
}

func TestNoteCompactor_ReducesAndReplaces(t *testing.T) {
	llm := &fakeConsolidator{out: []string{"a", "b", "c"}}
	store := &fakeNoteStore{notes: notes(minNotesToConsolidate)}
	nc := NewNoteCompactor(llm, store)

	if err := nc.Compact(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !store.didReplace {
		t.Fatal("expected store rewrite when consolidation reduces count")
	}
	if len(store.replaced) != 3 {
		t.Errorf("expected 3 merged notes, got %d", len(store.replaced))
	}
}

func TestNoteCompactor_NeverWipesOnEmptyResult(t *testing.T) {
	llm := &fakeConsolidator{out: []string{}}
	store := &fakeNoteStore{notes: notes(minNotesToConsolidate)}
	nc := NewNoteCompactor(llm, store)

	if err := nc.Compact(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.didReplace {
		t.Error("must never wipe notes when model returns an empty set")
	}
}

func TestNoteCompactor_SkipsWhenNoReduction(t *testing.T) {
	// model returned the same count (or more) -> not worth churning IDs.
	llm := &fakeConsolidator{out: make([]string, minNotesToConsolidate)}
	for i := range llm.out {
		llm.out[i] = "x"
	}
	store := &fakeNoteStore{notes: notes(minNotesToConsolidate)}
	nc := NewNoteCompactor(llm, store)

	if err := nc.Compact(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.didReplace {
		t.Error("should not rewrite when consolidation did not reduce the set")
	}
}

func TestNoteCompactor_LLMErrorDoesNotReplace(t *testing.T) {
	llm := &fakeConsolidator{err: errors.New("boom")}
	store := &fakeNoteStore{notes: notes(minNotesToConsolidate)}
	nc := NewNoteCompactor(llm, store)

	if err := nc.Compact(context.Background()); err == nil {
		t.Fatal("expected error to propagate")
	}
	if store.didReplace {
		t.Error("must not rewrite notes when the model call failed")
	}
}
