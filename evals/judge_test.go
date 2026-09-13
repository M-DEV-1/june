package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ora/internal/brain"
	"ora/internal/config"
)

// useTempQuota points the runner's shared counter at a temp directory for the length of one test and puts the real one back after, so a test never writes to the machine's own brain_quota.json. Input: the test. Output: the temp directory the counter is backed by.
func useTempQuota(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	was := evalQuota
	evalQuota = brain.NewQuotaState(dir)
	t.Cleanup(func() { evalQuota = was })
	return dir
}

// spendTodaysQuota writes a counter file in dir with model's whole background allowance already spent today, which is what the daemon's own file looks like after a busy day of background jobs. Input: the temp data directory and the model name. Output: none.
func spendTodaysQuota(t *testing.T, dir, model string) {
	t.Helper()
	limit, _ := brain.DefaultQuotaOptions().For(model)
	counts := map[string]map[string]int{
		brain.QuotaDay(): {model: limit.Limit - limit.Reserved},
	}
	data, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "brain_quota.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestMeteredGemini_TakesASlot checks that a Gemini call made by the eval runner moves the same brain_quota.json counter the daemon meters its own background jobs against. Before this, forty judge calls a run spent the day's free-tier allowance without the count moving, so the daemon still believed the requests it reserves for the user's own asks were there.
func TestMeteredGemini_TakesASlot(t *testing.T) {
	dir := useTempQuota(t)

	calls := 0
	if err := meteredGemini(context.Background(), config.TextModel, func(context.Context) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("meteredGemini returned %v, want the call to go through", err)
	}
	if calls != 1 {
		t.Fatalf("the call ran %d times, want once", calls)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "brain_quota.json"))
	if err != nil {
		t.Fatalf("no quota file was written: %v", err)
	}
	var counts map[string]map[string]int
	if err := json.Unmarshal(raw, &counts); err != nil {
		t.Fatal(err)
	}
	if got := counts[brain.QuotaDay()][config.TextModel]; got != 1 {
		t.Fatalf("today's count for %s = %d, want 1", config.TextModel, got)
	}
}

// TestJudgeAsk_RefusedOnceTheDailyQuotaIsSpent checks that the judge goes through that same gate: with the day's background allowance spent, ask must come back with ErrDailyQuota without ever reaching the client — the judge here holds a nil client, so a call that did happen would panic rather than pass.
func TestJudgeAsk_RefusedOnceTheDailyQuotaIsSpent(t *testing.T) {
	dir := useTempQuota(t)
	spendTodaysQuota(t, dir, config.TextModel)

	var out verdict
	err := (&judge{}).ask(context.Background(), "rubric", "material", &out)
	var quota *brain.ErrDailyQuota
	if !errors.As(err, &quota) {
		t.Fatalf("judge.ask returned %v, want a brain.ErrDailyQuota refusal", err)
	}
}
