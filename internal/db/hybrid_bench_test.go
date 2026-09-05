package db

import (
	"context"
	"fmt"
	"testing"
)

// benchStore fills a store with n episodes and n notes all mentioning the same three terms, so a lexical search fills its LIMIT and HybridSearchWindow does its per-candidate metadata work on a full pool rather than on one row.
func benchStore(tb testing.TB, n int) *Store {
	tb.Helper()
	store, err := New(":memory:")
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	tb.Cleanup(func() { store.Close() })
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := store.LogEpisode(ctx, fmt.Sprintf("App%d", i%5), fmt.Sprintf("cuda memory toolkit %d", i), fmt.Sprintf("the cuda toolkit ran out of memory again on run %d, a long enough capture that the excerpt cap actually bites into it", i)); err != nil {
			tb.Fatalf("LogEpisode: %v", err)
		}
		if _, err := store.LogNote(ctx, fmt.Sprintf("cuda memory toolkit note %d about running out of memory", i), "fact"); err != nil {
			tb.Fatalf("LogNote: %v", err)
		}
	}
	return store
}

// BenchmarkHybridSearchWindow_Lexical measures the whole lexical-only hybrid path — the one every ask runs — with the vector arm unwired, so what it times is the SQL the fusion path itself issues.
func BenchmarkHybridSearchWindow_Lexical(b *testing.B) {
	store := benchStore(b, 200)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.HybridSearch(ctx, "cuda memory toolkit", "", 10); err != nil {
			b.Fatalf("HybridSearch: %v", err)
		}
	}
}

// BenchmarkDiverseEpisodes measures the MMR path recall_subject runs, whose candidate pool is thirty episodes wide.
func BenchmarkDiverseEpisodes(b *testing.B) {
	store := benchStore(b, 200)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.DiverseEpisodes(ctx, "cuda memory toolkit", 10); err != nil {
			b.Fatalf("DiverseEpisodes: %v", err)
		}
	}
}
