package db

// hybrid_test.go lives in package db (white-box), not db_test, because it needs the store's own handle and unexported helpers (ensureNode, reconcileBackfillCandidates) to set up and check what HybridSearch and ReconcileVectors do.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/memory"
)

// newStore opens a throwaway in-memory store, closed when the test ends. Every test in this package uses it; the in-memory pool is pinned to one connection (see New) so concurrent readers see the same database.
func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// --- HybridSearch: Store-level integration tests, with fake embedder/vectorIndex ---

// fakeHybridEmbedder is a deterministic stand-in for internal/embed.Embedder — no real network calls. Text not present in vectors just returns a fixed zero-ish vector; the fake vector index below returns pre-programmed Results regardless of the query embedding's value, so the embedder only needs to exist and be callable.
type fakeHybridEmbedder struct {
	vectors map[string][]float32
}

func (f *fakeHybridEmbedder) Embed(ctx context.Context, task string, text string) ([]float32, error) {
	if v, ok := f.vectors[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 0}, nil
}

// fakeHybridVectorIndex returns pre-programmed Search results and records Add/Delete activity. addedIDs/deletedCalled are channels (not plain slices) so async-embed-goroutine tests can synchronize deterministically instead of sleeping — same non-blocking-select pattern as internal/agent's fakeLiveSession. Both are nil-safe: a test that doesn't care leaves them nil and the corresponding send is skipped. liveIDs is a real, mutex-guarded set mutated by Add/Delete and read by IDs() — reconciliation-sweep tests seed it directly to simulate "what's currently in the index" and then assert the sweep converges it toward the SQL-side truth.
type fakeHybridVectorIndex struct {
	results       []Result
	addCalls      int
	addedIDs      chan string
	deletedCalled chan string

	// batchErr, when set, makes every AddBatch call fail; batchSizes records the size of each AddBatch call. The fake implements batchVectorIndex so reconcile tests take the same branch production does with a real *vector.ChromemIndex.
	batchErr error

	mu         sync.Mutex
	liveIDs    map[string]bool
	addRecords []addRecord
	batchSizes []int
}

// AddBatch records the batch size and then adds each document through Add, so everything Add records (liveIDs, addRecords, addedIDs) stays true of the batch path too. Input: parallel slices of ids, contents, embeddings and metadata. Output: batchErr if the test set one, and nothing is added in that case.
func (f *fakeHybridVectorIndex) AddBatch(ctx context.Context, ids []string, contents []string, embeddings [][]float32, metadatas []map[string]string) error {
	f.mu.Lock()
	f.batchSizes = append(f.batchSizes, len(ids))
	err := f.batchErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	for i, id := range ids {
		if err := f.Add(ctx, id, contents[i], embeddings[i], metadatas[i]); err != nil {
			return err
		}
	}
	return nil
}

// addRecord is one recorded Add call's full arguments — addCalls/addedIDs only ever tracked the id; metadata-parity tests (W1) need the content and metadata too.
type addRecord struct {
	id       string
	content  string
	metadata map[string]string
}

func (f *fakeHybridVectorIndex) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	f.addCalls++
	f.mu.Lock()
	if f.liveIDs == nil {
		f.liveIDs = make(map[string]bool)
	}
	f.liveIDs[id] = true
	f.addRecords = append(f.addRecords, addRecord{id: id, content: content, metadata: metadata})
	f.mu.Unlock()
	if f.addedIDs != nil {
		f.addedIDs <- id
	}
	return nil
}

// addRecordFor returns the recorded Add call for id, or nil if none happened.
func (f *fakeHybridVectorIndex) addRecordFor(id string) *addRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.addRecords {
		if f.addRecords[i].id == id {
			return &f.addRecords[i]
		}
	}
	return nil
}

func (f *fakeHybridVectorIndex) Delete(ctx context.Context, id string) error {
	f.mu.Lock()
	delete(f.liveIDs, id)
	f.mu.Unlock()
	if f.deletedCalled != nil {
		f.deletedCalled <- id
	}
	return nil
}

func (f *fakeHybridVectorIndex) IDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.liveIDs))
	for id := range f.liveIDs {
		ids = append(ids, id)
	}
	return ids
}

// seedIDs marks ids as already present in the index without going through Add — for tests that need to simulate pre-existing/orphaned vector entries.
func (f *fakeHybridVectorIndex) seedIDs(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.liveIDs == nil {
		f.liveIDs = make(map[string]bool)
	}
	for _, id := range ids {
		f.liveIDs[id] = true
	}
}

func (f *fakeHybridVectorIndex) Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]Result, error) {
	var out []Result
	for _, r := range f.results {
		if !metadataMatchesWhere(r.Metadata, where) {
			continue
		}
		out = append(out, r)
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

func metadataMatchesWhere(meta, where map[string]string) bool {
	for k, v := range where {
		if meta[k] != v {
			return false
		}
	}
	return true
}

// erroringEmbedder always fails Embed — used to prove HybridSearch degrades to lexical-only instead of erroring out entirely when the semantic half is unavailable (e.g. daemon-down over IPC in production).
type erroringEmbedder struct{}

func (erroringEmbedder) Embed(ctx context.Context, task, text string) ([]float32, error) {
	return nil, fmt.Errorf("embed unavailable")
}

// erroringVectorIndex always fails Search — same purpose as erroringEmbedder, for the vector-search-specific failure mode.
type erroringVectorIndex struct{}

func (erroringVectorIndex) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	return nil
}
func (erroringVectorIndex) Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]Result, error) {
	return nil, fmt.Errorf("vector search unavailable")
}
func (erroringVectorIndex) Delete(ctx context.Context, id string) error { return nil }
func (erroringVectorIndex) IDs() []string                               { return nil }

// TestHybridSearch_EmbedFails_DegradesToLexicalOnly verifies an embed error (e.g. the client's daemon-IPC embedder call failing because the daemon is down) doesn't fail the whole HybridSearch call — it degrades to lexical-only fusion instead, since query_memory should still work off FTS5 alone.
func TestHybridSearch_EmbedFails_DegradesToLexicalOnly(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogNote(ctx, "the user likes writing golang", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	store.SetEmbedder(erroringEmbedder{})
	store.SetVectorIndex(&fakeHybridVectorIndex{})

	hits, err := store.HybridSearch(ctx, "golang", "", 10)
	if err != nil {
		t.Fatalf("expected HybridSearch to degrade gracefully, got error: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Content == "the user likes writing golang" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the lexical hit to still surface despite the embed failure, got: %+v", hits)
	}
}

// TestHybridSearch_VectorSearchFails_DegradesToLexicalOnly is the same property for a vector-index Search error specifically (embed itself succeeds, the search call fails).
func TestHybridSearch_VectorSearchFails_DegradesToLexicalOnly(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogNote(ctx, "the user likes writing golang", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(erroringVectorIndex{})

	hits, err := store.HybridSearch(ctx, "golang", "", 10)
	if err != nil {
		t.Fatalf("expected HybridSearch to degrade gracefully, got error: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Content == "the user likes writing golang" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the lexical hit to still surface despite the vector search failure, got: %+v", hits)
	}
}

// TestHybridSearch_TwoTermQuery_KeepsHitMatchingOneTerm is the other side of the floor: a two-word question is how people ask about one thing, not a demand that both words appear. Requiring both turned every two-term query into a strict AND, so "ora daemon" stopped matching "the daemon crashed at startup" — the floor has to scale with the query, not sit at a flat two terms.
func TestHybridSearch_TwoTermQuery_KeepsHitMatchingOneTerm(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogNote(ctx, "the daemon crashed at startup", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	hits, err := store.HybridSearch(ctx, "ora daemon", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected the one-term note to survive a two-term query, got %d hits: %+v", len(hits), hits)
	}
}

// TestHybridSearch_AbsentTopicQuery_ReturnsNoJunkRows is the combined relevance-floor property both minVectorSimilarity and the lexical term-overlap floor exist for: a multi-term query about a topic genuinely absent from the store returns zero hits, not 10 padded rows the model would confabulate an answer from — even though the store has content, and even though a weak vector match and single-term lexical matches both exist for it.
func TestHybridSearch_AbsentTopicQuery_ReturnsNoJunkRows(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogNote(ctx, "the user likes hiking on weekends", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	if _, err := store.LogNote(ctx, "the user works with docker containers", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	// This note shares exactly one term with the query, so FTS5 really does return it and the term-overlap floor is what removes it. Without a row like this the lexical half of the property is never exercised: no term overlaps at all, FTS5 returns nothing, and the floor is never reached.
	if _, err := store.LogNote(ctx, "the user pays a quarterly gym membership", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(&fakeHybridVectorIndex{
		results: []Result{
			{ID: "episode:1", Content: "a weakly related tangent", Similarity: 0.4},
		},
	})

	hits, err := store.HybridSearch(ctx, "quarterly tax filing deadline", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected zero hits for a topic absent from the store, got %d: %+v", len(hits), hits)
	}
}

// TestHybridSearch_VectorOnlyMatch_SurfacesSemanticHit proves the "vector recall" half of the hybrid property: an item the fake vector index returns (a semantic/paraphrase match) but that shares no keyword with the query, and so FTS5 alone would never find, still appears in HybridSearch's fused results.
func TestHybridSearch_VectorOnlyMatch_SurfacesSemanticHit(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(&fakeHybridVectorIndex{
		results: []Result{
			{ID: "episode:99", Content: "that GPU rabbit hole from last week", Similarity: 0.9},
		},
	})

	hits, err := store.HybridSearch(ctx, "cuda debugging session", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Content == "that GPU rabbit hole from last week" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the vector-only semantic hit to surface in fused results, got: %+v", hits)
	}
}

// TestHybridSearch_ConsensusItemRanksAboveSingleSourceItem mirrors the deck's consensus property at the Store/integration level: an item both lexical (FTS5) and vector agree on must rank above an item only one of them found.
func TestHybridSearch_ConsensusItemRanksAboveSingleSourceItem(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogNote(ctx, "fixed the cuda out of memory bug", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(&fakeHybridVectorIndex{
		results: []Result{
			// Same content the lexical search also finds -> consensus.
			{ID: "note:consensus", Content: "fixed the cuda out of memory bug", Similarity: 0.95},
			// Vector-only, no lexical overlap with the query "cuda".
			{ID: "episode:5", Content: "that unrelated GPU rabbit hole", Similarity: 0.4},
		},
	})

	hits, err := store.HybridSearch(ctx, "cuda", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("expected at least 2 hits, got %d: %+v", len(hits), hits)
	}
	if hits[0].Content != "fixed the cuda out of memory bug" {
		t.Errorf("expected the consensus item (found by both lexical and vector) to rank first, got: %+v", hits)
	}
}

// TestHybridSearch_DomainFilter_ExcludesNonMatchingVectorResult verifies domainFilter="work" hard-excludes a personal-domain-tagged vector result that would otherwise appear. The domain tag here rides on the fake vector index's Result.Metadata["domain"], testing the vector-sourced side of the filter specifically.
func TestHybridSearch_DomainFilter_ExcludesNonMatchingVectorResult(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(&fakeHybridVectorIndex{
		results: []Result{
			{ID: "episode:1", Content: "debugging the work CI pipeline", Metadata: map[string]string{"domain": "work"}, Similarity: 0.9},
			{ID: "episode:2", Content: "watching a personal show", Metadata: map[string]string{"domain": "personal"}, Similarity: 0.9},
		},
	})

	hits, err := store.HybridSearch(ctx, "something", "work", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	for _, h := range hits {
		if h.Content == "watching a personal show" {
			t.Errorf(`domainFilter="work" must exclude personal-tagged vector results, got: %+v`, hits)
		}
	}
	found := false
	for _, h := range hits {
		if h.Content == "debugging the work CI pipeline" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the work-tagged vector result to remain, got: %+v", hits)
	}
}

// TestHybridSearch_DomainFilter_ExcludesNonMatchingLexicalResult covers the lexical-side hard filter (candidateDomain's SQL lookup against episodes/nodes), complementing the vector-side test above. No embedder/vector index is configured, so this exercises the lexical-only path exclusively: two episodes share the keyword "cuda", one classifies to "work" via its app name (Slack), one to "personal" (Netflix), and domainFilter="work" must hard-exclude the personal one.
func TestHybridSearch_DomainFilter_ExcludesNonMatchingLexicalResult(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogEpisode(ctx, "Slack", "cuda debugging with the team", "discussing the cuda out of memory bug"); err != nil {
		t.Fatalf("LogEpisode (work): %v", err)
	}
	if _, err := store.LogEpisode(ctx, "Netflix", "cuda documentary", "watching a documentary that mentions cuda cores"); err != nil {
		t.Fatalf("LogEpisode (personal): %v", err)
	}

	hits, err := store.HybridSearch(ctx, "cuda", "work", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	for _, h := range hits {
		if h.Domain == "personal" {
			t.Errorf(`domainFilter="work" must exclude personal-tagged lexical results, got: %+v`, hits)
		}
	}
	found := false
	for _, h := range hits {
		if h.Domain == "work" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the work-tagged lexical result to remain, got: %+v", hits)
	}
}

// --- vector lifecycle: Delete propagation ---

// TestDeleteNote_DeletesVector verifies removing a note also removes its vector entry — without this, delete_note leaves the old content's vector behind, so a semantic query can still surface a "deleted" fact.
func TestDeleteNote_DeletesVector(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	vidx := &fakeHybridVectorIndex{deletedCalled: make(chan string, 1)}
	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(vidx)

	id, err := store.LogNote(ctx, "the user's favorite color is blue", "fact")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	if err := store.DeleteNote(ctx, id); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}

	select {
	case got := <-vidx.deletedCalled:
		want := fmt.Sprintf("note:%d", id)
		if got != want {
			t.Errorf("expected Delete(%q), got Delete(%q)", want, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the vector index Delete call")
	}
}

// TestUpdateNote_DeletesOldVectorAndReAddsNewContent verifies correcting a note's content removes the stale vector and re-embeds the corrected text — otherwise a semantic query can still surface the pre-correction wording after update_note runs.
func TestUpdateNote_DeletesOldVectorAndReAddsNewContent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	vidx := &fakeHybridVectorIndex{deletedCalled: make(chan string, 2), addedIDs: make(chan string, 2)}
	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(vidx)

	id, err := store.LogNote(ctx, "the user's favorite color is blue", "fact")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	// Drain LogNote's own async Add before exercising UpdateNote, so the two Add calls (LogNote's, then UpdateNote's re-embed) aren't ambiguous.
	select {
	case <-vidx.addedIDs:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for LogNote's initial vector add")
	}

	if err := store.UpdateNote(ctx, id, "the user's favorite color is green"); err != nil {
		t.Fatalf("UpdateNote: %v", err)
	}

	wantID := fmt.Sprintf("note:%d", id)
	select {
	case got := <-vidx.deletedCalled:
		if got != wantID {
			t.Errorf("expected Delete(%q), got Delete(%q)", wantID, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the stale vector's Delete call")
	}
	select {
	case got := <-vidx.addedIDs:
		if got != wantID {
			t.Errorf("expected re-Add(%q), got Add(%q)", wantID, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the corrected content's re-Add call")
	}
}

// backdateEpisode directly rewrites an episode's created_at into the past, so tests exercising age-based queries don't have to race real wall-clock second boundaries.
func backdateEpisode(t *testing.T, store *Store, id int64, age time.Duration) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE episodes SET created_at = datetime('now', '-' || ? || ' seconds') WHERE id = ?`, int64(age.Seconds()), id); err != nil {
		t.Fatalf("backdateEpisode: %v", err)
	}
}

// TestReplaceSummariesWithDigest_KeepsVectorsForReparentedSummaries verifies rolling summaries up into a digest does NOT delete the summaries' vectors — the nodes are reparented under the digest, not deleted, so their embeddings still point at live rows.
func TestReplaceSummariesWithDigest_KeepsVectorsForReparentedSummaries(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	vidx := &fakeHybridVectorIndex{deletedCalled: make(chan string, 1)}
	store.SetVectorIndex(vidx)
	vidx.mu.Lock()
	vidx.liveIDs = map[string]bool{}
	vidx.mu.Unlock()

	dayID, err := store.ensureNode(ctx, 0, "day", "2026-08-16")
	if err != nil {
		t.Fatalf("ensureNode(day): %v", err)
	}
	summaryID, err := store.ensureNode(ctx, dayID, "summary", "worked on the compiler")
	if err != nil {
		t.Fatalf("ensureNode(summary): %v", err)
	}
	summaryVecID := fmt.Sprintf("summary:%d", summaryID)
	vidx.mu.Lock()
	vidx.liveIDs[summaryVecID] = true
	vidx.mu.Unlock()

	if err := store.ReplaceSummariesWithDigest(ctx, dayID, []int64{summaryID}, "digest of the day"); err != nil {
		t.Fatalf("ReplaceSummariesWithDigest: %v", err)
	}

	select {
	case got := <-vidx.deletedCalled:
		t.Errorf("expected no Delete call for the reparented summary, got Delete(%q)", got)
	case <-time.After(200 * time.Millisecond):
		// no delete — as expected
	}

	vidx.mu.Lock()
	stillLive := vidx.liveIDs[summaryVecID]
	vidx.mu.Unlock()
	if !stillLive {
		t.Errorf("expected %q to remain in the vector index", summaryVecID)
	}
}

// TestReplaceSummariesWithDigest_DeletesVectorsForDroppedDuplicates verifies that when duplicate summary content collapses two summary nodes into one survivor, the dropped node's own vector entry is deleted too — its underlying node is gone from the nodes table, so a stale vidx row left pointing at it would sit there forever, the same problem DeleteNote's async delete already guards against for notes.
func TestReplaceSummariesWithDigest_DeletesVectorsForDroppedDuplicates(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	vidx := &fakeHybridVectorIndex{deletedCalled: make(chan string, 2)}
	store.SetVectorIndex(vidx)

	dayID, err := store.ensureNode(ctx, 0, "day", "2026-08-16")
	if err != nil {
		t.Fatalf("ensureNode(day): %v", err)
	}
	taskAID, err := store.ensureNode(ctx, dayID, "task", "Task A")
	if err != nil {
		t.Fatalf("ensureNode(task A): %v", err)
	}
	taskBID, err := store.ensureNode(ctx, dayID, "task", "Task B")
	if err != nil {
		t.Fatalf("ensureNode(task B): %v", err)
	}
	const dupContent = "fixed the flaky test uniquedup2"
	sumA, err := store.ensureNode(ctx, taskAID, "summary", dupContent)
	if err != nil {
		t.Fatalf("ensureNode(summary A): %v", err)
	}
	sumB, err := store.ensureNode(ctx, taskBID, "summary", dupContent)
	if err != nil {
		t.Fatalf("ensureNode(summary B): %v", err)
	}

	vidx.mu.Lock()
	vidx.liveIDs = map[string]bool{
		fmt.Sprintf("summary:%d", sumA): true,
		fmt.Sprintf("summary:%d", sumB): true,
	}
	vidx.mu.Unlock()

	if err := store.ReplaceSummariesWithDigest(ctx, dayID, []int64{sumA, sumB}, "digest covering both tasks"); err != nil {
		t.Fatalf("ReplaceSummariesWithDigest: %v", err)
	}

	// dedupeSummaryContent keeps the first of the two and drops the other; find out which survived so the test knows which vector must be gone and which must remain.
	var survivorID int64
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM nodes WHERE type='summary' AND content=?`, dupContent).Scan(&survivorID); err != nil {
		t.Fatalf("find surviving summary: %v", err)
	}
	dropped, survived := fmt.Sprintf("summary:%d", sumB), fmt.Sprintf("summary:%d", sumA)
	if survivorID == sumB {
		dropped, survived = fmt.Sprintf("summary:%d", sumA), fmt.Sprintf("summary:%d", sumB)
	}

	select {
	case got := <-vidx.deletedCalled:
		if got != dropped {
			t.Errorf("expected Delete(%q) for the dropped duplicate, got Delete(%q)", dropped, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the dropped duplicate's vector Delete call")
	}

	vidx.mu.Lock()
	stillLive := vidx.liveIDs[survived]
	vidx.mu.Unlock()
	if !stillLive {
		t.Errorf("expected the surviving summary's vector %q to remain in the index", survived)
	}
}

// TestReplaceSummariesWithDigest_AddsVectorForDigest verifies rolling summaries up into a digest also embeds and adds a vector for the new digest node — otherwise the day becomes FTS-only and invisible to the semantic half of HybridSearch, exactly the case digests exist to answer.
func TestReplaceSummariesWithDigest_AddsVectorForDigest(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	store.SetEmbedder(&fakeHybridEmbedder{})
	vidx := &fakeHybridVectorIndex{deletedCalled: make(chan string, 1), addedIDs: make(chan string, 1)}
	store.SetVectorIndex(vidx)

	dayID, err := store.ensureNode(ctx, 0, "day", "2026-08-16")
	if err != nil {
		t.Fatalf("ensureNode(day): %v", err)
	}
	summaryID, err := store.ensureNode(ctx, dayID, "summary", "worked on the compiler")
	if err != nil {
		t.Fatalf("ensureNode(summary): %v", err)
	}

	if err := store.ReplaceSummariesWithDigest(ctx, dayID, []int64{summaryID}, "digest of the day"); err != nil {
		t.Fatalf("ReplaceSummariesWithDigest: %v", err)
	}

	var digestID int64
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM nodes WHERE type = 'digest'`).Scan(&digestID); err != nil {
		t.Fatalf("select digest id: %v", err)
	}

	select {
	case got := <-vidx.addedIDs:
		want := fmt.Sprintf("digest:%d", digestID)
		if got != want {
			t.Errorf("expected Add(%q), got Add(%q)", want, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the digest's vector Add call")
	}

	rec := vidx.addRecordFor(fmt.Sprintf("digest:%d", digestID))
	if rec == nil {
		t.Fatal("expected an Add call for the digest")
	}
	if rec.content != "digest of the day" {
		t.Errorf("expected embedded content to be the digest text, got %q", rec.content)
	}
	if rec.metadata["source"] != "digest" || rec.metadata["kind"] != string(memory.KindPeriod) || rec.metadata["created_at"] == "" {
		t.Errorf("expected source/kind/created_at metadata matching the sibling write paths, got %+v", rec.metadata)
	}
	if _, ok := rec.metadata["domain"]; !ok {
		t.Errorf("expected a domain key in metadata (even if empty string), got %+v", rec.metadata)
	}
}

// TestHybridSearch_DigestShadowsItsOwnReparentedSummary verifies that when a compacted day's digest and its (now-reparented, still-live) summary both match a query, HybridSearch returns only the digest — otherwise the same day's content would appear twice in a 10-row result.
func TestHybridSearch_DigestShadowsItsOwnReparentedSummary(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	dayID, err := store.ensureNode(ctx, 0, "day", "2026-08-16")
	if err != nil {
		t.Fatalf("ensureNode(day): %v", err)
	}
	summaryID, err := store.ensureNode(ctx, dayID, "summary", "worked on uniqueshadowterm project")
	if err != nil {
		t.Fatalf("ensureNode(summary): %v", err)
	}

	if err := store.ReplaceSummariesWithDigest(ctx, dayID, []int64{summaryID}, "uniqueshadowterm day summary"); err != nil {
		t.Fatalf("ReplaceSummariesWithDigest: %v", err)
	}

	hits, err := store.HybridSearch(ctx, "uniqueshadowterm", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected exactly 1 hit (the digest shadowing its reparented summary), got %d: %+v", len(hits), hits)
	}
	if hits[0].Source != "digest" {
		t.Errorf("expected the surviving hit to be the digest, got source=%s", hits[0].Source)
	}
}

// TestReplaceAllNotes_DeletesVectorsForOldNotes verifies note consolidation (which renumbers every note) deletes the old notes' vectors — otherwise every note vector becomes an orphan after a single consolidation cycle, since the new rows get new ids.
func TestReplaceAllNotes_DeletesVectorsForOldNotes(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	store.SetEmbedder(&fakeHybridEmbedder{})
	vidx := &fakeHybridVectorIndex{deletedCalled: make(chan string, 1), addedIDs: make(chan string, 1)}
	store.SetVectorIndex(vidx)

	oldID, err := store.LogNote(ctx, "the user likes tea", "fact")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	select {
	case <-vidx.addedIDs: // drain LogNote's own async add
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for LogNote's initial vector add")
	}

	if err := store.ReplaceAllNotes(ctx, []string{"the user likes tea and coffee"}); err != nil {
		t.Fatalf("ReplaceAllNotes: %v", err)
	}

	select {
	case got := <-vidx.deletedCalled:
		want := fmt.Sprintf("note:%d", oldID)
		if got != want {
			t.Errorf("expected Delete(%q), got Delete(%q)", want, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the old note's vector Delete call")
	}
}

// --- ReconcileVectors: the sweep that heals orphaned/missing vectors ---

// TestReconcileVectors_DeletesOrphanedNoteVector verifies a vector whose backing note row no longer exists (e.g. from before Delete/UpdateNote propagated to vectors, or the dirty pre-existing store) gets removed.
func TestReconcileVectors_DeletesOrphanedNoteVector(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	vidx := &fakeHybridVectorIndex{}
	vidx.seedIDs("note:999") // no note with id 999 exists
	store.SetVectorIndex(vidx)

	report, err := store.ReconcileVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}
	if report.Deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", report.Deleted)
	}
	if ids := vidx.IDs(); len(ids) != 0 {
		t.Errorf("expected the orphaned vector gone from the index, got %v", ids)
	}
}

// TestReconcileVectors_DeletesVectorForThinnedEpisode verifies a vector for an episode whose screen_text has since been cleared gets removed even though the episode row itself still exists.
func TestReconcileVectors_DeletesVectorForThinnedEpisode(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	id, err := store.LogEpisode(ctx, "Code", "main.go", "some content")
	if err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE episodes SET screen_text = '' WHERE id = ?`, id); err != nil {
		t.Fatalf("thin episode: %v", err)
	}

	vidx := &fakeHybridVectorIndex{}
	vidx.seedIDs(fmt.Sprintf("episode:%d", id))
	store.SetVectorIndex(vidx)

	report, err := store.ReconcileVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}
	if report.Deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", report.Deleted)
	}
	if ids := vidx.IDs(); len(ids) != 0 {
		t.Errorf("expected the thinned episode's vector gone from the index, got %v", ids)
	}
}

// TestReconcileVectors_LeavesFreshEpisodeVectorAlone verifies an episode that still has its screen_text (not aged, backing row present) is left untouched.
func TestReconcileVectors_LeavesFreshEpisodeVectorAlone(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	id, err := store.LogEpisode(ctx, "Code", "main.go", "some content")
	if err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	vidx := &fakeHybridVectorIndex{}
	vidx.seedIDs(fmt.Sprintf("episode:%d", id))
	store.SetVectorIndex(vidx)

	report, err := store.ReconcileVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}
	if report.Deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", report.Deleted)
	}
	if ids := vidx.IDs(); len(ids) != 1 {
		t.Errorf("expected the fresh episode's vector to remain, got %v", ids)
	}
}

// TestReconcileVectors_RespectsEmbedCap verifies the sweep stops backfilling once embedCap is reached, instead of embedding every missing candidate in one pass — protects API quota on a large dirty store.
func TestReconcileVectors_RespectsEmbedCap(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	for _, content := range []string{"the user likes tea", "the user likes coffee", "the user likes cocoa"} {
		if _, err := store.LogNote(ctx, content, "fact"); err != nil {
			t.Fatalf("LogNote(%q): %v", content, err)
		}
	}

	store.SetEmbedder(&fakeHybridEmbedder{})
	vidx := &fakeHybridVectorIndex{}
	store.SetVectorIndex(vidx)

	report, err := store.ReconcileVectors(ctx, 2)
	if err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}
	if report.Backfilled != 2 {
		t.Errorf("expected exactly 2 backfilled (capped), got %d", report.Backfilled)
	}
	if len(vidx.IDs()) != 2 {
		t.Errorf("expected exactly 2 vectors in the index, got %d: %v", len(vidx.IDs()), vidx.IDs())
	}
}

// --- ReconcileVectors: backfill metadata parity with the original write paths (W1) ---

// TestReconcileVectors_BackfillsBySourceType verifies the sweep backfills a vector for a node with none, for every source type the sweep owns — note, summary, digest, episode and thread — matching that type's own async-embed write path (W1): the right embedded text (JSON-extracted for a summary, Document()-framed for an episode, the FTS-trigger text for a thread) and the right source/kind/created_at/domain metadata. A missing field here is not cosmetic: a missing domain silently drops the vector out of every domain-filtered search, and a missing created_at defeats recency decay.
func TestReconcileVectors_BackfillsBySourceType(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, ctx context.Context, store *Store) (wantID string)
		check func(t *testing.T, rec *addRecord)
	}{
		{
			name: "note",
			setup: func(t *testing.T, ctx context.Context, store *Store) string {
				// LogNote runs before the embedder/vector index are wired below, so its own async embed never fires — this note is missing a vector purely because ReconcileVectors needs to backfill it.
				id, err := store.LogNote(ctx, "the user's favorite color is blue", "fact")
				if err != nil {
					t.Fatalf("LogNote: %v", err)
				}
				return fmt.Sprintf("note:%d", id)
			},
			check: func(t *testing.T, rec *addRecord) {
				if rec.metadata["created_at"] == "" {
					t.Errorf("expected created_at metadata to be set, got %+v", rec.metadata)
				}
			},
		},
		{
			name: "summary",
			setup: func(t *testing.T, ctx context.Context, store *Store) string {
				dayID, err := store.ensureNode(ctx, 0, "day", "2026-08-16")
				if err != nil {
					t.Fatalf("ensureNode(day): %v", err)
				}
				// nodes.content for a summary is the full JSON-marshaled TaskSummary, not the plain summary text — LogSemanticNode's own embed goroutine embeds only summary.Summary, so backfill must extract the same field.
				summaryID, err := store.ensureNode(ctx, dayID, "summary", `{"same_task":false,"task_name":"debugging session","summary":"fixed the parser edge case"}`)
				if err != nil {
					t.Fatalf("ensureNode(summary): %v", err)
				}
				if _, err := store.db.Exec(`UPDATE nodes SET domain = 'work' WHERE id = ?`, summaryID); err != nil {
					t.Fatalf("set domain: %v", err)
				}
				return fmt.Sprintf("summary:%d", summaryID)
			},
			check: func(t *testing.T, rec *addRecord) {
				if rec.content != "fixed the parser edge case" {
					t.Errorf("expected the embedded content to be the extracted summary text, got %q", rec.content)
				}
				if rec.metadata["domain"] != "work" {
					t.Errorf("expected domain metadata %q, got %+v", "work", rec.metadata)
				}
				if rec.metadata["source"] != "summary" || rec.metadata["kind"] != string(memory.KindPeriod) || rec.metadata["created_at"] == "" {
					t.Errorf("expected source/kind/created_at metadata matching LogSemanticNode's write path, got %+v", rec.metadata)
				}
			},
		},
		{
			name: "digest",
			setup: func(t *testing.T, ctx context.Context, store *Store) string {
				dayID, err := store.ensureNode(ctx, 0, "day", "2026-08-16")
				if err != nil {
					t.Fatalf("ensureNode(day): %v", err)
				}
				digestID, err := store.ensureNode(ctx, dayID, "digest", "rolled-up digest of the day")
				if err != nil {
					t.Fatalf("ensureNode(digest): %v", err)
				}
				return fmt.Sprintf("digest:%d", digestID)
			},
			check: func(t *testing.T, rec *addRecord) {
				if rec.content != "rolled-up digest of the day" {
					t.Errorf("expected the embedded content to be the digest text, got %q", rec.content)
				}
				if rec.metadata["source"] != "digest" || rec.metadata["kind"] != string(memory.KindPeriod) || rec.metadata["created_at"] == "" {
					t.Errorf("expected source/kind/created_at metadata matching the digest write path, got %+v", rec.metadata)
				}
			},
		},
		{
			name: "episode",
			setup: func(t *testing.T, ctx context.Context, store *Store) string {
				id, err := store.LogEpisode(ctx, "Code", "auth.go", "debugging the token refresh flow")
				if err != nil {
					t.Fatalf("LogEpisode: %v", err)
				}
				return fmt.Sprintf("episode:%d", id)
			},
			check: func(t *testing.T, rec *addRecord) {
				if !strings.Contains(rec.content, "Code") || !strings.Contains(rec.content, "auth.go") {
					t.Errorf("expected the embedded content to be Document()-framed (app/title header), got %q", rec.content)
				}
				if !strings.Contains(rec.content, "debugging the token refresh flow") {
					t.Errorf("expected the embedded content to still carry the raw substance, got %q", rec.content)
				}
				if rec.metadata["source"] != "episode" || rec.metadata["kind"] != string(memory.KindMoment) || rec.metadata["created_at"] == "" {
					t.Errorf("expected source/kind/created_at metadata matching LogEpisode's write path, got %+v", rec.metadata)
				}
				if _, ok := rec.metadata["domain"]; !ok {
					t.Errorf("expected a domain key in metadata (even if empty string), got %+v", rec.metadata)
				}
			},
		},
		{
			name: "thread",
			setup: func(t *testing.T, ctx context.Context, store *Store) string {
				id, err := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "learning Vulkan", Kind: "learning", State: "working through the triangle tutorial"})
				if err != nil {
					t.Fatalf("UpsertThread: %v", err)
				}
				return fmt.Sprintf("thread:%d", id)
			},
			check: func(t *testing.T, rec *addRecord) {
				// The embedded text must match what the threads_ai FTS trigger indexes, so the lexical and vector halves of hybrid search see the same thread.
				if want := "learning Vulkan — working through the triangle tutorial"; rec.content != want {
					t.Errorf("thread content = %q, want %q", rec.content, want)
				}
				if rec.metadata["source"] != "thread" {
					t.Errorf("thread metadata source = %q, want thread", rec.metadata["source"])
				}
				if rec.metadata["kind"] != "arc" {
					t.Errorf("thread metadata kind = %q, want arc", rec.metadata["kind"])
				}
				if rec.metadata["created_at"] == "" {
					t.Error("thread metadata is missing created_at, which HybridSearch needs for recency")
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			store := newStore(t)
			wantID := c.setup(t, ctx, store)

			store.SetEmbedder(&fakeHybridEmbedder{})
			vidx := &fakeHybridVectorIndex{}
			store.SetVectorIndex(vidx)

			report, err := store.ReconcileVectors(ctx, 200)
			if err != nil {
				t.Fatalf("ReconcileVectors: %v", err)
			}
			if report.Backfilled != 1 {
				t.Errorf("expected 1 backfilled, got %d", report.Backfilled)
			}

			rec := vidx.addRecordFor(wantID)
			if rec == nil {
				t.Fatalf("expected an Add call for %q, got %v", wantID, vidx.IDs())
			}
			c.check(t, rec)
		})
	}
}

// TestHybridSearch_LexicalNoteHit_CarriesCreatedAt verifies HybridSearch keeps the timestamp SearchMemory already resolved for summary/note/thread hits. The episode loop below it sets createdAt; the memHits loop used to omit it, so every fact/arc/period reached FormatHit with a zero time and rendered undated.
func TestHybridSearch_LexicalNoteHit_CarriesCreatedAt(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogNote(ctx, "the user works with docker containers", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	hits, err := store.HybridSearch(ctx, "docker containers", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected a lexical note hit")
	}
	if hits[0].CreatedAt.IsZero() {
		t.Errorf("expected the note hit to carry its created_at through fusion, got the zero time: %+v", hits[0])
	}
}

// TestUpdateNote_MissingID_ErrorsAndSkipsVectorDelete verifies updating a note id that doesn't exist reports the failure instead of silently succeeding, and does not fire the async vector Delete for that id. A hallucinated id used to be "updated" successfully — the user's correction was dropped and the bogus "note:N" Delete still ran, which can evict a real note's vector.
func TestUpdateNote_MissingID_ErrorsAndSkipsVectorDelete(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	vidx := &fakeHybridVectorIndex{deletedCalled: make(chan string, 2), addedIDs: make(chan string, 2)}
	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(vidx)

	err := store.UpdateNote(ctx, 4242, "a correction aimed at a note that was never written")
	if err == nil {
		t.Fatal("expected an error updating a nonexistent note id, got nil")
	}
	if !strings.Contains(err.Error(), "4242") {
		t.Errorf("expected the error to name the missing id, got %q", err)
	}

	select {
	case got := <-vidx.deletedCalled:
		t.Errorf("expected no vector Delete for a note that doesn't exist, got Delete(%q)", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHybridSearch_RelativeVectorFloor_DropsHitsFarBelowTheBest verifies the similarity floor is relative to the best hit, not just the absolute minVectorSimilarity. Real cosine similarities on this store cluster in a narrow band (~0.58-0.69), so an absolute 0.55 floor lets a junk episode through alongside a genuine match.
func TestHybridSearch_RelativeVectorFloor_DropsHitsFarBelowTheBest(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(&fakeHybridVectorIndex{
		results: []Result{
			{ID: "episode:1", Content: "the genuine match", Similarity: 0.69},
			{ID: "episode:2", Content: "junk that clears the absolute floor", Similarity: 0.58},
		},
	})

	hits, err := store.HybridSearch(ctx, "the genuine match", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	var kept []string
	for _, h := range hits {
		kept = append(kept, h.Content)
	}
	if len(kept) != 1 || kept[0] != "the genuine match" {
		t.Errorf("expected only the top vector hit to survive the relative floor, got %v", kept)
	}
}

// TestReconcileVectors_DeletesOrphanedThreadVector verifies that now threads are backfilled, a vector for a thread row that has since been deleted is cleaned up too — otherwise every deleted thread would leak a permanent phantom hit.
func TestReconcileVectors_DeletesOrphanedThreadVector(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	vidx := &fakeHybridVectorIndex{}
	vidx.seedIDs("thread:4242") // no thread with id 4242 exists
	store.SetVectorIndex(vidx)

	report, err := store.ReconcileVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}
	if report.Deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", report.Deleted)
	}
}

// TestReconcileVectors_EpisodeWindowExcludesOldEpisodesByDefault pins the Gemini-path behavior: an episode older than the default window is not re-embedded, so a dirty store cannot run up an API bill re-embedding years of captures.
func TestReconcileVectors_EpisodeWindowExcludesOldEpisodesByDefault(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	id, err := store.LogEpisode(ctx, "Code", "old.go", "an old capture")
	if err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE episodes SET created_at = datetime('now','-60 days') WHERE id = ?`, id); err != nil {
		t.Fatalf("age episode: %v", err)
	}

	store.SetEmbedder(&fakeHybridEmbedder{})
	vidx := &fakeHybridVectorIndex{}
	store.SetVectorIndex(vidx)

	if _, err := store.ReconcileVectors(ctx, 200); err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}
	if rec := vidx.addRecordFor(fmt.Sprintf("episode:%d", id)); rec != nil {
		t.Errorf("a 60-day-old episode was backfilled on the default (metered) path, got %v", vidx.IDs())
	}
}

// TestReconcileVectors_UnmeteredEmbedsBackfillEveryOldEpisode verifies that with a local embedder wired (SetEmbedsAreFree), the age window is dropped entirely — the ~1000 old episodes that lost their vectors to API failures become searchable again, and re-embedding them costs nothing but CPU.
func TestReconcileVectors_UnmeteredEmbedsBackfillEveryOldEpisode(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	id, err := store.LogEpisode(ctx, "Code", "old.go", "an old capture")
	if err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE episodes SET created_at = datetime('now','-60 days') WHERE id = ?`, id); err != nil {
		t.Fatalf("age episode: %v", err)
	}

	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetEmbedsAreFree(true)
	vidx := &fakeHybridVectorIndex{}
	store.SetVectorIndex(vidx)

	if _, err := store.ReconcileVectors(ctx, 200); err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}
	if rec := vidx.addRecordFor(fmt.Sprintf("episode:%d", id)); rec == nil {
		t.Errorf("expected the 60-day-old episode backfilled with a free embedder, got %v", vidx.IDs())
	}
}

// TestHybridSearchVectorFloorFollowsTheEmbedder verifies the absolute cosine floor can be moved to match the embedder in use. minVectorSimilarity's 0.55 was measured against Gemini's similarity range; EmbeddingGemma scores the same genuinely-relevant documents lower, so leaving the floor at 0.55 would drop every vector candidate on the open-ended questions ("what did i do today") and silently reduce hybrid search to lexical-only.
func TestHybridSearchVectorFloorFollowsTheEmbedder(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	id, err := store.LogNote(ctx, "the user has been building a memory system all week", "fact")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	// A hit a local embedder would plausibly score: clearly relevant, comfortably under the Gemini-calibrated 0.55.
	vidx := &fakeHybridVectorIndex{}
	vidx.results = []Result{{
		ID:         fmt.Sprintf("note:%d", id),
		Content:    "the user has been building a memory system all week",
		Metadata:   map[string]string{"source": "note", "kind": string(memory.KindFact)},
		Similarity: 0.44,
	}}
	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(vidx)

	// Default floor: the 0.44 hit is below 0.55 and never enters fusion.
	hits, err := store.HybridSearch(ctx, "zzqqxx", "", 5)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected the 0.44 hit to be dropped at the default floor, got %d hits", len(hits))
	}

	store.SetVectorSimilarityFloor(0.40)
	hits, err = store.HybridSearch(ctx, "zzqqxx", "", 5)
	if err != nil {
		t.Fatalf("HybridSearch after lowering the floor: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected the 0.44 hit to survive a 0.40 floor, got %d hits", len(hits))
	}
}

// TestReconcileBackfillCandidates_BoundedByTheEmbedCap covers the sweep's memory cost: with a local embedder the episode query has neither an age window nor a limit, so it pulled every episode row — id plus the full capture text — into memory to hand ReconcileVectors a list it only ever reads the first embedCap entries of. The candidate list must stay proportional to what one sweep can actually embed.
func TestReconcileBackfillCandidates_BoundedByTheEmbedCap(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	for i := 0; i < 300; i++ {
		if _, err := store.LogEpisode(ctx, "Code", "main.go", fmt.Sprintf("capture %d", i)); err != nil {
			t.Fatalf("LogEpisode %d: %v", i, err)
		}
	}
	store.SetEmbedsAreFree(true)

	const embedCap = 10
	got := store.reconcileBackfillCandidates(ctx, nil, embedCap)
	if len(got) > 4*embedCap {
		t.Errorf("built %d candidates for an embed cap of %d, want no more than %d", len(got), embedCap, 4*embedCap)
	}
	if len(got) < embedCap {
		t.Fatalf("built only %d candidates, too few to fill a sweep of %d", len(got), embedCap)
	}
}

// TestReconcileBackfillCandidates_SkipsWhatTheIndexAlreadyHas verifies the already-indexed ids are dropped while the candidate list is being built, not after: with the list bounded, spending that budget on documents that already have vectors would starve the ones that don't.
func TestReconcileBackfillCandidates_SkipsWhatTheIndexAlreadyHas(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	var ids []int64
	for i := 0; i < 5; i++ {
		id, err := store.LogEpisode(ctx, "Code", "main.go", fmt.Sprintf("capture %d", i))
		if err != nil {
			t.Fatalf("LogEpisode %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	store.SetEmbedsAreFree(true)

	existing := map[string]bool{fmt.Sprintf("episode:%d", ids[0]): true, fmt.Sprintf("episode:%d", ids[1]): true}
	for _, c := range store.reconcileBackfillCandidates(ctx, existing, 100) {
		if existing[c.id] {
			t.Errorf("candidate %q is already in the index and should not have been built", c.id)
		}
	}
}

// TestUpdateThreadState_RewritesRowAndVector covers the repair path for a wrong thread summary: the model diagnosed a bad merge in a thread ("the mf x mdev meeting was on Teams" when the episodes say Google Meet) and had no way to fix it, because update_note only reaches the notes table. Rewriting the state has to land in the row, in FTS, and in the vector, or the wrong wording keeps coming back on the next semantic search.
func TestUpdateThreadState_RewritesRowAndVector(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	id, err := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "mf x mdev", Kind: "work", State: "recurring Microsoft Teams sync"})
	if err != nil {
		t.Fatalf("UpsertThread: %v", err)
	}

	vidx := &fakeHybridVectorIndex{deletedCalled: make(chan string, 2), addedIDs: make(chan string, 2)}
	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(vidx)

	if err := store.UpdateThreadState(ctx, id, "recurring Google Meet sync"); err != nil {
		t.Fatalf("UpdateThreadState: %v", err)
	}

	hits, err := store.SearchMemory(ctx, "Google Meet")
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("corrected thread state is not searchable")
	}
	if stale, _ := store.SearchMemory(ctx, "Teams"); len(stale) != 0 {
		t.Errorf("the wrong wording is still in FTS: %+v", stale)
	}

	wantID := fmt.Sprintf("thread:%d", id)
	select {
	case got := <-vidx.deletedCalled:
		if got != wantID {
			t.Errorf("expected Delete(%q), got Delete(%q)", wantID, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the stale thread vector's Delete call")
	}
	select {
	case got := <-vidx.addedIDs:
		if got != wantID {
			t.Errorf("expected Add(%q), got Add(%q)", wantID, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the corrected thread vector's Add call")
	}
	if rec := vidx.addRecordFor(wantID); rec == nil || rec.content != "mf x mdev — recurring Google Meet sync" {
		t.Errorf("re-embedded text = %+v, want the subject and the corrected state", rec)
	}
}

// TestUpdateThreadState_MissingID_Errors verifies a thread id the model invented is reported as a failure rather than silently succeeding — the same guard UpdateNote has, for the same reason: reporting "fixed" throws the user's correction away.
func TestUpdateThreadState_MissingID_Errors(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if err := store.UpdateThreadState(ctx, 4242, "a correction aimed at a thread that never existed"); err == nil {
		t.Fatal("expected an error for a thread id that does not exist")
	}
}

// --- HybridSearchWindow: time-windowed retrieval ---

// TestHybridSearchWindow_SparseWindow_SurfacesInWindowEpisodeSQLSide is the core "what was I doing Tuesday afternoon" property: an episode inside a sparse window must surface even when enough better-ranking out-of-window matches exist to fill the lexical top-10 on their own. Filtering after top-k would return nothing here — the window has to constrain the SQL itself.
func TestHybridSearchWindow_SparseWindow_SurfacesInWindowEpisodeSQLSide(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	// 12 fresh episodes that out-rank the old one on FTS (the term appears twice), so an implementation that filters the top-10 after ranking never sees the in-window row.
	for i := 0; i < 12; i++ {
		if _, err := store.LogEpisode(ctx, "Code", fmt.Sprintf("compose-%d.yaml", i), "docker compose deploy to the docker swarm"); err != nil {
			t.Fatalf("LogEpisode fresh %d: %v", i, err)
		}
	}
	oldID, err := store.LogEpisode(ctx, "Code", "registry.go", "debugging the registry migration in docker")
	if err != nil {
		t.Fatalf("LogEpisode old: %v", err)
	}
	backdateEpisode(t, store, oldID, 72*time.Hour)

	now := time.Now()
	hits, err := store.HybridSearchWindow(ctx, "docker", "", now.Add(-73*time.Hour), now.Add(-71*time.Hour), 10)
	if err != nil {
		t.Fatalf("HybridSearchWindow: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected exactly the one in-window episode, got %d hits: %+v", len(hits), hits)
	}
	if !strings.Contains(hits[0].Content, "registry migration") {
		t.Errorf("expected the backdated in-window episode, got: %+v", hits[0])
	}
}

// TestHybridSearchWindow_EmptyWindow_ReturnsEmptyNotFallback verifies a window containing nothing returns an honestly empty result — never a silent fallback to the unwindowed matches, which would answer "what did I do last Tuesday" with things from today.
func TestHybridSearchWindow_EmptyWindow_ReturnsEmptyNotFallback(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogEpisode(ctx, "Code", "main.go", "refactoring the tracker daemon"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	if _, err := store.LogNote(ctx, "the user is refactoring the tracker", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	now := time.Now()
	hits, err := store.HybridSearchWindow(ctx, "tracker", "", now.AddDate(0, 0, -10), now.AddDate(0, 0, -9), 10)
	if err != nil {
		t.Fatalf("HybridSearchWindow: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected an empty result for a window with nothing in it, got: %+v", hits)
	}
}

// TestHybridSearchWindow_OpenEndedBounds verifies a zero bound is open on that side: since-only means "from then until now", until-only means "everything up to then".
func TestHybridSearchWindow_OpenEndedBounds(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogEpisode(ctx, "Meet", "standup", "meeting about the quarterly roadmap"); err != nil {
		t.Fatalf("LogEpisode recent: %v", err)
	}
	oldID, err := store.LogEpisode(ctx, "Meet", "retro", "meeting notes from the sprint retro")
	if err != nil {
		t.Fatalf("LogEpisode old: %v", err)
	}
	backdateEpisode(t, store, oldID, 48*time.Hour)
	cut := time.Now().Add(-24 * time.Hour)

	hits, err := store.HybridSearchWindow(ctx, "meeting", "", cut, time.Time{}, 10)
	if err != nil {
		t.Fatalf("HybridSearchWindow since-only: %v", err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Content, "quarterly roadmap") {
		t.Errorf("since-only window: expected only the recent episode, got: %+v", hits)
	}

	hits, err = store.HybridSearchWindow(ctx, "meeting", "", time.Time{}, cut, 10)
	if err != nil {
		t.Fatalf("HybridSearchWindow until-only: %v", err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Content, "sprint retro") {
		t.Errorf("until-only window: expected only the old episode, got: %+v", hits)
	}
}

// TestHybridSearchWindow_NotesAndSummaries_FilteredByCreatedAt verifies the window reaches the memory_fts sources too (notes and summary nodes, whose timestamps live in their own tables), not just episodes.
func TestHybridSearchWindow_NotesAndSummaries_FilteredByCreatedAt(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogNote(ctx, "the user prefers oat milk lattes", "fact"); err != nil {
		t.Fatalf("LogNote fresh: %v", err)
	}
	oldNoteID, err := store.LogNote(ctx, "ordered an oat milk delivery", "fact")
	if err != nil {
		t.Fatalf("LogNote old: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE notes SET created_at = datetime('now','-10 days') WHERE id = ?`, oldNoteID); err != nil {
		t.Fatalf("backdate note: %v", err)
	}
	dayID, err := store.ensureNode(ctx, 0, "day", "2026-08-28")
	if err != nil {
		t.Fatalf("ensureNode(day): %v", err)
	}
	if _, err := store.ensureNode(ctx, dayID, "summary", "afternoon spent comparing oat milk brands"); err != nil {
		t.Fatalf("ensureNode(summary): %v", err)
	}

	hits, err := store.HybridSearchWindow(ctx, "oat milk", "", time.Now().AddDate(0, 0, -2), time.Time{}, 10)
	if err != nil {
		t.Fatalf("HybridSearchWindow: %v", err)
	}
	var contents []string
	for _, h := range hits {
		contents = append(contents, h.Content)
	}
	joined := strings.Join(contents, " | ")
	if !strings.Contains(joined, "prefers oat milk lattes") {
		t.Errorf("expected the in-window note to surface, got: %v", contents)
	}
	if !strings.Contains(joined, "comparing oat milk brands") {
		t.Errorf("expected the in-window summary to surface, got: %v", contents)
	}
	if strings.Contains(joined, "oat milk delivery") {
		t.Errorf("expected the 10-day-old note to be excluded from a 2-day window, got: %v", contents)
	}
}

// TestHybridSearchWindow_VectorCandidates_OutsideWindowOrUndatedDropped verifies the vector half honors the window too: a candidate whose created_at metadata falls outside it — or is missing, so membership can't be shown — must not ride into fused results. (Unwindowed search keeps undated vector hits; see TestHybridSearch_VectorOnlyMatch_SurfacesSemanticHit.)
func TestHybridSearchWindow_VectorCandidates_OutsideWindowOrUndatedDropped(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	now := time.Now().UTC()
	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(&fakeHybridVectorIndex{
		results: []Result{
			{ID: "episode:1", Content: "in-window gpu debugging", Similarity: 0.9,
				Metadata: map[string]string{"created_at": now.Add(-time.Hour).Format(time.RFC3339)}},
			{ID: "episode:2", Content: "out-of-window gpu debugging", Similarity: 0.9,
				Metadata: map[string]string{"created_at": now.AddDate(0, 0, -10).Format(time.RFC3339)}},
			{ID: "episode:3", Content: "undated gpu debugging", Similarity: 0.9},
		},
	})

	hits, err := store.HybridSearchWindow(ctx, "gpu", "", time.Now().Add(-24*time.Hour), time.Time{}, 10)
	if err != nil {
		t.Fatalf("HybridSearchWindow: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected only the in-window vector hit, got %d: %+v", len(hits), hits)
	}
	if hits[0].Content != "in-window gpu debugging" {
		t.Errorf("expected the in-window vector hit, got: %+v", hits[0])
	}
}

// A question about time of day — when someone usually stops working, what they did right before a meeting — is answerable only if the rows say what time things happened. Rendering the date without the clock made every such question unanswerable from any number of rows: "when do I usually stop working" was refused by all three model arms while the rows in front of them plainly described wrapping up for the night.
func TestFormatHit_CarriesTheClockTimeNotJustTheDate(t *testing.T) {
	when := time.Now().Add(-72 * time.Hour)
	got := FormatHit(MemoryHit{Source: "summary", Content: "wrapped up for the night", CreatedAt: when}, 0)
	if !strings.Contains(got, when.Local().Format("15:04")) {
		t.Errorf("hit does not carry the time of day: %q", got)
	}
}

// query_memory sends note hits to FormatNoteHit and every other source to FormatHit. Budgeting only the latter left meeting minutes truncated to 200 runes in the live agent while the eval, which formats everything through FormatHit, reported the fix working.
func TestFormatNoteHit_GetsTheSameBudgetAsFormatHit(t *testing.T) {
	long := strings.Repeat("a meeting minute. ", 200) // ~3600 runes, the size of real minutes
	got := FormatNoteHit(MemoryHit{Source: "note", RefID: 7, Content: long}, 0)
	if len([]rune(got)) < 2000 {
		t.Errorf("note excerpted to %d runes; it should get the note budget, not the episode one", len([]rune(got)))
	}
}

// A 50-question gold run on 2026-09-04 found answers with no evidence: the user could not see which stored row an answer came from, so a misheard name in a meeting went unnoticed. FormatHitWithSource carries the same line FormatHit renders plus a parseable source tag a caller can trace back to the row.
func TestFormatHitWithSource_AppendsParseableSourceTag(t *testing.T) {
	when := time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)
	h := MemoryHit{Source: "episode", RefID: 42, Content: "reviewing the PR", App: "Code", Title: "main.go", CreatedAt: when}
	got := FormatHitWithSource(h, 0)
	if !strings.HasPrefix(got, FormatHit(h, 0)) {
		t.Fatalf("expected FormatHitWithSource to lead with FormatHit's own line, got %q", got)
	}
	i := strings.Index(got, `{"source":`)
	if i < 0 {
		t.Fatalf("expected a {\"source\":...} tag, got %q", got)
	}
	var wrapped struct{ Source EvidenceSource }
	if err := json.Unmarshal([]byte(got[i:]), &wrapped); err != nil {
		t.Fatalf("source tag did not parse as JSON: %v (%q)", err, got[i:])
	}
	want := EvidenceSource{Kind: "episode", ID: 42, Title: "main.go", When: when.Format(time.RFC3339)}
	if wrapped.Source != want {
		t.Errorf("source = %+v, want %+v", wrapped.Source, want)
	}
}

// Consolidation merges facts into fewer, better-worded ones, but a merge is a model's summary and the originals are the source. On 2026-09-03 the store held 19 facts after 28 consolidation runs, and nothing on disk could say what those runs had folded away. The replaced facts now move to notes_archive with the time they were archived, so a wrong merge can be traced and undone. Storage is not a concern: every note in the store together is about 115 KB.
func TestReplaceAllNotes_ArchivesTheOldFacts(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	for _, c := range []string{"the user likes tea", "the user likes coffee"} {
		if _, err := store.LogNote(ctx, c, "fact"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReplaceAllNotes(ctx, []string{"the user likes tea and coffee"}); err != nil {
		t.Fatalf("ReplaceAllNotes: %v", err)
	}
	archived, err := store.ArchivedNotes(ctx)
	if err != nil {
		t.Fatalf("ArchivedNotes: %v", err)
	}
	if len(archived) != 2 {
		t.Fatalf("archived = %d rows, want the 2 replaced facts", len(archived))
	}
	got := map[string]bool{}
	for _, a := range archived {
		got[a.Content] = true
		if a.ArchivedAt.IsZero() {
			t.Errorf("archived note %q has no archived_at", a.Content)
		}
	}
	if !got["the user likes tea"] || !got["the user likes coffee"] {
		t.Fatalf("archived contents = %v, want both originals", got)
	}
}

// TestHybridSearch_DomainFilter_KeepsUntaggedNote checks that a domain filter narrows the sources that carry a domain instead of emptying the result. Notes, threads and diary rows have no domain column, so filtering them on exact equality dropped every one of them and domain="work" returned nothing but episodes and summaries.
func TestHybridSearch_DomainFilter_KeepsUntaggedNote(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogNote(ctx, "the user runs the kubernetes migration for the payments team", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	hits, err := store.HybridSearch(ctx, "kubernetes migration payments", "work", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Source == "note" {
			found = true
		}
	}
	if !found {
		t.Errorf(`domainFilter="work" dropped the note, which carries no domain at all: %+v`, hits)
	}
}

// A sweep against an index that takes batches sends its backfill in one AddBatch rather than one Add per document, and every document still lands in the index and is counted.
func TestReconcileVectors_BackfillsInOneBatch(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	for _, content := range []string{"the user likes tea", "the user likes coffee", "the user likes cocoa"} {
		if _, err := store.LogNote(ctx, content, "fact"); err != nil {
			t.Fatalf("LogNote(%q): %v", content, err)
		}
	}
	store.SetEmbedder(&fakeHybridEmbedder{})
	vidx := &fakeHybridVectorIndex{}
	store.SetVectorIndex(vidx)

	report, err := store.ReconcileVectors(ctx, 200)
	if err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}
	if report.Backfilled != 3 {
		t.Errorf("Backfilled = %d, want 3", report.Backfilled)
	}
	if len(vidx.batchSizes) != 1 || vidx.batchSizes[0] != 3 {
		t.Errorf("batch sizes = %v, want one batch of 3", vidx.batchSizes)
	}
	if len(vidx.IDs()) != 3 {
		t.Errorf("index holds %v, want all three notes", vidx.IDs())
	}
}

// A failed batch must not throw away the embeddings it was already charged for: the sweep re-adds each document on its own and counts the ones that land, so the next sweep does not have to pay for them again.
func TestReconcileVectors_FailedBatchAddsEachDocumentOnItsOwn(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	for _, content := range []string{"the user likes tea", "the user likes coffee"} {
		if _, err := store.LogNote(ctx, content, "fact"); err != nil {
			t.Fatalf("LogNote(%q): %v", content, err)
		}
	}
	store.SetEmbedder(&fakeHybridEmbedder{})
	vidx := &fakeHybridVectorIndex{batchErr: fmt.Errorf("chromem add batch: disk full")}
	store.SetVectorIndex(vidx)

	report, err := store.ReconcileVectors(ctx, 200)
	if err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}
	if report.Backfilled != 2 {
		t.Errorf("Backfilled = %d, want 2 — the retried documents count", report.Backfilled)
	}
	if len(vidx.IDs()) != 2 {
		t.Errorf("index holds %v, want both notes added one at a time after the batch failed", vidx.IDs())
	}
}

// deadEmbedder is the embedding engine after Close: every call fails at once, in memory, with no I/O. That speed is what turned one interrupted sweep into a log storm.
type deadEmbedder struct{ calls int }

func (d *deadEmbedder) Embed(ctx context.Context, task string, text string) ([]float32, error) {
	d.calls++
	return nil, errors.New("embed engine: closed")
}

// A sweep whose embedding engine has shut down under it must stop, not walk the rest of its candidate list one failure at a time.
// The daemon cancels the root context on SIGTERM and closes the embed engine a few steps later, while a sweep started at boot is still running. Because a closed engine refuses in microseconds with no I/O, the loop got through thousands of candidates per second, logging one ERROR each: 17,596 lines of "embed engine: closed" and 12,082 of "context canceled" in three bursts, 29,763 of the log's 29,941 ERROR lines in eight days, all from this one loop. Nothing was lost — the next sweep re-derives what still has no vector — so the only cost was the noise, and the noise buried everything else.
func TestReconcileVectors_StopsWhenTheEmbedEngineHasGone(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	for i := 0; i < 25; i++ {
		if _, err := store.LogNote(ctx, fmt.Sprintf("note number %d", i), "fact"); err != nil {
			t.Fatalf("LogNote: %v", err)
		}
	}
	emb := &deadEmbedder{}
	store.SetEmbedder(emb)
	store.SetVectorIndex(&fakeHybridVectorIndex{})

	if _, err := store.ReconcileVectors(ctx, 100); err == nil {
		t.Error("the sweep reported success while every embed failed")
	}
	if emb.calls != 1 {
		t.Errorf("the sweep called the dead engine %d times, want 1: it must give up on the first refusal, not once per candidate", emb.calls)
	}
}
