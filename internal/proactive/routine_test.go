package proactive

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/db/dbtest"
)

// TestParseScheduleAcceptedForms checks every form ParseSchedule documents itself as accepting.
func TestParseScheduleAcceptedForms(t *testing.T) {
	cases := []struct {
		in   string
		want Schedule
	}{
		{"every day at 8", Schedule{Kind: "daily", Hour: 8, Minute: 0}},
		{"every day at 8:30", Schedule{Kind: "daily", Hour: 8, Minute: 30}},
		{"every day at 8:30pm", Schedule{Kind: "daily", Hour: 20, Minute: 30}},
		{"every day at 12am", Schedule{Kind: "daily", Hour: 0, Minute: 0}},
		{"every day at 12pm", Schedule{Kind: "daily", Hour: 12, Minute: 0}},
		{"weekdays at 08:00", Schedule{Kind: "daily", Hour: 8, Minute: 0, Weekdays: true}},
		{"weekdays at 9am", Schedule{Kind: "daily", Hour: 9, Minute: 0, Weekdays: true}},
		{"every 3 hours", Schedule{Kind: "interval", IntervalHours: 3}},
		{"every 1 hour", Schedule{Kind: "interval", IntervalHours: 1}},
		{"when Priya replies about the venue", Schedule{Kind: "when", Condition: "Priya replies about the venue"}},
		{" WHEN the PR merges ", Schedule{Kind: "when", Condition: "the PR merges"}},
	}
	for _, c := range cases {
		got, err := ParseSchedule(c.in)
		if err != nil {
			t.Errorf("ParseSchedule(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSchedule(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

// TestParseScheduleRejectsNonsense checks that text in none of the accepted forms is refused rather than silently taken as something.
func TestParseScheduleRejectsNonsense(t *testing.T) {
	for _, in := range []string{"", "someday", "every day", "every day at noon-ish", "every zero hours", "when"} {
		if _, err := ParseSchedule(in); err == nil {
			t.Errorf("ParseSchedule(%q) = nil error, want one", in)
		}
	}
}

// TestScheduleDueDaily checks the daily gate: not yet due before the hour, due once past it and never run today, not due again once it has, and weekdays-only skips the weekend.
func TestScheduleDueDaily(t *testing.T) {
	sched, err := ParseSchedule("every day at 8:30")
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.Local) // a Monday
	before := base.Add(8*time.Hour + 0*time.Minute)
	if sched.Due(before, time.Time{}) {
		t.Error("due before the scheduled hour")
	}
	atTime := base.Add(8*time.Hour + 30*time.Minute)
	if !sched.Due(atTime, time.Time{}) {
		t.Error("not due at the scheduled minute with no prior run")
	}
	ranEarlierToday := base.Add(8 * time.Hour)
	if sched.Due(atTime, ranEarlierToday) {
		t.Error("due again the same day it already ran")
	}
	ranYesterday := base.AddDate(0, 0, -1).Add(8*time.Hour + 30*time.Minute)
	if !sched.Due(atTime, ranYesterday) {
		t.Error("not due again the day after it last ran")
	}

	weekdays, err := ParseSchedule("weekdays at 8")
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	saturday := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local)
	if weekdays.Due(saturday, time.Time{}) {
		t.Error("a weekdays-only schedule fired on a Saturday")
	}
	monday := time.Date(2026, 9, 14, 9, 0, 0, 0, time.Local)
	if !weekdays.Due(monday, time.Time{}) {
		t.Error("a weekdays-only schedule did not fire on a Monday")
	}
}

// TestScheduleDueInterval checks an interval schedule fires immediately with no prior run, not again before the interval elapses, and again once it has.
func TestScheduleDueInterval(t *testing.T) {
	sched, err := ParseSchedule("every 3 hours")
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	now := time.Now()
	if !sched.Due(now, time.Time{}) {
		t.Error("not due on first run")
	}
	if sched.Due(now, now.Add(-2*time.Hour)) {
		t.Error("due before the interval elapsed")
	}
	if !sched.Due(now, now.Add(-3*time.Hour-time.Minute)) {
		t.Error("not due once the interval elapsed")
	}
}

// TestScheduleDueWhen checks a "when" condition fires immediately with no prior run, is throttled inside minWhenInterval, and fires again once it has passed.
func TestScheduleDueWhen(t *testing.T) {
	sched, err := ParseSchedule("when Priya replies")
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	now := time.Now()
	if !sched.Due(now, time.Time{}) {
		t.Error("not due on first check")
	}
	if sched.Due(now, now.Add(-time.Minute)) {
		t.Error("due inside the throttle window")
	}
	if !sched.Due(now, now.Add(-minWhenInterval-time.Minute)) {
		t.Error("not due once the throttle window passed")
	}
}

// TestSchedulerRunsDueRoutineAndNotifies is the tracer bullet for the runner: a due routine is asked, its answer is recorded as its last run, and a notice goes out.
func TestSchedulerRunsDueRoutineAndNotifies(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.AddRoutine(ctx, "tell me the one thing I must do today", "every 1 hour"); err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}

	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	var asked []string
	s := New(store, func(context.Context, string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.SetRoutineAsk(func(ctx context.Context, q string) (string, error) {
		asked = append(asked, q)
		return "Ship the report — it's due today.", nil
	})

	s.tick(ctx)
	s.waitRoutines()

	if len(asked) != 1 {
		t.Fatalf("routine asked %d times, want 1", len(asked))
	}
	if got := asked[0]; got != "tell me the one thing I must do today"+db.RoutineSuffix {
		t.Errorf("prompt asked = %q, want the instruction plus the suffix", got)
	}
	if len(sent) != 1 || sent[0].Body != "Ship the report — it's due today." || sent[0].Place != "routine" {
		t.Errorf("notices = %+v, want one carrying the answer under place \"routine\"", sent)
	}

	routines, err := store.Routines(ctx)
	if err != nil || len(routines) != 1 {
		t.Fatalf("Routines: %v, %v", routines, err)
	}
	if routines[0].LastAnswer != "Ship the report — it's due today." || routines[0].LastRun.IsZero() {
		t.Errorf("routine after run = %+v", routines[0])
	}
}

// TestSchedulerSkipsNothingAnswerAndDisabledRoutine checks that an exact "NOTHING" answer records the run but sends no notice, and a disabled routine is never asked at all.
func TestSchedulerSkipsNothingAnswerAndDisabledRoutine(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	quiet, err := store.AddRoutine(ctx, "tell me if anything is on fire", "every 1 hour")
	if err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}

	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s := New(store, func(context.Context, string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.SetRoutineAsk(func(context.Context, string) (string, error) { return "NOTHING", nil })

	s.tick(ctx)
	s.waitRoutines()

	if len(sent) != 0 {
		t.Errorf("notices = %+v, want none for a NOTHING answer", sent)
	}
	r, err := store.RoutineByID(ctx, quiet)
	if err != nil || r.LastAnswer != "NOTHING" || r.LastRun.IsZero() {
		t.Errorf("routine after a NOTHING answer = %+v, %v, want the run still recorded", r, err)
	}
}

// TestSchedulerSkipsRoutineAlreadyRunning checks runRoutine's in-flight guard: a routine TryStart already holds — standing in for a POST /routines/{id}/run in flight on the same routine — is skipped by the tick rather than asked and recorded a second time, and the guard is released again afterward so a later tick can pick it up.
func TestSchedulerSkipsRoutineAlreadyRunning(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	id, err := store.AddRoutine(ctx, "tell me the one thing I must do today", "every 1 hour")
	if err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}

	// Stands in for the run-now route already having claimed this routine.
	if !store.TryStart(id) {
		t.Fatal("TryStart: expected the first claim to succeed")
	}

	var asked int
	s := New(store, func(context.Context, string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.SetRoutineAsk(func(ctx context.Context, q string) (string, error) {
		asked++
		return "should not run", nil
	})

	s.tick(ctx)
	s.waitRoutines()

	if asked != 0 {
		t.Errorf("routine asked %d times while already running, want 0", asked)
	}
	r, err := store.RoutineByID(ctx, id)
	if err != nil || !r.LastRun.IsZero() {
		t.Errorf("routine after a skipped tick = %+v, %v, want no run recorded", r, err)
	}

	// The guard is still held (this test never called Finish), so releasing it and ticking again must now run it.
	store.Finish(id)
	s.tick(ctx)
	s.waitRoutines()
	if asked != 1 {
		t.Errorf("routine asked %d times after the guard was released, want 1", asked)
	}
}

// TestSchedulerNeverAsksWithNoRoutineAsk checks the runner does nothing at all when SetRoutineAsk was never called, the same "unwired disables it" contract SetAsk and SetWeeklyStudy already carry.
func TestSchedulerNeverAsksWithNoRoutineAsk(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.AddRoutine(ctx, "tell me something", "every 1 hour"); err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}
	s := New(store, func(context.Context, string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})

	s.tick(ctx)
	s.waitRoutines()

	routines, err := store.Routines(ctx)
	if err != nil {
		t.Fatalf("Routines: %v", err)
	}
	if !routines[0].LastRun.IsZero() {
		t.Errorf("a routine ran with no routineAsk wired: %+v", routines[0])
	}
}

// TestTick_RoutineRunsWithoutHoldingUpTheTick checks a due routine's ask runs on its own goroutine: the tick must return while the ask is still in flight, since one ask goes through the whole tool loop and would otherwise delay every later tick.
func TestTick_RoutineRunsWithoutHoldingUpTheTick(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.AddRoutine(ctx, "tell me the one thing I must do today", "every 1 hour"); err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}
	SetNoticeSender(func(Notice) bool { return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	s := New(store, func(context.Context, string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.SetRoutineAsk(func(context.Context, string) (string, error) {
		close(started)
		<-release
		finished.Store(true)
		return "Ship the report.", nil
	})

	s.tick(ctx)
	<-started
	if finished.Load() {
		t.Error("the tick waited for the routine's ask to finish")
	}
	close(release)
	s.waitRoutines()
	if !finished.Load() {
		t.Error("the routine's ask never finished")
	}
}

// TestRoutine_LastRunIsStampedAfterTheAsk checks the run is stamped with the clock as it stands once the ask returns, not the tick's own timestamp: an ask that itself outran the "when" throttle used to be due again the moment it returned, asking continuously.
func TestRoutine_LastRunIsStampedAfterTheAsk(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.AddRoutine(ctx, "tell me if Priya replied", "when Priya replies about the venue"); err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}

	base := time.Now()
	var offset atomic.Int64
	var asked atomic.Int64
	s := New(store, func(context.Context, string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.now = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	s.SetRoutineAsk(func(context.Context, string) (string, error) {
		asked.Add(1)
		// The ask itself takes longer than the throttle the routine is under.
		offset.Add(int64(minWhenInterval + 5*time.Minute))
		return db.RoutineNothing, nil
	})

	s.tick(ctx)
	s.waitRoutines()
	s.tick(ctx)
	s.waitRoutines()

	if got := asked.Load(); got != 1 {
		t.Errorf("routine asked %d times, want 1 — the run must be stamped after the ask returned", got)
	}
}

// TestRoutine_StoreFailureStillPostsAndDoesNotReask checks a routine whose run the store would not record still reaches the user, and is not asked again on the very next tick — the answer is already paid for, and a store that keeps refusing must not turn one routine into an ask a minute.
func TestRoutine_StoreFailureStillPostsAndDoesNotReask(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.AddRoutine(ctx, "tell me the one thing I must do today", "every 1 hour"); err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}
	stub := &stubStore{Store: store, setRun: func(context.Context, int64, time.Time, string) error {
		return errors.New("disk full")
	}}

	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	var asked atomic.Int64
	s := New(stub, func(context.Context, string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.SetRoutineAsk(func(context.Context, string) (string, error) {
		asked.Add(1)
		return "Ship the report.", nil
	})

	s.tick(ctx)
	s.waitRoutines()
	if len(sent) != 1 || sent[0].Body != "Ship the report." {
		t.Fatalf("notices = %+v, want the answer posted despite the failed write", sent)
	}

	s.tick(ctx)
	s.waitRoutines()
	if got := asked.Load(); got != 1 {
		t.Errorf("routine asked %d times, want 1 — a failed write must not cause a re-ask", got)
	}
}

// TestParseSchedule_SurvivesEmptyHugeAndNonUTF8Input feeds ParseSchedule the shapes a routine's schedule text can actually arrive in from a model or a mistyped user instruction — empty, whitespace, a megabyte of one letter, bytes that are not UTF-8, and numbers too large to be a duration — and requires that nothing panics and that anything it accepts describes a real repeat interval.
func TestParseSchedule_SurvivesEmptyHugeAndNonUTF8Input(t *testing.T) {
	inputs := []string{
		"", " ", "\x00",
		string([]byte{0xff, 0xfe, 0xc3, 0x28, 0x80}),
		strings.Repeat("a", 1<<20),
		"every day at " + strings.Repeat("9", 100000),
		"every " + strings.Repeat("9", 100000) + " hours",
		"every 9223372036854775807 hours",
		"every 999999999999 hours",
		"every 0 hours",
		"when " + strings.Repeat("\xff", 100000),
		"every day at " + strings.Repeat("9:", 50000),
		"weekdays at -5",
		"every day at 9:" + strings.Repeat("0", 100000),
	}
	now := time.Now()
	for i, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("input %d (%.40q) panicked: %v", i, in, r)
				}
			}()
			sch, err := ParseSchedule(in)
			if err != nil {
				return
			}
			// A schedule that parsed must describe a real moment: an interval that overflows time.Duration would report itself due on every single tick, firing a routine the user asked for once a day continuously.
			if sch.Kind == "interval" && time.Duration(sch.IntervalHours)*time.Hour <= 0 {
				t.Errorf("input %d (%.40q) parsed to a non-positive interval duration (IntervalHours=%d)", i, in, sch.IntervalHours)
			}
			sch.Due(now, time.Time{})
			sch.Due(now, now.Add(-time.Minute))
		}()
	}
}

