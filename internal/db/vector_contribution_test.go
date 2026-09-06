package db

// This file tests recordVectorContribution (see hybrid.go), the self-accounting
// fusion counter: whether a vector-arm candidate survived fusion into
// HybridSearch's final top-k, written into the tally table under the
// "vector-queries" / "vector-hits" pseudo-providers.

import (
	"context"
	"testing"
	"time"
)

// tallyRow finds the row for provider in rows, or zero-value+false if absent.
func tallyRow(rows []TallyRow, provider string) (TallyRow, bool) {
	for _, r := range rows {
		if r.Provider == provider {
			return r, true
		}
	}
	return TallyRow{}, false
}

// TestHybridSearch_RecordsVectorContribution_WhenHitSurvives verifies a query where a high-similarity vector-only candidate survives into the final result records one "vector-queries" call and one "vector-hits" call whose total_ms holds the survivor count (1, here).
func TestHybridSearch_RecordsVectorContribution_WhenHitSurvives(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogNote(ctx, "the user likes writing golang", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	store.SetEmbedder(&fakeHybridEmbedder{})
	store.SetVectorIndex(&fakeHybridVectorIndex{results: []Result{
		{ID: "note:999", Content: "a semantically related but lexically unmatched fact", Similarity: 0.9},
	}})

	if _, err := store.HybridSearch(ctx, "golang", "", 10); err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}

	rows, err := store.TallyRowsSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("TallyRowsSince: %v", err)
	}

	queries, ok := tallyRow(rows, "vector-queries")
	if !ok || queries.Calls != 1 {
		t.Errorf("vector-queries row = %+v (found=%v), want calls=1", queries, ok)
	}
	hits, ok := tallyRow(rows, "vector-hits")
	if !ok || hits.Calls != 1 || hits.TotalMs != 1 {
		t.Errorf("vector-hits row = %+v (found=%v), want calls=1 and total_ms(survivor count)=1", hits, ok)
	}
}

// TestHybridSearch_RecordsVectorQuery_NoHitsRow_WhenVectorCandidateFiltered verifies a vector candidate that never clears the similarity floor (so it's dropped before fusion) counts as a query but not a hit — recordVectorContribution only sees the survivors that actually made it into `vector`, not the raw Search results.
func TestHybridSearch_RecordsVectorQuery_NoHitsRow_WhenVectorCandidateFiltered(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.LogNote(ctx, "the user likes writing golang", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	store.SetEmbedder(&fakeHybridEmbedder{})
	// Similarity 0.1 is well below minVectorSimilarity (0.55): HybridSearchWindow drops it before it ever reaches recordVectorContribution's `vector` slice.
	store.SetVectorIndex(&fakeHybridVectorIndex{results: []Result{
		{ID: "note:999", Content: "unrelated noise", Similarity: 0.1},
	}})

	if _, err := store.HybridSearch(ctx, "golang", "", 10); err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}

	rows, err := store.TallyRowsSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("TallyRowsSince: %v", err)
	}
	queries, ok := tallyRow(rows, "vector-queries")
	if !ok || queries.Calls != 1 {
		t.Errorf("vector-queries row = %+v (found=%v), want calls=1", queries, ok)
	}
	if _, ok := tallyRow(rows, "vector-hits"); ok {
		t.Errorf("vector-hits row exists for a candidate that never cleared the similarity floor, want none")
	}
}

// minMaxNormalize is reached only indirectly, through RankedEpisodes, where a wrong scaling shifts result order subtly instead of failing loudly. These cases pin the scaling itself so the arithmetic cannot drift unnoticed.
func TestMinMaxNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		want []float64
	}{
		// The span, not the magnitude, is what sets the scale: dividing by max alone would give [0.333, 0.667, 1] here.
		{"nonzero minimum stretches to the full range", []float64{10, 20, 30}, []float64{0, 0.5, 1}},
		// Negative values must still land inside [0,1], which only holds when the minimum is subtracted first.
		{"negatives normalize into range", []float64{-10, 0, 10}, []float64{0, 0.5, 1}},
		// Documented special case: an all-equal term contributes its full weight rather than collapsing the score to zero.
		{"all equal values normalize to one", []float64{5, 5, 5}, []float64{1, 1, 1}},
		{"single value normalizes to one", []float64{42}, []float64{1}},
		{"empty input returns empty", []float64{}, []float64{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := minMaxNormalize(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("minMaxNormalize(%v) returned %d values, want %d", tc.in, len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("minMaxNormalize(%v)[%d] = %v, want %v", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}
