package tally

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"ora/internal/db"
)

// weeklyWindow is how far back the weekly system log looks.
const weeklyWindow = 7 * 24 * time.Hour

// weeklyDayFormat is the local calendar-day key the tally/dream_runs tables use.
const weeklyDayFormat = "2006-01-02"

// providerStat accumulates one provider's tally rows over the window.
type providerStat struct {
	calls, failures int
	ms              int64
}

// RenderWeeklyLog renders the last 7 days of self-accounting as plain text: per-provider brain call counts, failures and mean latency; the vector-search contribution rate; how many nights the dreaming loop ran and how far each got; and how many diary entries of each kind were written. Read-only — nothing here writes to the store.
func RenderWeeklyLog(ctx context.Context, store *db.Store, now time.Time) (string, error) {
	since := now.Add(-weeklyWindow)

	tallyRows, err := store.TallyRowsSince(ctx, since)
	if err != nil {
		return "", fmt.Errorf("tally rows: %w", err)
	}
	dreams, err := store.DreamRunsSince(ctx, since.Format(weeklyDayFormat))
	if err != nil {
		return "", fmt.Errorf("dream runs: %w", err)
	}
	diaryKinds, err := store.DiaryKindCountsSince(ctx, since)
	if err != nil {
		return "", fmt.Errorf("diary kind counts: %w", err)
	}

	providers := map[string]*providerStat{}
	var vecQueries, vecHitQueries int
	var vecHitTotal int64
	for _, r := range tallyRows {
		switch r.Provider {
		case "vector-queries":
			vecQueries += r.Calls
		case "vector-hits":
			vecHitQueries += r.Calls
			vecHitTotal += r.TotalMs // repurposed: a raw hit count, not milliseconds — see db's hybrid.go recordVectorContribution.
		default:
			p := providers[r.Provider]
			if p == nil {
				p = &providerStat{}
				providers[r.Provider] = p
			}
			p.calls += r.Calls
			p.failures += r.Failures
			p.ms += r.TotalMs
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Weekly system log — %s to %s\n\n", since.Format("2 Jan"), now.Format("2 Jan"))

	b.WriteString("Brain calls:\n")
	writeProviderStats(&b, providers)

	b.WriteString("\nVector search contribution:\n")
	if vecQueries == 0 {
		b.WriteString("  (no retrieval queries this week)\n")
	} else {
		rate := float64(vecHitQueries) / float64(vecQueries) * 100
		fmt.Fprintf(&b, "  %d/%d queries (%.0f%%) had a surviving vector hit, %d hits total\n", vecHitQueries, vecQueries, rate, vecHitTotal)
	}

	b.WriteString("\nDreaming loop:\n")
	writeDreamStats(&b, dreams)

	b.WriteString("\nDiary entries written:\n")
	writeDiaryKindCounts(&b, diaryKinds)

	return b.String(), nil
}

// writeProviderStats renders one line per provider, sorted by name for determinism, "(none)" when empty.
func writeProviderStats(b *strings.Builder, providers map[string]*providerStat) {
	if len(providers) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := providers[name]
		var mean float64
		if p.calls > 0 {
			mean = float64(p.ms) / float64(p.calls)
		}
		fmt.Fprintf(b, "  %s: %d calls, %d failures, %.0fms mean latency\n", name, p.calls, p.failures, mean)
	}
}

// writeDreamStats renders how many nights ran, how many finished, and the set of stage tokens seen across them.
func writeDreamStats(b *strings.Builder, dreams []db.DreamRun) {
	if len(dreams) == 0 {
		b.WriteString("  (no nights ran)\n")
		return
	}
	finished := 0
	stageSeen := map[string]bool{}
	for _, d := range dreams {
		if d.Finished {
			finished++
		}
		for _, tok := range strings.Fields(d.StagesDone) {
			stageSeen[tok] = true
		}
	}
	stages := make([]string, 0, len(stageSeen))
	for s := range stageSeen {
		stages = append(stages, s)
	}
	sort.Strings(stages)
	fmt.Fprintf(b, "  %d nights ran, %d finished, stages seen: %s\n", len(dreams), finished, strings.Join(stages, " "))
}

// writeDiaryKindCounts renders one line per diary kind, sorted by name, "(none)" when empty.
func writeDiaryKindCounts(b *strings.Builder, counts map[string]int) {
	if len(counts) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintf(b, "  %s: %d\n", k, counts[k])
	}
}

// RunWeeklyLog renders the week's self-accounting and stores it as a kind="system-log" note — the same NotesOfKindSince-retrievable pattern meeting minutes already use — then logs one INFO line. Best-effort: a failure is logged and returned so the caller (the Sunday proactive trigger) can decide whether to log it again, but it never panics and never touches unrelated state.
func RunWeeklyLog(ctx context.Context, store *db.Store, now time.Time) error {
	text, err := RenderWeeklyLog(ctx, store, now)
	if err != nil {
		return fmt.Errorf("render weekly log: %w", err)
	}
	if _, err := store.LogNote(ctx, text, "system-log"); err != nil {
		return fmt.Errorf("store weekly log: %w", err)
	}
	slog.Info("weekly system log written", "kind", "system-log")
	return nil
}
