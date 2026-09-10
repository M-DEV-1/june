package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// exaSearchURL and tavilySearchURL are the two search providers branch() tries, in order: Exa first (20,000 free requests/month, no card), Tavily second (1,000 free/month, no card, a different vendor so the two running dry at once is unlikely).
const (
	exaSearchURL    = "https://api.exa.ai/search"
	tavilySearchURL = "https://api.tavily.com/search"
)

// searchResult is one hit from either provider, normalized to the same shape so formatSearchResult doesn't need to know which one answered.
type searchResult struct {
	Title   string
	Content string
}

// searchResponse is a normalized search API response: an optional synthesized answer (Tavily offers this on its free tier; Exa's plain search does not) plus the raw hits either way.
type searchResponse struct {
	Answer  string
	Results []searchResult
}

// defaultWebSearch is the production a.webSearch: tries Exa, then Tavily, using whichever of EXA_API_KEY/TAVILY_API_KEY is set in the environment. Returns an error (which branch() falls back to webAsk on) when neither key is configured or both calls fail.
func (a *Agent) defaultWebSearch(ctx context.Context, task string) (string, error) {
	if key := os.Getenv("EXA_API_KEY"); key != "" {
		resp, err := exaSearch(ctx, http.DefaultClient, exaSearchURL, key, task)
		if err == nil {
			return formatSearchResult(resp), nil
		}
		slog.Warn("branch: exa search failed, falling back to tavily", "error", err)
	}
	if key := os.Getenv("TAVILY_API_KEY"); key != "" {
		resp, err := tavilySearch(ctx, http.DefaultClient, tavilySearchURL, key, task)
		if err == nil {
			return formatSearchResult(resp), nil
		}
		return "", fmt.Errorf("tavily: %w", err)
	}
	return "", fmt.Errorf("web search: no search API configured (EXA_API_KEY or TAVILY_API_KEY)")
}

// exaSearch calls Exa's search endpoint and normalizes its response. Exa's plain search tier returns extracted page text per result but no synthesized answer, so Answer is always left empty here.
func exaSearch(ctx context.Context, client *http.Client, url, apiKey, query string) (searchResponse, error) {
	body, _ := json.Marshal(map[string]any{"query": query, "text": true, "numResults": 5})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return searchResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return searchResponse{}, fmt.Errorf("exa: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return searchResponse{}, fmt.Errorf("exa: status %d: %s", resp.StatusCode, string(b))
	}

	var raw struct {
		Results []struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return searchResponse{}, fmt.Errorf("exa: decode response: %w", err)
	}
	out := searchResponse{}
	for _, r := range raw.Results {
		out.Results = append(out.Results, searchResult{Title: r.Title, Content: r.Text})
	}
	return out, nil
}

// tavilySearch calls Tavily's search endpoint and normalizes its response. include_answer asks Tavily to also synthesize a short answer from its own results, which formatSearchResult prefers when present.
func tavilySearch(ctx context.Context, client *http.Client, url, apiKey, query string) (searchResponse, error) {
	body, _ := json.Marshal(map[string]any{"query": query, "include_answer": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return searchResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return searchResponse{}, fmt.Errorf("tavily: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return searchResponse{}, fmt.Errorf("tavily: status %d: %s", resp.StatusCode, string(b))
	}

	var raw struct {
		Answer  string `json:"answer"`
		Results []struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return searchResponse{}, fmt.Errorf("tavily: decode response: %w", err)
	}
	out := searchResponse{Answer: raw.Answer}
	for _, r := range raw.Results {
		out.Results = append(out.Results, searchResult{Title: r.Title, Content: r.Content})
	}
	return out, nil
}

// formatSearchResult turns a normalized search response into the plain-text string branch() hands back to the live model as the tool's result — the only thing the model ever sees of the search.
// Tavily's synthesized Answer, when present, is already the best single sentence to hand over, so it wins outright. Otherwise the top few Results are joined as "Title: content" — enough for the model to synthesize its own answer from, capped at 3 so a broad query doesn't dump an essay's worth of extracted page text into the conversation. Zero Results either way still returns a real sentence, not silence, since branch() is mid-conversation and the model needs something to say.
func formatSearchResult(resp searchResponse) string {
	if resp.Answer != "" {
		return resp.Answer
	}
	if len(resp.Results) == 0 {
		return "the search came back with nothing useful for that"
	}
	results := resp.Results
	const maxResults = 3
	if len(results) > maxResults {
		results = results[:maxResults]
	}
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if r.Title != "" {
			b.WriteString(r.Title)
			b.WriteString(": ")
		}
		b.WriteString(r.Content)
	}
	return b.String()
}
