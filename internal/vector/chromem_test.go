package vector

// Tests for ChromemIndex, exercising the real chromem-go-backed implementation in chromem.go.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestChromemIndex_AddThenSearch_SelfSimilarityNearOne verifies adding a vector then searching with it returns cosine similarity near 1.0.
func TestChromemIndex_AddThenSearch_SelfSimilarityNearOne(t *testing.T) {

	ctx := context.Background()
	dir := t.TempDir()
	idx, err := NewChromemIndex(dir, 4, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex: %v", err)
	}

	vec := []float32{0.1, 0.2, 0.3, 0.4}
	if err := idx.Add(ctx, "doc-1", "the user was debugging CUDA out of memory", vec, map[string]string{"domain": "work"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	results, err := idx.Search(ctx, vec, 5, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected at least one result, got 0")
	}
	var found *Result
	for i := range results {
		if results[i].ID == "doc-1" {
			found = &results[i]
		}
	}
	if found == nil {
		t.Fatalf("expected doc-1 in results, got: %+v", results)
	}
	const epsilon = 0.01
	if found.Similarity < 1.0-epsilon {
		t.Errorf("self-similarity = %v, want ~1.0 (within %v)", found.Similarity, epsilon)
	}
}

// TestChromemIndex_Search_RespectsNAndWhereFilter verifies Search caps results at n and applies exact-match metadata filtering via where.
func TestChromemIndex_Search_RespectsNAndWhereFilter(t *testing.T) {

	ctx := context.Background()
	dir := t.TempDir()
	idx, err := NewChromemIndex(dir, 3, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex: %v", err)
	}

	docs := []struct {
		id     string
		domain string
	}{
		{"work-1", "work"},
		{"work-2", "work"},
		{"personal-1", "personal"},
		{"personal-2", "personal"},
	}
	for i, d := range docs {
		vec := []float32{float32(i), float32(i + 1), float32(i + 2)}
		if err := idx.Add(ctx, d.id, "content "+d.id, vec, map[string]string{"domain": d.domain}); err != nil {
			t.Fatalf("Add(%s): %v", d.id, err)
		}
	}

	// n is respected.
	results, err := idx.Search(ctx, []float32{0, 1, 2}, 2, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) > 2 {
		t.Errorf("expected at most 2 results, got %d", len(results))
	}

	// where filters to only the matching domain.
	workOnly, err := idx.Search(ctx, []float32{0, 1, 2}, 10, map[string]string{"domain": "work"})
	if err != nil {
		t.Fatalf("Search with where: %v", err)
	}
	for _, r := range workOnly {
		if r.Metadata["domain"] != "work" {
			t.Errorf("expected only domain=work results, got %+v", r)
		}
	}
	if len(workOnly) != 2 {
		t.Errorf("expected exactly 2 work-domain results, got %d", len(workOnly))
	}
}

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

// TestChromemIndex_Delete_RemovesFromSearchAndCount verifies Delete removes an item from subsequent Search results and decrements Count().
func TestChromemIndex_Delete_RemovesFromSearchAndCount(t *testing.T) {

	ctx := context.Background()
	dir := t.TempDir()
	idx, err := NewChromemIndex(dir, 3, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex: %v", err)
	}

	vec := []float32{1, 2, 3}
	if err := idx.Add(ctx, "doomed", "will be deleted", vec, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	before := idx.Count()

	if err := idx.Delete(ctx, "doomed"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if got := idx.Count(); got != before-1 {
		t.Errorf("Count() after delete = %d, want %d", got, before-1)
	}
	results, err := idx.Search(ctx, vec, 10, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range results {
		if r.ID == "doomed" {
			t.Errorf("expected 'doomed' to be gone from search results, got: %+v", results)
		}
	}
}

// TestChromemIndex_IDs_ListsAllDocsAndReflectsDelete verifies IDs() reports every doc currently in the index and stops reporting one right after Delete — the reconciliation sweep (internal/db) walks this list to find orphaned/missing vectors, so it must reflect live state, not a stale snapshot.
func TestChromemIndex_IDs_ListsAllDocsAndReflectsDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx, err := NewChromemIndex(dir, 2, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex: %v", err)
	}

	if err := idx.Add(ctx, "note:1", "a", []float32{1, 0}, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := idx.Add(ctx, "note:2", "b", []float32{0, 1}, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ids := idx.IDs()
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %d: %v", len(ids), ids)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen["note:1"] || !seen["note:2"] {
		t.Errorf("expected both note:1 and note:2, got %v", ids)
	}

	if err := idx.Delete(ctx, "note:1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ids = idx.IDs()
	if len(ids) != 1 || ids[0] != "note:2" {
		t.Errorf("expected only note:2 to remain after delete, got %v", ids)
	}
}

// TestNewChromemIndex_CorruptSidecar_DegradesInsteadOfFailing verifies a truncated/corrupt sidecar file (as a crash mid-write would leave behind) doesn't fail NewChromemIndex — the sidecar only tracks createdAt for eviction ordering, so losing it should degrade to an empty map, not permanently disable semantic search (cmd/daemon.go only logs a warning and leaves vecIndex nil on a construction error, with nothing to repair the file afterward). It also verifies a normal write survives that recovery and is readable back by a fresh loadSidecar.
func TestNewChromemIndex_CorruptSidecar_DegradesInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sidecarPath := filepath.Join(dir, collectionName+"_meta.json")
	if err := os.WriteFile(sidecarPath, []byte(`{"doc-1": "2026-08-1`), 0644); err != nil {
		t.Fatalf("seed corrupt sidecar: %v", err)
	}

	idx, err := NewChromemIndex(dir, 3, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex should degrade past a corrupt sidecar, got error: %v", err)
	}

	if err := idx.Add(ctx, "doc-1", "content", []float32{1, 2, 3}, nil); err != nil {
		t.Fatalf("Add after corrupt-sidecar recovery: %v", err)
	}

	reloaded := loadSidecar(sidecarPath)
	if _, ok := reloaded["doc-1"]; !ok {
		t.Errorf("expected doc-1 to be readable back from the sidecar after recovery, got %+v", reloaded)
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

// TestChromemIndex_IDs_RebuiltFromTheCollectionWhenTheSidecarIsGone covers the reconciliation sweep's dependency on IDs(): the sidecar is bookkeeping that a crash or a stray delete can lose, and answering "no ids" for a collection full of documents makes the sweep skip every orphan delete, skip the over-cap eviction, and re-embed documents that are already there. The ids must come back from the collection itself.
func TestChromemIndex_IDs_RebuiltFromTheCollectionWhenTheSidecarIsGone(t *testing.T) {
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

	if err := os.Remove(filepath.Join(dir, collectionName+"_meta.json")); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	reopened, err := NewChromemIndex(dir, 3, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex after losing the sidecar: %v", err)
	}
	ids := reopened.IDs()
	if len(ids) != 3 {
		t.Fatalf("IDs() = %v (%d), want all 3 documents in the collection", ids, len(ids))
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

// A wrong-width vector must be rejected at Add, because one stored mismatch would make the width probe drop and rebuild the whole collection at next open.
func TestChromemIndex_Add_RejectsWrongWidth(t *testing.T) {
	idx, err := NewChromemIndex(t.TempDir(), 4, 100)
	if err != nil {
		t.Fatalf("NewChromemIndex: %v", err)
	}
	if err := idx.Add(context.Background(), "episode:1", "text", []float32{1, 2, 3}, nil); err == nil {
		t.Fatal("a 3-wide vector must be rejected by a 4-wide index")
	}
	if err := idx.Add(context.Background(), "episode:2", "text", []float32{1, 2, 3, 4}, nil); err != nil {
		t.Fatalf("a correct-width vector must be accepted: %v", err)
	}
}

// A batch add must land every document in the collection, with each one searchable and counted against maxDocs, and must reject the whole batch when any vector is the wrong width.
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
}
