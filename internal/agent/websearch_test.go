package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFormatSearchResult pins the contract formatSearchResult must satisfy regardless of which provider filled the response in: an answer, when present, must reach the model; result content must reach the model when there's no answer to lean on; and a genuinely empty response must still return something non-empty, since branch()'s caller (the live model, mid-conversation) needs a sentence to say, not silence or a panic.
func TestFormatSearchResult(t *testing.T) {
	t.Run("prefers the synthesized answer when present", func(t *testing.T) {
		got := formatSearchResult(searchResponse{
			Answer:  "Rayleigh scattering makes the sky look blue.",
			Results: []searchResult{{Title: "Sky color", Content: "unrelated snippet"}},
		})
		if !strings.Contains(got, "Rayleigh scattering") {
			t.Errorf("formatSearchResult = %q, want it to contain the answer text", got)
		}
	})

	t.Run("falls back to result content when there is no answer", func(t *testing.T) {
		got := formatSearchResult(searchResponse{
			Results: []searchResult{{Title: "OpenAI Navier-Stokes controversy", Content: "Buckmaster filed a complaint alleging his draft proofs were seen."}},
		})
		if !strings.Contains(got, "Buckmaster") {
			t.Errorf("formatSearchResult = %q, want it to contain result content since there is no answer", got)
		}
	})

	t.Run("never returns an empty string, even with nothing to report", func(t *testing.T) {
		got := formatSearchResult(searchResponse{})
		if strings.TrimSpace(got) == "" {
			t.Error("formatSearchResult returned an empty string for a response with no answer and no results — the live model needs something to say")
		}
	})
}

// TestExaSearch_ParsesResultsIntoNormalizedForm verifies exaSearch sends the query and API key correctly and maps Exa's title/text fields into searchResult without needing a real Exa account.
func TestExaSearch_ParsesResultsIntoNormalizedForm(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("x-api-key")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Riemann hypothesis","text":"a conjecture about zeta zeros"}]}`))
	}))
	defer srv.Close()

	resp, err := exaSearch(context.Background(), srv.Client(), srv.URL+"/search", "test-key", "riemann hypothesis")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "test-key" {
		t.Errorf("x-api-key header = %q, want %q", gotAuth, "test-key")
	}
	if gotPath != "/search" {
		t.Errorf("request path = %q, want /search", gotPath)
	}
	if len(resp.Results) != 1 || resp.Results[0].Title != "Riemann hypothesis" || resp.Results[0].Content != "a conjecture about zeta zeros" {
		t.Errorf("exaSearch result = %+v, want one mapped result", resp.Results)
	}
	if resp.Answer != "" {
		t.Errorf("exaSearch Answer = %q, want empty — the plain search endpoint never synthesizes one", resp.Answer)
	}
}

// TestTavilySearch_ParsesAnswerAndResults verifies tavilySearch sends Bearer auth and maps Tavily's answer/content fields into the normalized shape without needing a real Tavily account.
func TestTavilySearch_ParsesAnswerAndResults(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"It's the RTX 3050.","results":[{"title":"GPU","content":"laptop GPU details"}]}`))
	}))
	defer srv.Close()

	resp, err := tavilySearch(context.Background(), srv.Client(), srv.URL, "test-key", "what GPU")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-key")
	}
	if resp.Answer != "It's the RTX 3050." {
		t.Errorf("tavilySearch Answer = %q", resp.Answer)
	}
	if len(resp.Results) != 1 || resp.Results[0].Content != "laptop GPU details" {
		t.Errorf("tavilySearch Results = %+v", resp.Results)
	}
}
