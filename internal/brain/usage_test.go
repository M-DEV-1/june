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

// TestGeminiDaily_DrawsAnUnlistedGeminiModelAgainstTheDefaultCeiling checks that a Gemini model nobody has measured still draws a bar, against DefaultQuotaLimit, and carries a note saying the ceiling is a guess — rather than the bar silently disappearing, which is what config.TextFallbackModel and any pinned background model used to do.
func TestGeminiDaily_DrawsAnUnlistedGeminiModelAgainstTheDefaultCeiling(t *testing.T) {
	snap, ok := GeminiDaily(NewQuotaState(t.TempDir()), "gemini-9-imaginary", DefaultQuotaOptions(), time.Now())
	if !ok || len(snap.Limits) != 1 {
		t.Fatalf("an unlisted gemini model drew no window: ok=%v limits=%+v", ok, snap.Limits)
	}
	if snap.Note == "" {
		t.Errorf("the window carries no note, so the picker cannot say the ceiling is a default")
	}
}

// TestGeminiDaily_SaysNothingForAModelFromAnotherNamespace checks a name that is not a Gemini model at all draws no bar, since there is no free-tier ceiling it could be measured against.
func TestGeminiDaily_SaysNothingForAModelFromAnotherNamespace(t *testing.T) {
	if _, ok := GeminiDaily(NewQuotaState(t.TempDir()), "sonnet", DefaultQuotaOptions(), time.Now()); ok {
		t.Fatalf("a non-Gemini model name reported a daily window")
	}
}

// TestGeminiDaily_NoNoteForAMeasuredModel checks a model with its own measured ceiling carries no note, so an ordinary row does not tell the user the number is a guess.
func TestGeminiDaily_NoNoteForAMeasuredModel(t *testing.T) {
	snap, ok := GeminiDaily(NewQuotaState(t.TempDir()), "gemini-3.5-flash", DefaultQuotaOptions(), time.Now())
	if !ok {
		t.Fatal("a measured model drew no window")
	}
	if snap.Note != "" {
		t.Errorf("note = %q, want empty for a model whose ceiling was actually observed", snap.Note)
	}
}

// TestUsageStore_ATornWriteDoesNotLoseTheReadings checks the usage file is replaced by a rename rather than truncated in place, so a crash mid-write leaves the last good readings on disk instead of blanking every bar.
func TestUsageStore_ATornWriteDoesNotLoseTheReadings(t *testing.T) {
	dir := t.TempDir()
	NewUsageStore(dir).Record("codex", []UsageLimit{{Window: "5h", UsedFraction: 0.4}})
	if err := os.WriteFile(filepath.Join(dir, "brain_usage.json.tmp"), []byte(`{"cod`), 0o600); err != nil {
		t.Fatalf("write the torn file: %v", err)
	}
	if _, ok := NewUsageStore(dir).Get("codex"); !ok {
		t.Fatal("the codex reading was lost to a torn write")
	}
}

// The store is the recorder the agent package writes its readings into; this says so at compile time, so a change to agent.UsageRecorder breaks here rather than in cmd/daemon.go.
var _ agent.UsageRecorder = (*UsageStore)(nil)
