package dream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"june/internal/db"
	"june/internal/db/dbtest"
)

// fakeBrain answers each of the dream prompts with a canned reply, dispatching on the instruction text, and records what it was asked. Safe for the watcher goroutine's world: only Tick's goroutine calls it, but the mutex keeps the record readable after Tick returns.
type fakeBrain struct {
	mu       sync.Mutex
	verdicts string
	extract  string
	und      string
	compact  string
	// compactFailAfter, when above zero, makes every compact call past that number return an error, so a test can fail one week of a multi-week compaction.
	compactFailAfter int
	compacts         int
	report           string
	reportErr        error
	asked            []string
}

func (f *fakeBrain) fn(ctx context.Context, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(prompt, "testing your private hypotheses"):
		f.asked = append(f.asked, "verdicts")
		return f.verdicts, nil
	case strings.Contains(prompt, "Pull out the ones worth tracking"):
		f.asked = append(f.asked, "extract")
		return f.extract, nil
	case strings.Contains(prompt, "Rewrite your standing understanding"):
		f.asked = append(f.asked, "und")
		return f.und, nil
	case strings.Contains(prompt, "Collapse the diary entries below"):
		f.asked = append(f.asked, "compact")
		f.compacts++
		if f.compactFailAfter > 0 && f.compacts > f.compactFailAfter {
			return "", fmt.Errorf("the brain refused compact call %d", f.compacts)
		}
		return f.compact, nil
	case strings.Contains(prompt, "You just spent the night dreaming about the user"):
		f.asked = append(f.asked, "report")
		return f.report, f.reportErr
	}
	return "", fmt.Errorf("unrecognised prompt: %.80s", prompt)
}

func (f *fakeBrain) askedKinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// yesProbes says the machine is dreamable: on mains, locked, recorder quiet.
func yesProbes() Probes {
	return Probes{
		OnAC:              func() bool { return true },
		SessionLocked:     func() bool { return true },
		RecorderQuiescent: func() bool { return true },
	}
}

// newRunner builds a Runner on the fake brain with the clock pinned to now. The retention numbers are pinned too, so no test reads or writes the machine's real config file on its way through the pruning stage.
func newRunner(store *db.Store, b *fakeBrain, probes Probes, now time.Time) *Runner {
	r := New(store, b.fn, probes, 23, 9)
	r.now = func() time.Time { return now }
	r.watchEvery = time.Hour
	r.retention = func() (int, time.Duration) { return testActRunKeep, testFailedGrace }
	return r
}

// at returns today's local clock at the given hour and minute — every gating test runs against a fake now built from it.
func at(hour, min int) time.Time {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), hour, min, 0, 0, time.Local)
}

// insertEpisodeAt writes an episode row with a controlled created_at, in SQLite's UTC storage format, so idle math against the fake clock is exact.
func insertEpisodeAt(t *testing.T, store *db.Store, ts time.Time) {
	t.Helper()
	if _, err := store.DB().Exec(`INSERT INTO episodes (created_at, app, title) VALUES (?, 'code', 'x')`,
		ts.UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
}

// Each start condition individually blocks the run: no brain call, no run row. The final case proves the same setup does dream once nothing blocks.
func TestTick_ConditionsGate(t *testing.T) {
	night := at(23, 30).Format(time.DateOnly)

	cases := []struct {
		name  string
		prep  func(t *testing.T, s *db.Store, r *Runner)
		wants bool
	}{
		{"outside the window", func(t *testing.T, s *db.Store, r *Runner) {
			r.now = func() time.Time { return at(12, 0) }
		}, false},
		{"disabled by a negative hour", func(t *testing.T, s *db.Store, r *Runner) {
			r.dreamHour = -1
		}, false},
		{"on battery", func(t *testing.T, s *db.Store, r *Runner) {
			r.probes.OnAC = func() bool { return false }
		}, false},
		{"user active", func(t *testing.T, s *db.Store, r *Runner) {
			r.probes.SessionLocked = func() bool { return false }
			insertEpisodeAt(t, s, at(23, 25))
		}, false},
		{"recorder busy", func(t *testing.T, s *db.Store, r *Runner) {
			r.probes.RecorderQuiescent = func() bool { return false }
		}, false},
		{"past the 03:25 curfew, so a usage window opened now would bleed into the workday", func(t *testing.T, s *db.Store, r *Runner) {
			r.now = func() time.Time { return at(4, 0) }
		}, false},
		// An autoplaying video or an unread count changes a title and writes a fresh episode with nobody there; a healthy input-idle probe is the better presence signal.
		{"input idle past idleAfter opens the away-gate despite a fresh episode", func(t *testing.T, s *db.Store, r *Runner) {
			r.probes.SessionLocked = func() bool { return false }
			insertEpisodeAt(t, s, at(23, 29))
			var idle idleProbe
			idle.set(idleAfter)
			r.probes.InputIdle = idle.get
		}, true},
		{"an erroring input-idle probe falls back to the fresh episode", func(t *testing.T, s *db.Store, r *Runner) {
			r.probes.SessionLocked = func() bool { return false }
			insertEpisodeAt(t, s, at(23, 29))
			var idle idleProbe
			idle.fail(true)
			r.probes.InputIdle = idle.get
		}, false},
		{"diary missing inside the grace period", nil, false},
		{"night already finished", func(t *testing.T, s *db.Store, r *Runner) {
			ctx := context.Background()
			if err := s.StartDreamRun(ctx, night); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishDreamRun(ctx, night, "done", "done"); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"everything satisfied", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			store := dbtest.Open(t)
			brain := &fakeBrain{verdicts: "[]", extract: "[]", und: "An understanding."}
			r := newRunner(store, brain, yesProbes(), at(23, 30))
			// The evening close's entry exists by default; the missing-diary case skips writing it and, at 23:30, is still inside the two-hour grace.
			if c.name != "diary missing inside the grace period" {
				if err := store.SetDiaryEntry(ctx, night, "day", "A quiet day.\n\nHypotheses:\nHe codes at night. (likely)"); err != nil {
					t.Fatal(err)
				}
			}
			if c.prep != nil {
				c.prep(t, store, r)
			}
			r.Tick(ctx)
			dreamt := len(brain.askedKinds()) > 0
			if dreamt != c.wants {
				t.Errorf("brain asked %v, want dreamt=%v", brain.askedKinds(), c.wants)
			}
			if c.wants {
				if run, ok, _ := store.DreamRun(ctx, night); !ok || !run.Finished {
					t.Errorf("happy path did not finish the run: %+v ok=%v", run, ok)
				}
			}
		})
	}
}

// A resumed night runs only the stages missing from stages_done: with 'hyp' already committed, only the understanding rewrite and the closing diary-writing call are asked for, and the night still finishes.
func TestTick_ResumeSkipsDoneStages(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	if err := store.StartDreamRun(ctx, night); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitHypothesisStage(ctx, night, nil, nil); err != nil {
		t.Fatal(err)
	}

	brain := &fakeBrain{verdicts: "[]", extract: "[]", und: "Rewritten."}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	r.Tick(ctx)

	if asked := brain.askedKinds(); len(asked) != 2 || asked[0] != "und" || asked[1] != "report" {
		t.Errorf("asked = %v, want the understanding rewrite followed by the diary-writing call", asked)
	}
	run, _, _ := store.DreamRun(ctx, night)
	if !run.Finished || run.StagesDone != "hyp und compact procedures lessons prune" {
		t.Errorf("run = %+v, want finished with the remaining stages done", run)
	}
	if entry, _ := store.DiaryEntry(ctx, night, "dream"); !strings.Contains(entry, "already committed on an earlier wake") {
		t.Errorf("dream report does not note the resumed stage: %q", entry)
	}
}

// The full hypothesis stage on a night against a real store: garbage verdicts are dropped, valid ones apply, the adoption list is capped at five valid statements, a bad confidence defaults to low, and a never-tested thirty-day-old hypothesis retires without a verdict.
func TestHypStage_ValidationCapsAndStale(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day.\n\nHypotheses:\nplenty (likely)"); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertHypothesis(ctx, "He codes at night.", "low", nightMinus(night, 10)); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertHypothesis(ctx, "He hates mornings.", "low", nightMinus(night, 40)); err != nil {
		t.Fatal(err)
	}
	open, _ := store.OpenHypotheses(ctx)
	codes := open[0].ID
	if open[0].Statement != "He codes at night." {
		codes = open[1].ID
	}

	// One valid verdict, one for an unknown id, one with a bad enum — only the first survives. Seven proposals, one blank, one over 200 chars — five adopted.
	longStatement := strings.Repeat("x", 201)
	brain := &fakeBrain{
		verdicts: fmt.Sprintf(`[{"id": %d, "verdict": "supported", "confidence": "medium", "evidence": "late commits", "action": "keep"},
			{"id": 999, "verdict": "supported", "confidence": "low", "evidence": "", "action": "keep"},
			{"id": %d, "verdict": "maybe", "confidence": "low", "evidence": "", "action": "keep"}]`, codes, codes),
		extract: fmt.Sprintf(`[{"statement": "One.", "confidence": "low"}, {"statement": "Two.", "confidence": "silly"},
			{"statement": ""}, {"statement": %q}, {"statement": "Three.", "confidence": "medium"},
			{"statement": "Four.", "confidence": "low"}, {"statement": "Five.", "confidence": "low"},
			{"statement": "Six.", "confidence": "low"}]`, longStatement),
		und: "Rewritten.",
	}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	r.Tick(ctx)

	open, _ = store.OpenHypotheses(ctx)
	byStatement := map[string]db.Hypothesis{}
	for _, h := range open {
		byStatement[h.Statement] = h
	}
	if h := byStatement["He codes at night."]; h.TimesTested != 1 || h.Confidence != "medium" || !strings.Contains(h.Evidence, "late commits") {
		t.Errorf("judged hypothesis = %+v", h)
	}
	if _, still := byStatement["He hates mornings."]; still {
		t.Error("stale never-tested hypothesis was not retired")
	}
	for _, s := range []string{"One.", "Two.", "Three.", "Four.", "Five."} {
		if _, ok := byStatement[s]; !ok {
			t.Errorf("adopted hypothesis %q missing", s)
		}
	}
	if _, ok := byStatement["Six."]; ok {
		t.Error("adoption cap of five not enforced")
	}
	if h := byStatement["Two."]; h.Confidence != "low" {
		t.Errorf("bad confidence not defaulted to low: %+v", h)
	}
}

// A verdict reply that never parses is re-asked once, then the stage is skipped but still committed and journaled, and the night carries on to a finish.
func TestTick_WhollyInvalidReplySkipsJudging(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertHypothesis(ctx, "He codes at night.", "low", nightMinus(night, 10)); err != nil {
		t.Fatal(err)
	}
	brain := &fakeBrain{verdicts: "I would rather chat about it.", extract: "[]", und: "Rewritten."}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	r.Tick(ctx)

	asked := brain.askedKinds()
	verdictAsks := 0
	for _, k := range asked {
		if k == "verdicts" {
			verdictAsks++
		}
	}
	if verdictAsks != 2 {
		t.Errorf("verdict call asked %d times, want the original and one re-ask", verdictAsks)
	}
	run, _, _ := store.DreamRun(ctx, night)
	if !run.Finished || !strings.Contains(run.StagesDone, "hyp") {
		t.Errorf("run = %+v, want finished with the skipped stage committed", run)
	}
	open, _ := store.OpenHypotheses(ctx)
	if len(open) != 1 || open[0].TimesTested != 0 {
		t.Errorf("hypotheses were changed by a skipped stage: %+v", open)
	}
	if entry, _ := store.DiaryEntry(ctx, night, "dream"); !strings.Contains(entry, "never parsed") {
		t.Errorf("dream report does not journal the skip: %q", entry)
	}
}

// The morning report: a full happy-path night lands the diary kind='dream' row, stamps finished_at, files the one-line report, and rewrites the understanding.
func TestTick_DreamReportLands(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "The user built the dreaming loop.\n\nHypotheses:\nHe ships at night. (likely)"); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertHypothesis(ctx, "He codes at night.", "low", nightMinus(night, 10)); err != nil {
		t.Fatal(err)
	}
	open, _ := store.OpenHypotheses(ctx)
	brain := &fakeBrain{
		verdicts: fmt.Sprintf(`[{"id": %d, "verdict": "supported", "confidence": "high", "evidence": "late commits", "action": "keep"}]`, open[0].ID),
		extract:  `[{"statement": "He ships at night.", "confidence": "medium"}]`,
		und:      "A person who codes and ships at night.",
	}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	r.Tick(ctx)

	run, ok, _ := store.DreamRun(ctx, night)
	if !ok || !run.Finished {
		t.Fatalf("run not finished: %+v ok=%v", run, ok)
	}
	if !strings.Contains(run.Report, "1 tested") || !strings.Contains(run.Report, "1 adopted") {
		t.Errorf("one-line report = %q", run.Report)
	}
	entry, _ := store.DiaryEntry(ctx, night, "dream")
	if !strings.Contains(entry, "judge-only") || !strings.Contains(entry, "Adopted: He ships at night.") {
		t.Errorf("dream entry = %q", entry)
	}
	if u, _ := store.DiaryEntry(ctx, "", "understanding"); u != "A person who codes and ships at night." {
		t.Errorf("understanding = %q", u)
	}
}

// Preemption: when the watcher cancels mid-stage (the session unlocked), the stage's transaction never commits — no verdicts, no stage token, no finish.
func TestTick_PreemptionCommitsNothing(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertHypothesis(ctx, "He codes at night.", "low", nightMinus(night, 10)); err != nil {
		t.Fatal(err)
	}
	open, _ := store.OpenHypotheses(ctx)

	// The lock is up when the night starts; the brain call simulates the user returning by dropping the lock and then waiting for the watcher to cancel the run.
	var locked flag
	locked.set(true)
	probes := yesProbes()
	probes.SessionLocked = locked.get
	r := New(store, func(ctx context.Context, prompt string) (string, error) {
		locked.set(false)
		<-ctx.Done()
		return fmt.Sprintf(`[{"id": %d, "verdict": "supported", "confidence": "high", "evidence": "x", "action": "keep"}]`, open[0].ID), ctx.Err()
	}, probes, 23, 9)
	r.now = func() time.Time { return at(23, 30) }
	r.watchEvery = time.Millisecond

	r.Tick(ctx)

	run, ok, _ := store.DreamRun(ctx, night)
	if !ok {
		t.Fatal("the run row should exist — preemption struck after the start")
	}
	if run.Finished || run.StagesDone != "" {
		t.Errorf("preempted run committed something: %+v", run)
	}
	open, _ = store.OpenHypotheses(ctx)
	if len(open) != 1 || open[0].TimesTested != 0 {
		t.Errorf("preempted stage changed hypotheses: %+v", open)
	}
	if entry, _ := store.DiaryEntry(ctx, night, "dream"); entry != "" {
		t.Errorf("preempted run wrote a dream entry: %q", entry)
	}
}

// The fallback: with no evening diary entry and the grace period spent, the night dreams from the day's summaries and says so in the report.
func TestTick_MissingDiaryFallsBackAfterGrace(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	// 01:30 belongs to yesterday's night, two and a half hours past a 23:00 dream hour.
	now := at(1, 30)
	night := now.AddDate(0, 0, -1).Format(time.DateOnly)
	if err := store.InsertHypothesis(ctx, "He codes at night.", "low", nightMinus(night, 10)); err != nil {
		t.Fatal(err)
	}
	brain := &fakeBrain{verdicts: "[]", extract: "[]", und: "Rewritten."}
	r := newRunner(store, brain, yesProbes(), now)
	r.Tick(ctx)

	run, ok, _ := store.DreamRun(ctx, night)
	if !ok || !run.Finished {
		t.Fatalf("fallback night did not run: %+v ok=%v", run, ok)
	}
	if entry, _ := store.DiaryEntry(ctx, night, "dream"); !strings.Contains(entry, "summaries stood in") {
		t.Errorf("dream report does not journal the fallback: %q", entry)
	}
}

// flag is the mutex-guarded bool the preemption test shares between the brain closure and the watcher goroutine.
type flag struct {
	mu sync.Mutex
	v  bool
}

func (f *flag) set(v bool) { f.mu.Lock(); f.v = v; f.mu.Unlock() }
func (f *flag) get() bool  { f.mu.Lock(); defer f.mu.Unlock(); return f.v }

// idleProbe is the mutex-guarded fake InputIdle probe: v is the reported idle duration, failing makes it return an error instead — the way the tests drive the "probe unhealthy, fall back to the episode check" path.
type idleProbe struct {
	mu      sync.Mutex
	v       time.Duration
	failing bool
}

func (p *idleProbe) set(v time.Duration) { p.mu.Lock(); defer p.mu.Unlock(); p.v = v }
func (p *idleProbe) fail(b bool)         { p.mu.Lock(); defer p.mu.Unlock(); p.failing = b }
func (p *idleProbe) get() (time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failing {
		return 0, errors.New("fake probe error")
	}
	return p.v, nil
}

// While the input-idle probe stays healthy and idle, a new episode arriving mid-run (a title changing on its own) must not preempt the night — that is the whole point of the fix.
func TestWatcher_DoesNotPreemptOnEpisodeWhileInputStaysIdle(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}

	probes := yesProbes()
	probes.SessionLocked = func() bool { return false }
	var idle idleProbe
	idle.set(time.Hour) // stays comfortably above idleAfter and inputFreshAfter throughout
	probes.InputIdle = idle.get

	var once sync.Once
	brain := &fakeBrain{verdicts: "[]", extract: "[]", und: "Rewritten."}
	wrapped := func(ctx context.Context, prompt string) (string, error) {
		once.Do(func() {
			insertEpisodeAt(t, store, at(23, 35)) // after the run's baseline
			time.Sleep(20 * time.Millisecond)     // give the watcher several ticks to (wrongly) act on it
		})
		return brain.fn(ctx, prompt)
	}

	r := New(store, wrapped, probes, 23, 9)
	r.now = func() time.Time { return at(23, 30) }
	r.watchEvery = time.Millisecond
	r.retention = func() (int, time.Duration) { return testActRunKeep, testFailedGrace }

	r.Tick(ctx)

	run, ok, _ := store.DreamRun(ctx, night)
	if !ok || !run.Finished {
		t.Errorf("the night should have finished undisturbed: %+v ok=%v", run, ok)
	}
}

// When the input-idle probe reports fresh input (below inputFreshAfter), the watcher preempts — real input, not a title change, is what should wake the night.
func TestWatcher_PreemptsWhenInputIdleDropsFresh(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertHypothesis(ctx, "He codes at night.", "low", nightMinus(night, 10)); err != nil {
		t.Fatal(err)
	}
	open, _ := store.OpenHypotheses(ctx)

	probes := yesProbes()
	probes.SessionLocked = func() bool { return false } // unlocked from the start: InputIdle is the away signal here
	var idle idleProbe
	idle.set(idleAfter)
	probes.InputIdle = idle.get

	r := New(store, func(ctx context.Context, prompt string) (string, error) {
		idle.set(3 * time.Second) // real input just arrived
		<-ctx.Done()
		return fmt.Sprintf(`[{"id": %d, "verdict": "supported", "confidence": "high", "evidence": "x", "action": "keep"}]`, open[0].ID), ctx.Err()
	}, probes, 23, 9)
	r.now = func() time.Time { return at(23, 30) }
	r.watchEvery = time.Millisecond

	r.Tick(ctx)

	run, ok, _ := store.DreamRun(ctx, night)
	if !ok {
		t.Fatal("the run row should exist — preemption struck after the start")
	}
	if run.Finished || run.StagesDone != "" {
		t.Errorf("preempted run committed something: %+v", run)
	}
}

// setDiary is the compaction tests' shorthand for seeding one diary row.
func setDiary(t *testing.T, store *db.Store, day, kind, content string) {
	t.Helper()
	if err := store.SetDiaryEntry(context.Background(), day, kind, content); err != nil {
		t.Fatalf("SetDiaryEntry(%s, %s): %v", day, kind, err)
	}
}

// The week tier: a complete Mon-Sun week of dailies older than the horizon collapses into one kind='week' entry on the Monday and its dailies are deleted, while an incomplete week, recent dailies, and the diary's other kinds are all left alone.
func TestCompactStage_CompleteWeekCollapses(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := "2026-08-30"
	// 2026-08-03 is a Monday; the full week 03..09 is complete and old. The week of the 10th misses its Sunday (the 16th) and must wait.
	for d := 0; d < 7; d++ {
		setDiary(t, store, nightMinus("2026-08-03", -d), "day", fmt.Sprintf("Day %d of the complete week.", d))
	}
	for d := 0; d < 6; d++ {
		setDiary(t, store, nightMinus("2026-08-10", -d), "day", "A day of the incomplete week.")
	}
	setDiary(t, store, "2026-08-28", "day", "A recent day inside the seven-day horizon.")
	setDiary(t, store, "2026-08-04", "dream", "A morning report.")
	setDiary(t, store, "2026-08-05", "brief", "A brief.")
	setDiary(t, store, "", "understanding", "The standing doc.")

	brain := &fakeBrain{compact: "A remembered week."}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	if err := store.StartDreamRun(ctx, night); err != nil {
		t.Fatal(err)
	}
	rep, err := r.compactStage(ctx, night)
	if err != nil {
		t.Fatalf("compactStage: %v", err)
	}
	if rep.weeks != 1 || rep.months != 0 {
		t.Errorf("report = %+v, want exactly one week compacted", rep)
	}
	if got, _ := store.DiaryEntry(ctx, "2026-08-03", "week"); got != "A remembered week." {
		t.Errorf("week entry = %q", got)
	}
	// The dailies are kept and reparented under the week rather than deleted, so they stay readable; DiaryEntriesThrough is what puts them out of the next night's reach.
	for d := 0; d < 7; d++ {
		if got, _ := store.DiaryEntry(ctx, nightMinus("2026-08-03", -d), "day"); got == "" {
			t.Errorf("constituent daily %d was destroyed by the compaction", d)
		}
	}
	if left, err := store.DiaryEntriesThrough(ctx, "day", "2026-08-09"); err != nil {
		t.Fatal(err)
	} else if len(left) != 0 {
		t.Errorf("a compacted week's dailies are still offered for compaction: %+v", left)
	}
	for d := 0; d < 6; d++ {
		if got, _ := store.DiaryEntry(ctx, nightMinus("2026-08-10", -d), "day"); got == "" {
			t.Error("an incomplete week's daily was deleted")
		}
	}
	if got, _ := store.DiaryEntry(ctx, "2026-08-28", "day"); got == "" {
		t.Error("a recent daily was deleted")
	}
	for _, k := range [][2]string{{"2026-08-04", "dream"}, {"2026-08-05", "brief"}, {"", "understanding"}} {
		if got, _ := store.DiaryEntry(ctx, k[0], k[1]); got == "" {
			t.Errorf("protected kind %q was touched by compaction", k[1])
		}
	}
	run, _, _ := store.DreamRun(ctx, night)
	if run.StagesDone != "compact" {
		t.Errorf("stages_done = %q, want the compact token committed", run.StagesDone)
	}
}

// Night traces: every brain call the dream makes appends one JSONL line — kind and raw reply — to the run's session.jsonl under <DataDir>/dreams/<night>/.
func TestTraces_OneLinePerBrainCall(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day.\n\nHypotheses:\nHe codes at night. (likely)"); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertHypothesis(ctx, "He codes at night.", "low", nightMinus(night, 10)); err != nil {
		t.Fatal(err)
	}
	open, _ := store.OpenHypotheses(ctx)
	brain := &fakeBrain{
		verdicts: fmt.Sprintf(`[{"id": %d, "verdict": "supported", "confidence": "high", "evidence": "late commits", "action": "keep"}]`, open[0].ID),
		extract:  `[]`,
		und:      "Thinking about it...\n\nA person who codes at night.",
	}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	r.DataDir = t.TempDir()
	r.Tick(ctx)

	raw, err := os.ReadFile(filepath.Join(r.DataDir, "dreams", night+".jsonl"))
	if err != nil {
		t.Fatalf("reading the night's trace file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != len(brain.askedKinds()) {
		t.Fatalf("%d trace lines for %d brain calls", len(lines), len(brain.askedKinds()))
	}
	kinds := map[string]string{}
	for _, l := range lines {
		var rec struct {
			At    string `json:"at"`
			Kind  string `json:"kind"`
			Reply string `json:"reply"`
		}
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("trace line is not JSON: %q: %v", l, err)
		}
		if rec.At == "" || rec.Kind == "" {
			t.Errorf("trace line missing at/kind: %q", l)
		}
		kinds[rec.Kind] = rec.Reply
	}
	for _, want := range []string{"verdicts", "extract", "understanding"} {
		if kinds[want] == "" {
			t.Errorf("no trace with a reply for the %q call: %v", want, kinds)
		}
	}
	// The raw reply lands verbatim, thinking text included, before any parsing strips it.
	if !strings.Contains(kinds["understanding"], "Thinking about it...") {
		t.Errorf("the understanding trace lost the model's thinking text: %q", kinds["understanding"])
	}
}

// A shadow brain gets fired alongside every primary call and its reply lands under kind+"-shadow" in the same night's trace file — but a shadow that errors out never fails a stage, since it never feeds anything the night acts on.
func TestShadow_TracesAlongsidePrimaryAndNeverFailsAStage(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day.\n\nHypotheses:\nHe codes at night. (likely)"); err != nil {
		t.Fatal(err)
	}
	primary := &fakeBrain{verdicts: "[]", extract: "[]", und: "An understanding."}
	r := newRunner(store, primary, yesProbes(), at(23, 30))
	r.DataDir = t.TempDir()
	// The shadow always errors — proving its failure is only logged, never propagated.
	r.Shadow = func(ctx context.Context, prompt string) (string, error) {
		return "", errors.New("the local model choked")
	}

	r.Tick(ctx)

	run, ok, _ := store.DreamRun(ctx, night)
	if !ok || !run.Finished {
		t.Fatalf("a failing shadow must not stop the night from finishing: %+v ok=%v", run, ok)
	}

	raw, err := os.ReadFile(filepath.Join(r.DataDir, "dreams", night+".jsonl"))
	if err != nil {
		t.Fatalf("reading the night's trace file: %v", err)
	}
	var shadowKinds, primaryKinds int
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec struct {
			Kind  string `json:"kind"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("trace line is not JSON: %q: %v", l, err)
		}
		if strings.HasSuffix(rec.Kind, "-shadow") {
			shadowKinds++
			if rec.Error == "" {
				t.Errorf("shadow trace line missing the error the fake shadow returned: %q", l)
			}
		} else {
			primaryKinds++
		}
	}
	if shadowKinds == 0 {
		t.Fatal("no -shadow trace lines were written")
	}
	if shadowKinds != primaryKinds {
		t.Errorf("%d shadow lines for %d primary lines, want one shadow line per primary call", shadowKinds, primaryKinds)
	}
}

// A lifecycle whose Start succeeds is stopped exactly once, after the night's stages, whether or not a Shadow ever answered anything useful. GPUReleaser is asked to free the card before the shadow lifecycle starts, since the embedding server may still be sitting on the GPU the shadow needs.
func TestShadowLifecycle_StartSucceeds_StopRunsAfterNight(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	primary := &fakeBrain{verdicts: "[]", extract: "[]", und: "An understanding."}
	r := newRunner(store, primary, yesProbes(), at(23, 30))
	r.DataDir = t.TempDir()
	r.Shadow = func(ctx context.Context, prompt string) (string, error) { return "ok", nil }

	released, startedAfterReleased := false, false
	r.GPUReleaser = func() bool {
		released = true
		return true
	}
	started, stopped := false, false
	r.ShadowLifecycle = ShadowLifecycle{
		Start: func(ctx context.Context) error {
			if stopped {
				t.Error("Start observed after Stop already ran")
			}
			startedAfterReleased = released
			started = true
			return nil
		},
		Stop: func() {
			if !started {
				t.Error("Stop ran without a prior successful Start")
			}
			stopped = true
		},
	}

	r.Tick(ctx)

	if !started || !stopped {
		t.Errorf("started=%v stopped=%v, want both true", started, stopped)
	}
	if !released {
		t.Error("GPUReleaser was never called")
	}
	if !startedAfterReleased {
		t.Error("ShadowLifecycle.Start ran before GPUReleaser was called")
	}
}

// A week that fails used to take every week already compacted down with it: compactEntry's error returned before CommitCompactStage ran, so the brain calls for the earlier weeks were paid for and thrown away, every night, forever. The weeks already built are now committed before the error goes up.
func TestCompactStage_CommitsTheWeeksBuiltBeforeAFailure(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := "2026-08-30"
	// Two complete, old weeks: 2026-07-06 and 2026-07-13, both Mondays. The compaction of the second one fails.
	for _, monday := range []string{"2026-07-06", "2026-07-13"} {
		for d := 0; d < 7; d++ {
			setDiary(t, store, nightMinus(monday, -d), "day", fmt.Sprintf("A day of the week of %s.", monday))
		}
	}

	brain := &fakeBrain{compact: "A remembered week.", compactFailAfter: 1}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	if err := store.StartDreamRun(ctx, night); err != nil {
		t.Fatal(err)
	}
	rep, err := r.compactStage(ctx, night)
	if err == nil {
		t.Fatal("compactStage swallowed the failing week")
	}
	if rep.weeks != 1 {
		t.Errorf("report says %d weeks compacted, want the one that succeeded", rep.weeks)
	}
	if got, _ := store.DiaryEntry(ctx, "2026-07-06", "week"); got != "A remembered week." {
		t.Errorf("the week compacted before the failure was thrown away: %q", got)
	}
}

// TestShadow_AnswersTheNightWhenThePrimaryBrainIsDown is the real failure this machine hit: the dream brain (grok) returned "402 Payment Required" on every call from 2026-09-03 onward, so every stage aborted and four nights in a row committed nothing — while the local shadow model answered all 174 of those calls and its replies were thrown away by design. When the primary is down and a shadow is running, the shadow's reply is what the night uses.
func TestShadow_AnswersTheNightWhenThePrimaryBrainIsDown(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day.\n\nHypotheses:\nHe codes at night. (likely)"); err != nil {
		t.Fatal(err)
	}
	down := &fakeBrain{}
	r := newRunner(store, down, yesProbes(), at(23, 30))
	r.brain = func(ctx context.Context, prompt string) (string, error) {
		return "", errors.New("API error (status 402 Payment Required)")
	}
	local := &fakeBrain{verdicts: "[]", extract: `[{"statement":"He reads the changelog before upgrading anything.","confidence":"medium"}]`, und: "An understanding the local model wrote.", compact: "A week.", report: "The night, in one line."}
	r.Shadow = local.fn

	r.Tick(ctx)

	run, ok, err := store.DreamRun(ctx, night)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !run.Finished {
		t.Fatalf("the night must finish on the shadow's answers when the primary brain is down: %+v ok=%v", run, ok)
	}
	open, err := store.OpenHypotheses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var adopted bool
	for _, h := range open {
		if strings.Contains(h.Statement, "changelog") {
			adopted = true
		}
	}
	if !adopted {
		t.Errorf("the shadow's adopted hypothesis never landed: %+v", open)
	}
	doc, err := store.DiaryEntry(ctx, "", "understanding")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, "local model wrote") {
		t.Errorf("the understanding doc was not rewritten from the shadow's reply, got %q", doc)
	}
}

// TestDream_StopsRetryingANightWhoseBrainKeepsFailing bounds the retry storm behind the same outage: the daemon ticks every five minutes, so a stage that aborts on a dead brain was re-attempted 76 times in one night (2026-09-03's trace). After failedAttemptCap aborts the night is left for tomorrow.
func TestDream_StopsRetryingANightWhoseBrainKeepsFailing(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	var calls int
	down := &fakeBrain{}
	r := newRunner(store, down, yesProbes(), at(23, 30))
	r.brain = func(ctx context.Context, prompt string) (string, error) {
		calls++
		return "", errors.New("API error (status 402 Payment Required)")
	}

	for i := 0; i < 12; i++ {
		r.Tick(ctx)
	}

	if calls > failedAttemptCap {
		t.Errorf("a dead brain cost %d calls across 12 ticks, want no more than %d", calls, failedAttemptCap)
	}
	if calls == 0 {
		t.Error("the night never tried at all")
	}
}
