package memory_test

import (
	"context"
	"ora/internal/memory"
	"testing"
	"time"
)

// TestGeminiSummarizer_DeriveState_EmptyInputsShortCircuit checks that DeriveState returns ("", nil) immediately on empty inputs, with no network call — NewGeminiSummarizer doesn't dial out; auth happens lazily on the first real API call.
// The elapsed-time check below catches it if that ever stops being true.
func TestGeminiSummarizer_DeriveState_EmptyInputsShortCircuit(t *testing.T) {
	start := time.Now()

	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}

	state, err := summarizer.DeriveState(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("DeriveState: unexpected error: %v", err)
	}
	if state != "" {
		t.Errorf("expected empty state for empty inputs, got %q", state)
	}

	// A real Gemini call takes ~100s of ms minimum; 50ms leaves headroom above local overhead but is well under any real network round-trip.
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("construction+DeriveState took %v — expected near-instant with no network call; "+
			"NewGeminiSummarizer or DeriveState's guard clause may have started making one", elapsed)
	}
}
