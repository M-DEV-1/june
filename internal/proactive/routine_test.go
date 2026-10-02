package proactive

import (
	"context"
	"testing"
	"time"

	"june/internal/config"
	"june/internal/db"
	"june/internal/db/dbtest"
)

// TestParseScheduleAcceptedForms checks every form ParseSchedule documents itself as accepting.
func TestParseScheduleAcceptedForms(t *testing.T) {
	cases := []struct {
		in   string
		want Schedule
	}{
		{"every day at 8", Schedule{Kind: "daily", Hour: 8, Minute: 0}},
		{"every day at 8:30pm", Schedule{Kind: "daily", Hour: 20, Minute: 30}},
		{"every day at 12am", Schedule{Kind: "daily", Hour: 0, Minute: 0}},
		{"weekdays at 9am", Schedule{Kind: "daily", Hour: 9, Minute: 0, Weekdays: true}},
		{"every 3 hours", Schedule{Kind: "interval", IntervalHours: 3}},
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

// TestScheduleDueWhen checks a "when" condition fires immediately with no prior run, is throttled inside minWhenInterval, and fires again once it has passed.
func TestScheduleDueWhen(t *testing.T) {
	sched, err := ParseSchedule("when Vexil replies")
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
