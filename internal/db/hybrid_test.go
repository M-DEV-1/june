package db

// hybrid_test.go lives in package db (white-box), not db_test, because it needs the store's own handle and unexported helpers (ensureNode, reconcileBackfillCandidates) to set up and check what HybridSearch and ReconcileVectors do.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"june/internal/memory"
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

// TestHybridSearch_EmbedFails_DegradesToLexicalOnly verifies an embed error doesn't fail the whole HybridSearch call — it degrades to lexical-only fusion instead, since query_memory should still work off FTS5 alone.
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

// TestHybridSearch_TwoTermQuery_KeepsHitMatchingOneTerm is the other side of the floor: a two-word question is how people ask about one thing, not a demand that both words appear. Requiring both turned every two-term query into a strict AND, so "june daemon" stopped matching "the daemon crashed at startup" — the floor has to scale with the query, not sit at a flat two terms.
func TestHybridSearch_TwoTermQuery_KeepsHitMatchingOneTerm(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogNote(ctx, "the daemon crashed at startup", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	hits, err := store.HybridSearch(ctx, "june daemon", "", 10)
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
