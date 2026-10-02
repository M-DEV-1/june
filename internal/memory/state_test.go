package memory_test

import (
	"fmt"
	"june/internal/memory"
	"testing"
	"time"

	"google.golang.org/genai"
)

// TestStateGate_ChangeTriggers checks that a derive is refused inside the ten-minute floor even when the material has changed completely, that the floor passing is not on its own enough, and the two things that do earn a derive once it has passed: a new app or window title, and enough new summaries under an unchanged signature.
func TestStateGate_ChangeTriggers(t *testing.T) {
	start := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		after      time.Duration
		signature  string
		summaries  int
		wantDerive bool
	}{
		{"everything changed but the floor has not passed", 9 * time.Minute, "browser|docs", 140, false},
		{"the same signature and too few new summaries does not derive", 11 * time.Minute, "code|main.go", 102, false},
		{"a new window title derives", 11 * time.Minute, "code|other.go", 100, true},
		{"enough new summaries derive without a signature change", 11 * time.Minute, "code|main.go", 100 + memory.StateSummaryThreshold, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gate memory.StateGate
			if !gate.ShouldDerive(start, "code|main.go", 100) {
				t.Fatal("first derive must be allowed: nothing has been derived yet")
			}
			gate.Derived(start, "code|main.go", 100)
			if got := gate.ShouldDerive(start.Add(tc.after), tc.signature, tc.summaries); got != tc.wantDerive {
				t.Errorf("ShouldDerive = %v, want %v", got, tc.wantDerive)
			}
		})
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
		{"500 server error", genai.APIError{Code: 500}, false},
		{"a wrapped 429 is still seen", fmt.Errorf("derive state llm call: %w", genai.APIError{Code: 429}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := memory.QuotaOrOverload(tc.err); got != tc.want {
				t.Errorf("QuotaOrOverload(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
