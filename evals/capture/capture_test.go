// capture stage: write a fact in, read it back out, check nothing changed.
// capture already works — retrieval and structuring are the real problems, so this is just a sanity check.
package capture_test

import (
	"context"
	"testing"

	"ora/evals/dataset"
	"ora/evals/harness"
	"ora/internal/memory"
)

// every fact in the shared dataset gets written through the storage API matching its Kind, then read back — must come back identical.
// uses evals/harness, same WAL-mode sqlite file shape as production.
func TestCapture_RoundTrip(t *testing.T) {
	facts, err := dataset.Load()
	if err != nil {
		t.Fatalf("dataset.Load() failed: %v", err)
	}
	if len(facts) == 0 {
		t.Fatal("dataset.Load() returned no facts — nothing to eval")
	}

	for _, fact := range facts {
		fact := fact
		t.Run(fact.ID, func(t *testing.T) {
			ctx := context.Background()
			store := harness.NewStore(t)

			switch fact.Kind {
			case "note":
				id, err := store.LogNote(ctx, fact.Content, fact.Kind)
				if err != nil {
					t.Fatalf("LogNote(%q) failed: %v", fact.ID, err)
				}

				notes, err := store.GetNotes(ctx)
				if err != nil {
					t.Fatalf("GetNotes failed: %v", err)
				}
				found := false
				for _, n := range notes {
					if n.ID == id {
						found = true
						if n.Content != fact.Content {
							t.Errorf("note content mismatch for %q:\n  want: %q\n  got:  %q", fact.ID, fact.Content, n.Content)
						}
					}
				}
				if !found {
					t.Fatalf("note %q (id=%d) not found in GetNotes after capture", fact.ID, id)
				}

			case "thread":
				id, err := store.UpsertThread(ctx, memory.ThreadUpdate{
					Subject: fact.Content,
					Kind:    fact.Kind,
					State:   "captured",
				})
				if err != nil {
					t.Fatalf("UpsertThread(%q) failed: %v", fact.ID, err)
				}

				threads, err := store.GetLiveThreads(ctx, 100)
				if err != nil {
					t.Fatalf("GetLiveThreads failed: %v", err)
				}
				found := false
				for _, th := range threads {
					if th.ID == id {
						found = true
						if th.Subject != fact.Content {
							t.Errorf("thread subject mismatch for %q:\n  want: %q\n  got:  %q", fact.ID, fact.Content, th.Subject)
						}
					}
				}
				if !found {
					t.Fatalf("thread %q (id=%d) not found in GetLiveThreads after capture", fact.ID, id)
				}

			default:
				t.Fatalf("fact %q has unrecognized kind %q (expected \"note\" or \"thread\")", fact.ID, fact.Kind)
			}
		})
	}
}

// notes table has a UNIQUE(content, kind) index, so capturing the identical fact twice must not create two rows.
// this is the anti-context-rot mechanism at the capture layer — an always-on capture loop will keep re-observing the same fact.
func TestCapture_NoteDedup(t *testing.T) {
	ctx := context.Background()
	store := harness.NewStore(t)

	facts, err := dataset.Load()
	if err != nil {
		t.Fatalf("dataset.Load() failed: %v", err)
	}
	var note dataset.Fact
	for _, f := range facts {
		if f.Kind == "note" {
			note = f
			break
		}
	}
	if note.ID == "" {
		t.Fatal("dataset has no note-kind fact to test dedup with")
	}

	firstID, err := store.LogNote(ctx, note.Content, note.Kind)
	if err != nil {
		t.Fatalf("first LogNote failed: %v", err)
	}
	secondID, err := store.LogNote(ctx, note.Content, note.Kind)
	if err != nil {
		t.Fatalf("second (duplicate) LogNote failed: %v", err)
	}
	if firstID != secondID {
		t.Errorf("re-capturing the same (content, kind) returned a different id: first=%d second=%d, want identical", firstID, secondID)
	}

	notes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes failed: %v", err)
	}
	count := 0
	for _, n := range notes {
		if n.Content == note.Content && n.Kind == note.Kind {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 row for duplicated (content, kind), got %d", count)
	}
}
