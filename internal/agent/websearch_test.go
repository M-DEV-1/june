package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

// TestTavilyUsage_ParsesAccountPlanUsage checks tavilyUsage sends Bearer auth and reads the account's monthly plan usage into a "monthly" UsageLimit, the shape GET /usage's Limits map already carries for Claude and Codex.
func TestTavilyUsage_ParsesAccountPlanUsage(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":{"usage":150,"limit":1000},"account":{"current_plan":"Bootstrap","plan_usage":500,"plan_limit":15000}}`))
	}))
	defer srv.Close()

	limit, err := tavilyUsage(context.Background(), srv.Client(), srv.URL, "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-key")
	}
	if limit.Window != "monthly" {
		t.Errorf("limit.Window = %q, want %q", limit.Window, "monthly")
	}
	wantFraction := 500.0 / 15000.0
	if limit.UsedFraction != wantFraction {
		t.Errorf("limit.UsedFraction = %v, want %v (500/15000 from account.plan_usage/plan_limit)", limit.UsedFraction, wantFraction)
	}
}

// A result's URL is the one thing branch() has to hand back and never did. Without it June told the user it had no links, then invented one — boards.greenhouse.io/emergentlabs/jobs/4011400008, which 404'd. Both providers mark url as a required field on every result.
func TestSearchProvidersKeepTheResultURL(t *testing.T) {
	exa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Careers at Northwind","url":"https://example.com/careers","text":"We are hiring."}]}`))
	}))
	defer exa.Close()
	got, err := exaSearch(context.Background(), exa.Client(), exa.URL, "k", "northwind careers")
	if err != nil {
		t.Fatalf("exaSearch: %v", err)
	}
	if len(got.Results) != 1 || got.Results[0].URL != "https://example.com/careers" {
		t.Errorf("exaSearch results = %+v, want the url kept", got.Results)
	}

	tav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"They are hiring.","results":[{"title":"Careers","url":"https://example.org/careers","content":"Open roles."}]}`))
	}))
	defer tav.Close()
	got, err = tavilySearch(context.Background(), tav.Client(), tav.URL, "k", "brightpath careers")
	if err != nil {
		t.Fatalf("tavilySearch: %v", err)
	}
	if len(got.Results) != 1 || got.Results[0].URL != "https://example.org/careers" {
		t.Errorf("tavilySearch results = %+v, want the url kept", got.Results)
	}
}
