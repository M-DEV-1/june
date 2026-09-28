package dream

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"june/internal/db"
)

// undStage rewrites the standing understanding doc, one brain call, committed with the stage token in one transaction. It reads the whole diary pyramid rather than only the last week: every year entry, the last twelve months, the last eight weeks and the recent dailies. The coarse tiers are already compressed, so the whole of the user's record costs a few thousand tokens, and the standing model is then built from all of it instead of from a seven-day window that quietly drops everything older.
func (r *Runner) undStage(ctx context.Context, night string) error {
	current, err := r.store.DiaryEntry(ctx, "", "understanding")
	if err != nil {
		return err
	}
	strong, err := r.store.StrongHypotheses(ctx)
	if err != nil {
		return err
	}
	days, err := r.store.DiaryDays(ctx, nightMinus(night, r.sweepBack(ctx, night)), night)
	if err != nil {
		return err
	}
	past, err := r.pyramid(ctx, night)
	if err != nil {
		return err
	}
	reply, err := r.ask(ctx, night, "understanding", understandingPrompt(current, strong, past, days))
	if err != nil {
		return err
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return fmt.Errorf("the understanding rewrite returned nothing")
	}
	return r.store.CommitUnderstandingStage(ctx, night, reply)
}

// coarseTiers is how many of each coarse diary tier the standing understanding reads, newest first: every year, then a year of months, then two months of weeks. Beyond that the tier above already carries the period.
var coarseTiers = []struct {
	kind string
	keep int
}{{"year", 0}, {"month", 12}, {"week", 8}}

// coarseEntry is one rolled-up diary entry with the tier it came from, since DiaryDay itself carries only the day and the content.
type coarseEntry struct {
	kind    string
	day     string
	content string
}

// pyramid reads the diary's coarse tiers for the understanding rewrite, coarsest first. Input: the night key, which bounds every tier to entries at or before it. Output: the entries in the order the prompt prints them — years, then months, then weeks — each tier trimmed to its newest coarseTiers.keep entries, 0 meaning all of them.
func (r *Runner) pyramid(ctx context.Context, night string) ([]coarseEntry, error) {
	var out []coarseEntry
	for _, tier := range coarseTiers {
		entries, err := r.store.DiaryEntriesThrough(ctx, tier.kind, night)
		if err != nil {
			return nil, err
		}
		if tier.keep > 0 && len(entries) > tier.keep {
			entries = entries[len(entries)-tier.keep:]
		}
		for _, e := range entries {
			out = append(out, coarseEntry{kind: tier.kind, day: e.Day, content: e.Content})
		}
	}
	return out, nil
}

// compactReport is what the compaction stage hands the morning report: how many coarse entries each tier wrote.
type compactReport struct{ weeks, months, years int }

// compactStage collapses the diary's old fine entries into coarser ones: complete Mon-Sun weeks of dailies all older than compactAfterDays become one kind='week' entry on the Monday, and a month's worth of week entries all older than compactWeeksToMonth weeks becomes one kind='month' entry on the first. One brain call per coarse entry, one transaction per tier, and the 'compact' token commits with the month tier — so a preemption between tiers costs nothing: the committed week entries simply give the next wake's re-run less to do. A night with nothing to compact commits the token with zero diary writes.
func (r *Runner) compactStage(ctx context.Context, night string) (compactReport, error) {
	var rep compactReport

	// Week tier. Querying only through the horizon is itself the age gate: a week can only reach seven fetched dailies when its Sunday is already past the horizon.
	horizon := nightMinus(night, compactAfterDays+1)
	dailies, err := r.store.DiaryEntriesThrough(ctx, "day", horizon)
	if err != nil {
		return rep, err
	}
	byMonday := map[string][]db.DiaryDay{}
	for _, d := range dailies {
		byMonday[mondayOf(d.Day)] = append(byMonday[mondayOf(d.Day)], d)
	}
	var weekComps []db.DiaryCompaction
	// A failing week stops the loop but is not returned yet: every week already compacted is a brain call that has been paid for, and returning here dropped all of them, so a single week that always failed meant the tier never made progress on any night.
	var weekErr error
	for _, monday := range slices.Sorted(maps.Keys(byMonday)) {
		days := byMonday[monday]
		if len(days) != 7 {
			// An incomplete week waits; a daily that never gets written holds its week (and its month) open indefinitely, which is the deliberate trade for never compacting around a hole.
			continue
		}
		entry, err := r.compactEntry(ctx, night, "compact-week", fmt.Sprintf("The week of Monday %s through Sunday %s.", monday, nightMinus(monday, -6)), days)
		if err != nil {
			weekErr = err
			break
		}
		weekComps = append(weekComps, db.DiaryCompaction{Day: monday, Kind: "week", Content: entry, ConstituentKind: "day", ConstituentDays: dayKeys(days)})
	}
	if len(weekComps) > 0 {
		if err := r.store.CommitCompactStage(ctx, night, weekComps, false); err != nil {
			return rep, err
		}
		rep.weeks = len(weekComps)
	}
	if weekErr != nil {
		return rep, weekErr
	}

	// Month tier. A month is ready once every one of its Mondays has a week entry inside the ten-week horizon; the capped query again doubles as the age gate.
	weekHorizon := nightMinus(night, 7*compactWeeksToMonth)
	weeks, err := r.store.DiaryEntriesThrough(ctx, "week", weekHorizon)
	if err != nil {
		return rep, err
	}
	haveWeek := map[string]bool{}
	byMonth := map[string][]db.DiaryDay{}
	for _, w := range weeks {
		haveWeek[w.Day] = true
		byMonth[w.Day[:7]] = append(byMonth[w.Day[:7]], w)
	}
	var monthComps []db.DiaryCompaction
	for _, month := range slices.Sorted(maps.Keys(byMonth)) {
		complete := true
		for _, monday := range mondaysOf(month) {
			if !haveWeek[monday] {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		entry, err := r.compactEntry(ctx, night, "compact-month", fmt.Sprintf("The month of %s.", month), byMonth[month])
		if err != nil {
			return rep, err
		}
		monthComps = append(monthComps, db.DiaryCompaction{Day: month + "-01", Kind: "month", Content: entry, ConstituentKind: "week", ConstituentDays: dayKeys(byMonth[month])})
	}
	rep.months = len(monthComps)
	if len(monthComps) > 0 {
		if err := r.store.CommitCompactStage(ctx, night, monthComps, false); err != nil {
			return rep, err
		}
	}

	// Year tier. A year is ready once all twelve of its months have month entries older than the horizon. It exists so the standing understanding has something to read about a year the user lived through, instead of that year surviving only as twelve month entries the rewrite has to re-read every night.
	monthHorizon := nightMinus(night, 30*compactMonthsToYear)
	months, err := r.store.DiaryEntriesThrough(ctx, "month", monthHorizon)
	if err != nil {
		return rep, err
	}
	byYear := map[string][]db.DiaryDay{}
	for _, m := range months {
		byYear[m.Day[:4]] = append(byYear[m.Day[:4]], m)
	}
	var yearComps []db.DiaryCompaction
	for _, year := range slices.Sorted(maps.Keys(byYear)) {
		if len(byYear[year]) != 12 {
			continue
		}
		entry, err := r.compactEntry(ctx, night, "compact-year", fmt.Sprintf("The year %s.", year), byYear[year])
		if err != nil {
			return rep, err
		}
		yearComps = append(yearComps, db.DiaryCompaction{Day: year + "-01-01", Kind: "year", Content: entry, ConstituentKind: "month", ConstituentDays: dayKeys(byYear[year])})
	}
	rep.years = len(yearComps)
	return rep, r.store.CommitCompactStage(ctx, night, yearComps, true)
}

// compactEntry makes one traced brain call to collapse a run of diary entries, refusing an empty reply.
func (r *Runner) compactEntry(ctx context.Context, night, kind, period string, entries []db.DiaryDay) (string, error) {
	reply, err := r.ask(ctx, night, kind, compactPrompt(period, entries))
	if err != nil {
		return "", err
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return "", fmt.Errorf("the compaction of %q returned nothing", period)
	}
	return reply, nil
}

// mondayOf returns the Monday of the local week a 'YYYY-MM-DD' day falls in, as the same kind of string.
func mondayOf(day string) string {
	d, err := time.ParseInLocation(time.DateOnly, day, time.Local)
	if err != nil {
		return day
	}
	return d.AddDate(0, 0, -int(d.Weekday()+6)%7).Format(time.DateOnly)
}

// mondaysOf returns every Monday date inside a 'YYYY-MM' month, oldest first — the week entries a month must hold before it may compact.
func mondaysOf(month string) []string {
	first, err := time.ParseInLocation("2006-01", month, time.Local)
	if err != nil {
		return nil
	}
	var out []string
	for d := first; d.Format("2006-01") == month; d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Monday {
			out = append(out, d.Format(time.DateOnly))
		}
	}
	return out
}

// dayKeys lists the day keys of a run of diary entries, for the compaction's constituent deletes.
func dayKeys(entries []db.DiaryDay) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Day
	}
	return out
}

// finish writes the morning report — the diary kind='dream' row, deliberately FTS-indexed so "what did you dream last night" works — and stamps the run finished with its one-line summary. The diary content is the night's own brain writing its entry in prose, with a compact audit footer of the hard numbers appended; if that call fails or comes back empty, the old fixed-template entry stands in, so a night never ends without a diary entry.
func (r *Runner) finish(ctx context.Context, night string, took time.Duration, hyp *stageReport, undRan bool, comp *compactReport, replay *replayReport, notes []string) error {
	entry := r.diaryEntry(ctx, night, took, hyp, undRan, comp, replay, notes)

	line := fmt.Sprintf("judge-only in %s", took.Round(time.Second))
	if hyp != nil {
		line = fmt.Sprintf("judge-only: %d tested, %d promoted, %d retired, %d adopted, in %s", hyp.tested, hyp.promoted, hyp.retired, hyp.adopted, took.Round(time.Second))
	}
	return r.store.FinishDreamRun(ctx, night, entry, line)
}

// diaryEntry asks the night's own brain to write the diary entry in prose, appending a compact audit footer so the hard numbers survive regardless of what the model chose to say. A transport error or an empty reply falls back to the old templated entry instead — every number the template names, nothing in the model's own words, but a diary entry all the same.
func (r *Runner) diaryEntry(ctx context.Context, night string, took time.Duration, hyp *stageReport, undRan bool, comp *compactReport, replay *replayReport, notes []string) string {
	prose, err := r.ask(ctx, night, "report", diaryPrompt(hyp, undRan, comp, replay, notes))
	prose = strings.TrimSpace(prose)
	switch {
	case err != nil:
		slog.Warn("dreaming: the diary-writing call failed, falling back to the templated entry", "night", night, "error", err)
	case prose == "":
		slog.Warn("dreaming: the diary-writing call returned nothing, falling back to the templated entry", "night", night)
	default:
		return prose + "\n\n" + dreamFooter(hyp, undRan, comp, replay, took)
	}
	return templateEntry(night, took, hyp, undRan, comp, replay, notes)
}

// dreamFooter renders the always-present one-line audit trail: the hard numbers behind the night, in the same compact shape regardless of whether the entry above it came from the model or the fallback template, so eval and recall code that greps for facts finds them either way.
func dreamFooter(hyp *stageReport, undRan bool, comp *compactReport, replay *replayReport, took time.Duration) string {
	tested, promoted, retired, adopted := 0, 0, 0, 0
	if hyp != nil {
		tested, promoted, retired, adopted = hyp.tested, hyp.promoted, hyp.retired, hyp.adopted
	}
	weeks, months := 0, 0
	if comp != nil {
		weeks, months = comp.weeks, comp.months
	}
	items, piles := 0, 0
	if replay != nil {
		items, piles = replay.items, replay.piles
	}
	rewritten := "understanding not rewritten"
	if undRan {
		rewritten = "understanding rewritten"
	}
	return fmt.Sprintf("[tested %d: %d promoted, %d retired, %d adopted; %s; compacted %dw/%dm; replayed %d items into %d piles; %s]",
		tested, promoted, retired, adopted, rewritten, weeks, months, items, piles, took.Round(time.Second))
}

// templateEntry is the old fixed-template morning report, kept as the fallback for when the diary-writing call fails or returns nothing.
func templateEntry(night string, took time.Duration, hyp *stageReport, undRan bool, comp *compactReport, replay *replayReport, notes []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "I dreamt on the night of %s, judge-only, for %s.\n", night, took.Round(time.Second))
	if hyp != nil {
		fmt.Fprintf(&b, "I tested %d hypotheses: %d promoted, %d retired, %d adopted new.\n", hyp.tested, hyp.promoted, hyp.retired, hyp.adopted)
		for _, l := range hyp.lines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	} else {
		b.WriteString("The hypothesis stage had already committed on an earlier wake tonight.\n")
	}
	if undRan {
		b.WriteString("I rewrote my understanding of the user.\n")
	} else {
		b.WriteString("The understanding had already been rewritten on an earlier wake tonight.\n")
	}
	switch {
	case comp == nil:
		b.WriteString("The diary compaction had already run on an earlier wake tonight.\n")
	case comp.weeks == 0 && comp.months == 0:
		b.WriteString("Nothing in the diary was old enough to compact.\n")
	default:
		fmt.Fprintf(&b, "I compacted the diary: %d weeks folded into week entries, %d months folded into month entries.\n", comp.weeks, comp.months)
	}
	if replay == nil {
		b.WriteString("The replay stage had already run on an earlier wake tonight.\n")
	} else {
		b.WriteString(replayLine(*replay))
	}
	for _, n := range notes {
		b.WriteString(n)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
