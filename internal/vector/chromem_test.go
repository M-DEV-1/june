package vector

// Tests for ChromemIndex, exercising the real chromem-go-backed implementation in chromem.go.

import (
	"context"
	"testing"
)

// TestChromemIndex_AddThenSearch_SelfSimilarityNearOne verifies adding a vector then searching with it returns cosine similarity near 1.0.
func TestChromemIndex_AddThenSearch_SelfSimilarityNearOne(t *testing.T) {

	ctx := context.Background()
	dir := t.TempDir()
	idx, err := NewChromemIndex(dir, "test-collection", 100)
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
	idx, err := NewChromemIndex(dir, "test-collection", 100)
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
	idx, err := NewChromemIndex(dir, "test-collection", maxDocs)
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
	var gotIDs []string
	for _, r := range results {
		gotIDs = append(gotIDs, r.ID)
	}
	for _, id := range gotIDs {
		if id == "oldest" {
			t.Errorf("expected 'oldest' to be evicted, but it was found in results: %v", gotIDs)
		}
	}
	foundNewest := false
	for _, id := range gotIDs {
		if id == "newest" {
			foundNewest = true
		}
	}
	if !foundNewest {
		t.Errorf("expected 'newest' to still be present, got: %v", gotIDs)
	}
}

// TestChromemIndex_Delete_RemovesFromSearchAndCount verifies Delete removes an item from subsequent Search results and decrements Count().
func TestChromemIndex_Delete_RemovesFromSearchAndCount(t *testing.T) {

	ctx := context.Background()
	dir := t.TempDir()
	idx, err := NewChromemIndex(dir, "test-collection", 100)
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

// TestChromemIndex_PersistenceAcrossRestart_RespectsMaxDocsCap verifies a second ChromemIndex opened on the same dbPath finds docs added by a first, discarded one (simulating a restart), and that the maxDocs sidecar survives too, not just chromem-go's own on-disk data.
func TestChromemIndex_PersistenceAcrossRestart_RespectsMaxDocsCap(t *testing.T) {

	ctx := context.Background()
	dir := t.TempDir()
	const maxDocs = 2

	first, err := NewChromemIndex(dir, "test-collection", maxDocs)
	if err != nil {
		t.Fatalf("NewChromemIndex (first): %v", err)
	}
	if err := first.Add(ctx, "before-restart", "content before restart", []float32{1, 0, 0}, nil); err != nil {
		t.Fatalf("Add (first): %v", err)
	}

	// simulate a restart: discard the first handle, open a fresh one on the same path.
	second, err := NewChromemIndex(dir, "test-collection", maxDocs)
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
