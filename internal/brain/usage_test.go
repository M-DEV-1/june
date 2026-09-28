package brain

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// A successful answer from the provider is proof the login works again, even one that carries no windows. The Codex login check answers with no rate-limit headers, and Record ignored it, so after `codex login` the row stayed greyed out until a real call happened to carry headers.
func TestUsageStore_AnEmptyReadingClearsASignedOutMark(t *testing.T) {
	store := NewUsageStore(t.TempDir())
	store.Record("codex", []UsageLimit{{Window: "5h", UsedFraction: 0.65}})
	store.RecordSignedOut("codex", "run codex login")
	store.Record("codex", nil)

	snap, _ := store.Get("codex")
	if snap.SignedOut || snap.Note != "" {
		t.Errorf("after the login answered the store still says %+v", snap)
	}
	if len(snap.Limits) != 1 {
		t.Errorf("the empty reading wiped the last good window: %+v", snap.Limits)
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
	// The provider's midnight, not the machine's: Google's free tier rolls over on Pacific time, and naming local midnight told the user their allowance returned hours before it did.
	pacific, err := time.LoadLocation(quotaResetZone)
	if err != nil {
		t.Skip("no zone database on this machine")
	}
	if want := time.Date(2026, 9, 6, 0, 0, 0, 0, pacific); !got.ResetsAt.Equal(want) {
		t.Errorf("resets at %v, want the provider's next midnight %v", got.ResetsAt, want)
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

// The Gemini bar has to read the same day the counter wrote. The count is keyed on the provider's own day (QuotaDay, America/Los_Angeles) because Google's free-tier allowance rolls over on Pacific midnight, but the reading looked the count up under the machine's local date. On IST those two differ from local midnight until about 12:30 every day, so for the whole of a working morning the bar read a day the counter had never written and drew an empty allowance over a spent one.
func TestGeminiDaily_ReadsTheDayTheCounterWrote(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	pacific, err2 := time.LoadLocation(quotaResetZone)
	if err != nil || err2 != nil {
		t.Skip("no zone database on this machine")
	}
	// Nine in the morning in Kolkata, which is still the previous day in Los Angeles.
	now := time.Date(2026, 9, 15, 9, 0, 0, 0, kolkata)
	day := now.In(pacific).Format("2006-01-02")
	if day == now.Format("2006-01-02") {
		t.Fatalf("the test's own premise is wrong: %s and %s are the same day", day, now.Format("2006-01-02"))
	}

	// Twelve requests spent, written under the provider's day exactly as the counter writes them.
	dir := t.TempDir()
	body := fmt.Sprintf(`{%q:{"gemini-3.5-flash":12}}`, day)
	if err := os.WriteFile(filepath.Join(dir, "brain_quota.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	state := NewQuotaState(dir)
	opts := QuotaOptions{"gemini-3.5-flash": {Limit: 100}}

	snap, ok := GeminiDaily(state, "gemini-3.5-flash", opts, now)
	if !ok || len(snap.Limits) != 1 {
		t.Fatalf("no reading: ok=%v snap=%+v", ok, snap)
	}
	if got := snap.Limits[0].UsedFraction; got != 0.12 {
		t.Errorf("used = %v, want 0.12 — the bar read a different day than the counter wrote", got)
	}
	// The window turns over on the provider's midnight, not the machine's, or the bar promises a refill hours before one happens.
	if reset := snap.Limits[0].ResetsAt.In(pacific); reset.Hour() != 0 {
		t.Errorf("resets at %s in the provider's zone, want its midnight", reset.Format(time.RFC3339))
	}
}
