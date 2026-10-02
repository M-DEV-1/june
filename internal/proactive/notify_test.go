package proactive

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"june/internal/config"
	"june/internal/db"
	"june/internal/db/dbtest"
	"june/internal/memory"
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

// TestNotify_CarriesOnlyOpen checks that a moment posted through proactive.Notify offers one button, "Open in June", on both surfaces it can reach: the bus notifier when no window is listening, and the window's own card when one is. Nothing posted this way has a task behind it, so Done and the three snoozes have nothing to act on — a "Transcribing meeting" card that offered them answered "Could not do that" when one was pressed.
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
		t.Errorf("actions = %+v, want just Open in June", f.sent[0].actions)
	}

	f.press(t, 0, actionOpen)
	if opened != 1 {
		t.Errorf("pressing Open in June opened the window %d times, want 1", opened)
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
