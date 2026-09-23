package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ora/internal/db"
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

// TestTavilySearch_RecordsTokenLedgerRowEvenOnFailure checks that a failed Tavily call still files its row: it was a call against the account's quota whether or not it answered, so a row that only exists for the calls that worked would undercount what the quota actually spent.
func TestTavilySearch_RecordsTokenLedgerRowEvenOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var got []db.TokenUse
	SetSearchUsageRecorder(func(u db.TokenUse) { got = append(got, u) })
	defer SetSearchUsageRecorder(nil)

	if _, err := tavilySearch(context.Background(), srv.Client(), srv.URL, "test-key", "what GPU"); err == nil {
		t.Fatal("expected an error from the 429")
	}
	if len(got) != 1 || got[0].Provider != "tavily" || got[0].Question != "what GPU" {
		t.Fatalf("recorded rows = %+v, want one tavily row for the failed call", got)
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

// Exa takes its content options nested under "contents", not as a top-level "text" field. Ora asked the top-level way, so on 2026-09-12 all eight branch calls came back as a title, a colon and nothing at all — the titles decoded and the bodies never arrived.
func TestExaSearch_AsksForTextTheWayExaWants(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	if _, err := exaSearch(context.Background(), srv.Client(), srv.URL, "k", "anything"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	contents, ok := body["contents"].(map[string]any)
	if !ok {
		t.Fatalf("request body = %v, want a nested \"contents\" object", body)
	}
	// Either shape Exa accepts is fine here — a bare true, or an options object. What must not happen is text being asked for outside "contents", which is what returned empty bodies.
	if _, asked := contents["text"]; !asked {
		t.Errorf("contents = %v, want text requested inside it", contents)
	}
	if _, stray := body["text"]; stray {
		t.Errorf("request body = %v, want no top-level \"text\" field; Exa ignores it", body)
	}
}

// A result's URL is the one thing branch() has to hand back and never did. Without it Ora told the user it had no links, then invented one — boards.greenhouse.io/emergentlabs/jobs/4011400008, which 404'd. Both providers mark url as a required field on every result.
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

// The model only ever sees formatSearchResult's string, so a URL kept by the decoder and dropped here is still a URL Ora cannot give the user or hand to open_url.
func TestFormatSearchResult_CarriesTheURLs(t *testing.T) {
	t.Run("beside each result when there is no answer", func(t *testing.T) {
		got := formatSearchResult(searchResponse{Results: []searchResult{
			{Title: "Careers at Northwind", URL: "https://example.com/careers", Content: "We are hiring."},
		}})
		for _, want := range []string{"Careers at Northwind", "https://example.com/careers", "We are hiring."} {
			if !strings.Contains(got, want) {
				t.Errorf("formatSearchResult = %q, want it to contain %q", got, want)
			}
		}
	})

	// Tavily's synthesized answer used to win outright and take the results with it, so the one path that produced the best prose was also the one that left Ora with no link to open.
	t.Run("under the answer, which used to swallow them", func(t *testing.T) {
		got := formatSearchResult(searchResponse{
			Answer:  "Brightpath is hiring remotely.",
			Results: []searchResult{{Title: "Careers", URL: "https://example.org/careers", Content: "Open roles."}},
		})
		if !strings.Contains(got, "Brightpath is hiring remotely.") {
			t.Errorf("formatSearchResult = %q, want the answer kept", got)
		}
		if !strings.Contains(got, "https://example.org/careers") {
			t.Errorf("formatSearchResult = %q, want the url alongside the answer", got)
		}
	})

	// A result with no url at all must not leave a dangling separator in the line.
	t.Run("and says nothing where there is no url", func(t *testing.T) {
		got := formatSearchResult(searchResponse{Results: []searchResult{{Title: "Sky color", Content: "Rayleigh scattering."}}})
		if strings.Contains(got, "http") {
			t.Errorf("formatSearchResult = %q, want no url text", got)
		}
	})
}

// On 2026-09-12 Ora answered every web search twice. The log is unambiguous: branch was called at 16:10:46 and in the same instant Ora said "Let me check for you... Right, it looks like there are about one hundred and forty thousand neurons in that map", the search came back at 16:10:47, and at 16:10:53 Ora said the same sentence again.
//
// The prompt was asking for it. Among the holding lines it offered as examples was "from what I know it's X, but let me check", which is an instruction to answer from memory before the search returns, and it sat two sentences above "Never answer such a question from memory". "Then keep talking" did the rest: the model ran straight from its holding line into a full answer it had invented, and then gave that answer again when the real result landed.
func TestVoicePromptDoesNotInviteAnsweringBeforeTheSearchReturns(t *testing.T) {
	prompt := systemInstructionStable("linux", "amd64", "sh", voiceCommunicationStyle, 20)

	// The example that caused it, and the phrase that let the model carry on past its holding line.
	for _, banned := range []string{"from what I know it's X", "Then keep talking"} {
		if strings.Contains(prompt, banned) {
			t.Errorf("the prompt still carries %q, which is what produced the doubled answer", banned)
		}
	}
	// A holding line is still wanted; it is answering over the top of it that is not.
	if !strings.Contains(prompt, "branch") {
		t.Fatal("the prompt no longer tells the model how to reach the web at all")
	}
	// The first fix for this told the model to go silent after its holding line, which stopped the doubling by stopping the conversation. Wrong half: talking was never the bug, and a search that takes half a minute cannot be spent in silence. What waits for the result is the answer, and nothing else does.
	if strings.Contains(prompt, "stop there") {
		t.Error("the prompt still tells the model to stop talking after its holding line, so a long search is spent in silence")
	}
	for _, want := range []string{"carry on talking", "until the result", "cut in"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not say %q, so the model does not know it may keep the conversation going while the search runs", want)
		}
	}
}
