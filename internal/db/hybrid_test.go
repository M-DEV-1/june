package db

// hybrid_test.go lives in package db (white-box), not db_test, because it needs direct access to unexported reciprocalRankFusion/rrfCandidate to test the pure fusion function numerically against the deck's worked example. db_test.go stays in package db_test, untouched.

import (
	"context"
	"fmt"
	"math"
	"testing"
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

// fakeHybridVectorIndex returns a pre-programmed set of Results for any Search call, honoring n and an exact-match where filter — mirroring chromem-go's own exact-string-match metadata filtering, without needing the real dependency.
type fakeHybridVectorIndex struct {
	results  []Result
	addCalls int
}

func (f *fakeHybridVectorIndex) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	f.addCalls++
	return nil
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

func (f *fakeHybridVectorIndex) Count() int { return len(f.results) }

func metadataMatchesWhere(meta, where map[string]string) bool {
	for k, v := range where {
		if meta[k] != v {
			return false
		}
	}
	return true
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
