// usage.go holds GET /usage: what each model provider has cost in tokens — today, over the last seven days, day by day, and call by call. Every number comes from the store's own token ledger; nothing here estimates or prices anything.
package ipc

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"ora/internal/agent"
	"ora/internal/brain"
	"ora/internal/db"
	"ora/internal/util"
)

// usageDays is how many days the week window and the bar series cover, today counted as one of them.
const usageDays = 7

// usageRecent is how many finished calls the log carries, which is enough to scroll through a working session without handing the window a page it cannot draw.
const usageRecent = 50

// usageQuestionCap trims a logged question to the opening the log has room for.
const usageQuestionCap = 120

// TokenTotal, TokenDay and TokenUse are the store's own token-ledger rows, named here so this route's shapes read on their own. TokenTotal is one model's spend over a window, TokenDay one provider's spend on one local day, and TokenUse one finished call; the definitions live in internal/db/token_use.go.
type (
	TokenTotal = db.TokenTotal
	TokenDay   = db.TokenDay
	TokenUse   = db.TokenUse
)

// TokenLedger is the part of the store this route reads: totals since a moment, the most recent calls, and the per-day series. Input to each call is a context and a window; output is what the ledger holds for it, or the store's error.
type TokenLedger interface {
	TokenTotalsSince(ctx context.Context, since time.Time) ([]TokenTotal, error)
	TokenUseRecent(ctx context.Context, limit int) ([]TokenUse, error)
	TokenDaysBack(ctx context.Context, days int) ([]TokenDay, error)
}

// The store is the ledger this route is written against; this says so at compile time, so a change to internal/db/token_use.go's signatures breaks here rather than in cmd/daemon.go.
var _ TokenLedger = (*db.Store)(nil)

// ProviderTotal is one provider's whole spend over a window, its models added together.
type ProviderTotal struct {
	Provider     string `json:"provider"`
	Calls        int    `json:"calls"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	TotalTokens  int    `json:"total_tokens"`
	// CachedInputTokens is the part of InputTokens the provider answered out of its own prompt cache rather than reading afresh. Zero for a provider that reports none.
	CachedInputTokens int `json:"cached_input_tokens"`
	// BudgetUsedFraction is today's spend against the provider's config.OraConfig.DailyTokenBudget, as a fraction from 0 up (over 1 once the budget is spent), so the app can warn at 80%. Zero when the provider has no budget set, which reads the same as "spent nothing" and so never warns either way. Only ever computed for the Today window: a daily budget measured against the week's total would warn on a number no single day produced.
	BudgetUsedFraction float64 `json:"budget_used_fraction"`
}

// ModelTotal is one model's spend over a window, named with the provider it belongs to.
type ModelTotal struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Calls        int    `json:"calls"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	TotalTokens  int    `json:"total_tokens"`
	// CachedInputTokens is the part of InputTokens the provider answered out of its own prompt cache rather than reading afresh. Zero for a provider that reports none.
	CachedInputTokens int `json:"cached_input_tokens"`
}

// UsageWindow is one stretch of time on GET /usage: the same spend added up two ways, per provider and per model, both biggest first.
type UsageWindow struct {
	Providers []ProviderTotal `json:"providers"`
	Models    []ModelTotal    `json:"models"`
}

// UsageDay is one column of the bar series: everything spent on that local day, every provider added together.
type UsageDay struct {
	Day         string `json:"day"`
	Calls       int    `json:"calls"`
	TotalTokens int    `json:"total_tokens"`
}

// UsageCall is one line of the log: when the call was made, who answered it, what it cost, how long it took, and the opening of the question that drew it.
type UsageCall struct {
	ID           int64  `json:"id"`
	When         string `json:"when"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Channel      string `json:"channel"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	TotalTokens  int    `json:"total_tokens"`
	// CachedInputTokens is the part of InputTokens this one question was answered out of the provider's own prompt cache rather than read afresh; zero for a provider that reports none.
	CachedInputTokens int `json:"cached_input_tokens"`
	// Rounds is how many model calls this one question took.
	Rounds     int    `json:"rounds"`
	DurationMS int64  `json:"duration_ms"`
	Question   string `json:"question"`
}

// ProviderLimits is one brain's allowance windows on GET /usage — the same reading GET /brains carries on its row — so the settings page can draw the bars beside the token spend. LimitsAt is when they were read, RFC 3339.
type ProviderLimits struct {
	Limits   []brain.UsageLimit `json:"limits"`
	LimitsAt string             `json:"limits_at"`
}

// UsageView is GET /usage. Today is since midnight this morning and Week is the last seven days counting today; Days is one column per day over that same week, oldest first, with a zero column for a day nothing was spent on; Recent is the last calls, newest first; Limits is the allowance windows each brain's provider exposes, keyed by brain id and holding only the brains that expose any. No list is ever null — the window renders these shapes directly.
type UsageView struct {
	Today  UsageWindow               `json:"today"`
	Week   UsageWindow               `json:"week"`
	Days   []UsageDay                `json:"days"`
	Recent []UsageCall               `json:"recent"`
	Limits map[string]ProviderLimits `json:"limits"`
}

// Usage builds the GET /usage handler. Input: the store's token ledger, the daily budget set for a provider (config.OraConfig.DailyTokenBudgetFor; nil is the same as a config with no budgets set), the Exa plan's monthly request ceiling (config.OraConfig.ExaMonthlyRequests; 0 means unset), and optionally the same allowance lookup GET /brains uses, so the settings page can draw the providers' own usage bars beside the token spend. Output: a handler writing UsageView as JSON, or 500 when the ledger cannot answer — an empty answer would read as "you have spent nothing", which is a different thing from "the store is broken".
func Usage(ledger TokenLedger, budgetFor func(provider string) int, exaMonthlyRequests int, limitsFor ...BrainLimits) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		today := db.DayStart(time.Now())

		todayTotals, err := ledger.TokenTotalsSince(ctx, today)
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		weekTotals, err := ledger.TokenTotalsSince(ctx, today.AddDate(0, 0, -(usageDays-1)))
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		// monthTotals backs the Exa row of Limits below: Exa's own usage endpoint needs a team-management key Ora does not hold (see internal/agent/websearch.go), so calls-this-month off the same ledger every other row is built from is the only reading there is.
		monthTotals, err := ledger.TokenTotalsSince(ctx, startOfMonth(today))
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		days, err := ledger.TokenDaysBack(ctx, usageDays)
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		recent, err := ledger.TokenUseRecent(ctx, usageRecent)
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}

		todayWindow := usageWindow(todayTotals)
		applyBudgets(todayWindow.Providers, budgetFor)

		limits := providerLimits(ctx, limitsFor)
		if exa, ok := exaProviderLimits(monthTotals, exaMonthlyRequests); ok {
			limits["exa"] = exa
		}

		util.WriteJSON(w, UsageView{
			Today:  todayWindow,
			Week:   usageWindow(weekTotals),
			Days:   daySeries(days, today),
			Recent: usageCalls(recent),
			Limits: limits,
		})
	}
}

// startOfMonth is midnight on the 1st of t's own calendar month, in t's own location — the window exaProviderLimits' "calls this month" is counted over.
func startOfMonth(t time.Time) time.Time {
	y, m, _ := t.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, t.Location())
}

// providerLimits collects each brain's allowance windows for the settings page, plus Tavily's own monthly plan usage under the id "tavily". Tavily is not a brain the picker offers, but its reading rides the same recorder Claude's and Codex's already fill (see internal/agent/websearch.go's RefreshTavilyUsage), so it is read here — throttled to once every ten minutes by RefreshTavilyUsage itself, the same rule Claude's own read holds to — and surfaced through the exact same lookup and map every brain row already uses, with no changes to that lookup itself. Input: the request's context and the optional lookup Usage was given. Output: a map from id to its windows, holding only the ones that have any, and empty rather than null when no lookup was given.
func providerLimits(ctx context.Context, limitsFor []BrainLimits) map[string]ProviderLimits {
	out := map[string]ProviderLimits{}
	if len(limitsFor) == 0 || limitsFor[0] == nil {
		return out
	}
	agent.RefreshTavilyUsage(ctx)
	ids := append(append([]string{}, brainIDs...), "tavily")
	for _, id := range ids {
		if snap, ok := limitsFor[0](ctx, id); ok && len(snap.Limits) > 0 {
			out[id] = ProviderLimits{Limits: snap.Limits, LimitsAt: rfc3339(snap.At)}
		}
	}
	return out
}

// exaProviderLimits builds the exa row for the usage view's Limits map: a local monthly call count against exaMonthlyRequests, since Exa's own usage endpoint needs a team-management key Ora does not hold (see internal/agent/websearch.go). Input: this month's per-provider ledger totals — the same rows AddTokenUse gets one of for every Exa call, see internal/agent/websearch.go's recordSearchUse — and the configured monthly ceiling, 0 meaning unknown. Output: the row and true when Exa has been called at all this month; false, meaning nothing to show, when it has not been called this month at all. UsedFraction is left at zero, which the window reads as "no bar to draw", when the ceiling is unknown; the call count itself is always named in Source so the number is visible either way, and no plan size is ever guessed at.
func exaProviderLimits(monthTotals []TokenTotal, exaMonthlyRequests int) (ProviderLimits, bool) {
	calls := 0
	for _, t := range monthTotals {
		if t.Provider == "exa" {
			calls += t.Calls
		}
	}
	if calls == 0 {
		return ProviderLimits{}, false
	}
	limit := brain.UsageLimit{
		Window: "monthly",
		Source: fmt.Sprintf("%d calls this month (token_use ledger)", calls),
	}
	if exaMonthlyRequests > 0 {
		limit.UsedFraction = float64(calls) / float64(exaMonthlyRequests)
	}
	return ProviderLimits{Limits: []brain.UsageLimit{limit}, LimitsAt: rfc3339(time.Now())}, true
}

// applyBudgets sets BudgetUsedFraction on each of today's provider rows, in place. Input: today's provider totals and the budget lookup Usage was given, nil meaning no budgets are set at all. Output: none; a provider budgetFor reports 0 for is left at the zero value, the same as "spent nothing".
func applyBudgets(providers []ProviderTotal, budgetFor func(provider string) int) {
	if budgetFor == nil {
		return
	}
	for i := range providers {
		if budget := budgetFor(providers[i].Provider); budget > 0 {
			providers[i].BudgetUsedFraction = float64(providers[i].TotalTokens) / float64(budget)
		}
	}
}

// usageWindow adds one window's ledger rows up two ways. Input: the ledger's per-model totals for that window. Output: a row per provider and a row per model, each list ordered by total tokens with the biggest first and ties broken by name, and empty rather than null when nothing was spent.
func usageWindow(totals []TokenTotal) UsageWindow {
	byProvider := map[string]*ProviderTotal{}
	models := make([]ModelTotal, 0, len(totals))
	for _, t := range totals {
		p, ok := byProvider[t.Provider]
		if !ok {
			p = &ProviderTotal{Provider: t.Provider}
			byProvider[t.Provider] = p
		}
		p.Calls += t.Calls
		p.InputTokens += t.InputTokens
		p.OutputTokens += t.OutputTokens
		p.TotalTokens += t.TotalTokens
		p.CachedInputTokens += t.CachedInputTokens
		models = append(models, ModelTotal{
			Provider: t.Provider, Model: t.Model, Calls: t.Calls,
			InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, TotalTokens: t.TotalTokens,
			CachedInputTokens: t.CachedInputTokens,
		})
	}

	providers := make([]ProviderTotal, 0, len(byProvider))
	for _, p := range byProvider {
		providers = append(providers, *p)
	}
	sort.Slice(providers, func(i, j int) bool {
		if providers[i].TotalTokens != providers[j].TotalTokens {
			return providers[i].TotalTokens > providers[j].TotalTokens
		}
		return providers[i].Provider < providers[j].Provider
	})
	sort.Slice(models, func(i, j int) bool {
		if models[i].TotalTokens != models[j].TotalTokens {
			return models[i].TotalTokens > models[j].TotalTokens
		}
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return models[i].Model < models[j].Model
	})
	return UsageWindow{Providers: providers, Models: models}
}

// daySeries turns the ledger's per-day-per-provider rows into one column per day. Input: the ledger's rows and midnight this morning. Output: exactly usageDays columns ending today, oldest first, each day's providers added together, and a zero column for a day the ledger has no row for — the bar chart needs a column per day whether or not anything was spent on it.
func daySeries(rows []TokenDay, today time.Time) []UsageDay {
	sums := map[string]UsageDay{}
	for _, r := range rows {
		d := sums[r.Day]
		d.Calls += r.Calls
		d.TotalTokens += r.TotalTokens
		sums[r.Day] = d
	}
	series := make([]UsageDay, 0, usageDays)
	for i := usageDays - 1; i >= 0; i-- {
		day := today.AddDate(0, 0, -i).Format("2006-01-02")
		d := sums[day]
		d.Day = day
		series = append(series, d)
	}
	return series
}

// usageCalls turns the ledger's recent rows into the window's log. Input: the ledger's rows, newest first. Output: the same rows with the time as RFC3339 and the question cut to its opening, empty rather than null when nothing has been asked.
func usageCalls(rows []TokenUse) []UsageCall {
	calls := make([]UsageCall, 0, len(rows))
	for _, r := range rows {
		calls = append(calls, UsageCall{
			ID: r.ID, When: rfc3339(r.At), Provider: r.Provider, Model: r.Model, Channel: r.Channel,
			InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, TotalTokens: r.TotalTokens,
			CachedInputTokens: r.CachedTokens, Rounds: r.Rounds,
			DurationMS: r.DurationMS, Question: util.Runes(r.Question, usageQuestionCap),
		})
	}
	return calls
}
