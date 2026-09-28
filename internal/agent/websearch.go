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
	"sync"
	"time"

	"june/internal/db"
)

// exaSearchURL and tavilySearchURL are the two search providers branch() tries, in order: Exa first (20,000 free requests/month, no card), Tavily second (1,000 free/month, no card, a different vendor so the two running dry at once is unlikely).
const (
	exaSearchURL    = "https://api.exa.ai/search"
	tavilySearchURL = "https://api.tavily.com/search"
	tavilyUsageURL  = "https://api.tavily.com/usage"
)

// providerExa and providerTavily name the two search providers on the token ledger and in the usage view, the same way ProviderGemini/ProviderCodex name the model providers.
const (
	providerExa    = "exa"
	providerTavily = "tavily"
)

// exaTextChars bounds how much of each page Exa is asked to extract. Uncapped, a hit is the whole page, and formatSearchResult joins three of them into one tool result that the live voice model then reads aloud from — a careers page's worth of extracted text is both slow to speak over and most of a prompt. Twelve hundred characters is a few paragraphs, which is what a model needs to answer from a page it cannot open.
const exaTextChars = 1200

// searchResult is one hit from either provider, normalized to the same shape so formatSearchResult doesn't need to know which one answered.
//
// URL is the page the hit came from. It is not a nicety: branch() is the only way June reaches the web, so a hit with no URL is a page June can describe and never open. On 2026-09-12 that cost a whole conversation — asked for the job links it had just found, June said it had none, then invented boards.greenhouse.io/emergentlabs/jobs/4011400008, which 404'd in front of the user. Both providers mark url required on every result, so keeping it costs nothing.
type searchResult struct {
	Title   string
	URL     string
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
// One token-ledger row is filed for the call, win or lose (see recordSearchUse) — a failed call still spent against the account's monthly quota, so it belongs on the ledger the same as a successful one.
func exaSearch(ctx context.Context, client *http.Client, url, apiKey, query string) (searchResponse, error) {
	start := time.Now()
	defer func() { recordSearchUse(providerExa, query, time.Since(start)) }()

	// Content options are nested under "contents"; a top-level "text" is not the field Exa reads, and asking that way returns every result with its title and an empty body — which is exactly what all eight of 2026-09-12's branch calls came back as. See https://exa.ai/docs/reference/search.
	body, _ := json.Marshal(map[string]any{
		"query":      query,
		"numResults": 5,
		"contents":   map[string]any{"text": map[string]any{"maxCharacters": exaTextChars}},
	})
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
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return searchResponse{}, fmt.Errorf("exa: decode response: %w", err)
	}
	out := searchResponse{}
	for _, r := range raw.Results {
		out.Results = append(out.Results, searchResult{Title: r.Title, URL: r.URL, Content: r.Text})
	}
	return out, nil
}

// tavilySearch calls Tavily's search endpoint and normalizes its response. include_answer asks Tavily to also synthesize a short answer from its own results, which formatSearchResult prefers when present.
// One token-ledger row is filed for the call, win or lose — see exaSearch's comment on recordSearchUse.
func tavilySearch(ctx context.Context, client *http.Client, url, apiKey, query string) (searchResponse, error) {
	start := time.Now()
	defer func() { recordSearchUse(providerTavily, query, time.Since(start)) }()

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
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return searchResponse{}, fmt.Errorf("tavily: decode response: %w", err)
	}
	out := searchResponse{Answer: raw.Answer}
	for _, r := range raw.Results {
		out.Results = append(out.Results, searchResult{Title: r.Title, URL: r.URL, Content: r.Content})
	}
	return out, nil
}

// formatSearchResult turns a normalized search response into the plain-text string branch() hands back to the live model as the tool's result — the only thing the model ever sees of the search.
// Tavily's synthesized Answer, when present, is the best prose to hand over, so it leads. The top few Results follow either way as "Title — url: content", capped at 3 so a broad query doesn't dump an essay's worth of extracted page text into the conversation. Zero Results and no answer still returns a real sentence, not silence, since branch() is mid-conversation and the model needs something to say.
//
// The answer used to win outright and return on its own, which meant the one path that produced the best prose was also the one that left June with no link. Asked for the job pages it had just described on 2026-09-12, it said it had none and then made one up. The URLs now ride along underneath the answer instead of being replaced by it.
func formatSearchResult(resp searchResponse) string {
	results := resp.Results
	const maxResults = 3
	if len(results) > maxResults {
		results = results[:maxResults]
	}
	if resp.Answer == "" && len(results) == 0 {
		return "the search came back with nothing useful for that"
	}

	var b strings.Builder
	if resp.Answer != "" {
		b.WriteString(resp.Answer)
	}
	for _, r := range results {
		line := searchResultLine(r)
		if line == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(line)
	}
	return b.String()
}

// searchResultLine renders one hit as the model reads it: its title, then its URL where there is one, then what the page said. Input: the hit. Output: the line, or "" when the hit carried nothing at all — a result with no title, no URL and no text is not worth a blank line in the tool result.
// The URL goes next to the title rather than at the end so it stays attached to the page it belongs to when three hits are read in a row, and is left out entirely when absent so the line never trails a separator with nothing after it.
func searchResultLine(r searchResult) string {
	head := strings.TrimSpace(r.Title)
	if url := strings.TrimSpace(r.URL); url != "" {
		if head != "" {
			head += " — " + url
		} else {
			head = url
		}
	}
	content := strings.TrimSpace(r.Content)
	switch {
	case head == "" && content == "":
		return ""
	case head == "":
		return content
	case content == "":
		return head
	}
	return head + ": " + content
}

// searchUsageRecorder is where every Exa/Tavily call's token-ledger row goes, guarded the same way usageRecorder (codex.go) is, because searches run concurrently with whatever sets this up. Nil means nothing is recorded, which is what every test gets unless it calls SetSearchUsageRecorder.
var searchUsageRecorder struct {
	sync.Mutex
	to func(db.TokenUse)
}

// SetSearchUsageRecorder tells this package where to file a token-ledger row for every web search provider call, win or lose, so a search counts against the same cost view as a model call. Input: the recorder, or nil to record nothing. Output: none.
func SetSearchUsageRecorder(record func(db.TokenUse)) {
	searchUsageRecorder.Lock()
	defer searchUsageRecorder.Unlock()
	searchUsageRecorder.to = record
}

// recordSearchUse files one row for one provider call. Input: the provider ("exa" or "tavily"), the query that was asked, and how long the call took. Output: none; a nil recorder (the common case in tests, and any build that never wired one) makes this a no-op. There are no model tokens to count for a search call, so every count is left at zero — see db.AddTokenUse's own comment on why a call with nothing to report is still filed rather than dropped.
func recordSearchUse(provider, query string, duration time.Duration) {
	searchUsageRecorder.Lock()
	record := searchUsageRecorder.to
	searchUsageRecorder.Unlock()
	if record == nil {
		return
	}
	record(db.TokenUse{
		Provider:   provider,
		Model:      "search",
		Channel:    "branch",
		Rounds:     1,
		DurationMS: duration.Milliseconds(),
		Question:   query,
	})
}

// tavilyUsagePoll is the shortest gap between two reads of Tavily's usage endpoint, the same rule claudeUsagePoll holds Claude's reads to.
const tavilyUsagePoll = 10 * time.Minute

// tavilyUsageTimeout bounds one read, so a slow endpoint cannot hold up GET /usage.
const tavilyUsageTimeout = 5 * time.Second

// tavilyUsageResponse is the account object https://api.tavily.com/usage answers with — see the "key" and "account" objects in Tavily's docs. Only the account's monthly plan usage is read: it is the number a free or paid tier actually caps, where the per-key figures are a finer breakdown of the same thing.
type tavilyUsageResponse struct {
	Account struct {
		PlanUsage int `json:"plan_usage"`
		PlanLimit int `json:"plan_limit"`
	} `json:"account"`
}

// tavilyUsage reads the account's monthly plan usage off the endpoint. Input: a context, the HTTP client, the endpoint, and the API key. Output: one UsageLimit window named "monthly", or an error naming what went wrong.
func tavilyUsage(ctx context.Context, client *http.Client, url, apiKey string) (UsageLimit, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return UsageLimit{}, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return UsageLimit{}, fmt.Errorf("tavily usage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return UsageLimit{}, fmt.Errorf("tavily usage: HTTP %d", resp.StatusCode)
	}
	var body tavilyUsageResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&body); err != nil {
		return UsageLimit{}, fmt.Errorf("tavily usage: decode response: %w", err)
	}
	limit := UsageLimit{Window: "monthly", Source: "api.tavily.com/usage account.plan_usage"}
	if body.Account.PlanLimit > 0 {
		limit.UsedFraction = float64(body.Account.PlanUsage) / float64(body.Account.PlanLimit)
	}
	return limit, nil
}

// tavilyUsagePolled is when the usage endpoint was last read, so RefreshTavilyUsage can hold itself to tavilyUsagePoll however often it is called — the same pattern claude.go's claudeUsagePolled follows for the Claude subscription's own usage endpoint.
var tavilyUsagePolled struct {
	sync.Mutex
	at time.Time
}

// RefreshTavilyUsage reads Tavily's usage endpoint and records what it says into the recorder set by SetUsageRecorder (codex.go) under the provider name "tavily" — the same recorder Claude and Codex already fill in, so GET /usage's existing per-provider limits map picks it up with no changes of its own. Reads TAVILY_API_KEY from the environment and does nothing when it is unset. At most once every tavilyUsagePoll however often it is called. Input: a context. Output: none — a failure is logged and the last good reading stands.
func RefreshTavilyUsage(ctx context.Context) {
	key := os.Getenv("TAVILY_API_KEY")
	if key == "" {
		return
	}
	tavilyUsagePolled.Lock()
	if time.Since(tavilyUsagePolled.at) < tavilyUsagePoll {
		tavilyUsagePolled.Unlock()
		return
	}
	tavilyUsagePolled.at = time.Now()
	tavilyUsagePolled.Unlock()

	ctx, cancel := context.WithTimeout(ctx, tavilyUsageTimeout)
	defer cancel()
	limit, err := tavilyUsage(ctx, http.DefaultClient, tavilyUsageURL, key)
	if err != nil {
		slog.Debug("tavily: could not read the account's usage", "error", err)
		return
	}
	recordUsage(providerTavily, []UsageLimit{limit})
}
