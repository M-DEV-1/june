// routine.go parses a routine's schedule text into a Schedule that says when to check it, and runs the ones that are due the same way the brief and close already do: ask the daemon's own ask path, skip an exact "NOTHING" answer, and post a notice for anything else. A schedule that fails to parse is skipped and logged rather than treated as due, so a typo in the schedule never fires a routine on every tick.
package proactive

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ora/internal/db"
)

// Schedule is one routine's parsed schedule. Kind is "daily" (Weekdays true restricts it to Monday-Friday), "interval", or "when" — the last left as free text for the model to judge for itself every time it is checked, since no clock can tell whether Vexil has replied.
type Schedule struct {
	Kind          string
	Hour, Minute  int
	Weekdays      bool
	IntervalHours int
	Condition     string
}

// The four forms ParseSchedule accepts. Anything else is an error.
var (
	dailyRe    = regexp.MustCompile(`(?i)^every day at (.+)$`)
	weekdaysRe = regexp.MustCompile(`(?i)^weekdays at (.+)$`)
	intervalRe = regexp.MustCompile(`(?i)^every (\d+) hours?$`)
	whenRe     = regexp.MustCompile(`(?i)^when (.+)$`)
)

// maxIntervalHours is the largest "every N hours" a routine may ask for: a year. Past this the number is a typo or a model's invention rather than a schedule, and large enough values overflow the time.Duration multiplication in Due.
const maxIntervalHours = 24 * 366

// ParseSchedule reads a routine's schedule text. Input: "every day at H[:MM][am/pm]", "weekdays at H[:MM][am/pm]", "every N hours", or "when <condition>" (the condition kept verbatim, for the model to judge). Output: the parsed Schedule, or an error naming what could not be read.
func ParseSchedule(s string) (Schedule, error) {
	s = strings.TrimSpace(s)
	if m := dailyRe.FindStringSubmatch(s); m != nil {
		hour, minute, err := parseClock(m[1])
		if err != nil {
			return Schedule{}, err
		}
		return Schedule{Kind: "daily", Hour: hour, Minute: minute}, nil
	}
	if m := weekdaysRe.FindStringSubmatch(s); m != nil {
		hour, minute, err := parseClock(m[1])
		if err != nil {
			return Schedule{}, err
		}
		return Schedule{Kind: "daily", Hour: hour, Minute: minute, Weekdays: true}, nil
	}
	if m := intervalRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		// An hour count large enough to overflow time.Duration when Due multiplies it by time.Hour comes back negative, which makes the routine report itself due on every single tick — the opposite of the rare schedule the user asked for. Anything past a year is not a repeat schedule anyone means, so the range is checked here rather than left to wrap in Due.
		if err != nil || n <= 0 || n > maxIntervalHours {
			return Schedule{}, fmt.Errorf("%q is not a whole number of hours between 1 and %d", m[1], maxIntervalHours)
		}
		return Schedule{Kind: "interval", IntervalHours: n}, nil
	}
	if m := whenRe.FindStringSubmatch(s); m != nil {
		condition := strings.TrimSpace(m[1])
		if condition == "" {
			return Schedule{}, fmt.Errorf("%q names no condition", s)
		}
		return Schedule{Kind: "when", Condition: condition}, nil
	}
	return Schedule{}, fmt.Errorf("%q is not a schedule Ora understands", s)
}

// parseClock reads a clock time as "8", "8:30", "08:00", "8am" or "8:30pm". Input: the text after "at ". Output: the local hour (0-23) and minute.
func parseClock(s string) (hour, minute int, err error) {
	orig := s
	s = strings.ToLower(strings.TrimSpace(s))
	pm, am := false, false
	switch {
	case strings.HasSuffix(s, "am"):
		am = true
		s = strings.TrimSpace(strings.TrimSuffix(s, "am"))
	case strings.HasSuffix(s, "pm"):
		pm = true
		s = strings.TrimSpace(strings.TrimSuffix(s, "pm"))
	}
	h, m := s, "0"
	if i := strings.IndexByte(s, ':'); i >= 0 {
		h, m = s[:i], s[i+1:]
	}
	hour, err = strconv.Atoi(h)
	if err != nil {
		return 0, 0, fmt.Errorf("%q is not a time Ora understands", orig)
	}
	minute, err = strconv.Atoi(m)
	if err != nil {
		return 0, 0, fmt.Errorf("%q is not a time Ora understands", orig)
	}
	if pm && hour < 12 {
		hour += 12
	}
	if am && hour == 12 {
		hour = 0
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%q is not a time of day", orig)
	}
	return hour, minute, nil
}

// minWhenInterval throttles a "when <condition>" routine to being checked at most this often, since every check is a full ask through the daemon's tool loop and not free.
// ponytail: one fixed throttle for every "when" routine; a per-routine interval if some conditions genuinely need checking more or less often than this.
const minWhenInterval = 15 * time.Minute

// Due reports whether now is the moment to check this schedule, given when it was last run (the zero time if never). A "daily" schedule fires once the clock has reached its hour and minute and it has not already run today, and never fires on a weekend when Weekdays is set. An "interval" schedule fires once IntervalHours have passed since the last run. A "when" schedule fires at most once every minWhenInterval.
func (sch Schedule) Due(now, lastRun time.Time) bool {
	switch sch.Kind {
	case "daily":
		if sch.Weekdays && (now.Weekday() == time.Saturday || now.Weekday() == time.Sunday) {
			return false
		}
		if now.Hour() < sch.Hour || (now.Hour() == sch.Hour && now.Minute() < sch.Minute) {
			return false
		}
		return lastRun.IsZero() || !sameLocalDay(lastRun, now)
	case "interval":
		return lastRun.IsZero() || now.Sub(lastRun) >= time.Duration(sch.IntervalHours)*time.Hour
	case "when":
		return lastRun.IsZero() || now.Sub(lastRun) >= minWhenInterval
	default:
		return false
	}
}

// sameLocalDay reports whether a and b fall on the same local calendar day.
func sameLocalDay(a, b time.Time) bool {
	return a.Local().Format(time.DateOnly) == b.Local().Format(time.DateOnly)
}

// SetRoutineAsk wires the daemon's own ask path — the same one POST /ask answers through, tools included — that a due routine's instruction is put to. Unset, routines are never checked; cmd/daemon.go is the only production caller, wired once the ask agent exists.
func (s *Scheduler) SetRoutineAsk(fn func(ctx context.Context, question string) (string, error)) {
	s.routineAsk = fn
}

// maybeRoutines checks every enabled routine and runs the ones whose schedule says they are due. A routine whose schedule fails to parse is skipped and logged, not treated as due.
func (s *Scheduler) maybeRoutines(ctx context.Context) {
	if s.routineAsk == nil {
		return
	}
	routines, err := s.store.Routines(ctx)
	if err != nil {
		slog.Warn("routines: reading routines failed", "error", err)
		return
	}
	now := s.now()
	for _, r := range routines {
		if !r.Enabled {
			continue
		}
		sched, err := ParseSchedule(r.Schedule)
		if err != nil {
			slog.Warn("routines: schedule does not parse, skipping", "id", r.ID, "schedule", r.Schedule, "error", err)
			continue
		}
		if !sched.Due(now, s.lastRunOf(r)) {
			continue
		}
		// One run goes through the whole tool loop, which the tick must not wait for: several due routines used to run one after another and hold up every duty on the next tick. TryStart inside runRoutine is what stops two runs of the same routine overlapping.
		s.running.Add(1)
		go func() {
			defer s.running.Done()
			runCtx, cancel := context.WithTimeout(ctx, routineTimeout)
			defer cancel()
			if err := s.runRoutine(runCtx, r); err != nil {
				slog.Warn("routine failed, retrying next tick", "id", r.ID, "error", err)
			}
		}()
	}
}

// lastRunOf is when a routine last actually ran. Input: the routine as the store has it. Output: the later of the stored last run and this process's own record of one, so a run the store refused to record does not make the routine due again on the very next tick.
func (s *Scheduler) lastRunOf(r db.Routine) time.Time {
	s.lastRunMu.Lock()
	defer s.lastRunMu.Unlock()
	if ran, ok := s.lastRun[r.ID]; ok && ran.After(r.LastRun) {
		return ran
	}
	return r.LastRun
}

// markRun records in memory that a routine ran at the given moment. Input: the routine's id and when its ask returned. Output: nothing.
func (s *Scheduler) markRun(id int64, at time.Time) {
	s.lastRunMu.Lock()
	defer s.lastRunMu.Unlock()
	s.lastRun[id] = at
}

// runRoutine puts one routine's instruction to the daemon's ask path, records the answer as its last run, and posts a notice for anything but an exact "NOTHING" answer.
// The run is stamped with the clock as it stands once the ask returns, not the moment the tick started: a "when" routine whose ask took longer than its own throttle used to come back due the instant it returned, and asked continuously from then on.
// A run the store will not record is still posted and still remembered in memory — the answer has already been paid for, and re-asking every minute while the disk is full costs a full tool-loop ask each time.
// TryStart/Finish stop this from overlapping a POST /routines/{id}/run for the same routine: if one is already in flight (either path), this tick skips the routine silently rather than asking and recording its result a second time.
func (s *Scheduler) runRoutine(ctx context.Context, r db.Routine) error {
	if !s.store.TryStart(r.ID) {
		return nil
	}
	defer s.store.Finish(r.ID)
	answer, err := s.routineAsk(ctx, r.Text+db.RoutineSuffix)
	if err != nil {
		return err
	}
	answer = strings.TrimSpace(answer)
	ranAt := s.now()
	s.markRun(r.ID, ranAt)
	if err := s.store.SetRoutineRun(ctx, r.ID, ranAt, answer); err != nil {
		slog.Warn("routines: could not record a routine's run, posting its answer anyway", "id", r.ID, "error", err)
	}
	if answer != db.RoutineNothing {
		s.say(Notice{Title: "Routine", Body: answer, Place: "routine", ID: strconv.FormatInt(r.ID, 10), Kind: "routine"})
	}
	return nil
}
