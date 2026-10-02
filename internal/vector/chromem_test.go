package vector

// Tests for ChromemIndex, exercising the real chromem-go-backed implementation in chromem.go.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestChromemIndex_Add_EvictsOldestOverMaxDocs verifies the oldest-added item gets evicted first once maxDocs is exceeded (insertion order, not relevance), keeping Count() at or under the cap.
func TestChromemIndex_Add_EvictsOldestOverMaxDocs(t *testing.T) {

	ctx := context.Background()
	dir := t.TempDir()
	const maxDocs = 3
	idx, err := NewChromemIndex(dir, 3, maxDocs)
	if err != nil {
		t.Fatalf("NewChromemIndex: %v", err)
	}

	ids := []string{"oldest", "second", "third", "newest"}
	for i, id := range ids {
		vec := []float32{float32(i), float32(i), float32(i)}
		if err := idx.Add(ctx, id, "content "+id, vec, nil); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}

	if got := idx.Count(); got > maxDocs {
		t.Errorf("Count() = %d, want <= %d", got, maxDocs)
	}

	results, err := idx.Search(ctx, []float32{0, 0, 0}, 10, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.ID] = true
	}
	if seen["oldest"] {
		t.Errorf("expected 'oldest' to be evicted, but it was found in results: %v", results)
	}
	if !seen["newest"] {
		t.Errorf("expected 'newest' to still be present, got: %v", results)
	}
}

// The sidecar only tracks insertion time for eviction, and a crash mid-write leaves it truncated. A corrupt sidecar must not fail NewChromemIndex, because the daemon then runs with no semantic search and nothing repairs the file. IDs() must come back from the collection itself, because the reconciliation sweep walks it to delete orphans and evict past the cap, and it must drop a document as soon as it is deleted.
func TestNewChromemIndex_CorruptSidecar_RebuildsIDsFromTheCollection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	idx, err := NewChromemIndex(dir, 3, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex: %v", err)
	}
	for i, id := range []string{"note:1", "note:2", "episode:7"} {
		if err := idx.Add(ctx, id, "content", []float32{float32(i + 1), 1, 0}, nil); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, collectionName+"_meta.json"), []byte(`{"note:1": "2026-08-1`), 0644); err != nil {
		t.Fatalf("corrupt the sidecar: %v", err)
	}

	reopened, err := NewChromemIndex(dir, 3, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex should degrade past a corrupt sidecar, got error: %v", err)
	}
	if ids := reopened.IDs(); len(ids) != 3 {
		t.Fatalf("IDs() = %v, want all 3 documents in the collection", ids)
	}
	if err := reopened.Delete(ctx, "note:1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, id := range reopened.IDs() {
		if id == "note:1" {
			t.Errorf("IDs() still lists note:1 after Delete: %v", reopened.IDs())
		}
	}
}

// TestChromemIndex_PersistenceAcrossRestart_RespectsMaxDocsCap verifies a second ChromemIndex opened on the same dbPath finds docs added by a first, discarded one (simulating a restart), and that the maxDocs sidecar survives too, not just chromem-go's own on-disk data.
func TestChromemIndex_PersistenceAcrossRestart_RespectsMaxDocsCap(t *testing.T) {

	ctx := context.Background()
	dir := t.TempDir()
	const maxDocs = 2

	first, err := NewChromemIndex(dir, 3, maxDocs)
	if err != nil {
		t.Fatalf("NewChromemIndex (first): %v", err)
	}
	if err := first.Add(ctx, "before-restart", "content before restart", []float32{1, 0, 0}, nil); err != nil {
		t.Fatalf("Add (first): %v", err)
	}

	// simulate a restart: discard the first handle, open a fresh one on the same path.
	second, err := NewChromemIndex(dir, 3, maxDocs)
	if err != nil {
		t.Fatalf("NewChromemIndex (second): %v", err)
	}

	results, err := second.Search(ctx, []float32{1, 0, 0}, 10, nil)
	if err != nil {
		t.Fatalf("Search (second): %v", err)
	}
	found := false
	for _, r := range results {
		if r.ID == "before-restart" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'before-restart' doc to survive a restart, got: %+v", results)
	}

	// push past maxDocs on the reopened index and confirm eviction is still enforced — the sidecar must have reloaded from disk, not reset empty.
	if err := second.Add(ctx, "after-restart-1", "content 1", []float32{2, 0, 0}, nil); err != nil {
		t.Fatalf("Add (after-restart-1): %v", err)
	}
	if err := second.Add(ctx, "after-restart-2", "content 2", []float32{3, 0, 0}, nil); err != nil {
		t.Fatalf("Add (after-restart-2): %v", err)
	}
	if got := second.Count(); got > maxDocs {
		t.Errorf("Count() after restart + more adds = %d, want <= %d", got, maxDocs)
	}
}

// TestNewChromemIndex_CollectionOfAnotherWidth_IsRebuiltNotLeftUnusable covers an embedding-model swap: chromem refuses every query that mixes vector widths, so a collection written by the old model would make semantic search fail silently and forever. Opening at the new width must leave a collection that answers queries, even at the cost of the old vectors.
func TestNewChromemIndex_CollectionOfAnotherWidth_IsRebuiltNotLeftUnusable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	old, err := NewChromemIndex(dir, 3, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex (old width): %v", err)
	}
	if err := old.Add(ctx, "note:1", "written by the old model", []float32{1, 0, 0}, nil); err != nil {
		t.Fatalf("Add (old width): %v", err)
	}

	swapped, err := NewChromemIndex(dir, 4, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex (new width): %v", err)
	}
	if err := swapped.Add(ctx, "note:2", "written by the new model", []float32{1, 0, 0, 0}, nil); err != nil {
		t.Fatalf("Add (new width): %v", err)
	}
	results, err := swapped.Search(ctx, []float32{1, 0, 0, 0}, 5, nil)
	if err != nil {
		t.Fatalf("Search after the width swap: %v", err)
	}
	if len(results) != 1 || results[0].ID != "note:2" {
		t.Fatalf("expected only the new-width document to be searchable, got %+v", results)
	}
}

// A batch add must land every document in the collection, with each one searchable and counted against maxDocs. A wrong-width vector is rejected by AddBatch and by Add, because one stored mismatch would make the width probe drop and rebuild the whole collection at next open.
func TestChromemIndex_AddBatch_AddsEveryDocumentAndRejectsWrongWidth(t *testing.T) {
	ctx := context.Background()
	idx, err := NewChromemIndex(t.TempDir(), 3, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex: %v", err)
	}

	ids := []string{"note:1", "note:2", "note:3"}
	contents := []string{"tea", "coffee", "cocoa"}
	embeddings := [][]float32{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	metadatas := []map[string]string{{"source": "note"}, {"source": "note"}, {"source": "note"}}
	if err := idx.AddBatch(ctx, ids, contents, embeddings, metadatas); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	if got := idx.Count(); got != 3 {
		t.Errorf("Count() = %d, want 3", got)
	}
	got := map[string]bool{}
	for _, id := range idx.IDs() {
		got[id] = true
	}
	for _, id := range ids {
		if !got[id] {
			t.Errorf("%q missing from the index after AddBatch, have %v", id, idx.IDs())
		}
	}
	results, err := idx.Search(ctx, []float32{0, 1, 0}, 1, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].ID != "note:2" || results[0].Content != "coffee" {
		t.Errorf("nearest to the second vector = %+v, want note:2 / coffee", results)
	}

	if err := idx.AddBatch(ctx, []string{"note:4"}, []string{"milk"}, [][]float32{{1, 1}}, []map[string]string{nil}); err == nil {
		t.Fatal("a 2-wide vector must be rejected by a 3-wide index")
	}
	if err := idx.Add(ctx, "note:5", "milk", []float32{1, 1}, nil); err == nil {
		t.Fatal("a 2-wide vector must be rejected by Add too")
	}
}
