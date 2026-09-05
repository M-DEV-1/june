// usage.go holds GET /usage: what each model provider has cost in tokens — today, over the last seven days, day by day, and call by call. Every number comes from the store's own token ledger; nothing here estimates or prices anything.
package ipc

import (
	"context"
	"net/http"
	"sort"
	"time"

	"ora/internal/db"
	strtrunc "ora/internal/text"
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

// UsageView is GET /usage. Today is since midnight this morning and Week is the last seven days counting today; Days is one column per day over that same week, oldest first, with a zero column for a day nothing was spent on; Recent is the last calls, newest first. No list is ever null — the window renders these shapes directly.
type UsageView struct {
	Today  UsageWindow `json:"today"`
	Week   UsageWindow `json:"week"`
	Days   []UsageDay  `json:"days"`
	Recent []UsageCall `json:"recent"`
}

// Usage builds the GET /usage handler. Input: the store's token ledger, and the daily budget set for a provider (config.OraConfig.DailyTokenBudgetFor; nil is the same as a config with no budgets set). Output: a handler writing UsageView as JSON, or 500 when the ledger cannot answer — an empty answer would read as "you have spent nothing", which is a different thing from "the store is broken".
func Usage(ledger TokenLedger, budgetFor func(provider string) int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		today := startOfToday(time.Now())

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

		writeJSON(w, UsageView{
			Today:  todayWindow,
			Week:   usageWindow(weekTotals),
			Days:   daySeries(days, today),
			Recent: usageCalls(recent),
		})
	}
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
			DurationMS: r.DurationMS, Question: strtrunc.Runes(r.Question, usageQuestionCap),
		})
	}
	return calls
}
