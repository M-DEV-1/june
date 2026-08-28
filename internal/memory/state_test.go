package memory_test

import (
	"context"
	"ora/internal/memory"
	"testing"
)

// TestGeminiSummarizer_DeriveState_EmptyInputsShortCircuit checks that DeriveState returns ("", nil) on empty inputs instead of calling the API with nothing to summarize.
func TestGeminiSummarizer_DeriveState_EmptyInputsShortCircuit(t *testing.T) {
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
}
