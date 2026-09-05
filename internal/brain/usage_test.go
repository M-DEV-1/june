package brain

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"ora/internal/agent"
)

// TestUsageStore_KeepsTheLastReadingAcrossARestart checks a reading survives the daemon stopping: the picker draws a bar the moment the window opens, from what the last response said, rather than staying blank until the next call to that provider.
func TestUsageStore_KeepsTheLastReadingAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	resets := time.Date(2026, 9, 5, 17, 56, 0, 0, time.UTC)

	store := NewUsageStore(dir)
	store.Record("codex", []UsageLimit{{Window: "5h", UsedFraction: 0.65, ResetsAt: resets, Source: "x-codex-primary-*"}})

	if _, err := os.Stat(filepath.Join(dir, "brain_usage.json")); err != nil {
		t.Fatalf("the reading was not written beside brain_quota.json: %v", err)
	}

	reopened := NewUsageStore(dir)
	snap, ok := reopened.Get("codex")
	if !ok {
		t.Fatalf("a fresh store on the same directory has no codex reading")
	}
	if len(snap.Limits) != 1 || snap.Limits[0].Window != "5h" || snap.Limits[0].UsedFraction != 0.65 {
		t.Errorf("reloaded limits = %+v, want the 5h window at 0.65", snap.Limits)
	}
	if !snap.Limits[0].ResetsAt.Equal(resets) {
		t.Errorf("reloaded reset time = %v, want %v", snap.Limits[0].ResetsAt, resets)
	}
	if snap.At.IsZero() {
		t.Errorf("the reloaded reading has no time on it, so nothing can say how stale it is")
	}
}

// TestUsageStore_AnEmptyReadingLeavesTheLastGoodOne checks a response that carried no rate-limit headers does not wipe the bar: an unknown reading and a spent allowance look the same on screen, and only one of them is true.
func TestUsageStore_AnEmptyReadingLeavesTheLastGoodOne(t *testing.T) {
	store := NewUsageStore(t.TempDir())
	store.Record("codex", []UsageLimit{{Window: "5h", UsedFraction: 0.65}})
	store.Record("codex", nil)

	snap, ok := store.Get("codex")
	if !ok || len(snap.Limits) != 1 {
		t.Fatalf("after an empty reading the store holds %+v, want the last good one", snap)
	}
}

// TestUsageStore_SaysWhenItHasNothing checks a provider nothing has ever been recorded for reports no reading rather than an empty one, so /brains can leave its limits array empty.
func TestUsageStore_SaysWhenItHasNothing(t *testing.T) {
	store := NewUsageStore(t.TempDir())
	if snap, ok := store.Get("grok"); ok {
		t.Fatalf("grok has a reading nobody recorded: %+v", snap)
	}
}

// TestUsageStore_StaleReadingsStillComeBackWithTheirOwnTime checks an old reading is returned as it was read rather than silently refreshed, because the window decides how much weight to give a bar from what limits_at says.
func TestUsageStore_StaleReadingsStillComeBackWithTheirOwnTime(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-72 * time.Hour).Round(time.Second)
	if err := os.WriteFile(filepath.Join(dir, "brain_usage.json"), []byte(`{"codex":{"limits":[{"window":"weekly","used_fraction":0.4,"resets_at":"2026-09-08T09:00:00Z","source":"x-codex-secondary-*"}],"at":"`+old.Format(time.RFC3339)+`"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	snap, ok := NewUsageStore(dir).Get("codex")
	if !ok {
		t.Fatalf("the store dropped a three-day-old reading")
	}
	if !snap.At.Equal(old) {
		t.Errorf("At = %v, want the moment it was read, %v", snap.At, old)
	}
}

// TestGeminiDaily_CountsTodayAgainstTheModelsCeiling checks the Gemini row's bar is the free-tier daily request count against the configured ceiling, read on a fixed clock so the reset is midnight of that same day and not of the day the test runs.
func TestGeminiDaily_CountsTodayAgainstTheModelsCeiling(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 5, 14, 30, 0, 0, time.Local)
	if err := os.WriteFile(filepath.Join(dir, "brain_quota.json"), []byte(`{"2026-09-05":{"gemini-3.5-flash":5},"2026-09-04":{"gemini-3.5-flash":19}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	state := NewQuotaState(dir)

	snap, ok := GeminiDaily(state, "gemini-3.5-flash", DefaultQuotaOptions(), now)
	if !ok {
		t.Fatalf("gemini-3.5-flash has a configured ceiling but reported no window")
	}
	if len(snap.Limits) != 1 {
		t.Fatalf("limits = %+v, want the one daily window", snap.Limits)
	}
	got := snap.Limits[0]
	// Five of the twenty requests the free tier allows for this model today.
	if got.Window != "daily" || got.UsedFraction != 0.25 {
		t.Errorf("limit = %+v, want the daily window at 0.25", got)
	}
	if want := time.Date(2026, 9, 6, 0, 0, 0, 0, time.Local); !got.ResetsAt.Equal(want) {
		t.Errorf("resets at %v, want the next local midnight %v", got.ResetsAt, want)
	}
	if got.Source != "brain_quota.json gemini-3.5-flash" {
		t.Errorf("source = %q, want the file and model the count came from", got.Source)
	}
	if !snap.At.Equal(now) {
		t.Errorf("At = %v, want the clock it was read on %v", snap.At, now)
	}
}

// TestGeminiDaily_SaysNothingForAModelWithNoCeiling checks a model DefaultQuotaOptions does not meter draws no bar, since there is no number to draw one against.
func TestGeminiDaily_SaysNothingForAModelWithNoCeiling(t *testing.T) {
	if _, ok := GeminiDaily(NewQuotaState(t.TempDir()), "gemini-9-imaginary", DefaultQuotaOptions(), time.Now()); ok {
		t.Fatalf("an unmetered model reported a daily window")
	}
}

// The store is the recorder the agent package writes its readings into; this says so at compile time, so a change to agent.UsageRecorder breaks here rather than in cmd/daemon.go.
var _ agent.UsageRecorder = (*UsageStore)(nil)
