package dream

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/db"
)

// testStore opens a throwaway in-memory store closed with the test.
func testStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// fakeBrain answers each of the three dream prompts with a canned reply, dispatching on the instruction text, and records what it was asked. Safe for the watcher goroutine's world: only Tick's goroutine calls it, but the mutex keeps the record readable after Tick returns.
type fakeBrain struct {
	mu       sync.Mutex
	verdicts string
	extract  string
	und      string
	asked    []string
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

// A resumed night runs only the stages missing from stages_done: with 'hyp' already committed, only the understanding rewrite is asked for, and the night still finishes.
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

	if asked := brain.askedKinds(); len(asked) != 1 || asked[0] != "und" {
		t.Errorf("asked = %v, want only the understanding rewrite", asked)
	}
	run, _, _ := store.DreamRun(ctx, night)
	if !run.Finished || run.StagesDone != "hyp und" {
		t.Errorf("run = %+v, want finished with both stages done", run)
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
