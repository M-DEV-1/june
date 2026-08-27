package db

// hybrid_test.go lives in package db (white-box), not db_test, because it needs direct access to unexported reciprocalRankFusion/rrfCandidate to test the pure fusion function numerically against the deck's worked example. db_test.go stays in package db_test, untouched.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/memory"
)

func almostEqual(a, b, epsilon float64) bool {
	return math.Abs(a-b) <= epsilon
}

// --- reciprocalRankFusion: pure-function tests against the deck's §02 worked example ---

// deckWorkedExampleLists reproduces docs/ora-memory-deck.html §02's worked example verbatim (k=50): lexical ranks "cuda-fix" > "cuda-toolkit" > "gpu-infra-doc"; vector ranks "gpu-rabbit-hole" > "cuda-fix" > "vllm-tuning".
func deckWorkedExampleLists() (lexical, vector []rrfCandidate) {
	lexical = []rrfCandidate{
		{id: "cuda-fix", content: "CUDA out of memory fix", source: "episode"},
		{id: "cuda-toolkit", content: "cuda toolkit install", source: "episode"},
		{id: "gpu-infra-doc", content: "gpu infra doc", source: "summary"},
	}
	vector = []rrfCandidate{
		{id: "gpu-rabbit-hole", content: "that GPU rabbit hole", source: "episode"},
		{id: "cuda-fix", content: "CUDA out of memory fix", source: "episode"},
		{id: "vllm-tuning", content: "vLLM tuning notes", source: "episode"},
	}
	return
}

// TestReciprocalRankFusion_DeckWorkedExample_ExactScores asserts the exact per-item fused scores from the deck's worked example, within a small float epsilon (not exact equality, since the deck's own numbers are rounded to 5 decimal places).
func TestReciprocalRankFusion_DeckWorkedExample_ExactScores(t *testing.T) {
	lexical, vector := deckWorkedExampleLists()
	result := reciprocalRankFusion(rrfK, lexical, vector)

	scores := make(map[string]float64, len(result))
	for _, c := range result {
		scores[c.id] = c.score
	}

	const epsilon = 0.0001
	if got, want := scores["cuda-fix"], 1.0/51+1.0/52; !almostEqual(got, want, epsilon) {
		t.Errorf(`"cuda-fix" score = %.5f, want ~%.5f (1/51 + 1/52, rank1 lexical + rank2 vector)`, got, want)
	}
	if got, want := scores["gpu-rabbit-hole"], 1.0/51; !almostEqual(got, want, epsilon) {
		t.Errorf(`"gpu-rabbit-hole" score = %.5f, want ~%.5f (1/51, rank1 vector only)`, got, want)
	}
	if got, want := scores["cuda-toolkit"], 1.0/52; !almostEqual(got, want, epsilon) {
		t.Errorf(`"cuda-toolkit" score = %.5f, want ~%.5f (1/52, rank2 lexical only)`, got, want)
	}
}

// TestReciprocalRankFusion_DeckWorkedExample_FinalOrder asserts the exact fused ordering the deck calls out: the consensus item ("cuda-fix", found by both lists) wins outright over items either list ranked higher on its own.
func TestReciprocalRankFusion_DeckWorkedExample_FinalOrder(t *testing.T) {
	lexical, vector := deckWorkedExampleLists()
	result := reciprocalRankFusion(rrfK, lexical, vector)

	var gotOrder []string
	for _, c := range result {
		gotOrder = append(gotOrder, c.id)
	}
	wantOrder := []string{"cuda-fix", "gpu-rabbit-hole", "cuda-toolkit", "gpu-infra-doc", "vllm-tuning"}

	if fmt.Sprint(gotOrder) != fmt.Sprint(wantOrder) {
		t.Errorf("fused order = %v, want %v (the consensus item must win outright; ties broken by id)", gotOrder, wantOrder)
	}
}

// TestReciprocalRankFusion_NoListsInput_ReturnsEmptyNotPanic documents that calling reciprocalRankFusion with zero input lists must return an empty slice, not panic.
func TestReciprocalRankFusion_NoListsInput_ReturnsEmptyNotPanic(t *testing.T) {
	result := reciprocalRankFusion(rrfK)
	if len(result) != 0 {
		t.Errorf("expected empty result for zero input lists, got %d items: %+v", len(result), result)
	}
}

// TestReciprocalRankFusion_LargerKCompressesScores proves rrfK is actually wired into the scoring math (not hardcoded elsewhere): a much larger k should compress the top/bottom score ratio toward 1, compared to a lower k.
func TestReciprocalRankFusion_LargerKCompressesScores(t *testing.T) {
	lexical, vector := deckWorkedExampleLists()

	resultLowK := reciprocalRankFusion(50, lexical, vector)
	resultHighK := reciprocalRankFusion(1000, lexical, vector)

	if len(resultLowK) == 0 || len(resultHighK) == 0 {
		t.Fatalf("expected non-empty results, got low-k=%d high-k=%d", len(resultLowK), len(resultHighK))
	}

	ratioLowK := resultLowK[0].score / resultLowK[len(resultLowK)-1].score
	ratioHighK := resultHighK[0].score / resultHighK[len(resultHighK)-1].score

	if ratioHighK >= ratioLowK {
		t.Errorf("expected top/bottom score ratio to shrink as k grows: ratio(k=50)=%.4f, ratio(k=1000)=%.4f", ratioLowK, ratioHighK)
	}
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

	mu         sync.Mutex
	liveIDs    map[string]bool
	addRecords []addRecord
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
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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

// TestHybridSearch_VectorCandidateBelowSimilarityFloor_Dropped verifies a vector hit with cosine similarity below minVectorSimilarity is dropped before fusion — without a floor, chromem always returns its n nearest neighbors regardless of how weakly related they actually are, so a query about an absent topic still gets padded with barely-related rows.
func TestHybridSearch_VectorCandidateBelowSimilarityFloor_Dropped(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(&fakeHybridVectorIndex{
		results: []Result{
			{ID: "episode:1", Content: "barely related weak match", Similarity: 0.3},
		},
	})

	hits, err := store.HybridSearch(ctx, "something entirely unrelated", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	for _, h := range hits {
		if h.Content == "barely related weak match" {
			t.Errorf("expected the below-floor vector hit to be dropped, got it in: %+v", hits)
		}
	}
}

// TestHybridSearch_MultiTermQuery_DropsLexicalHitsMatchingOnlyOneTerm verifies that for a query with 2+ significant terms, a lexical candidate matching only one of them (buildFTSMatch's OR-of-terms means FTS5 alone would return it) is dropped from fusion — without this floor, a query like "hiking docker" surfaces any row containing EITHER word, even ones about neither topic together.
func TestHybridSearch_MultiTermQuery_DropsLexicalHitsMatchingOnlyOneTerm(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	if _, err := store.LogNote(ctx, "the user likes hiking on weekends", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	if _, err := store.LogNote(ctx, "the user works with docker containers", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	hits, err := store.HybridSearch(ctx, "hiking docker", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected both single-term-overlap notes to be dropped, got: %+v", hits)
	}
}

// TestHybridSearch_MultiTermQuery_KeepsLexicalHitMatchingBothTerms is the positive control for the term-overlap floor above: a note containing both query terms must still surface.
func TestHybridSearch_MultiTermQuery_KeepsLexicalHitMatchingBothTerms(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	if _, err := store.LogNote(ctx, "the user went hiking then worked on docker deployment", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	hits, err := store.HybridSearch(ctx, "hiking docker", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected the both-terms note to survive the floor, got %d hits: %+v", len(hits), hits)
	}
}

// TestHybridSearch_AbsentTopicQuery_ReturnsNoJunkRows is the combined relevance-floor property both minVectorSimilarity and the lexical term-overlap floor exist for: a multi-term query about a topic genuinely absent from the store returns zero hits, not 10 padded rows the model would confabulate an answer from — even though the store has content, and even though a weak vector match and single-term lexical matches both exist for it.
func TestHybridSearch_AbsentTopicQuery_ReturnsNoJunkRows(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	if _, err := store.LogNote(ctx, "the user likes hiking on weekends", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	if _, err := store.LogNote(ctx, "the user works with docker containers", "fact"); err != nil {
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

// TestHybridSearch_LexicalOnly_NoEmbedderConfigured_BaselineNoCrash verifies that a Store with neither SetEmbedder nor SetVectorIndex called degrades gracefully to lexical-only fusion instead of erroring/panicking.
func TestHybridSearch_LexicalOnly_NoEmbedderConfigured_BaselineNoCrash(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	if _, err := store.LogNote(ctx, "the user likes writing golang", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	hits, err := store.HybridSearch(ctx, "golang", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected at least one lexical hit for 'golang'")
	}
	found := false
	for _, h := range hits {
		if h.Content == "the user likes writing golang" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the logged note to surface via lexical-only fusion, got: %+v", hits)
	}
}

// TestHybridSearch_VectorOnlyMatch_SurfacesSemanticHit proves the "vector recall" half of the hybrid property: an item the fake vector index returns (a semantic/paraphrase match) but that shares no keyword with the query, and so FTS5 alone would never find, still appears in HybridSearch's fused results.
func TestHybridSearch_VectorOnlyMatch_SurfacesSemanticHit(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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

// TestHybridSearch_LimitHonored verifies HybridSearch never returns more than `limit` items even with many lexical + vector candidates available.
func TestHybridSearch_LimitHonored(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	for i := 0; i < 5; i++ {
		if _, err := store.LogNote(ctx, fmt.Sprintf("keyword match item number %d", i), "fact"); err != nil {
			t.Fatalf("LogNote: %v", err)
		}
	}
	var vecResults []Result
	for i := 0; i < 5; i++ {
		vecResults = append(vecResults, Result{
			ID:      fmt.Sprintf("episode:%d", i),
			Content: fmt.Sprintf("semantic match item number %d", i),
		})
	}
	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(&fakeHybridVectorIndex{results: vecResults})

	const limit = 3
	hits, err := store.HybridSearch(ctx, "keyword match item", "", limit)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hits) > limit {
		t.Errorf("expected at most %d hits, got %d: %+v", limit, len(hits), hits)
	}
}

// TestHybridSearch_EmptyQuery_ReturnsEmptyNoError documents and asserts the chosen behavior for an empty query: an empty result and no error, mirroring SearchMemory's existing "empty query -> empty result, no error" contract elsewhere in this package.
func TestHybridSearch_EmptyQuery_ReturnsEmptyNoError(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	hits, err := store.HybridSearch(ctx, "", "", 10)
	if err != nil {
		t.Fatalf("expected no error for an empty query, got: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected empty result for an empty query, got: %+v", hits)
	}
}

// --- vector lifecycle: Delete propagation ---

// TestDeleteNote_DeletesVector verifies removing a note also removes its vector entry — without this, delete_note leaves the old content's vector behind, so a semantic query can still surface a "deleted" fact.
func TestDeleteNote_DeletesVector(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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

// backdateEpisode directly rewrites an episode's created_at into the past, so AgeEpisodes/PruneAncientEpisodes tests don't have to race real wall-clock second boundaries with a tiny/negative keepRawFor (SQLite's datetime() modifier also can't take a negative interval — the "-" || secs string concat produces an invalid double-negative for secs < 0).
func backdateEpisode(t *testing.T, store *Store, id int64, age time.Duration) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE episodes SET created_at = datetime('now', '-' || ? || ' seconds') WHERE id = ?`, int64(age.Seconds()), id); err != nil {
		t.Fatalf("backdateEpisode: %v", err)
	}
}

// TestAgeEpisodes_DeletesVectorsForAgedEpisodes verifies clearing an episode's screen_text also deletes its vector — otherwise the aged episode's raw text lives on in the vector index even though the SQL row was thinned specifically to reclaim that content.
func TestAgeEpisodes_DeletesVectorsForAgedEpisodes(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	vidx := &fakeHybridVectorIndex{deletedCalled: make(chan string, 1)}
	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(vidx)

	id, err := store.LogEpisode(ctx, "Code", "main.go", "short capture")
	if err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	backdateEpisode(t, store, id, 24*time.Hour)

	n, err := store.AgeEpisodes(ctx, time.Hour, 1.1) // importanceFloor > any possible score, so this episode always qualifies
	if err != nil {
		t.Fatalf("AgeEpisodes: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 aged row, got %d", n)
	}

	select {
	case got := <-vidx.deletedCalled:
		want := fmt.Sprintf("episode:%d", id)
		if got != want {
			t.Errorf("expected Delete(%q), got Delete(%q)", want, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the aged episode's vector Delete call")
	}
}

// TestPruneAncientEpisodes_DeletesVectorsForPrunedEpisodes verifies deleting an already-thinned episode row also deletes any leftover vector for it.
func TestPruneAncientEpisodes_DeletesVectorsForPrunedEpisodes(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	vidx := &fakeHybridVectorIndex{deletedCalled: make(chan string, 2)}
	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(vidx)

	id, err := store.LogEpisode(ctx, "Code", "main.go", "short capture")
	if err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	backdateEpisode(t, store, id, 400*24*time.Hour)
	if _, err := store.AgeEpisodes(ctx, time.Hour, 1.1); err != nil {
		t.Fatalf("AgeEpisodes (pre-thin): %v", err)
	}
	select {
	case <-vidx.deletedCalled: // drain AgeEpisodes' own delete before exercising PruneAncientEpisodes
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for AgeEpisodes' vector delete")
	}

	n, err := store.PruneAncientEpisodes(ctx, 365*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneAncientEpisodes: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 pruned row, got %d", n)
	}

	select {
	case got := <-vidx.deletedCalled:
		want := fmt.Sprintf("episode:%d", id)
		if got != want {
			t.Errorf("expected Delete(%q), got Delete(%q)", want, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the pruned episode's vector Delete call")
	}
}

// TestReplaceSummariesWithDigest_DeletesVectorsForReplacedSummaries verifies rolling summaries up into a digest also deletes the replaced summaries' vectors — they no longer exist as nodes, so their vectors would otherwise be permanent orphans.
func TestReplaceSummariesWithDigest_DeletesVectorsForReplacedSummaries(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	vidx := &fakeHybridVectorIndex{deletedCalled: make(chan string, 1)}
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

	select {
	case got := <-vidx.deletedCalled:
		want := fmt.Sprintf("summary:%d", summaryID)
		if got != want {
			t.Errorf("expected Delete(%q), got Delete(%q)", want, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the replaced summary's vector Delete call")
	}
}

// TestReplaceAllNotes_DeletesVectorsForOldNotes verifies note consolidation (which renumbers every note) deletes the old notes' vectors — otherwise every note vector becomes an orphan after a single consolidation cycle, since the new rows get new ids.
func TestReplaceAllNotes_DeletesVectorsForOldNotes(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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

// TestReconcileVectors_DeletesVectorForThinnedEpisode verifies a vector for an episode whose screen_text has since been cleared (AgeEpisodes) gets removed even though the episode row itself still exists.
func TestReconcileVectors_DeletesVectorForThinnedEpisode(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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

// TestReconcileVectors_BackfillsMissingNoteVector verifies a note that has no vector entry at all (e.g. from before hybrid search was wired client-side, or a dirty pre-existing store) gets embedded and added.
func TestReconcileVectors_BackfillsMissingNoteVector(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	// Configure the vector index only AFTER LogNote, so LogNote's own async embed never fires — this note is missing a vector purely because ReconcileVectors needs to backfill it, not because of a race with LogNote's own embed goroutine.
	id, err := store.LogNote(ctx, "the user's favorite color is blue", "fact")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}
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
	wantID := fmt.Sprintf("note:%d", id)
	found := false
	for _, got := range vidx.IDs() {
		if got == wantID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %q to be backfilled into the index, got %v", wantID, vidx.IDs())
	}
}

// TestReconcileVectors_BackfillsLiveSummaryNode verifies a summary node that survived (not yet rolled into a digest) but has no vector gets backfilled.
func TestReconcileVectors_BackfillsLiveSummaryNode(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	dayID, err := store.ensureNode(ctx, 0, "day", "2026-08-16")
	if err != nil {
		t.Fatalf("ensureNode(day): %v", err)
	}
	summaryID, err := store.ensureNode(ctx, dayID, "summary", "worked on the compiler")
	if err != nil {
		t.Fatalf("ensureNode(summary): %v", err)
	}

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
	wantID := fmt.Sprintf("summary:%d", summaryID)
	found := false
	for _, got := range vidx.IDs() {
		if got == wantID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %q to be backfilled into the index, got %v", wantID, vidx.IDs())
	}
}

// TestReconcileVectors_BackfillsRecentEpisode_SkipsOldOne verifies only episodes within the recency window get backfilled — an episode outside it is intentionally left without a vector rather than re-embedding stale raw captures indefinitely.
func TestReconcileVectors_BackfillsRecentEpisode_SkipsOldOne(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	recentID, err := store.LogEpisode(ctx, "Code", "main.go", "recent capture")
	if err != nil {
		t.Fatalf("LogEpisode(recent): %v", err)
	}
	oldID, err := store.LogEpisode(ctx, "Code", "old.go", "old capture")
	if err != nil {
		t.Fatalf("LogEpisode(old): %v", err)
	}
	backdateEpisode(t, store, oldID, 30*24*time.Hour) // well past the 10-day recency window

	store.SetEmbedder(&fakeHybridEmbedder{})
	vidx := &fakeHybridVectorIndex{}
	store.SetVectorIndex(vidx)

	report, err := store.ReconcileVectors(ctx, 200)
	if err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}
	if report.Backfilled != 1 {
		t.Errorf("expected 1 backfilled (recent only), got %d", report.Backfilled)
	}
	ids := vidx.IDs()
	wantRecent := fmt.Sprintf("episode:%d", recentID)
	dontWantOld := fmt.Sprintf("episode:%d", oldID)
	foundRecent, foundOld := false, false
	for _, got := range ids {
		if got == wantRecent {
			foundRecent = true
		}
		if got == dontWantOld {
			foundOld = true
		}
	}
	if !foundRecent {
		t.Errorf("expected the recent episode %q to be backfilled, got %v", wantRecent, ids)
	}
	if foundOld {
		t.Errorf("expected the old episode %q NOT to be backfilled, got %v", dontWantOld, ids)
	}
}

// TestReconcileVectors_RespectsEmbedCap verifies the sweep stops backfilling once embedCap is reached, instead of embedding every missing candidate in one pass — protects API quota on a large dirty store.
func TestReconcileVectors_RespectsEmbedCap(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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

// TestReconcileVectors_BackfillsNote_IncludesCreatedAt verifies note backfill sets created_at metadata, matching LogNote's own async embed goroutine.
func TestReconcileVectors_BackfillsNote_IncludesCreatedAt(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	id, err := store.LogNote(ctx, "the user's favorite color is blue", "fact")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	store.SetEmbedder(&fakeHybridEmbedder{})
	vidx := &fakeHybridVectorIndex{}
	store.SetVectorIndex(vidx)

	if _, err := store.ReconcileVectors(ctx, 200); err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}

	rec := vidx.addRecordFor(fmt.Sprintf("note:%d", id))
	if rec == nil {
		t.Fatal("expected an Add call for the note")
	}
	if rec.metadata["created_at"] == "" {
		t.Errorf("expected created_at metadata to be set, got %+v", rec.metadata)
	}
}

// TestReconcileVectors_BackfillsSummary_ExtractsSummaryTextAndFullMetadata verifies summary backfill embeds the TaskSummary.Summary text (nodes.content is the full JSON-marshaled TaskSummary, not the plain summary — LogSemanticNode embeds only summary.Summary) with domain/source/kind/created_at all set, matching LogSemanticNode's own async embed goroutine.
func TestReconcileVectors_BackfillsSummary_ExtractsSummaryTextAndFullMetadata(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	dayID, err := store.ensureNode(ctx, 0, "day", "2026-08-16")
	if err != nil {
		t.Fatalf("ensureNode(day): %v", err)
	}
	summaryID, err := store.ensureNode(ctx, dayID, "summary", `{"same_task":false,"task_name":"debugging session","summary":"fixed the parser edge case"}`)
	if err != nil {
		t.Fatalf("ensureNode(summary): %v", err)
	}
	if _, err := store.db.Exec(`UPDATE nodes SET domain = 'work' WHERE id = ?`, summaryID); err != nil {
		t.Fatalf("set domain: %v", err)
	}

	store.SetEmbedder(&fakeHybridEmbedder{})
	vidx := &fakeHybridVectorIndex{}
	store.SetVectorIndex(vidx)

	if _, err := store.ReconcileVectors(ctx, 200); err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}

	rec := vidx.addRecordFor(fmt.Sprintf("summary:%d", summaryID))
	if rec == nil {
		t.Fatal("expected an Add call for the summary")
	}
	if rec.content != "fixed the parser edge case" {
		t.Errorf("expected the embedded content to be the extracted summary text, got %q", rec.content)
	}
	if rec.metadata["domain"] != "work" {
		t.Errorf("expected domain metadata %q, got %+v", "work", rec.metadata)
	}
	if rec.metadata["source"] != "summary" || rec.metadata["kind"] != string(memory.KindPeriod) || rec.metadata["created_at"] == "" {
		t.Errorf("expected source/kind/created_at metadata matching LogSemanticNode's write path, got %+v", rec.metadata)
	}
}

// TestReconcileVectors_BackfillsEpisode_UsesDocumentTextAndFullMetadata verifies episode backfill embeds the same app/title-framed Document() text LogEpisode embeds (not bare screen_text) with domain/source/kind/created_at all set — a missing domain in particular would silently exclude the backfilled vector from every domain-filtered search (chromem's exact-match where fails on a missing key).
func TestReconcileVectors_BackfillsEpisode_UsesDocumentTextAndFullMetadata(t *testing.T) {
	ctx := context.Background()
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	id, err := store.LogEpisode(ctx, "Code", "auth.go", "debugging the token refresh flow")
	if err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	store.SetEmbedder(&fakeHybridEmbedder{})
	vidx := &fakeHybridVectorIndex{}
	store.SetVectorIndex(vidx)

	if _, err := store.ReconcileVectors(ctx, 200); err != nil {
		t.Fatalf("ReconcileVectors: %v", err)
	}

	rec := vidx.addRecordFor(fmt.Sprintf("episode:%d", id))
	if rec == nil {
		t.Fatal("expected an Add call for the episode")
	}
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
}
