package agent

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/genai"

	"ora/internal/config"
)

// On 2026-09-04 gemini-3.5-flash-lite answered 503 UNAVAILABLE ("high demand") for a whole evening and every ask through the window died with it. A 503 is the one failure a second model can answer; everything else (a bad key, a bad request, a cancelled context) would fail the same way on any model and must not be retried.
func TestShouldFallBack_OnlyOnUnavailable(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"503":         {genai.APIError{Code: 503, Status: "UNAVAILABLE"}, true},
		"503 pointer": {&genai.APIError{Code: 503}, true},
		"wrapped 503": {fmt.Errorf("ask text: generate (iteration 0): %w", genai.APIError{Code: 503}), true},
		"429":         {genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED"}, false},
		"400":         {genai.APIError{Code: 400}, false},
		"plain":       {errors.New("dial tcp: connection refused"), false},
		"nil":         {nil, false},
	}
	for name, c := range cases {
		if got := shouldFallBack(c.err); got != c.want {
			t.Errorf("%s: shouldFallBack = %v, want %v", name, got, c.want)
		}
	}
	if config.TextFallbackModel == "" || config.TextFallbackModel == config.TextModel {
		t.Errorf("TextFallbackModel = %q, want a model other than TextModel %q", config.TextFallbackModel, config.TextModel)
	}
}
