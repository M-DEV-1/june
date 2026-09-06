// This file tests the token ledger: one row per model call, filed with the provider and model it went to, and the three reads over it — the per-provider totals, the live log, and the per-day bars.
package db

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// addUse files one call and fails the test if the store refuses it. Input: the store, the row. Output: the new row's id.
func addUse(t *testing.T, store *Store, use TokenUse) int64 {
	t.Helper()
	id, err := store.AddTokenUse(context.Background(), use)
	if err != nil {
		t.Fatalf("AddTokenUse(%+v): %v", use, err)
	}
	if id == 0 {
		t.Fatalf("AddTokenUse(%+v) returned id 0", use)
	}
	return id
}

// TestAddTokenUse covers what AddTokenUse does to a row on the way in and back out, one property per subtest: every field round-trips as given, including an explicit time; the cache/rounds fields the cost view needs beyond the plain counts round-trip too; a call filed with no time is stamped now and a question over 200 runes is cut to 200 without splitting a character; a call that reported no usage is still stored, counted, and zeroed rather than dropped; and a call reporting only the input/output halves gets the total filed as their sum.
func TestAddTokenUse(t *testing.T) {
	t.Run("round trips every field, including an explicit time", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()

		at := time.Now().Add(-90 * time.Second).Truncate(time.Second)
		addUse(t, store, TokenUse{
			Provider:     "gemini",
			Model:        "gemini-3-flash",
			Channel:      "voice",
			InputTokens:  1200,
			OutputTokens: 340,
			TotalTokens:  1540,
			DurationMS:   2750,
			Question:     "what did priya ask about",
			At:           at,
		})

		got, err := store.TokenUseRecent(ctx, 10)
		if err != nil {
			t.Fatalf("TokenUseRecent: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("TokenUseRecent returned %d rows, want 1", len(got))
		}
		u := got[0]
		if u.Provider != "gemini" || u.Model != "gemini-3-flash" || u.Channel != "voice" {
			t.Errorf("provider/model/channel came back %q/%q/%q", u.Provider, u.Model, u.Channel)
		}
		if u.InputTokens != 1200 || u.OutputTokens != 340 || u.TotalTokens != 1540 {
			t.Errorf("counts came back %d in, %d out, %d total", u.InputTokens, u.OutputTokens, u.TotalTokens)
		}
		if u.DurationMS != 2750 {
			t.Errorf("duration came back %d ms, want 2750", u.DurationMS)
		}
		if u.Question != "what did priya ask about" {
			t.Errorf("question came back %q", u.Question)
		}
		if !u.At.Equal(at) {
			t.Errorf("time came back %v, want the instant %v that went in", u.At.UTC(), at.UTC())
		}
	})

	t.Run("round trips cached tokens and rounds", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()

		addUse(t, store, TokenUse{
			Provider: "codex", Model: "gpt-5.5", Channel: "text",
			InputTokens: 4614, OutputTokens: 80, TotalTokens: 4694,
			CachedTokens: 3840, Rounds: 3,
			Question: "click the merge button",
		})

		got, err := store.TokenUseRecent(ctx, 1)
		if err != nil {
			t.Fatalf("TokenUseRecent: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("TokenUseRecent returned %d rows, want 1", len(got))
		}
		if got[0].CachedTokens != 3840 {
			t.Errorf("cached tokens came back %d, want 3840", got[0].CachedTokens)
		}
		if got[0].Rounds != 3 {
			t.Errorf("rounds came back %d, want 3", got[0].Rounds)
		}
	})

	t.Run("fills in the clock and caps the question", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()

		before := time.Now()
		long := strings.Repeat("प", 500) // 500 runes, 1500 bytes: a byte cut would return mojibake and a wrong count
		addUse(t, store, TokenUse{Provider: "claude", Model: "opus-5", Channel: "text", InputTokens: 10, OutputTokens: 2, TotalTokens: 12, Question: long})

		got, err := store.TokenUseRecent(ctx, 1)
		if err != nil {
			t.Fatalf("TokenUseRecent: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("TokenUseRecent returned %d rows, want 1", len(got))
		}
		if n := len([]rune(got[0].Question)); n != 200 {
			t.Errorf("stored question is %d runes, want it cut to 200", n)
		}
		if got[0].Question != strings.Repeat("प", 200) {
			t.Errorf("stored question is not the first 200 runes of what was asked")
		}
		// Two seconds either way covers the whole-second truncation the timestamp column uses; anything larger is a zone read the wrong way round.
		if off := got[0].At.Sub(before); off < -2*time.Second || off > 2*time.Second {
			t.Errorf("unstamped call filed at %v, %v away from when it was written", got[0].At, off)
		}
	})

	t.Run("records a call that reported no usage", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()

		addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", Question: "what is on my screen"})

		recent, err := store.TokenUseRecent(ctx, 10)
		if err != nil {
			t.Fatalf("TokenUseRecent: %v", err)
		}
		if len(recent) != 1 {
			t.Fatalf("a call that reported no usage left %d rows, want 1", len(recent))
		}
		if recent[0].InputTokens != 0 || recent[0].OutputTokens != 0 || recent[0].TotalTokens != 0 {
			t.Errorf("a call that reported no usage came back with %+v, want zeroes", recent[0])
		}

		totals, err := store.TokenTotalsSince(ctx, time.Time{})
		if err != nil {
			t.Fatalf("TokenTotalsSince: %v", err)
		}
		if len(totals) != 1 || totals[0].Calls != 1 || totals[0].TotalTokens != 0 {
			t.Errorf("totals over one zero-usage call = %+v, want one row, one call, zero tokens", totals)
		}
	})

	t.Run("sums the total when the provider only reported the halves", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()

		addUse(t, store, TokenUse{Provider: "ollama", Model: "qwen3", Channel: "text", InputTokens: 100, OutputTokens: 20})

		got, err := store.TokenUseRecent(ctx, 1)
		if err != nil {
			t.Fatalf("TokenUseRecent: %v", err)
		}
		if len(got) != 1 || got[0].TotalTokens != 120 {
			t.Errorf("total came back %+v, want 120 summed from the halves", got)
		}
	})
}

// TestTokenTotalsSinceSumsCachedInputTokens checks that a window's cached-token count is the sum over its calls, the same way every other count on TokenTotal already is.
func TestTokenTotalsSinceSumsCachedInputTokens(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", InputTokens: 4614, OutputTokens: 80, TotalTokens: 4694, CachedTokens: 3840})
	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", InputTokens: 3000, OutputTokens: 40, TotalTokens: 3040, CachedTokens: 1500})
	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3-flash", Channel: "voice", InputTokens: 500, OutputTokens: 100, TotalTokens: 600})

	got, err := store.TokenTotalsSince(ctx, time.Time{})
	if err != nil {
		t.Fatalf("TokenTotalsSince: %v", err)
	}
	var codex, gemini TokenTotal
	for _, row := range got {
		switch row.Provider {
		case "codex":
			codex = row
		case "gemini":
			gemini = row
		}
	}
	if codex.CachedInputTokens != 5340 {
		t.Errorf("codex cached input = %d, want 3840+1500=5340", codex.CachedInputTokens)
	}
	if gemini.CachedInputTokens != 0 {
		t.Errorf("gemini cached input = %d, want 0: it reports no cache", gemini.CachedInputTokens)
	}
}

// TestTokenTotalsSinceGroupsByProviderAndModel is the question the feature exists to answer: what has each provider and model cost over a window. Two calls to the same model are one row with two calls; two models under one provider stay apart; the heaviest row comes first.
func TestTokenTotalsSinceGroupsByProviderAndModel(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	for _, u := range []TokenUse{
		{Provider: "codex", Model: "gpt-5.5", Channel: "text", InputTokens: 100, OutputTokens: 20, TotalTokens: 120, At: now.Add(-50 * time.Minute)},
		{Provider: "codex", Model: "gpt-5.5", Channel: "subtask", InputTokens: 200, OutputTokens: 30, TotalTokens: 230, At: now.Add(-40 * time.Minute)},
		{Provider: "codex", Model: "gpt-5.5-mini", Channel: "text", InputTokens: 10, OutputTokens: 5, TotalTokens: 15, At: now.Add(-30 * time.Minute)},
		{Provider: "gemini", Model: "gemini-3-flash", Channel: "voice", InputTokens: 500, OutputTokens: 100, TotalTokens: 600, At: now.Add(-20 * time.Minute)},
		{Provider: "claude", Model: "opus-5", Channel: "dream", InputTokens: 50, OutputTokens: 10, TotalTokens: 60, At: now.Add(-10 * time.Minute)},
	} {
		addUse(t, store, u)
	}

	got, err := store.TokenTotalsSince(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("TokenTotalsSince: %v", err)
	}
	want := []TokenTotal{
		{Provider: "gemini", Model: "gemini-3-flash", Calls: 1, InputTokens: 500, OutputTokens: 100, TotalTokens: 600},
		{Provider: "codex", Model: "gpt-5.5", Calls: 2, InputTokens: 300, OutputTokens: 50, TotalTokens: 350},
		{Provider: "claude", Model: "opus-5", Calls: 1, InputTokens: 50, OutputTokens: 10, TotalTokens: 60},
		{Provider: "codex", Model: "gpt-5.5-mini", Calls: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
	if len(got) != len(want) {
		t.Fatalf("TokenTotalsSince returned %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestTokenTotalsSinceWindowEdges pins which side of the window a call on the boundary falls. A call made at exactly the since instant is inside the window; one a second earlier is outside it. Asserting only that an hour-old call is counted would pass just as well with the comparison the wrong way round.
func TestTokenTotalsSinceWindowEdges(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	since := time.Now().Add(-time.Hour).Truncate(time.Second)
	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", InputTokens: 1, OutputTokens: 0, TotalTokens: 1, At: since.Add(-time.Second)})
	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", InputTokens: 10, OutputTokens: 0, TotalTokens: 10, At: since})
	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", InputTokens: 100, OutputTokens: 0, TotalTokens: 100, At: since.Add(time.Second)})

	got, err := store.TokenTotalsSince(ctx, since)
	if err != nil {
		t.Fatalf("TokenTotalsSince: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("TokenTotalsSince returned %d rows, want 1: %+v", len(got), got)
	}
	// 110, not 111: the call one second before the window is out. Not 100: the call exactly on the boundary is in.
	if got[0].Calls != 2 || got[0].TotalTokens != 110 {
		t.Errorf("window [since, now] holds %d calls and %d tokens, want 2 and 110", got[0].Calls, got[0].TotalTokens)
	}

	all, err := store.TokenTotalsSince(ctx, time.Time{})
	if err != nil {
		t.Fatalf("TokenTotalsSince(zero): %v", err)
	}
	if len(all) != 1 || all[0].Calls != 3 || all[0].TotalTokens != 111 {
		t.Errorf("a zero since must reach every call, got %+v", all)
	}
}

// TestTokenUseRecentOrderAndLimit checks the live log view: newest call first, no more than the limit asked for, and two calls filed in the same second still come back newest-written first rather than in an order the store picked at random.
func TestTokenUseRecentOrderAndLimit(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", Question: "oldest", At: now.Add(-3 * time.Minute)})
	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3-flash", Channel: "text", Question: "middle", At: now.Add(-2 * time.Minute)})
	addUse(t, store, TokenUse{Provider: "claude", Model: "opus-5", Channel: "text", Question: "newest", At: now.Add(-time.Minute)})

	got, err := store.TokenUseRecent(ctx, 10)
	if err != nil {
		t.Fatalf("TokenUseRecent: %v", err)
	}
	want := []string{"newest", "middle", "oldest"}
	if len(got) != len(want) {
		t.Fatalf("TokenUseRecent returned %d rows, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Question != w {
			t.Errorf("row %d = %q, want %q", i, got[i].Question, w)
		}
	}

	capped, err := store.TokenUseRecent(ctx, 2)
	if err != nil {
		t.Fatalf("TokenUseRecent(2): %v", err)
	}
	if len(capped) != 2 || capped[0].Question != "newest" || capped[1].Question != "middle" {
		t.Errorf("a limit of 2 returned %+v, want the two newest", capped)
	}

	for _, limit := range []int{0, -5} {
		none, err := store.TokenUseRecent(ctx, limit)
		if err != nil {
			t.Fatalf("TokenUseRecent(%d): %v", limit, err)
		}
		if len(none) != 0 {
			t.Errorf("TokenUseRecent(%d) returned %d rows, want none", limit, len(none))
		}
	}

	// Two calls in the same second: the tie is broken by which was written last, so a live log never reorders under a refresh.
	same := now.Add(-4 * time.Minute)
	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", Question: "tie first", At: same})
	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", Question: "tie second", At: same})
	tied, err := store.TokenUseRecent(ctx, 10)
	if err != nil {
		t.Fatalf("TokenUseRecent after ties: %v", err)
	}
	if len(tied) != 5 || tied[3].Question != "tie second" || tied[4].Question != "tie first" {
		t.Errorf("same-second calls came back as %+v, want the later write ahead of the earlier", tied)
	}
}

// TestTokenDaysBackGroupsByLocalDay is the per-day bar chart: two calls a minute either side of the user's own midnight belong to different days, and the days come back oldest first. Grouping on the stored UTC text instead would put both on whichever side of midnight UTC happened to fall.
func TestTokenDaysBackGroupsByLocalDay(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	today := midnight.Format("2006-01-02")
	yesterday := midnight.AddDate(0, 0, -1).Format("2006-01-02")

	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", InputTokens: 30, OutputTokens: 10, TotalTokens: 40, At: midnight.Add(-30 * time.Minute)})
	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", InputTokens: 5, OutputTokens: 5, TotalTokens: 10, At: midnight.Add(30 * time.Minute)})
	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3-flash", Channel: "voice", InputTokens: 200, OutputTokens: 100, TotalTokens: 300, At: midnight.Add(time.Hour)})
	// Well outside a two-day window: it must not be counted at all.
	addUse(t, store, TokenUse{Provider: "claude", Model: "opus-5", Channel: "text", InputTokens: 900, OutputTokens: 100, TotalTokens: 1000, At: midnight.AddDate(0, 0, -5)})

	got, err := store.TokenDaysBack(ctx, 2)
	if err != nil {
		t.Fatalf("TokenDaysBack: %v", err)
	}
	want := []TokenDay{
		{Day: yesterday, Provider: "codex", TotalTokens: 40, Calls: 1},
		{Day: today, Provider: "codex", TotalTokens: 10, Calls: 1},
		{Day: today, Provider: "gemini", TotalTokens: 300, Calls: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("TokenDaysBack(2) returned %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A wider window reaches the five-day-old call; the days still arrive oldest first.
	wide, err := store.TokenDaysBack(ctx, 7)
	if err != nil {
		t.Fatalf("TokenDaysBack(7): %v", err)
	}
	if len(wide) != 4 {
		t.Fatalf("TokenDaysBack(7) returned %d rows, want 4: %+v", len(wide), wide)
	}
	if wide[0].Provider != "claude" || wide[0].TotalTokens != 1000 {
		t.Errorf("the oldest day came back as %+v, want the claude call five days ago", wide[0])
	}
	for i := 1; i < len(wide); i++ {
		if wide[i-1].Day > wide[i].Day {
			t.Errorf("day %q came before %q, want oldest first", wide[i-1].Day, wide[i].Day)
		}
	}

	for _, days := range []int{0, -3} {
		none, err := store.TokenDaysBack(ctx, days)
		if err != nil {
			t.Fatalf("TokenDaysBack(%d): %v", days, err)
		}
		if len(none) != 0 {
			t.Errorf("TokenDaysBack(%d) returned %d rows, want none", days, len(none))
		}
	}
}

// TestTokenDaysBackCountsOneDayAsToday checks the smallest window the caller can ask for: one day is today alone, so yesterday's calls are out.
func TestTokenDaysBackCountsOneDayAsToday(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", InputTokens: 7, OutputTokens: 3, TotalTokens: 10, At: midnight.Add(-time.Minute)})
	addUse(t, store, TokenUse{Provider: "codex", Model: "gpt-5.5", Channel: "text", InputTokens: 1, OutputTokens: 1, TotalTokens: 2, At: midnight.Add(time.Minute)})

	got, err := store.TokenDaysBack(ctx, 1)
	if err != nil {
		t.Fatalf("TokenDaysBack(1): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("TokenDaysBack(1) returned %d rows, want 1: %+v", len(got), got)
	}
	if got[0].Day != midnight.Format("2006-01-02") || got[0].TotalTokens != 2 {
		t.Errorf("TokenDaysBack(1) = %+v, want today alone with 2 tokens", got[0])
	}
}

// TestModelCallsBackNonPositiveDaysReturnsNil checks the same guard TokenDaysBack has: a window of zero or fewer days returns nil and no error rather than one running backwards.
func TestModelCallsBackNonPositiveDaysReturnsNil(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	for _, days := range []int{0, -3} {
		got, err := store.ModelCallsBack(ctx, days)
		if err != nil {
			t.Fatalf("ModelCallsBack(%d): %v", days, err)
		}
		if got != nil {
			t.Errorf("ModelCallsBack(%d) = %+v, want nil", days, got)
		}
	}
}

// TestModelCallsBackSeparatesModelsUnderOneProvider is the question the feature exists to answer: which model spent the daily allowance. Two models under the same provider on the same day must come back as two rows, each with its own call count and token total.
func TestModelCallsBackSeparatesModelsUnderOneProvider(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3.5-flash-lite", Channel: "text", InputTokens: 10, OutputTokens: 5, TotalTokens: 15, At: midnight.Add(time.Hour)})
	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3.5-flash-lite", Channel: "text", InputTokens: 20, OutputTokens: 5, TotalTokens: 25, At: midnight.Add(2 * time.Hour)})
	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3.5-flash", Channel: "text", InputTokens: 100, OutputTokens: 50, TotalTokens: 150, At: midnight.Add(3 * time.Hour)})

	got, err := store.ModelCallsBack(ctx, 1)
	if err != nil {
		t.Fatalf("ModelCallsBack(1): %v", err)
	}
	today := midnight.Format("2006-01-02")
	want := []ModelDay{
		{Day: today, Provider: "gemini", Model: "gemini-3.5-flash", Calls: 1, TotalTokens: 150},
		{Day: today, Provider: "gemini", Model: "gemini-3.5-flash-lite", Calls: 2, TotalTokens: 40},
	}
	if len(got) != len(want) {
		t.Fatalf("ModelCallsBack(1) returned %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestModelCallsBackGroupsByLocalDayAndExcludesOutsideWindow checks the local-calendar-day grouping TokenDaysBack already relies on, and that a call well outside the window is not counted at all. Two calls the same local day but different UTC-adjacent clock times land in one row; a call five days back is excluded from a two-day window.
func TestModelCallsBackGroupsByLocalDayAndExcludesOutsideWindow(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	today := midnight.Format("2006-01-02")

	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3.5-flash", Channel: "text", InputTokens: 1, OutputTokens: 1, TotalTokens: 2, At: midnight.Add(time.Hour)})
	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3.5-flash", Channel: "text", InputTokens: 3, OutputTokens: 1, TotalTokens: 4, At: midnight.Add(23 * time.Hour)})
	// Five days back: outside a two-day window, must not be counted.
	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3.5-flash", Channel: "text", InputTokens: 900, OutputTokens: 100, TotalTokens: 1000, At: midnight.AddDate(0, 0, -5)})

	got, err := store.ModelCallsBack(ctx, 2)
	if err != nil {
		t.Fatalf("ModelCallsBack(2): %v", err)
	}
	want := []ModelDay{
		{Day: today, Provider: "gemini", Model: "gemini-3.5-flash", Calls: 2, TotalTokens: 6},
	}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("ModelCallsBack(2) = %+v, want %+v", got, want)
	}
}

// TestModelCallsBackOrdersOldestDayFirst checks the order ModelCallsBack promises across a multi-day window: oldest day first, then provider, then model, so the order never shifts between reads.
func TestModelCallsBackOrdersOldestDayFirst(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3.5-flash", Channel: "text", InputTokens: 1, OutputTokens: 0, TotalTokens: 1, At: midnight.AddDate(0, 0, -1).Add(time.Hour)})
	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3.5-flash", Channel: "text", InputTokens: 2, OutputTokens: 0, TotalTokens: 2, At: midnight.Add(time.Hour)})

	got, err := store.ModelCallsBack(ctx, 2)
	if err != nil {
		t.Fatalf("ModelCallsBack(2): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ModelCallsBack(2) returned %d rows, want 2: %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Day > got[i].Day {
			t.Errorf("day %q came before %q, want oldest first", got[i-1].Day, got[i].Day)
		}
	}
}

// TestModelCallsOnHeaviestFirst checks the read that answers "which job spent today's allowance": rows for one local day come back with the heaviest call count first, ties broken by provider then model.
func TestModelCallsOnHeaviestFirst(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3.5-flash", Channel: "text", InputTokens: 100, OutputTokens: 0, TotalTokens: 100, At: midnight.Add(time.Hour)})
	for i := 0; i < 5; i++ {
		addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3.5-flash-lite", Channel: "text", InputTokens: 1, OutputTokens: 0, TotalTokens: 1, At: midnight.Add(time.Duration(i+2) * time.Hour)})
	}
	// A call well outside the day being asked about must not be counted.
	addUse(t, store, TokenUse{Provider: "gemini", Model: "gemini-3.5-flash-lite", Channel: "text", InputTokens: 9, OutputTokens: 0, TotalTokens: 9, At: midnight.AddDate(0, 0, -1).Add(time.Hour)})

	got, err := store.ModelCallsOn(ctx, midnight)
	if err != nil {
		t.Fatalf("ModelCallsOn: %v", err)
	}
	today := midnight.Format("2006-01-02")
	want := []ModelDay{
		{Day: today, Provider: "gemini", Model: "gemini-3.5-flash-lite", Calls: 5, TotalTokens: 5},
		{Day: today, Provider: "gemini", Model: "gemini-3.5-flash", Calls: 1, TotalTokens: 100},
	}
	if len(got) != len(want) {
		t.Fatalf("ModelCallsOn returned %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestAddTokenUseUnderConcurrentWriters files calls from many goroutines at once against a file-backed store, so the DSN's WAL and busy_timeout are really exercised — the in-memory pool is pinned to one connection and would never see a busy database. Every write must land: a dropped row is a call the user is charged for and cannot see.
func TestAddTokenUseUnderConcurrentWriters(t *testing.T) {
	store, err := New(t.TempDir() + "/ora.db")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()

	const writers, each = 8, 25
	providers := []string{"codex", "gemini", "claude", "ollama"}
	errs := make(chan error, writers*each)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				_, err := store.AddTokenUse(ctx, TokenUse{
					Provider:     providers[w%len(providers)],
					Model:        "m",
					Channel:      "text",
					InputTokens:  1,
					OutputTokens: 1,
					TotalTokens:  2,
					DurationMS:   int64(i),
					Question:     fmt.Sprintf("writer %d call %d", w, i),
				})
				if err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AddTokenUse under concurrent writers: %v", err)
	}

	totals, err := store.TokenTotalsSince(ctx, time.Time{})
	if err != nil {
		t.Fatalf("TokenTotalsSince: %v", err)
	}
	calls := 0
	for _, tot := range totals {
		calls += tot.Calls
	}
	if calls != writers*each {
		t.Errorf("stored %d calls, want %d", calls, writers*each)
	}
	if len(totals) != len(providers) {
		t.Errorf("totals cover %d provider-and-model pairs, want %d", len(totals), len(providers))
	}
}
