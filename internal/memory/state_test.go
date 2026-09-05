package memory_test

import (
	"context"
	"errors"
	"fmt"
	"ora/internal/memory"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
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

// TestStateGate_TenMinuteFloor checks that a second derive is refused inside the ten-minute floor even when the material has changed completely.
func TestStateGate_TenMinuteFloor(t *testing.T) {
	var gate memory.StateGate
	start := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)

	if !gate.ShouldDerive(start, "code|main.go", 9) {
		t.Fatal("first derive must be allowed: nothing has been derived yet")
	}
	gate.Derived(start, "code|main.go", 9)

	if gate.ShouldDerive(start.Add(9*time.Minute), "browser|docs", 40) {
		t.Error("derive at +9m allowed, want refused by the ten-minute floor")
	}
	if !gate.ShouldDerive(start.Add(10*time.Minute), "browser|docs", 40) {
		t.Error("derive at +10m refused, want allowed once the floor has passed")
	}
}

// TestStateGate_UnchangedMaterialIsRefused checks that the floor passing is not on its own enough: with the same apps and titles and too few new summaries, the derive is skipped.
func TestStateGate_UnchangedMaterialIsRefused(t *testing.T) {
	var gate memory.StateGate
	start := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	gate.Derived(start, "code|main.go", 100)

	if gate.ShouldDerive(start.Add(30*time.Minute), "code|main.go", 102) {
		t.Error("derive allowed for the same signature and 2 new summaries, want refused")
	}
}

// TestStateGate_ChangeTriggers checks the two things that do earn a derive once the floor has passed: a new app or window title, and enough new summaries under an unchanged signature.
func TestStateGate_ChangeTriggers(t *testing.T) {
	start := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	later := start.Add(11 * time.Minute)

	cases := []struct {
		name       string
		signature  string
		summaries  int
		wantDerive bool
	}{
		{"a new window title derives", "code|other.go", 100, true},
		{"a new app derives", "browser|main.go", 100, true},
		{"enough new summaries derive without a signature change", "code|main.go", 100 + memory.StateSummaryThreshold, true},
		{"one short of the threshold does not derive", "code|main.go", 100 + memory.StateSummaryThreshold - 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gate memory.StateGate
			gate.Derived(start, "code|main.go", 100)
			if got := gate.ShouldDerive(later, tc.signature, tc.summaries); got != tc.wantDerive {
				t.Errorf("ShouldDerive = %v, want %v", got, tc.wantDerive)
			}
		})
	}
}

// TestEpisodeSignature_OrderIndependent checks that the signature names the set of apps and window titles seen, so the same set in a different order is the same signature and does not by itself earn a derive.
func TestEpisodeSignature_OrderIndependent(t *testing.T) {
	a := memory.EpisodeSignature([]string{"code|main.go", "browser|docs"})
	b := memory.EpisodeSignature([]string{"browser|docs", "code|main.go"})
	if a != b {
		t.Errorf("signature depends on order: %q vs %q", a, b)
	}
	if a == memory.EpisodeSignature([]string{"code|main.go"}) {
		t.Error("dropping a window left the signature unchanged, want a different signature")
	}
}

// TestQuotaOrOverload checks the predicate that decides when a background Gemini call is worth handing to another provider: the spent-quota 429 and the everything-overloaded 503, and nothing else.
func TestQuotaOrOverload(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"429 quota exhausted", genai.APIError{Code: 429}, true},
		{"503 overloaded", genai.APIError{Code: 503}, true},
		{"400 bad request", genai.APIError{Code: 400}, false},
		{"500 server error", genai.APIError{Code: 500}, false},
		{"a wrapped 429 is still seen", fmt.Errorf("derive state llm call: %w", genai.APIError{Code: 429}), true},
		{"a plain error", errors.New("no network"), false},
		{"no error at all", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := memory.QuotaOrOverload(tc.err); got != tc.want {
				t.Errorf("QuotaOrOverload(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDeriveState_LocalBackendReplacesTheAPI checks that a summarizer given a local backend answers from it and never reaches the metered API, and that every pending summary arrives in the one prompt rather than one call each.
func TestDeriveState_LocalBackendReplacesTheAPI(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}

	calls := 0
	var seen string
	summarizer.SetStateBackend(func(ctx context.Context, prompt string) (string, error) {
		calls++
		seen = prompt
		return "  you are still on the quota work  ", nil
	})

	state, err := summarizer.DeriveState(context.Background(), []string{"first summary", "second summary", "third summary"}, []string{"a stable fact"})
	if err != nil {
		t.Fatalf("DeriveState: %v", err)
	}
	if calls != 1 {
		t.Errorf("local backend called %d times, want exactly 1 for the whole batch", calls)
	}
	if state != "you are still on the quota work" {
		t.Errorf("state = %q, want the local reply trimmed", state)
	}
	for _, want := range []string{"first summary", "second summary", "third summary", "a stable fact"} {
		if !strings.Contains(seen, want) {
			t.Errorf("prompt is missing %q, want every pending summary combined into the one call", want)
		}
	}
}

// TestDeriveState_EmptyInputsSkipEvenTheLocalCall checks that nothing to summarize still costs no call at all, local or metered.
func TestDeriveState_EmptyInputsSkipEvenTheLocalCall(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	called := false
	summarizer.SetStateBackend(func(ctx context.Context, prompt string) (string, error) {
		called = true
		return "x", nil
	})
	if _, err := summarizer.DeriveState(context.Background(), nil, nil); err != nil {
		t.Fatalf("DeriveState: %v", err)
	}
	if called {
		t.Error("local backend was called with nothing to summarize")
	}
}
