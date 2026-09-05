package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeLedger stands in for the store's token ledger: it answers with whatever rows a test seeded, remembers the windows it was asked for, and can be made to fail.
type fakeLedger struct {
	totals   []TokenTotal
	recent   []TokenUse
	days     []TokenDay
	since    []time.Time
	limit    int
	daysBack int
	err      error
}

func (f *fakeLedger) TokenTotalsSince(_ context.Context, since time.Time) ([]TokenTotal, error) {
	f.since = append(f.since, since)
	if f.err != nil {
		return nil, f.err
	}
	return f.totals, nil
}

func (f *fakeLedger) TokenUseRecent(_ context.Context, limit int) ([]TokenUse, error) {
	f.limit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.recent, nil
}

func (f *fakeLedger) TokenDaysBack(_ context.Context, days int) ([]TokenDay, error) {
	f.daysBack = days
	if f.err != nil {
		return nil, f.err
	}
	return f.days, nil
}

// usageServer wires the /usage handler behind a real HTTP server under the same path cmd/daemon.go gives it.
func usageServer(t *testing.T, ledger TokenLedger) *httptest.Server {
	t.Helper()
	return usageServerWithBudget(t, ledger, nil)
}

// usageServerWithBudget is usageServer with a daily-token-budget lookup wired in, for the tests that check budget_used_fraction. nil behaves exactly like usageServer: no budgets set.
func usageServerWithBudget(t *testing.T, ledger TokenLedger, budgetFor func(provider string) int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/usage", Usage(ledger, budgetFor))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUsage_TotalsPerProviderAndModel(t *testing.T) {
	ledger := &fakeLedger{totals: []TokenTotal{
		{Provider: "gemini", Model: "gemini-3-pro", Calls: 2, InputTokens: 1000, OutputTokens: 200, TotalTokens: 1200},
		{Provider: "gemini", Model: "gemini-3-flash", Calls: 5, InputTokens: 300, OutputTokens: 100, TotalTokens: 400},
		{Provider: "anthropic", Model: "claude-opus-5", Calls: 1, InputTokens: 8000, OutputTokens: 900, TotalTokens: 8900},
	}}
	srv := usageServer(t, ledger)

	var view UsageView
	getUsage(t, srv, &view)

	if len(view.Today.Providers) != 2 {
		t.Fatalf("today providers = %+v, want one row per provider", view.Today.Providers)
	}
	// Biggest spender first, so the row that costs the most is the one at the top.
	first := view.Today.Providers[0]
	if first.Provider != "anthropic" || first.Calls != 1 || first.InputTokens != 8000 || first.OutputTokens != 900 || first.TotalTokens != 8900 {
		t.Errorf("first provider row = %+v, want anthropic's own totals first", first)
	}
	second := view.Today.Providers[1]
	if second.Provider != "gemini" || second.Calls != 7 || second.InputTokens != 1300 || second.OutputTokens != 300 || second.TotalTokens != 1600 {
		t.Errorf("second provider row = %+v, want gemini's two models added up", second)
	}
	if len(view.Today.Models) != 3 || view.Today.Models[0].Model != "claude-opus-5" {
		t.Errorf("today models = %+v, want one row per model, biggest first", view.Today.Models)
	}
	if view.Today.Models[0].Provider != "anthropic" {
		t.Errorf("model row = %+v, want the provider it belongs to named on it", view.Today.Models[0])
	}
	if len(view.Week.Providers) != 2 {
		t.Errorf("week providers = %+v, want the same shape as today", view.Week.Providers)
	}
}

// TestUsage_WindowsAreTodayAndSevenDays checks the two windows the route reads: midnight this morning, and midnight seven days ago counting today as one of them.
func TestUsage_WindowsAreTodayAndSevenDays(t *testing.T) {
	ledger := &fakeLedger{}
	srv := usageServer(t, ledger)

	var view UsageView
	getUsage(t, srv, &view)

	if len(ledger.since) != 2 {
		t.Fatalf("TokenTotalsSince called %d times, want once per window", len(ledger.since))
	}
	today := startOfToday(time.Now())
	if !ledger.since[0].Equal(today) {
		t.Errorf("today window starts at %s, want midnight this morning (%s)", ledger.since[0], today)
	}
	week := today.AddDate(0, 0, -(usageDays - 1))
	if !ledger.since[1].Equal(week) {
		t.Errorf("week window starts at %s, want %s", ledger.since[1], week)
	}
	if ledger.daysBack != usageDays {
		t.Errorf("TokenDaysBack asked for %d days, want %d", ledger.daysBack, usageDays)
	}
	if ledger.limit != usageRecent {
		t.Errorf("TokenUseRecent asked for %d rows, want %d", ledger.limit, usageRecent)
	}
}

// TestUsage_DaySeriesIsSevenColumnsOldestFirst checks that the bar series always has a column per day, zero for a day nothing was spent, with each day's providers added together.
func TestUsage_DaySeriesIsSevenColumnsOldestFirst(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	ledger := &fakeLedger{days: []TokenDay{
		{Day: today, Provider: "gemini", TotalTokens: 400, Calls: 2},
		{Day: today, Provider: "anthropic", TotalTokens: 600, Calls: 1},
		{Day: yesterday, Provider: "gemini", TotalTokens: 50, Calls: 1},
	}}
	srv := usageServer(t, ledger)

	var view UsageView
	getUsage(t, srv, &view)

	if len(view.Days) != usageDays {
		t.Fatalf("days = %+v, want %d columns", view.Days, usageDays)
	}
	if view.Days[0].Day >= view.Days[len(view.Days)-1].Day {
		t.Errorf("days run %s to %s, want oldest first", view.Days[0].Day, view.Days[len(view.Days)-1].Day)
	}
	last := view.Days[len(view.Days)-1]
	if last.Day != today || last.TotalTokens != 1000 || last.Calls != 3 {
		t.Errorf("today's column = %+v, want both providers added together", last)
	}
	if view.Days[len(view.Days)-2].TotalTokens != 50 {
		t.Errorf("yesterday's column = %+v, want the one row it had", view.Days[len(view.Days)-2])
	}
	if view.Days[0].TotalTokens != 0 || view.Days[0].Calls != 0 {
		t.Errorf("a day with nothing spent = %+v, want a zero column rather than a missing one", view.Days[0])
	}
}

// TestUsage_RecentCallsAreTheLog checks the scrolling log: every field the window draws, with the time as RFC3339 and a long question cut to a readable opening.
func TestUsage_RecentCallsAreTheLog(t *testing.T) {
	at := time.Date(2026, 9, 4, 15, 12, 0, 0, time.Local)
	long := "why did the reconciliation job run twice last night and which of the two writes won, and what did that do to the September numbers"
	ledger := &fakeLedger{recent: []TokenUse{
		{ID: 7, Provider: "gemini", Model: "gemini-3-flash", Channel: "ask", InputTokens: 900, OutputTokens: 120, TotalTokens: 1020, DurationMS: 2400, Question: long, At: at},
	}}
	srv := usageServer(t, ledger)

	var view UsageView
	getUsage(t, srv, &view)

	if len(view.Recent) != 1 {
		t.Fatalf("recent = %+v, want the one call the ledger holds", view.Recent)
	}
	call := view.Recent[0]
	if call.ID != 7 || call.Provider != "gemini" || call.Model != "gemini-3-flash" || call.Channel != "ask" {
		t.Errorf("call = %+v, want the ledger's own row", call)
	}
	if call.InputTokens != 900 || call.OutputTokens != 120 || call.TotalTokens != 1020 || call.DurationMS != 2400 {
		t.Errorf("call numbers = %+v, want the ledger's counts and duration", call)
	}
	if call.When != at.Format(time.RFC3339) {
		t.Errorf("when = %q, want RFC3339", call.When)
	}
	if len([]rune(call.Question)) != usageQuestionCap {
		t.Errorf("question is %d runes, want the opening %d of it", len([]rune(call.Question)), usageQuestionCap)
	}
}

// TestUsage_CarriesCachedTokensAndRoundsPerCall checks the two fields a cost-per-question view needs beyond the plain counts: how much of a call's input the provider answered from its own prompt cache, and how many model calls the question took.
func TestUsage_CarriesCachedTokensAndRoundsPerCall(t *testing.T) {
	ledger := &fakeLedger{recent: []TokenUse{
		{ID: 1, Provider: "codex", Model: "gpt-5.5", Channel: "text", InputTokens: 4614, OutputTokens: 80, TotalTokens: 4694, CachedTokens: 3840, Rounds: 3, Question: "click the merge button"},
	}}
	srv := usageServer(t, ledger)

	var view UsageView
	getUsage(t, srv, &view)

	if len(view.Recent) != 1 {
		t.Fatalf("recent = %+v, want 1", view.Recent)
	}
	call := view.Recent[0]
	if call.CachedInputTokens != 3840 {
		t.Errorf("cached input tokens = %d, want 3840", call.CachedInputTokens)
	}
	if call.Rounds != 3 {
		t.Errorf("rounds = %d, want 3", call.Rounds)
	}
}

// TestUsage_TotalsCarryCachedInputTokens checks that a window's provider and model rows sum cached tokens the same way they already sum every other count.
func TestUsage_TotalsCarryCachedInputTokens(t *testing.T) {
	ledger := &fakeLedger{totals: []TokenTotal{
		{Provider: "codex", Model: "gpt-5.5", Calls: 2, InputTokens: 7614, OutputTokens: 100, TotalTokens: 7714, CachedInputTokens: 5340},
	}}
	srv := usageServer(t, ledger)

	var view UsageView
	getUsage(t, srv, &view)

	if len(view.Today.Providers) != 1 || view.Today.Providers[0].CachedInputTokens != 5340 {
		t.Errorf("today providers = %+v, want codex's cached input carried through", view.Today.Providers)
	}
	if len(view.Today.Models) != 1 || view.Today.Models[0].CachedInputTokens != 5340 {
		t.Errorf("today models = %+v, want codex's cached input carried through", view.Today.Models)
	}
}

// TestUsage_BudgetUsedFractionOnTodayOnly checks the field the app warns from: it is today's spend over the provider's daily budget, set only on the Today window (a daily budget measured against the week's total would warn on a number no single day produced), and left at zero for a provider with no budget set.
func TestUsage_BudgetUsedFractionOnTodayOnly(t *testing.T) {
	ledger := &fakeLedger{totals: []TokenTotal{
		{Provider: "codex", Model: "gpt-5.5", Calls: 2, InputTokens: 400000, OutputTokens: 0, TotalTokens: 400000},
		{Provider: "gemini", Model: "gemini-3-flash", Calls: 1, InputTokens: 1000, OutputTokens: 0, TotalTokens: 1000},
	}}
	budgetFor := func(provider string) int {
		if provider == "codex" {
			return 500000
		}
		return 0
	}
	srv := usageServerWithBudget(t, ledger, budgetFor)

	var view UsageView
	getUsage(t, srv, &view)

	var codex, gemini ProviderTotal
	for _, p := range view.Today.Providers {
		switch p.Provider {
		case "codex":
			codex = p
		case "gemini":
			gemini = p
		}
	}
	if got, want := codex.BudgetUsedFraction, 0.8; got != want {
		t.Errorf("codex budget_used_fraction = %v, want %v (400000/500000)", got, want)
	}
	if gemini.BudgetUsedFraction != 0 {
		t.Errorf("gemini budget_used_fraction = %v, want 0: it has no budget set", gemini.BudgetUsedFraction)
	}
	for _, p := range view.Week.Providers {
		if p.BudgetUsedFraction != 0 {
			t.Errorf("week provider %+v carries a budget_used_fraction; it must only ever be set on Today", p)
		}
	}
}

// TestUsage_NilBudgetLookupLeavesFractionZero checks that Usage(ledger, nil) — the shape every route registered before budgets existed — behaves exactly as before: no budget warning, ever.
func TestUsage_NilBudgetLookupLeavesFractionZero(t *testing.T) {
	ledger := &fakeLedger{totals: []TokenTotal{
		{Provider: "codex", Model: "gpt-5.5", Calls: 1, InputTokens: 999999999, OutputTokens: 0, TotalTokens: 999999999},
	}}
	srv := usageServer(t, ledger)

	var view UsageView
	getUsage(t, srv, &view)

	if len(view.Today.Providers) != 1 || view.Today.Providers[0].BudgetUsedFraction != 0 {
		t.Errorf("today providers = %+v, want budget_used_fraction 0 with no budget lookup at all", view.Today.Providers)
	}
}

// TestUsage_EmptyLedgerIsEmptyListsNotNull checks that a machine that has spent nothing yet gets lists the window can draw rather than nulls.
func TestUsage_EmptyLedgerIsEmptyListsNotNull(t *testing.T) {
	srv := usageServer(t, &fakeLedger{})

	body := getUsageRaw(t, srv)
	for _, want := range []string{`"providers":[]`, `"models":[]`, `"recent":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("empty usage body = %s, want it to carry %s", body, want)
		}
	}
	var view UsageView
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Days) != usageDays {
		t.Errorf("days = %+v, want a zero column per day even with nothing spent", view.Days)
	}
}

// TestUsage_LedgerFailureIs500 checks that a store that cannot answer is reported as an error rather than as an empty ledger, which would read as "you have spent nothing".
func TestUsage_LedgerFailureIs500(t *testing.T) {
	srv := usageServer(t, &fakeLedger{err: errors.New("database is locked")})

	resp, err := http.Get(srv.URL + "/usage")
	if err != nil {
		t.Fatalf("GET /usage: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("GET /usage with a broken ledger = %d, want 500", resp.StatusCode)
	}
}

// getUsage performs GET /usage against the test server and decodes the body, failing the test on any status other than 200.
func getUsage(t *testing.T, srv *httptest.Server, out *UsageView) {
	t.Helper()
	if err := json.Unmarshal([]byte(getUsageRaw(t, srv)), out); err != nil {
		t.Fatalf("GET /usage: decode: %v", err)
	}
}

// getUsageRaw performs GET /usage and returns the body as it came, so a test can look at the JSON itself rather than at what decoding made of it.
func getUsageRaw(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/usage")
	if err != nil {
		t.Fatalf("GET /usage: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /usage: status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET /usage: read: %v", err)
	}
	return string(body)
}

// TestUsage_AgainstTheRealStore runs the route against a real store rather than a fake, so the day keys the ledger groups by and the ones the bar series fills in are checked against each other.
func TestUsage_AgainstTheRealStore(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()
	for _, use := range []TokenUse{
		{Provider: "claude", Model: "sonnet", Channel: "text", InputTokens: 1200, OutputTokens: 80, TotalTokens: 1280, DurationMS: 3100, Question: "what did the review land on", At: time.Now()},
		{Provider: "gemini", Model: "gemini-3.6-flash", Channel: "voice", InputTokens: 400, OutputTokens: 60, TotalTokens: 460, DurationMS: 700, Question: "read the last capture", At: time.Now()},
	} {
		if _, err := store.AddTokenUse(ctx, use); err != nil {
			t.Fatalf("AddTokenUse: %v", err)
		}
	}

	srv := usageServer(t, store)
	var view UsageView
	getUsage(t, srv, &view)

	if len(view.Today.Providers) != 2 || view.Today.Providers[0].Provider != "claude" || view.Today.Providers[0].TotalTokens != 1280 {
		t.Errorf("today = %+v, want both providers with claude's 1280 first", view.Today.Providers)
	}
	if len(view.Recent) != 2 || view.Recent[0].Question == "" {
		t.Errorf("recent = %+v, want both calls with their questions", view.Recent)
	}
	today := view.Days[len(view.Days)-1]
	if today.Day != time.Now().Format("2006-01-02") || today.TotalTokens != 1740 || today.Calls != 2 {
		t.Errorf("today's column = %+v, want both calls counted on today's own day", today)
	}
}
