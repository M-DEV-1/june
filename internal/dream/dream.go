// Package dream is the daemon's overnight loop: while the machine sits idle on mains between the dream hour and the morning brief, it tests the hypotheses Ora's diary has accumulated against the week's evidence, adopts new ones, rewrites the standing understanding of the user, and leaves a morning report in the diary. This slice is judge-only — every model call goes to the configured brain; the local grinder model is a later slice.
package dream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"ora/internal/brain"
	"ora/internal/db"
)

// dayFormat is the local calendar-day key shared with the diary and dream_runs tables.
const dayFormat = "2006-01-02"

const (
	// brainCurfewMinutes is the local time-of-day, in minutes after midnight, past which no unforced dream may start a brain call: the user's Claude subscription runs in five-hour windows and his workday window opens at 08:30, so any call after 03:28 would open an overnight window that bleeds into it. A forced run (the dream-now marker) bypasses the curfew — forcing is the user's own choice.
	brainCurfewMinutes = 3*60 + 25
	// idleAfter is how long the newest episode must be old for the user to count as away when the screen is not locked.
	idleAfter = 15 * time.Minute
	// missingDiaryGrace is how long past the dream hour the runner waits for the evening close's diary entry before dreaming from raw day summaries instead.
	missingDiaryGrace = 2 * time.Hour
	// maxNewHypotheses caps how many hypotheses one night may adopt from the diary.
	maxNewHypotheses = 5
	// maxStatementLen caps an adopted hypothesis statement; longer ones are dropped as malformed rather than truncated mid-sentence.
	maxStatementLen = 200
	// maxEvidenceLen caps a verdict's evidence citation; longer ones are truncated.
	maxEvidenceLen = 500
	// promoteAfterTests and promoteMinAgeDays are the promotion mechanics: the judge may recommend, but only a hypothesis tested this many times and born at least this many days ago is promoted.
	promoteAfterTests = 3
	promoteMinAgeDays = 7
	// retireContradictions retires a hypothesis once the evidence log holds this many contradictions.
	retireContradictions = 2
	// staleAfterDays retires an open hypothesis that has sat this long without ever being tested.
	staleAfterDays = 30
	// defaultWatchEvery is how often the preemption watcher polls for the user's return.
	defaultWatchEvery = 5 * time.Second
)

// contradictedMark is what a contradiction's evidence line contains, and what the retirement mechanic counts. The lines are written by evidenceLine below, so the format is ours to rely on.
const contradictedMark = "] contradicted —"

// errUnparsable marks a brain reply that failed to parse as JSON even after one re-ask, so the caller skips the work instead of retrying it all night.
var errUnparsable = errors.New("the reply never parsed as JSON")

// Probes are the runner's yes/no reads on the machine — mains power, the lock screen, and whether the meeting recorder is mid-flight — injected so tests can flip them freely.
type Probes struct {
	OnAC              func() bool
	SessionLocked     func() bool
	RecorderQuiescent func() bool
}

// Runner owns one machine's dreaming. Construct with New; the daemon calls Tick on a ticker and everything else is private.
type Runner struct {
	store  *db.Store
	brain  brain.Brain
	probes Probes
	// dreamHour opens the window (negative disables dreaming); briefHour closes it. Both local hours.
	dreamHour int
	briefHour int
	// now is the clock, replaceable in tests. All night-key and window math is local time.
	now func() time.Time
	// watchEvery paces the preemption watcher. Tests shorten it.
	watchEvery time.Duration

	// ForceMarker is the path of a file whose presence makes the next tick dream immediately, bypassing the window and away-gates — the way to watch a dream run without leaving the machine. The marker is consumed, and a forced run arms no preemption watcher, since the user being present is the whole point.
	ForceMarker string
}

// New builds a Runner from the store, a one-shot brain, the machine probes, and the two local hours that bound the window.
func New(store *db.Store, b brain.Brain, probes Probes, dreamHour, briefHour int) *Runner {
	return &Runner{store: store, brain: b, probes: probes, dreamHour: dreamHour, briefHour: briefHour, now: time.Now, watchEvery: defaultWatchEvery}
}

// nightKey returns the night a moment belongs to: the current local day once the dream hour has passed, otherwise the day before — so 23:30 and 02:00 the next morning are the same night.
func (r *Runner) nightKey(now time.Time) string {
	if now.Hour() >= r.dreamHour {
		return now.Format(dayFormat)
	}
	return now.AddDate(0, 0, -1).Format(dayFormat)
}

// inWindow reports whether the hour lies in [dreamHour, briefHour), handling the usual case where the window wraps midnight.
func (r *Runner) inWindow(hour int) bool {
	if r.dreamHour < r.briefHour {
		return hour >= r.dreamHour && hour < r.briefHour
	}
	return hour >= r.dreamHour || hour < r.briefHour
}

// windowStart is the local instant the night's window opened, for the missing-diary grace check.
func (r *Runner) windowStart(night string) time.Time {
	d, err := time.ParseInLocation(dayFormat, night, time.Local)
	if err != nil {
		return time.Time{}
	}
	return time.Date(d.Year(), d.Month(), d.Day(), r.dreamHour, 0, 0, 0, time.Local)
}

// nightMinus returns the night key days earlier, for age comparisons — ISO date strings compare lexically.
func nightMinus(night string, days int) string {
	d, err := time.ParseInLocation(dayFormat, night, time.Local)
	if err != nil {
		return night
	}
	return d.AddDate(0, 0, -days).Format(dayFormat)
}

// Tick is the per-wake entry point, called from the daemon's five-minute ticker. It returns without a trace when the window is closed or any start condition fails, and otherwise starts or resumes the night's run. Conditions, all required: inside the window, the night not yet finished, on mains, the user idle (screen locked or no episode in idleAfter), the recorder quiescent, and the evening close's diary entry present — or missingDiaryGrace past the dream hour, in which case day summaries stand in.
func (r *Runner) Tick(ctx context.Context) {
	if r.dreamHour < 0 {
		return
	}
	now := r.now().In(time.Local)
	forced := r.consumeForceMarker()
	if !forced && !r.inWindow(now.Hour()) {
		return
	}
	// Past the curfew the whole run waits for tomorrow night: a dream started at 03:20 could still be making brain calls at 03:40, so the gate is on starting at all, with a resumed run equally held.
	if !forced && now.Hour()*60+now.Minute() >= brainCurfewMinutes && now.Hour() < r.briefHour {
		return
	}
	night := r.nightKey(now)
	run, exists, err := r.store.DreamRun(ctx, night)
	if err != nil {
		slog.Warn("dreaming: reading the run row failed", "night", night, "error", err)
		return
	}
	if exists && run.Finished {
		return
	}
	locked := r.probes.SessionLocked()
	lastEpisode, err := r.store.MemoryAsOf(ctx, "episode:recent")
	if err != nil {
		slog.Warn("dreaming: reading the newest episode failed", "error", err)
		return
	}
	if !forced {
		if !r.probes.OnAC() {
			return
		}
		if !locked && !lastEpisode.IsZero() && now.Sub(lastEpisode) < idleAfter {
			return
		}
		if !r.probes.RecorderQuiescent() {
			return
		}
	}
	dayEntry, err := r.store.DiaryEntry(ctx, night, "day")
	if err != nil {
		slog.Warn("dreaming: reading the night's diary entry failed", "error", err)
		return
	}
	fallback := dayEntry == ""
	if !forced && fallback && now.Sub(r.windowStart(night)) < missingDiaryGrace {
		return
	}
	r.dream(ctx, night, run, exists, locked && !forced, lastEpisode, fallback, forced)
}

// consumeForceMarker reports whether the manual dream trigger is set, removing it so one touch means one run.
func (r *Runner) consumeForceMarker() bool {
	if r.ForceMarker == "" {
		return false
	}
	if _, err := os.Stat(r.ForceMarker); err != nil {
		return false
	}
	if err := os.Remove(r.ForceMarker); err != nil {
		slog.Warn("dreaming: could not consume the force marker", "path", r.ForceMarker, "error", err)
		return false
	}
	slog.Info("dreaming: manual trigger, running now with the away-gates bypassed")
	return true
}

// dream runs (or resumes) one night: start the run row, arm the preemption watcher, run the missing stages, and finish with the morning report. Any stage error — a cancelled context included — just returns; nothing partial was committed and the next wake resumes from stages_done.
func (r *Runner) dream(ctx context.Context, night string, run db.DreamRun, exists, lockedAtStart bool, baseline time.Time, fallback, forced bool) {
	if !exists {
		if err := r.store.StartDreamRun(ctx, night); err != nil {
			slog.Warn("dreaming: starting the run failed", "night", night, "error", err)
			return
		}
		slog.Info("dreaming: starting the night", "night", night, "mode", "judge-only")
	} else {
		slog.Info("dreaming: resuming the night", "night", night, "stages_done", run.StagesDone)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !forced {
		go r.watchForUser(ctx, cancel, lockedAtStart, baseline)
	}

	started := r.now()
	done := strings.Fields(run.StagesDone)
	var notes []string
	if fallback {
		notes = append(notes, "Tonight's diary entry was missing, so the day's screen summaries stood in for it.")
	}

	var hyp *stageReport
	if !slices.Contains(done, "hyp") {
		rep, err := r.hypStage(ctx, night, fallback)
		if err != nil {
			slog.Warn("dreaming: hypothesis stage did not commit", "night", night, "error", err)
			return
		}
		hyp = &rep
		slog.Info("dreaming: hypothesis stage committed", "night", night, "tested", rep.tested, "promoted", rep.promoted, "retired", rep.retired, "adopted", rep.adopted)
	}

	undRan := false
	if !slices.Contains(done, "und") {
		if err := r.undStage(ctx, night); err != nil {
			slog.Warn("dreaming: understanding stage did not commit", "night", night, "error", err)
			return
		}
		undRan = true
		slog.Info("dreaming: understanding stage committed", "night", night)
	}

	if err := r.finish(ctx, night, r.now().Sub(started), hyp, undRan, notes); err != nil {
		slog.Warn("dreaming: finishing the night failed", "night", night, "error", err)
		return
	}
	slog.Info("dreaming: night finished", "night", night)
}

// watchForUser cancels the night's work the moment the user comes back: the session unlocking (when the lock was the idle signal at start) or any episode newer than the baseline. A cancelled stage's transaction never commits, so preemption loses at most one in-flight brain call.
func (r *Runner) watchForUser(ctx context.Context, cancel context.CancelFunc, lockedAtStart bool, baseline time.Time) {
	t := time.NewTicker(r.watchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if lockedAtStart && !r.probes.SessionLocked() {
				slog.Info("dreaming preempted: the session unlocked")
				cancel()
				return
			}
			if last, err := r.store.MemoryAsOf(ctx, "episode:recent"); err == nil && last.After(baseline) {
				slog.Info("dreaming preempted: a new episode arrived")
				cancel()
				return
			}
		}
	}
}

// stageReport is what the hypothesis stage hands the morning report: counts plus the sentences worth repeating.
type stageReport struct {
	tested, promoted, retired, adopted int
	lines                              []string
}

// rawVerdict is the judge's JSON for one hypothesis, before Go's mechanics decide what actually happens to the row.
type rawVerdict struct {
	ID         int64  `json:"id"`
	Verdict    string `json:"verdict"`
	Confidence string `json:"confidence"`
	Evidence   string `json:"evidence"`
	Action     string `json:"action"`
}

// rawHypothesis is the extraction call's JSON for one proposed new hypothesis.
type rawHypothesis struct {
	Statement  string `json:"statement"`
	Confidence string `json:"confidence"`
}

// hypStage tests every open hypothesis against the week's evidence and adopts new ones from the dailies, committing everything (and the stage token) in one transaction. A brain transport error aborts the stage for this wake; a reply that never parses skips the judging but still commits the token, journaled, so the night does not burn calls retrying.
func (r *Runner) hypStage(ctx context.Context, night string, fallback bool) (stageReport, error) {
	var rep stageReport

	open, err := r.store.OpenHypotheses(ctx)
	if err != nil {
		return rep, err
	}
	material, haveDailies, err := r.evidenceMaterial(ctx, night, fallback)
	if err != nil {
		return rep, err
	}

	var verdicts []db.HypothesisVerdict
	judged := map[int64]bool{}
	if len(open) > 0 {
		var raw []rawVerdict
		err := r.askJSON(ctx, verdictPrompt(open, material), &raw)
		switch {
		case errors.Is(err, errUnparsable):
			rep.lines = append(rep.lines, "The judge's verdicts never parsed as JSON, so no hypotheses were tested tonight.")
		case err != nil:
			return rep, err
		default:
			byID := make(map[int64]db.Hypothesis, len(open))
			for _, h := range open {
				byID[h.ID] = h
			}
			for _, v := range validVerdicts(raw, byID) {
				update, note := decide(byID[v.ID], v, night)
				verdicts = append(verdicts, update)
				judged[v.ID] = true
				rep.tested++
				switch update.Status {
				case "promoted":
					rep.promoted++
					rep.lines = append(rep.lines, fmt.Sprintf("Promoted: %s (%s).", byID[v.ID].Statement, update.Reason))
				case "retired":
					rep.retired++
					rep.lines = append(rep.lines, fmt.Sprintf("Retired: %s (%s).", byID[v.ID].Statement, update.Reason))
				}
				if note != "" {
					rep.lines = append(rep.lines, note)
				}
			}
		}
	}

	// The stale mechanic is Go's alone: an open hypothesis the judge never reached tonight, thirty days old and never once tested, is dead weight and retires without a verdict.
	for _, h := range open {
		if judged[h.ID] || h.TimesTested > 0 || h.Born > nightMinus(night, staleAfterDays) {
			continue
		}
		verdicts = append(verdicts, db.HypothesisVerdict{ID: h.ID, Confidence: h.Confidence, Status: "retired", Reason: fmt.Sprintf("open %d days and never tested", staleAfterDays)})
		rep.retired++
		rep.lines = append(rep.lines, fmt.Sprintf("Retired: %s (open %d days and never tested).", h.Statement, staleAfterDays))
	}

	var adopted []db.NewHypothesis
	if haveDailies {
		var raw []rawHypothesis
		err := r.askJSON(ctx, extractPrompt(open, material), &raw)
		switch {
		case errors.Is(err, errUnparsable):
			rep.lines = append(rep.lines, "The extraction reply never parsed as JSON, so no new hypotheses were adopted.")
		case err != nil:
			return rep, err
		default:
			adopted = validNew(raw)
			rep.adopted = len(adopted)
			for _, a := range adopted {
				rep.lines = append(rep.lines, fmt.Sprintf("Adopted: %s.", a.Statement))
			}
		}
	}

	return rep, r.store.CommitHypothesisStage(ctx, night, verdicts, adopted)
}

// evidenceMaterial assembles the evidence the two hypothesis calls share: the last seven diary day entries and, when the fallback is on, the night's raw summary timeline. haveDailies reports whether any diary material exists for the extraction call to mine.
func (r *Runner) evidenceMaterial(ctx context.Context, night string, fallback bool) (material string, haveDailies bool, err error) {
	dailies, err := r.store.DiaryDays(ctx, nightMinus(night, 6), night)
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	for _, d := range dailies {
		fmt.Fprintf(&b, "\n--- Diary entry, %s ---\n%s\n", d.Day, d.Content)
	}
	if fallback {
		start := r.windowStart(night)
		dayStart := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.Local)
		summaries, err := r.store.SummaryTimeline(ctx, dayStart, r.now())
		if err != nil {
			return "", false, err
		}
		b.WriteString("\n--- Today's screen summaries (no diary entry was written tonight) ---\n")
		if len(summaries) == 0 {
			b.WriteString("(none)\n")
		}
		for _, w := range summaries {
			fmt.Fprintf(&b, "%s — %s\n", w.CreatedAt.Local().Format("15:04"), summaryText(w.Content))
		}
	}
	if b.Len() == 0 {
		b.WriteString("\n(no material)\n")
	}
	return b.String(), len(dailies) > 0, nil
}

// askJSON runs one brain call and decodes its JSON reply into out, re-asking the same prompt once when the reply fails to parse. A transport error returns as-is so the stage can retry on a later wake; a reply that never parses returns errUnparsable so the caller skips instead.
func (r *Runner) askJSON(ctx context.Context, prompt string, out any) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		reply, err := r.brain(ctx, prompt)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(stripFence(reply)), out); err == nil {
			return nil
		} else {
			lastErr = err
			slog.Warn("dreaming: a brain reply failed to parse as JSON", "attempt", attempt+1, "error", err)
		}
	}
	return fmt.Errorf("%w: %v", errUnparsable, lastErr)
}

// stripFence removes a markdown code fence around a JSON body — the same defence evals' judge needed, since a model wrapping its answer in ``` is the most common way a JSON reply is lost.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	} else {
		return s
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// validVerdicts keeps the structurally sound verdicts: a known id (once each), enum-valid verdict, confidence and action, evidence truncated to its cap. Everything else is dropped and logged.
func validVerdicts(raw []rawVerdict, open map[int64]db.Hypothesis) []rawVerdict {
	var out []rawVerdict
	seen := map[int64]bool{}
	for _, v := range raw {
		_, known := open[v.ID]
		if !known || seen[v.ID] ||
			!slices.Contains([]string{"supported", "contradicted", "unclear"}, v.Verdict) ||
			!slices.Contains([]string{"low", "medium", "high"}, v.Confidence) ||
			!slices.Contains([]string{"keep", "promote", "retire"}, v.Action) {
			slog.Warn("dreaming: dropping an invalid verdict", "id", v.ID, "verdict", v.Verdict, "confidence", v.Confidence, "action", v.Action)
			continue
		}
		seen[v.ID] = true
		if len(v.Evidence) > maxEvidenceLen {
			v.Evidence = v.Evidence[:maxEvidenceLen]
		}
		out = append(out, v)
	}
	return out
}

// validNew keeps at most maxNewHypotheses proposals with a non-empty statement under the length cap; confidence defaults to low when not enum-valid. Duplicates of existing statements are handled by the table's UNIQUE index, not here.
func validNew(raw []rawHypothesis) []db.NewHypothesis {
	var out []db.NewHypothesis
	for _, h := range raw {
		s := strings.TrimSpace(h.Statement)
		if s == "" || len(s) > maxStatementLen {
			slog.Warn("dreaming: dropping an invalid new hypothesis", "len", len(s))
			continue
		}
		if !slices.Contains([]string{"low", "medium", "high"}, h.Confidence) {
			h.Confidence = "low"
		}
		out = append(out, db.NewHypothesis{Statement: s, Confidence: h.Confidence})
		if len(out) == maxNewHypotheses {
			break
		}
	}
	return out
}

// decide applies the mechanics to one judged hypothesis. The model recommends; Go decides: promotion needs promoteAfterTests tests and promoteMinAgeDays of age, a retireContradictions-th contradiction retires regardless of the recommendation, and everything else stays open with the judge's confidence. The returned note, when non-empty, is a journal line for the morning report.
func decide(h db.Hypothesis, v rawVerdict, night string) (db.HypothesisVerdict, string) {
	update := db.HypothesisVerdict{
		ID:           h.ID,
		Confidence:   v.Confidence,
		Status:       "open",
		LastTested:   night,
		EvidenceLine: fmt.Sprintf("[%s] %s — %s", night, v.Verdict, v.Evidence),
		Tested:       true,
	}
	contradictions := strings.Count(h.Evidence, contradictedMark)
	if v.Verdict == "contradicted" {
		contradictions++
	}
	tests := h.TimesTested + 1
	note := ""
	switch {
	case contradictions >= retireContradictions:
		update.Status, update.Reason = "retired", fmt.Sprintf("contradicted %d times", contradictions)
	case v.Action == "retire":
		update.Status, update.Reason = "retired", "retired by the nightly judge"
	case v.Action == "promote":
		if tests >= promoteAfterTests && h.Born <= nightMinus(night, promoteMinAgeDays) {
			update.Status, update.Reason = "promoted", fmt.Sprintf("promoted after %d tests", tests)
		} else {
			note = fmt.Sprintf("Promotion refused for %q: %d tests, born %s.", h.Statement, tests, h.Born)
		}
	}
	return update, note
}

// undStage rewrites the standing understanding doc from the current doc, the strong hypotheses and the week's diary first lines, one brain call, committed with the stage token in one transaction.
func (r *Runner) undStage(ctx context.Context, night string) error {
	current, err := r.store.DiaryEntry(ctx, "", "understanding")
	if err != nil {
		return err
	}
	strong, err := r.store.StrongHypotheses(ctx)
	if err != nil {
		return err
	}
	week, err := r.store.DiaryDays(ctx, nightMinus(night, 6), night)
	if err != nil {
		return err
	}
	reply, err := r.brain(ctx, understandingPrompt(current, strong, week))
	if err != nil {
		return err
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return fmt.Errorf("the understanding rewrite returned nothing")
	}
	return r.store.CommitUnderstandingStage(ctx, night, reply)
}

// finish writes the morning report — the diary kind='dream' row, deliberately FTS-indexed so "what did you dream last night" works — and stamps the run finished with its one-line summary.
func (r *Runner) finish(ctx context.Context, night string, took time.Duration, hyp *stageReport, undRan bool, notes []string) error {
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
	for _, n := range notes {
		b.WriteString(n)
		b.WriteString("\n")
	}

	line := fmt.Sprintf("judge-only in %s", took.Round(time.Second))
	if hyp != nil {
		line = fmt.Sprintf("judge-only: %d tested, %d promoted, %d retired, %d adopted, in %s", hyp.tested, hyp.promoted, hyp.retired, hyp.adopted, took.Round(time.Second))
	}
	return r.store.FinishDreamRun(ctx, night, strings.TrimSpace(b.String()), line)
}

// summaryText pulls the prose out of a summary node's content — task summaries are stored as marshalled JSON and the model should read the sentence, not the blob. Non-JSON content passes through unchanged.
func summaryText(content string) string {
	var t struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(content), &t); err == nil && strings.TrimSpace(t.Summary) != "" {
		return t.Summary
	}
	return content
}
