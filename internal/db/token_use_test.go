// This file tests the token ledger: one row per model call, filed with the provider and model it went to, and the three reads over it — the per-provider totals, the live log, and the per-day bars.
package db

import (
	"context"
	"strings"
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

// TestAddTokenUse covers what AddTokenUse does to a row on the way in and back out, one property per subtest: every field round-trips as given, including an explicit time; a call filed with no time is stamped now and a question over 200 runes is cut to 200 without splitting a character; a call that reported no usage is still stored, counted, and zeroed rather than dropped; and a call reporting only the input/output halves gets the total filed as their sum.
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
			Question:     "what did vexil ask about",
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
		if u.Question != "what did vexil ask about" {
			t.Errorf("question came back %q", u.Question)
		}
		if !u.At.Equal(at) {
			t.Errorf("time came back %v, want the instant %v that went in", u.At.UTC(), at.UTC())
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
