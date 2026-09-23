package proactive

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/db/dbtest"
	"ora/internal/memory"
)

// posted is one notification the fake notifier was asked to show.
type posted struct {
	key, title, body string
	actions          []Action
	chose            func(string)
}

// fakeNotifier stands in for the desktop in tests: it records what each notification offered, lets the test press one of its buttons, and records which notice keys were later asked to close.
type fakeNotifier struct {
	mu     sync.Mutex
	sent   []posted
	closed []string
}

// Notify records the notification instead of showing it. Input: the same arguments the real notifier takes. Output: always nil.
func (f *fakeNotifier) Notify(key, title, body string, actions []Action, chose func(string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, posted{key: key, title: title, body: body, actions: actions, chose: chose})
	return nil
}

// Close records the key instead of reaching for a real banner. Output: always nil.
func (f *fakeNotifier) Close(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, key)
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
	store := dbtest.Open(t)
	s := New(store, nil, func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	f := &fakeNotifier{}
	s.SetNotifier(f)
	return s, store, f
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

	s.say(Notice{Title: "Routine", Body: "Vexil replied about the venue.", Kind: "routine", ID: "7"})
	f.press(t, 0, "done")

	if called != 0 {
		t.Errorf("task-done path called %d times for a routine notice, want 0", called)
	}
	assertNoSnoozes(t, ctx, store)
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

// TestAct_Done_ClosesTheTask checks Act's "done" takes the same task-done path a D-Bus press does, so the route and the notification apply Done identically.
func TestAct_Done_ClosesTheTask(t *testing.T) {
	ctx := context.Background()
	s, store, _ := testScheduler(t)
	var closed []string
	s.SetTaskDone(func(ctx context.Context, id string) error {
		closed = append(closed, id)
		return nil
	})

	if err := s.Act(ctx, "task", "42", "Still open", "Send the invoice", "done"); err != nil {
		t.Fatalf("Act: %v", err)
	}

	if len(closed) != 1 || closed[0] != "42" {
		t.Errorf("task-done path called with %v, want [42]", closed)
	}
	assertNoSnoozes(t, ctx, store)
}

// TestAct_Snooze_WritesTheSameSnoozeAPressWould checks Act's snooze buttons write the same pending snooze a D-Bus press writes, keyed off the kind and id it was called with rather than a notice built from a live posting.
func TestAct_Snooze_WritesTheSameSnoozeAPressWould(t *testing.T) {
	ctx := context.Background()
	s, store, _ := testScheduler(t)
	now := time.Date(2026, 9, 5, 9, 15, 0, 0, time.Local)
	s.now = func() time.Time { return now }

	if err := s.Act(ctx, "task", "42", "Still open", "Send the invoice", "hour"); err != nil {
		t.Fatalf("Act: %v", err)
	}

	due, err := store.DueSnoozes(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("DueSnoozes: %v", err)
	}
	if len(due) != 1 || due[0].Kind != "task" || due[0].NoticeID != "42" || due[0].Title != "Still open" || due[0].Body != "Send the invoice" {
		t.Errorf("DueSnoozes = %+v, want one snooze for the notice Act was called with", due)
	}
}

// TestSay_WindowUp_SendsOnlyToTheWindow checks a notice raised while a window is reading the event stream reaches that window's own card and posts no desktop banner, so the user is asked once about a thing rather than twice.
func TestSay_WindowUp_SendsOnlyToTheWindow(t *testing.T) {
	s, _, f := testScheduler(t)
	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s.say(Notice{Title: "Morning brief", Body: "Three things today.", Kind: "brief"})

	if len(sent) != 1 || sent[0].Title != "Morning brief" {
		t.Errorf("window got %+v, want the one notice", sent)
	}
	if f.count() != 0 {
		t.Errorf("desktop got %d notifications, want none while a window is up", f.count())
	}
}

// TestSay_WindowDown_PostsTheBanner checks a notice raised with no window listening still reaches the desktop with its full set of buttons, which is the only surface such a machine has.
func TestSay_WindowDown_PostsTheBanner(t *testing.T) {
	s, _, f := testScheduler(t)
	SetNoticeSender(func(Notice) bool { return false })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s.say(Notice{Title: "Morning brief", Body: "Three things today.", Kind: "brief"})

	if f.count() != 1 {
		t.Fatalf("desktop got %d notifications, want 1 with no window up", f.count())
	}
	if f.sent[0].title != "Morning brief" || len(f.sent[0].actions) != len(noticeActions) {
		t.Errorf("posted %q with %d buttons, want the notice's own title and all %d buttons", f.sent[0].title, len(f.sent[0].actions), len(noticeActions))
	}
}

// TestMaybeTaskNotices_WindowUp_SendsOnlyToTheWindow checks the task-notice path takes the same one-surface rule as every other notice: an action item newly lifted from a meeting while a window is up becomes a card there and no banner.
func TestMaybeTaskNotices_WindowUp_SendsOnlyToTheWindow(t *testing.T) {
	ctx := context.Background()
	s, store, f := testScheduler(t)
	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })
	// The first tick only seeds the watermark, so the item added after it counts as newly raised.
	s.maybeTaskNotices(ctx)
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{
		{Owner: "Me", Text: "Send the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Standup", Raised: time.Now()},
	}); err != nil {
		t.Fatalf("AddActionItems: %v", err)
	}

	s.maybeTaskNotices(ctx)

	if len(sent) != 1 || sent[0].Kind != "task" || sent[0].Title != "New task from Standup" {
		t.Errorf("window got %+v, want the one task notice", sent)
	}
	// A task is the one thing that can be completed or pushed to later, so its card carries the full set; the window draws what the notice names rather than assuming every notice can answer them.
	if len(sent) == 1 && len(sent[0].Actions) != len(noticeActions) {
		t.Errorf("task notice carried %d actions, want all %d", len(sent[0].Actions), len(noticeActions))
	}

	// Everything else reaching a window carries the one button it can answer. A routine's report is Ora saying what it found: nothing to complete, nowhere to push it to, and its card offered Done and three snoozes that all came back "Could not do that".
	sent = nil
	s.say(Notice{Title: "Routine", Body: "The window in front is Discord.", Kind: "routine"})
	if len(sent) != 1 || len(sent[0].Actions) != 1 || sent[0].Actions[0].Key != actionOpen {
		t.Errorf("routine notice carried %+v, want just Open in Ora", sent)
	}
	if f.count() != 0 {
		t.Errorf("desktop got %d notifications, want none while a window is up", f.count())
	}
}

// TestAct_ClosesTheBannerAWindowPressCameFrom checks a window's own POST /notices/{kind}/{id}/action — which answers through Act — also dismisses that notice's desktop banner, so a task closed from the window does not leave its notification still asking the same question in the message tray.
func TestAct_ClosesTheBannerAWindowPressCameFrom(t *testing.T) {
	ctx := context.Background()
	s, _, f := testScheduler(t)
	s.say(Notice{Title: "Still open", Body: "Send the invoice", Kind: "task", ID: "42"})

	if err := s.Act(ctx, "task", "42", "Still open", "Send the invoice", "done"); err != nil {
		t.Fatalf("Act: %v", err)
	}

	if len(f.closed) != 1 || f.closed[0] != "task|42" {
		t.Errorf("closed = %v, want the banner for task|42 closed", f.closed)
	}
}

// TestAct_DoesNotCloseABannerOnABadAction checks a refused action leaves the banner alone, since nothing was actually applied for it to be answering.
func TestAct_DoesNotCloseABannerOnABadAction(t *testing.T) {
	s, _, f := testScheduler(t)
	s.say(Notice{Title: "Still open", Body: "Send the invoice", Kind: "task", ID: "42"})

	if err := s.Act(context.Background(), "task", "42", "Still open", "Send the invoice", "snooze-forever"); !errors.Is(err, ErrBadNoticeAction) {
		t.Fatalf("Act = %v, want ErrBadNoticeAction", err)
	}
	if len(f.closed) != 0 {
		t.Errorf("closed = %v, want none for a refused action", f.closed)
	}
}

// TestNotify_CarriesOnlyOpen checks that a moment posted through proactive.Notify offers one button, "Open in Ora", on both surfaces it can reach: the bus notifier when no window is listening, and the window's own card when one is. Nothing posted this way has a task behind it, so Done and the three snoozes have nothing to act on — a "Transcribing meeting" card that offered them answered "Could not do that" when one was pressed.
func TestNotify_CarriesOnlyOpen(t *testing.T) {
	s, _, f := testScheduler(t)
	opened := 0
	s.SetOpenWindow(func() { opened++ })
	SetNoticeSender(func(Notice) bool { return false })
	t.Cleanup(func() { SetNoticeSender(nil) })

	Notify("audio-input-microphone", "Before you join: standup", "Last time you owed the deck.")

	if f.count() != 1 {
		t.Fatalf("posted %d notifications through the bus, want 1", f.count())
	}
	if f.sent[0].title != "Before you join: standup" || f.sent[0].body != "Last time you owed the deck." {
		t.Errorf("posted %q / %q, want the notice's own text", f.sent[0].title, f.sent[0].body)
	}
	if len(f.sent[0].actions) != 1 || f.sent[0].actions[0].Key != actionOpen {
		t.Errorf("actions = %+v, want just Open in Ora", f.sent[0].actions)
	}

	f.press(t, 0, actionOpen)
	if opened != 1 {
		t.Errorf("pressing Open in Ora opened the window %d times, want 1", opened)
	}
}

// TestMaybeTaskNotices_CapsPerMeetingAndSkipsOthers checks a meeting's newly-lifted action items each raise a task notice carrying the full action set, capped at three per meeting with the rest folded into the third's body, and that an item owed by somebody else never gets one.
func TestMaybeTaskNotices_CapsPerMeetingAndSkipsOthers(t *testing.T) {
	ctx := context.Background()
	s, store, f := testScheduler(t)

	// The first tick only seeds the watermark, so the items added after it count as newly raised.
	s.maybeTaskNotices(ctx)
	items := []memory.ActionItem{
		{Owner: "Me", Text: "Send the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Standup", Raised: time.Now()},
		{Owner: "Me", Text: "Book the venue", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Standup", Raised: time.Now()},
		{Owner: "Me", Text: "Review the PR", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Standup", Raised: time.Now()},
		{Owner: "Me", Text: "Write the doc", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Standup", Raised: time.Now()},
		{Owner: "Vexil", Text: "Confirm the vendor", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Standup", Raised: time.Now()},
	}
	if _, err := store.AddActionItems(ctx, items); err != nil {
		t.Fatalf("AddActionItems: %v", err)
	}

	s.maybeTaskNotices(ctx)

	if f.count() != 3 {
		t.Fatalf("posted %d task notices, want 3 (capped, none for the item owned by Vexil)", f.count())
	}
	for i, p := range f.sent {
		if p.title != "New task from Standup" {
			t.Errorf("notice %d title = %q, want %q", i, p.title, "New task from Standup")
		}
		if len(p.actions) != len(noticeActions) {
			t.Errorf("notice %d offered %d buttons, want the full set of %d", i, len(p.actions), len(noticeActions))
		}
		if strings.Contains(p.body, "Vexil") || strings.Contains(p.body, "vendor") {
			t.Errorf("notice %d body = %q, should never carry someone else's item", i, p.body)
		}
	}
	if !strings.Contains(f.sent[2].body, "and 1 more in Tasks") {
		t.Errorf("last notice body = %q, want it to mention the 1 item left over", f.sent[2].body)
	}

	// A second tick with nothing new raises no further notices — the watermark stops it re-announcing the same meeting.
	s.maybeTaskNotices(ctx)
	if f.count() != 3 {
		t.Errorf("posted %d task notices after a second tick with nothing new, want still 3", f.count())
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

// TestMaybeTaskNotices_FirstTickSeedsTheWatermark checks a daemon meeting a store full of open items announces none of them on its first tick and only records where it got to, so an old store does not flood the desk, and that an item raised after that is announced.
func TestMaybeTaskNotices_FirstTickSeedsTheWatermark(t *testing.T) {
	ctx := context.Background()
	s, store, f := testScheduler(t)
	old := []memory.ActionItem{{Owner: "Me", Text: "Send the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Standup", Raised: time.Now()}}
	if _, err := store.AddActionItems(ctx, old); err != nil {
		t.Fatalf("AddActionItems: %v", err)
	}

	s.maybeTaskNotices(ctx)
	if f.count() != 0 {
		t.Fatalf("first tick posted %d task notices, want none", f.count())
	}
	if mark, _ := store.DiaryEntry(ctx, "", taskNoticeWatermarkKind); mark == "" {
		t.Fatal("first tick left no watermark behind")
	}

	fresh := []memory.ActionItem{{Owner: "Me", Text: "Book the venue", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Planning", Raised: time.Now()}}
	if _, err := store.AddActionItems(ctx, fresh); err != nil {
		t.Fatalf("AddActionItems: %v", err)
	}
	s.maybeTaskNotices(ctx)
	if f.count() != 1 || !strings.Contains(f.sent[0].body, "Book the venue") {
		t.Errorf("second tick posted %d notices (%+v), want the one new item", f.count(), f.sent)
	}
}

// TestMaybeTaskNotices_CapsPerTick checks three meetings each raising three items produce five notices in one tick, not nine.
func TestMaybeTaskNotices_CapsPerTick(t *testing.T) {
	ctx := context.Background()
	s, store, f := testScheduler(t)
	s.maybeTaskNotices(ctx)
	var items []memory.ActionItem
	for _, meeting := range []string{"Standup", "Planning", "Review"} {
		for i := 0; i < 3; i++ {
			items = append(items, memory.ActionItem{Owner: "Me", Text: fmt.Sprintf("%s item %d", meeting, i), Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: meeting, Raised: time.Now()})
		}
	}
	if _, err := store.AddActionItems(ctx, items); err != nil {
		t.Fatalf("AddActionItems: %v", err)
	}
	s.maybeTaskNotices(ctx)
	if f.count() != maxTaskNoticesPerTick {
		t.Fatalf("posted %d task notices, want the per-tick cap of %d", f.count(), maxTaskNoticesPerTick)
	}
	// Nine items, five shown: the last notice says how many were held back, once, so nothing vanishes untold.
	if !strings.Contains(f.sent[4].body, "and 4 more in Tasks") {
		t.Errorf("last notice body = %q, want it to count the 4 items held back", f.sent[4].body)
	}
	for _, p := range f.sent[:4] {
		if strings.Contains(p.body, "more in Tasks") {
			t.Errorf("an earlier notice carries the count: %q", p.body)
		}
	}
}

// TestAct_Snooze_StoreFailureIsReturned checks a snooze the store could not write comes back as an error, so the route answers with a failure instead of a 200 that snoozed nothing.
func TestAct_Snooze_StoreFailureIsReturned(t *testing.T) {
	ctx := context.Background()
	s, store, _ := testScheduler(t)
	store.Close()
	if err := s.Act(ctx, "task", "42", "Still open", "Send the invoice", actionEvening); err == nil {
		t.Error("Act returned nil for a snooze the store refused")
	}
}

// TestMaybeTaskNotices_FailedWatermarkWriteDoesNotReannounce checks a watermark the store would not write is still remembered for this process, so the same items are not announced again on the next tick, once a minute, for as long as the disk is full.
func TestMaybeTaskNotices_FailedWatermarkWriteDoesNotReannounce(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if err := store.SetDiaryEntry(ctx, "", taskNoticeWatermarkKind, "0"); err != nil {
		t.Fatalf("SetDiaryEntry: %v", err)
	}
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{
		{Owner: "Me", Text: "Send the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Standup", Raised: time.Now()},
	}); err != nil {
		t.Fatalf("AddActionItems: %v", err)
	}

	stub := &stubStore{Store: store, setDiary: func(context.Context, string, string, string) error {
		return errors.New("disk full")
	}}
	s := New(stub, nil, func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	f := &fakeNotifier{}
	s.SetNotifier(f)

	s.maybeTaskNotices(ctx)
	if f.count() != 1 {
		t.Fatalf("posted %d task notices, want 1", f.count())
	}
	s.maybeTaskNotices(ctx)
	if f.count() != 1 {
		t.Errorf("posted %d task notices after a failed watermark write, want still 1", f.count())
	}
}

// TestSnooze_RefiredNoticeCarriesItsPlace checks a snooze coming back points at the same screen the first posting did. The snooze row does not carry the place, so it is derived from the kind; without it the card opened nothing.
func TestSnooze_RefiredNoticeCarriesItsPlace(t *testing.T) {
	cases := []struct{ kind, place string }{
		{"task", "tasks"},
		{"routine", "routine"},
		{"close", "days"},
		{"brief", "tasks"},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			ctx := context.Background()
			store := dbtest.Open(t)
			if _, err := store.AddSnooze(ctx, c.kind, "42", "Still open", "Send the invoice", time.Now().Add(-time.Minute)); err != nil {
				t.Fatalf("AddSnooze: %v", err)
			}
			var sent []Notice
			SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
			t.Cleanup(func() { SetNoticeSender(nil) })

			s := New(store, nil, func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
			s.maybeSnoozes(ctx)

			if len(sent) != 1 || sent[0].Place != c.place {
				t.Errorf("re-fired notice = %+v, want one with place %q", sent, c.place)
			}
		})
	}
}

// TestBusNotifier_SlowNotifyDoesNotStallAPress checks the D-Bus lock is not held across the Notify call: a wedged notification daemon must stall only its own post, not every pending button press on every other notice.
func TestBusNotifier_SlowNotifyDoesNotStallAPress(t *testing.T) {
	pressed := make(chan string, 1)
	release := make(chan struct{})
	n := &BusNotifier{
		waiting: map[uint32]func(string){7: func(key string) { pressed <- key }},
		keyed:   map[string]uint32{"task|42": 7},
	}
	n.notifyCall = func(title, body string, actions []string) (uint32, error) {
		<-release
		return 8, nil
	}

	done := make(chan error, 1)
	go func() { done <- n.Notify("task|43", "Still open", "Book the venue", noticeActions, func(string) {}) }()
	go n.finish(7, actionDone)

	select {
	case key := <-pressed:
		if key != actionDone {
			t.Errorf("press came back as %q, want %q", key, actionDone)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a press was stalled by a Notify call that had not returned")
	}

	close(release)
	if err := <-done; err != nil {
		t.Errorf("Notify: %v", err)
	}
	if n.keyed["task|43"] != 8 {
		t.Errorf("keyed = %v, want the second notice recorded once its call returned", n.keyed)
	}
}

// A snooze coming back carries the same buttons it was pushed back with, so it can be done or pushed back again. It named none, which sendNotice fills in as Open alone, so the card that came back an hour later was a dead end: the one thing the user could not do with a snoozed task was snooze it again.
func TestSnooze_RefiredNoticeCarriesItsButtons(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.AddSnooze(ctx, "task", "42", "Still open", "Send the invoice", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("AddSnooze: %v", err)
	}
	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s := New(store, nil, func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.maybeSnoozes(ctx)

	if len(sent) != 1 {
		t.Fatalf("sent %d notices, want 1", len(sent))
	}
	var keys []string
	for _, a := range sent[0].Actions {
		keys = append(keys, a.Key)
	}
	want := []string{actionOpen, actionDone, actionHour, actionEvening, actionTomorrow}
	if !slices.Equal(keys, want) {
		t.Errorf("re-fired notice buttons = %v, want %v", keys, want)
	}
}

// Act promises in its own comment that applying a button also dismisses that notice's desktop banner, so a question answered in the window does not sit on screen asking it again. Two of its three paths returned before ever getting there: a press a goroutine was waiting on, and a press some code had registered an action for. Only the fallback switch closed anything.
// This is the "sometimes notifications don't close" of 2026-09-12: sometimes, because which path a press takes decides it, and the registered-action path is the common one.
func TestAct_ClosesTheBannerOnEveryPathThatApplied(t *testing.T) {
	t.Run("a press some code registered an action for", func(t *testing.T) {
		ctx := context.Background()
		s, _, f := testScheduler(t)
		ran := false
		setNoticeActionFor("meeting", "7", actionDone, func() error { ran = true; return nil })
		t.Cleanup(func() { setNoticeActionFor("meeting", "7", actionDone, nil) })
		s.say(Notice{Title: "Recording?", Body: "A call is on screen", Kind: "meeting", ID: "7"})

		if err := s.Act(ctx, "meeting", "7", "Recording?", "A call is on screen", actionDone); err != nil {
			t.Fatalf("Act: %v", err)
		}
		if !ran {
			t.Fatal("the registered action did not run, so this test is not exercising that path")
		}
		if len(f.closed) != 1 || f.closed[0] != "meeting|7" {
			t.Errorf("closed = %v, want the banner for meeting|7 closed", f.closed)
		}
	})

	t.Run("a press a goroutine was waiting on", func(t *testing.T) {
		ctx := context.Background()
		s, _, f := testScheduler(t)
		s.say(Notice{Title: "Still open?", Body: "Send the invoice", Kind: "stale", ID: "155"})

		// The stale-item question waits on its own answer channel, which is what carries a button its switch has never heard of, such as "dropped".
		key := noticeKey(Notice{Title: "Still open?", Kind: "stale", ID: "155"})
		answered, stop := awaitAnswer(key, []string{"dropped"})
		t.Cleanup(stop)

		if err := s.Act(ctx, "stale", "155", "Still open?", "Send the invoice", "dropped"); err != nil {
			t.Fatalf("Act: %v", err)
		}
		if got := <-answered; got != "dropped" {
			t.Fatalf("the waiter heard %q, want dropped", got)
		}
		if len(f.closed) != 1 || f.closed[0] != "stale|155" {
			t.Errorf("closed = %v, want the banner for stale|155 closed", f.closed)
		}
	})
}
