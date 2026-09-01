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

	"ora/internal/db"
)

// testStore opens a throwaway store on a per-test temp file, closed with the test. File-backed rather than ":memory:" because the preemption watcher queries concurrently with the stages, and plain :memory: is per-connection in database/sql + modernc/sqlite — a second pool connection would see an empty database.
func testStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(filepath.Join(t.TempDir(), "dream-test.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// fakeBrain answers each of the dream prompts with a canned reply, dispatching on the instruction text, and records what it was asked. Safe for the watcher goroutine's world: only Tick's goroutine calls it, but the mutex keeps the record readable after Tick returns.
type fakeBrain struct {
	mu        sync.Mutex
	verdicts  string
	extract   string
	und       string
	compact   string
	report    string
	reportErr error
	asked     []string
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

// newRunner builds a Runner on the fake brain with the clock pinned to now.
func newRunner(store *db.Store, b *fakeBrain, probes Probes, now time.Time) *Runner {
	r := New(store, b.fn, probes, 23, 9)
	r.now = func() time.Time { return now }
	r.watchEvery = time.Hour
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

// The night key folds the small hours back onto the evening's date, and the window is [dreamHour, briefHour) across midnight.
func TestNightKeyAndWindow(t *testing.T) {
	r := &Runner{dreamHour: 23, briefHour: 9}
	base := time.Date(2026, 8, 30, 0, 0, 0, 0, time.Local)
	cases := []struct {
		hour     int
		inWindow bool
		night    string
	}{
		{22, false, "2026-08-29"},
		{23, true, "2026-08-30"},
		{0, true, "2026-08-29"},
		{8, true, "2026-08-29"},
		{9, false, "2026-08-29"},
		{12, false, "2026-08-29"},
	}
	for _, c := range cases {
		now := base.Add(time.Duration(c.hour) * time.Hour)
		if got := r.inWindow(now.Hour()); got != c.inWindow {
			t.Errorf("inWindow(%02d:00) = %v, want %v", c.hour, got, c.inWindow)
		}
		if got := r.nightKey(now); got != c.night {
			t.Errorf("nightKey(%02d:00) = %q, want %q", c.hour, got, c.night)
		}
	}

	// A window that does not wrap midnight still works.
	r = &Runner{dreamHour: 1, briefHour: 9}
	if r.inWindow(0) || !r.inWindow(1) || !r.inWindow(8) || r.inWindow(9) {
		t.Error("non-wrapping window [1,9) misbehaves")
	}
}

// Each start condition individually blocks the run: no brain call, no run row. The final case proves the same setup does dream once nothing blocks.
func TestTick_ConditionsGate(t *testing.T) {
	night := at(23, 30).Format(dayFormat)

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
			store := testStore(t)
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
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
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
	if !run.Finished || run.StagesDone != "hyp und compact" {
		t.Errorf("run = %+v, want finished with the remaining stages done", run)
	}
	if entry, _ := store.DiaryEntry(ctx, night, "dream"); !strings.Contains(entry, "already committed on an earlier wake") {
		t.Errorf("dream report does not note the resumed stage: %q", entry)
	}
}

// decide is the mechanics: the judge recommends, Go enforces the promotion age and test count, the second contradiction, and passthrough for everything else.
func TestDecideMechanics(t *testing.T) {
	night := "2026-08-29"
	old := nightMinus(night, 10)
	young := nightMinus(night, 2)
	cases := []struct {
		name       string
		h          db.Hypothesis
		v          rawVerdict
		wantStatus string
		wantNote   bool
	}{
		{"kept open on support", db.Hypothesis{ID: 1, Born: old, TimesTested: 1}, rawVerdict{ID: 1, Verdict: "supported", Confidence: "medium", Action: "keep"}, "open", false},
		{"promotion granted when tested and old", db.Hypothesis{ID: 1, Born: old, TimesTested: 2}, rawVerdict{ID: 1, Verdict: "supported", Confidence: "high", Action: "promote"}, "promoted", false},
		{"promotion refused when young", db.Hypothesis{ID: 1, Born: young, TimesTested: 5}, rawVerdict{ID: 1, Verdict: "supported", Confidence: "high", Action: "promote"}, "open", true},
		{"promotion refused when undertested", db.Hypothesis{ID: 1, Born: old, TimesTested: 0}, rawVerdict{ID: 1, Verdict: "supported", Confidence: "high", Action: "promote"}, "open", true},
		{"first contradiction stays open", db.Hypothesis{ID: 1, Born: old, TimesTested: 1}, rawVerdict{ID: 1, Verdict: "contradicted", Confidence: "low", Action: "keep"}, "open", false},
		{"second contradiction retires regardless", db.Hypothesis{ID: 1, Born: old, TimesTested: 1, Evidence: "[2026-08-25] contradicted — he slept early"}, rawVerdict{ID: 1, Verdict: "contradicted", Confidence: "low", Action: "keep"}, "retired", false},
		{"judge may retire", db.Hypothesis{ID: 1, Born: old, TimesTested: 1}, rawVerdict{ID: 1, Verdict: "unclear", Confidence: "low", Action: "retire"}, "retired", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			update, note := decide(c.h, c.v, night)
			if update.Status != c.wantStatus {
				t.Errorf("status = %q, want %q", update.Status, c.wantStatus)
			}
			if (note != "") != c.wantNote {
				t.Errorf("note = %q, wantNote=%v", note, c.wantNote)
			}
			if !update.Tested || update.LastTested != night {
				t.Errorf("verdict not marked tested tonight: %+v", update)
			}
		})
	}
}

// The full hypothesis stage against a real store: garbage verdicts are dropped, valid ones apply, the adoption list is capped at five valid statements, and a never-tested thirty-day-old hypothesis retires without a verdict.
func TestHypStage_ValidationCapsAndStale(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
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
	}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	if err := store.StartDreamRun(ctx, night); err != nil {
		t.Fatal(err)
	}
	rep, err := r.hypStage(ctx, night, false)
	if err != nil {
		t.Fatalf("hypStage: %v", err)
	}
	if rep.tested != 1 || rep.adopted != 5 || rep.retired != 1 {
		t.Errorf("report = %+v, want 1 tested, 5 adopted, 1 stale-retired", rep)
	}

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
	run, _, _ := store.DreamRun(ctx, night)
	if run.StagesDone != "hyp" {
		t.Errorf("stages_done = %q, want hyp", run.StagesDone)
	}
}

// A verdict reply that never parses is re-asked once, then the stage is skipped but still committed and journaled, and the night carries on to a finish.
func TestTick_WhollyInvalidReplySkipsJudging(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
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
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
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
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
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
	store := testStore(t)
	// 01:30 belongs to yesterday's night, two and a half hours past a 23:00 dream hour.
	now := at(1, 30)
	night := now.AddDate(0, 0, -1).Format(dayFormat)
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

// A healthy input-idle probe reporting idleAfter or more opens the away-gate even though the newest episode is a minute old — the fix for autoplay/unread-count title changes wrongly reading as presence.
func TestTick_InputIdleOpensAwayGateDespiteFreshEpisode(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	insertEpisodeAt(t, store, at(23, 29)) // one minute old: fresh enough to block the old heuristic

	brain := &fakeBrain{verdicts: "[]", extract: "[]", und: "Rewritten."}
	probes := yesProbes()
	probes.SessionLocked = func() bool { return false }
	var idle idleProbe
	idle.set(idleAfter)
	probes.InputIdle = idle.get

	r := newRunner(store, brain, probes, at(23, 30))
	r.Tick(ctx)

	if len(brain.askedKinds()) == 0 {
		t.Error("a healthy input-idle probe at idleAfter did not open the away-gate despite a fresh episode")
	}
}

// A probe that errors on every call (as if the D-Bus service is unreachable) falls back to the episode heuristic exactly like a nil probe.
func TestTick_InputIdleErrorFallsBackToEpisodeHeuristic(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	insertEpisodeAt(t, store, at(23, 29))

	brain := &fakeBrain{verdicts: "[]", extract: "[]", und: "Rewritten."}
	probes := yesProbes()
	probes.SessionLocked = func() bool { return false }
	var idle idleProbe
	idle.fail(true)
	probes.InputIdle = idle.get

	r := newRunner(store, brain, probes, at(23, 30))
	r.Tick(ctx)

	if len(brain.askedKinds()) != 0 {
		t.Error("an erroring InputIdle probe should fall back to the episode heuristic and block on a fresh episode")
	}
}

// While the input-idle probe stays healthy and idle, a new episode arriving mid-run (a title changing on its own) must not preempt the night — that is the whole point of the fix.
func TestWatcher_DoesNotPreemptOnEpisodeWhileInputStaysIdle(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
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

	r.Tick(ctx)

	run, ok, _ := store.DreamRun(ctx, night)
	if !ok || !run.Finished {
		t.Errorf("the night should have finished undisturbed: %+v ok=%v", run, ok)
	}
}

// The regression this replaces: with InputIdle nil, a new episode arriving mid-run still preempts, exactly as before the fix.
func TestWatcher_NilInputIdlePreemptsOnEpisode(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertHypothesis(ctx, "He codes at night.", "low", nightMinus(night, 10)); err != nil {
		t.Fatal(err)
	}
	open, _ := store.OpenHypotheses(ctx)

	probes := yesProbes()
	probes.SessionLocked = func() bool { return false } // InputIdle left nil

	r := New(store, func(ctx context.Context, prompt string) (string, error) {
		insertEpisodeAt(t, store, at(23, 35))
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

// When the input-idle probe reports fresh input (below inputFreshAfter), the watcher preempts — real input, not a title change, is what should wake the night.
func TestWatcher_PreemptsWhenInputIdleDropsFresh(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
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

// The force marker makes a tick dream immediately with the away-gates bypassed, and is consumed so one touch means one run.
func TestTick_ForceMarkerBypassesGates(t *testing.T) {
	store := testStore(t)
	if err := store.SetDiaryEntry(context.Background(), at(23, 30).Format(dayFormat), "day", "A day."); err != nil {
		t.Fatal(err)
	}
	brain := &fakeBrain{verdicts: "[]", extract: "[]", und: "An understanding."}
	probes := Probes{OnAC: func() bool { return false }, SessionLocked: func() bool { return false }, RecorderQuiescent: func() bool { return false }}
	r := newRunner(store, brain, probes, at(23, 30))
	marker := filepath.Join(t.TempDir(), "dream-now")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r.ForceMarker = marker

	r.Tick(context.Background())

	if len(brain.askedKinds()) == 0 {
		t.Error("a forced tick must dream despite every away-gate failing")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("the force marker must be consumed by the run")
	}
}

// Past the Claude curfew (03:25) an unforced dream must not start: an overnight five-hour usage window opened after it would bleed into the user's 08:30 workday window.
func TestTick_CurfewHoldsTheNight(t *testing.T) {
	store := testStore(t)
	night := at(4, 0).AddDate(0, 0, -1).Format(dayFormat)
	if err := store.SetDiaryEntry(context.Background(), night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	brain := &fakeBrain{verdicts: "[]", extract: "[]", und: "An understanding."}
	r := newRunner(store, brain, yesProbes(), at(4, 0))

	r.Tick(context.Background())

	if len(brain.askedKinds()) != 0 {
		t.Errorf("a 04:00 tick made brain calls %v, want none past the curfew", brain.askedKinds())
	}
}

// The grounded evidence carries all four labelled sections — the diary, the week's work summaries (with the compiler's raw-log fallback buckets skipped), the active threads, and the week's meeting minutes — and the extraction view keeps only the diary.
func TestEvidenceMaterial_AllFourSectionsPresent(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.SetDiaryEntry(ctx, night, "day", "DIARYTEXT about the day."); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{
		`{"task_name": "Ora dreaming loop", "summary": "WORKTEXT built the compactor"}`,
		`{"task_name": "Raw Activity Log", "summary": "RAWLOGTEXT app|title noise"}`,
	} {
		if _, err := store.DB().Exec(`INSERT INTO nodes (type, content, created_at) VALUES ('summary', ?, datetime('now','-2 hours'))`, content); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`INSERT INTO threads (subject, kind, state) VALUES ('THREADTEXT ora', 'project', 'mid-flight')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogNote(ctx, "MEETINGTEXT standup minutes.", "meeting"); err != nil {
		t.Fatal(err)
	}

	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	ev, err := r.evidenceMaterial(ctx, night, false)
	if err != nil {
		t.Fatalf("evidenceMaterial: %v", err)
	}
	for _, want := range []string{"Diary:", "The week's work:", "Ongoing threads:", "Meetings:", "DIARYTEXT", "WORKTEXT", "THREADTEXT", "mid-flight", "MEETINGTEXT"} {
		if !strings.Contains(ev.full, want) {
			t.Errorf("full evidence lacks %q:\n%s", want, ev.full)
		}
	}
	if strings.Contains(ev.full, "RAWLOGTEXT") {
		t.Error("the Raw Activity Log bucket must be skipped")
	}
	if !ev.haveDailies {
		t.Error("haveDailies must be true with a diary entry on file")
	}
	if !strings.Contains(ev.diary, "DIARYTEXT") || strings.Contains(ev.diary, "Ongoing threads:") || strings.Contains(ev.diary, "THREADTEXT") || strings.Contains(ev.diary, "MEETINGTEXT") {
		t.Errorf("the extraction view must be diary-only:\n%s", ev.diary)
	}
}

// The evidence budget holds: when the week's material overflows ~24KB, the oldest items fall away and the newest survive.
func TestEvidenceMaterial_BudgetDropsOldestFirst(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
	big := strings.Repeat("filler sentence to bloat the entry. ", 280)
	if err := store.SetDiaryEntry(ctx, nightMinus(night, 3), "day", "OLDESTMARK "+big); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDiaryEntry(ctx, nightMinus(night, 2), "day", "MIDMARK "+big); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDiaryEntry(ctx, nightMinus(night, 1), "day", "LATERMARK "+big); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDiaryEntry(ctx, night, "day", "NEWESTMARK a small entry."); err != nil {
		t.Fatal(err)
	}

	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	ev, err := r.evidenceMaterial(ctx, night, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.full) > evidenceBudget+512 {
		t.Errorf("evidence is %d bytes, want at most the ~%d budget", len(ev.full), evidenceBudget)
	}
	if strings.Contains(ev.full, "OLDESTMARK") {
		t.Error("the oldest entry must be the one truncated away")
	}
	for _, want := range []string{"MIDMARK", "LATERMARK", "NEWESTMARK"} {
		if !strings.Contains(ev.full, want) {
			t.Errorf("newer entry %q must survive the budget", want)
		}
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
	store := testStore(t)
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
	for d := 0; d < 7; d++ {
		if got, _ := store.DiaryEntry(ctx, nightMinus("2026-08-03", -d), "day"); got != "" {
			t.Errorf("constituent daily %d survived the compaction: %q", d, got)
		}
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

// The month tier: once every Monday of a month holds a week entry older than ten weeks, the weeks collapse into one kind='month' entry on the first; a month missing one of its Mondays waits.
func TestCompactStage_MonthTierCollapsesCompleteMonths(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := "2026-08-30"
	// March 2026's Mondays are the 2nd, 9th, 16th, 23rd and 30th, all beyond the ten-week horizon. April misses the 27th, so it waits.
	march := []string{"2026-03-02", "2026-03-09", "2026-03-16", "2026-03-23", "2026-03-30"}
	for _, m := range march {
		setDiary(t, store, m, "week", "The week of "+m+".")
	}
	for _, m := range []string{"2026-04-06", "2026-04-13", "2026-04-20"} {
		setDiary(t, store, m, "week", "The week of "+m+".")
	}

	brain := &fakeBrain{compact: "A remembered month."}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	if err := store.StartDreamRun(ctx, night); err != nil {
		t.Fatal(err)
	}
	rep, err := r.compactStage(ctx, night)
	if err != nil {
		t.Fatalf("compactStage: %v", err)
	}
	if rep.weeks != 0 || rep.months != 1 {
		t.Errorf("report = %+v, want exactly one month compacted", rep)
	}
	if got, _ := store.DiaryEntry(ctx, "2026-03-01", "month"); got != "A remembered month." {
		t.Errorf("month entry = %q", got)
	}
	for _, m := range march {
		if got, _ := store.DiaryEntry(ctx, m, "week"); got != "" {
			t.Errorf("constituent week %s survived the compaction: %q", m, got)
		}
	}
	if got, _ := store.DiaryEntry(ctx, "2026-04-01", "month"); got != "" {
		t.Error("an incomplete month was compacted")
	}
	if got, _ := store.DiaryEntry(ctx, "2026-04-06", "week"); got == "" {
		t.Error("an incomplete month's week entry was deleted")
	}
}

// A quiet night — nothing old enough to compact — makes no brain calls and still commits the stage token with zero diary writes.
func TestCompactStage_QuietNightCommitsToken(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
	setDiary(t, store, night, "day", "Tonight's entry.")

	brain := &fakeBrain{}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	if err := store.StartDreamRun(ctx, night); err != nil {
		t.Fatal(err)
	}
	rep, err := r.compactStage(ctx, night)
	if err != nil {
		t.Fatalf("compactStage: %v", err)
	}
	if rep.weeks != 0 || rep.months != 0 || len(brain.askedKinds()) != 0 {
		t.Errorf("quiet night compacted %+v with calls %v, want nothing", rep, brain.askedKinds())
	}
	run, _, _ := store.DreamRun(ctx, night)
	if run.StagesDone != "compact" {
		t.Errorf("stages_done = %q, want the token committed on a quiet night", run.StagesDone)
	}
	if got, _ := store.DiaryEntry(ctx, night, "day"); got == "" {
		t.Error("a quiet night must write nothing and delete nothing")
	}
}

// Night traces: every brain call the dream makes appends one JSONL line — kind and raw reply — to the run's session.jsonl under <DataDir>/dreams/<night>/.
func TestTraces_OneLinePerBrainCall(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
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

// A trace directory that cannot be created never fails a stage: the night still finishes.
func TestTraces_FailureIsBestEffort(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	brain := &fakeBrain{verdicts: "[]", extract: "[]", und: "An understanding."}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	// A regular file where the data dir should be makes every MkdirAll under it fail.
	r.DataDir = filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(r.DataDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	if run, ok, _ := store.DreamRun(ctx, night); !ok || !run.Finished {
		t.Errorf("a failing trace write must not fail the night: %+v ok=%v", run, ok)
	}
}

// A model that says a sentence and then answers still gets its JSON read: the payload between the outermost brackets is the answer.
func TestAskJSON_RecoversPaddedArrays(t *testing.T) {
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.SetDiaryEntry(context.Background(), night, "day", "A day.\n\nHypotheses:\nHe codes at night. (likely)"); err != nil {
		t.Fatal(err)
	}
	brain := &fakeBrain{verdicts: "Here are my verdicts:\n[]", extract: "Sure!\n```json\n[{\"statement\":\"He prefers evenings for deep work.\",\"confidence\":\"low\"}]\n```", und: "An understanding."}
	r := newRunner(store, brain, yesProbes(), at(23, 30))

	r.Tick(context.Background())

	open, err := store.OpenHypotheses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("got %d adopted hypotheses, want the padded reply recovered and adopted", len(open))
	}
}

// A shadow brain gets fired alongside every primary call and its reply lands under kind+"-shadow" in the same night's trace file — but a shadow that errors out never fails a stage, since it never feeds anything the night acts on.
func TestShadow_TracesAlongsidePrimaryAndNeverFailsAStage(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
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

// A lifecycle whose Start fails leaves the night running without a shadow: no -shadow trace lines appear, and the primary stages still complete normally.
func TestShadowLifecycle_StartFailureRunsShadowless(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	primary := &fakeBrain{verdicts: "[]", extract: "[]", und: "An understanding."}
	r := newRunner(store, primary, yesProbes(), at(23, 30))
	r.DataDir = t.TempDir()
	shadowCalled := false
	r.Shadow = func(ctx context.Context, prompt string) (string, error) {
		shadowCalled = true
		return "should never run", nil
	}
	r.ShadowLifecycle = ShadowLifecycle{
		Start: func(ctx context.Context) error { return errors.New("the local server never came up") },
		Stop:  func() { t.Error("Stop must not run when Start failed") },
	}

	r.Tick(ctx)

	if shadowCalled {
		t.Error("the shadow brain was called even though its lifecycle failed to start")
	}
	run, ok, _ := store.DreamRun(ctx, night)
	if !ok || !run.Finished {
		t.Fatalf("a lifecycle start failure must not stop the night from finishing: %+v ok=%v", run, ok)
	}
	raw, err := os.ReadFile(filepath.Join(r.DataDir, "dreams", night+".jsonl"))
	if err != nil {
		t.Fatalf("reading the night's trace file: %v", err)
	}
	if strings.Contains(string(raw), "-shadow") {
		t.Error("no -shadow trace lines should exist when the lifecycle never started")
	}
}

// A lifecycle whose Start succeeds is stopped exactly once, after the night's stages, whether or not a Shadow ever answered anything useful.
func TestShadowLifecycle_StartSucceeds_StopRunsAfterNight(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}
	primary := &fakeBrain{verdicts: "[]", extract: "[]", und: "An understanding."}
	r := newRunner(store, primary, yesProbes(), at(23, 30))
	r.DataDir = t.TempDir()
	r.Shadow = func(ctx context.Context, prompt string) (string, error) { return "ok", nil }

	started, stopped := false, false
	r.ShadowLifecycle = ShadowLifecycle{
		Start: func(ctx context.Context) error {
			if stopped {
				t.Error("Start observed after Stop already ran")
			}
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
}

// GPUReleaser is asked to free the card before the shadow lifecycle starts, since the embedding server may still be sitting on the GPU the shadow needs.
func TestGPUReleaser_CalledBeforeShadowLifecycleStart(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := at(23, 30).Format(dayFormat)
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
	r.ShadowLifecycle = ShadowLifecycle{
		Start: func(ctx context.Context) error {
			startedAfterReleased = released
			return nil
		},
		Stop: func() {},
	}

	r.Tick(ctx)

	if !released {
		t.Error("GPUReleaser was never called")
	}
	if !startedAfterReleased {
		t.Error("ShadowLifecycle.Start ran before GPUReleaser was called")
	}
}

// When the diary-writing call succeeds, the diary entry is the model's own prose plus a compact audit footer carrying the real numbers — so eval/recall code that greps for facts still finds them even though the prose above is free-form.
func TestFinish_ModelWritesDiaryEntryWithAuditFooter(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := "2026-08-30"
	brain := &fakeBrain{report: "Tonight I turned over what the user believes about their own mornings."}
	r := newRunner(store, brain, yesProbes(), at(23, 30))

	hyp := &stageReport{tested: 3, promoted: 0, retired: 1, adopted: 1, lines: []string{"Retired: He hates mornings (open 30 days and never tested)."}}
	comp := &compactReport{weeks: 0, months: 0}
	replay := &replayReport{items: 52, piles: 14}
	took := 11*time.Minute + 14*time.Second

	if err := r.finish(ctx, night, took, hyp, true, comp, replay, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}

	entry, _ := store.DiaryEntry(ctx, night, "dream")
	if !strings.Contains(entry, "Tonight I turned over what the user believes") {
		t.Errorf("entry does not carry the model's prose: %q", entry)
	}
	wantFooter := "[tested 3: 0 promoted, 1 retired, 1 adopted; understanding rewritten; compacted 0w/0m; replayed 52 items into 14 piles; 11m14s]"
	if !strings.Contains(entry, wantFooter) {
		t.Errorf("entry footer = %q, want it to contain %q", entry, wantFooter)
	}
	if asked := brain.askedKinds(); len(asked) != 1 || asked[0] != "report" {
		t.Errorf("asked = %v, want a single traced \"report\" call", asked)
	}
}

// When the diary-writing call errors, the night still gets its old templated entry — a night must never end without a diary entry.
func TestFinish_FallsBackToTemplateOnBrainError(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := "2026-08-30"
	brain := &fakeBrain{reportErr: errors.New("the model choked")}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	hyp := &stageReport{tested: 1, adopted: 1, lines: []string{"Adopted: He ships at night."}}

	if err := r.finish(ctx, night, time.Minute, hyp, true, &compactReport{}, &replayReport{}, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	entry, _ := store.DiaryEntry(ctx, night, "dream")
	if !strings.Contains(entry, "judge-only") || !strings.Contains(entry, "Adopted: He ships at night.") {
		t.Errorf("entry did not fall back to the template on a brain error: %q", entry)
	}
}

// When the diary-writing call comes back empty, the night still gets its old templated entry.
func TestFinish_FallsBackToTemplateOnEmptyReply(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	night := "2026-08-30"
	brain := &fakeBrain{report: "   "}
	r := newRunner(store, brain, yesProbes(), at(23, 30))
	hyp := &stageReport{tested: 1, adopted: 1, lines: []string{"Adopted: He ships at night."}}

	if err := r.finish(ctx, night, time.Minute, hyp, true, &compactReport{}, &replayReport{}, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	entry, _ := store.DiaryEntry(ctx, night, "dream")
	if !strings.Contains(entry, "judge-only") || !strings.Contains(entry, "Adopted: He ships at night.") {
		t.Errorf("entry did not fall back to the template on an empty reply: %q", entry)
	}
}

// A meeting's minutes end with what people agreed to do. Carrying only the leading lines of one dropped that section entirely — on a real 48-line minutes file the 40-line cap reached Attendees, Key points and Decisions, and cut Action items off the end. The evidence budget already bounds the assembly by dropping whole items oldest-first, which is the right shape: a meeting is included or it is not, never included headless.
func TestBuildEvidence_CarriesAMeetingsActionItems(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Meeting minutes\n\n## Attendees\n")
	for i := 0; i < 40; i++ {
		b.WriteString(fmt.Sprintf("- attendee %d — spoke throughout\n", i))
	}
	b.WriteString("\n## Action items\n- Alex: push the value chain branch\n")

	got := meetingEvidenceBody(b.String())

	if !strings.Contains(got, "push the value chain branch") {
		t.Error("the action items were cut off the end of the minutes")
	}
}
