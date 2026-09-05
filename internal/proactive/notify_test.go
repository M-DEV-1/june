package proactive

import (
	"context"
	"sync"
	"testing"
	"time"

	"ora/internal/config"
	"ora/internal/db"
)

// posted is one notification the fake notifier was asked to show.
type posted struct {
	title, body string
	actions     []Action
	chose       func(string)
}

// fakeNotifier stands in for the desktop in tests: it records what each notification offered and lets the test press one of its buttons.
type fakeNotifier struct {
	mu   sync.Mutex
	sent []posted
}

// Notify records the notification instead of showing it. Input: the same arguments the real notifier takes. Output: always nil.
func (f *fakeNotifier) Notify(title, body string, actions []Action, chose func(string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, posted{title: title, body: body, actions: actions, chose: chose})
	return nil
}

// press clicks one button on the i-th notification, "" standing for a dismissal with no button at all.
func (f *fakeNotifier) press(t *testing.T, i int, key string) {
	t.Helper()
	f.mu.Lock()
	if i >= len(f.sent) {
		f.mu.Unlock()
		t.Fatalf("no notification %d; %d were posted", i, len(f.sent))
	}
	chose := f.sent[i].chose
	f.mu.Unlock()
	chose(key)
}

// count is how many notifications have been posted so far.
func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// testScheduler builds a scheduler with both daily hours disabled and a fake notifier wired, which is every snooze test's starting point.
func testScheduler(t *testing.T) (*Scheduler, *db.Store, *fakeNotifier) {
	t.Helper()
	store := testStore(t)
	s := New(store, nil, func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	f := &fakeNotifier{}
	s.SetNotifier(f)
	return s, store, f
}

// TestNotice_OffersEveryAction checks a notice reaching the desktop carries all five buttons, in order, so every notification can be opened, completed or pushed to later.
func TestNotice_OffersEveryAction(t *testing.T) {
	s, _, f := testScheduler(t)

	s.say(Notice{Title: "Morning brief", Body: "Three things today.", Kind: "brief"})

	if f.count() != 1 {
		t.Fatalf("posted %d notifications, want 1", f.count())
	}
	want := []Action{
		{"default", "Open in Ora"},
		{"done", "Done"},
		{"hour", "In an hour"},
		{"evening", "This evening"},
		{"tomorrow", "Tomorrow"},
	}
	got := f.sent[0].actions
	if len(got) != len(want) {
		t.Fatalf("actions = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("action %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if f.sent[0].title != "Morning brief" || f.sent[0].body != "Three things today." {
		t.Errorf("posted %q / %q, want the notice's own title and body", f.sent[0].title, f.sent[0].body)
	}
}

// TestSnoozeButtons_WriteDueAt presses each snooze button against a fixed clock and checks the moment the notice is due back, across a morning, an afternoon and an hour past the evening hour.
func TestSnoozeButtons_WriteDueAt(t *testing.T) {
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 9, 5, hour, minute, 0, 0, time.Local)
	}
	on := func(day, hour int) time.Time {
		return time.Date(2026, 9, day, hour, 0, 0, 0, time.Local)
	}
	cases := []struct {
		name string
		now  time.Time
		key  string
		want time.Time
	}{
		{"morning, in an hour", at(9, 15), "hour", at(10, 15)},
		{"morning, this evening", at(9, 15), "evening", on(5, eveningHour)},
		{"morning, tomorrow", at(9, 15), "tomorrow", on(6, morningHour)},
		{"afternoon, in an hour", at(14, 30), "hour", at(15, 30)},
		{"afternoon, this evening", at(14, 30), "evening", on(5, eveningHour)},
		{"afternoon, tomorrow", at(14, 30), "tomorrow", on(6, morningHour)},
		{"late evening, in an hour", at(23, 30), "hour", time.Date(2026, 9, 6, 0, 30, 0, 0, time.Local)},
		{"late evening, this evening is tomorrow's", at(23, 30), "evening", on(6, eveningHour)},
		{"late evening, tomorrow", at(23, 30), "tomorrow", on(6, morningHour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, store, f := testScheduler(t)
			s.now = func() time.Time { return tc.now }

			s.say(Notice{Title: "Still open", Body: "Send the invoice", Kind: "task", ID: "42"})
			f.press(t, 0, tc.key)

			due, err := store.DueSnoozes(ctx, tc.want)
			if err != nil {
				t.Fatalf("DueSnoozes: %v", err)
			}
			if len(due) != 1 {
				t.Fatalf("DueSnoozes at %v = %+v, want the one snooze", tc.want, due)
			}
			if !due[0].Due.Equal(tc.want) {
				t.Errorf("due at %v, want %v", due[0].Due.Local(), tc.want)
			}
			if due[0].Kind != "task" || due[0].NoticeID != "42" || due[0].Title != "Still open" || due[0].Body != "Send the invoice" {
				t.Errorf("snooze = %+v, want the notice it was made from", due[0])
			}
		})
	}
}

// TestTick_RefiresDueSnoozeOnce checks the per-minute tick posts a due snooze again, with the same buttons so it can be pushed back a second time, and does not post it once more on the next tick.
func TestTick_RefiresDueSnoozeOnce(t *testing.T) {
	ctx := context.Background()
	s, store, f := testScheduler(t)
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.Local)
	s.now = func() time.Time { return now }

	if _, err := store.AddSnooze(ctx, "task", "42", "Still open", "Send the invoice", now.Add(-time.Minute)); err != nil {
		t.Fatalf("AddSnooze: %v", err)
	}

	s.tick(ctx)
	if f.count() != 1 {
		t.Fatalf("posted %d notifications on the first tick, want 1", f.count())
	}
	if len(f.sent[0].actions) != len(noticeActions) {
		t.Errorf("a re-fired snooze offered %d buttons, want all %d so it can be snoozed again", len(f.sent[0].actions), len(noticeActions))
	}
	if f.sent[0].title != "Still open" || f.sent[0].body != "Send the invoice" {
		t.Errorf("re-fired %q / %q, want the snoozed notice's own text", f.sent[0].title, f.sent[0].body)
	}

	s.tick(ctx)
	if f.count() != 1 {
		t.Errorf("posted %d notifications after the second tick, want the snooze fired only once", f.count())
	}
}

// TestDone_OnTaskNotice_ClosesTheTask checks Done on a task notice goes through the daemon's own task-done path, carrying the task id the notice named.
func TestDone_OnTaskNotice_ClosesTheTask(t *testing.T) {
	ctx := context.Background()
	s, store, f := testScheduler(t)
	var closed []string
	s.SetTaskDone(func(ctx context.Context, id string) error {
		closed = append(closed, id)
		return nil
	})

	s.say(Notice{Title: "Still open", Body: "Send the invoice", Kind: "task", ID: "42"})
	f.press(t, 0, "done")

	if len(closed) != 1 || closed[0] != "42" {
		t.Errorf("task-done path called with %v, want [42]", closed)
	}
	assertNoSnoozes(t, ctx, store)
}

// TestDone_OnRoutineNotice_ClosesNothing checks Done on a routine notice never reaches the task path, since a routine is Ora reporting rather than work owed, and leaves no snooze behind.
func TestDone_OnRoutineNotice_ClosesNothing(t *testing.T) {
	ctx := context.Background()
	s, store, f := testScheduler(t)
	called := 0
	s.SetTaskDone(func(context.Context, string) error { called++; return nil })

	s.say(Notice{Title: "Routine", Body: "Priya replied about the venue.", Kind: "routine", ID: "7"})
	f.press(t, 0, "done")

	if called != 0 {
		t.Errorf("task-done path called %d times for a routine notice, want 0", called)
	}
	assertNoSnoozes(t, ctx, store)
}

// TestDefault_OpensTheWindow checks clicking the body of a notification, or its Open in Ora button, does what the tray's own Open Ora item does.
func TestDefault_OpensTheWindow(t *testing.T) {
	s, _, f := testScheduler(t)
	opened := 0
	s.SetOpenWindow(func() { opened++ })

	s.say(Notice{Title: "Morning brief", Body: "Three things today.", Kind: "brief"})
	f.press(t, 0, "default")

	if opened != 1 {
		t.Errorf("opened the window %d times, want 1", opened)
	}
}

// TestDismissal_LeavesNothingBehind checks a notification closed without a button press — which is what NotificationClosed reports — changes nothing at all.
func TestDismissal_LeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	s, store, f := testScheduler(t)
	closed, opened := 0, 0
	s.SetTaskDone(func(context.Context, string) error { closed++; return nil })
	s.SetOpenWindow(func() { opened++ })

	s.say(Notice{Title: "Still open", Body: "Send the invoice", Kind: "task", ID: "42"})
	f.press(t, 0, "")

	if closed != 0 || opened != 0 {
		t.Errorf("a dismissal closed %d tasks and opened the window %d times, want 0 and 0", closed, opened)
	}
	assertNoSnoozes(t, ctx, store)
}

// TestDone_CancelsAPendingSnooze checks that pressing Done while a snooze is still pending for the same notice stops that snooze coming back, closing the gap where a snoozed notice fires again even after the user has already dealt with it.
func TestDone_CancelsAPendingSnooze(t *testing.T) {
	ctx := context.Background()
	s, store, f := testScheduler(t)
	now := time.Date(2026, 9, 5, 9, 15, 0, 0, time.Local)
	s.now = func() time.Time { return now }

	s.say(Notice{Title: "Still open", Body: "Send the invoice", Kind: "task", ID: "42"})
	f.press(t, 0, "hour")
	f.press(t, 0, "done")

	assertNoSnoozes(t, ctx, store)
}

// TestDone_OnARefiredSnooze_CancelsIt checks the chain that matters most: a notice is snoozed, the scheduler tick re-fires it, and Done on that re-fired notification still cancels it — proving the re-fired notice carries the same kind and id as the original rather than minting a new one.
func TestDone_OnARefiredSnooze_CancelsIt(t *testing.T) {
	ctx := context.Background()
	s, store, f := testScheduler(t)
	now := time.Date(2026, 9, 5, 9, 15, 0, 0, time.Local)
	s.now = func() time.Time { return now }

	s.say(Notice{Title: "Still open", Body: "Send the invoice", Kind: "task", ID: "42"})
	f.press(t, 0, "hour")

	now = now.Add(time.Hour)
	s.tick(ctx)
	if f.count() != 2 {
		t.Fatalf("posted %d notifications after the tick, want 2 (the original and the re-fired one)", f.count())
	}

	f.press(t, 1, "done")
	assertNoSnoozes(t, ctx, store)
}

// TestSnooze_OnARefiredSnooze_ReplacesIt checks that snoozing again from a re-fired notification pushes the same notice further out rather than leaving two snoozes pending for it.
func TestSnooze_OnARefiredSnooze_ReplacesIt(t *testing.T) {
	ctx := context.Background()
	s, store, f := testScheduler(t)
	now := time.Date(2026, 9, 5, 9, 15, 0, 0, time.Local)
	s.now = func() time.Time { return now }

	s.say(Notice{Title: "Still open", Body: "Send the invoice", Kind: "task", ID: "42"})
	f.press(t, 0, "hour")

	now = now.Add(time.Hour)
	s.tick(ctx)
	f.press(t, 1, "hour")

	due, err := store.DueSnoozes(ctx, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("DueSnoozes: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("DueSnoozes = %+v, want exactly one pending snooze for the notice, not one per snooze button pressed", due)
	}
}

// assertNoSnoozes fails the test when anything was put away for later.
func assertNoSnoozes(t *testing.T, ctx context.Context, store *db.Store) {
	t.Helper()
	due, err := store.DueSnoozes(ctx, time.Now().AddDate(1, 0, 0))
	if err != nil {
		t.Fatalf("DueSnoozes: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("snoozes = %+v, want none", due)
	}
}
