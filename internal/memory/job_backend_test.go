package memory_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"june/internal/config"
	"june/internal/memory"
)

// newSummarizer builds a summarizer with a key that can never reach the network, so any duty that is not routed to a backend fails rather than quietly calling Gemini during a test.
func newSummarizer(t *testing.T) *memory.GeminiSummarizer {
	t.Helper()
	s, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	return s
}

// A backend installed for one duty answers that duty and no other. This is the whole point of the per-job seam: the working state can run on the local llama-server while note consolidation still goes to Gemini, and later either can be pointed at a CLI login instead.
func TestSetJobBackend_AnswersOnlyItsOwnJob(t *testing.T) {
	s := newSummarizer(t)
	var asked string
	s.SetJobBackend(config.JobWorkingState, func(ctx context.Context, prompt string) (string, error) {
		asked = prompt
		return "the state, from the backend", nil
	})

	state, err := s.DeriveState(context.Background(), []string{"wrote the asker"}, []string{"prefers Linux"})
	if err != nil {
		t.Fatalf("DeriveState: %v", err)
	}
	if state != "the state, from the backend" {
		t.Errorf("DeriveState returned %q, want the backend's answer", state)
	}
	if !strings.Contains(asked, "wrote the asker") {
		t.Errorf("the backend was handed a prompt without the material in it: %q", asked)
	}

	// Nothing was installed for note consolidation, so it still takes the Gemini path — which with this key cannot succeed, and that failure is the proof it was not routed to the working-state backend.
	if _, err := s.ConsolidateNotes(context.Background(), []string{"one", "two"}); err == nil {
		t.Error("note consolidation answered with no backend of its own, so it was routed to the wrong job's backend")
	}
}

// A backend's own failure is returned rather than silently falling through to the metered API, or a duty deliberately moved off Gemini would spend the free tier the move was meant to protect.
func TestSetJobBackend_ReportsTheBackendsOwnFailure(t *testing.T) {
	s := newSummarizer(t)
	s.SetJobBackend(config.JobWorkingState, func(ctx context.Context, prompt string) (string, error) {
		return "", errors.New("the CLI is not logged in")
	})

	_, err := s.DeriveState(context.Background(), []string{"wrote the asker"}, nil)
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("DeriveState error = %v, want the backend's own reason", err)
	}
}
