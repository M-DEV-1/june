package proactive

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/db/dbtest"
	"ora/internal/memory"
)

// notification is one captured notify call.
type notification struct {
	title, body string
}

// The tests that exercise a duty end to end zero its hour gate and keep the real clock, because the activity checks compare the clock against episode rows written at real wall time — a faked evening clock hours away from a just-written episode would trip them. The hour gate itself is pinned with a fake clock in its own test, where no store row is ever consulted.

// TestScheduler_Close_WritesDiaryUnderstandingAndNotifies is the tracer bullet for the evening close: with activity today, past the close hour, one tick must produce a diary entry grounded in the assembled material, a rewritten understanding doc, and exactly one notification carrying the entry's first line — and a second tick must do nothing more.
func TestScheduler_Close_WritesDiaryUnderstandingAndNotifies(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)

	if _, err := store.LogEpisode(ctx, "code", "ora — diary.go", "building the diary seam"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	if err := store.SetPersonalContext(ctx, "identity", "The user is Alex."); err != nil {
		t.Fatalf("SetPersonalContext: %v", err)
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format(dayFormat)
	if err := store.SetDiaryEntry(ctx, yesterday, "day", "Yesterday was quiet."); err != nil {
		t.Fatalf("SetDiaryEntry(yesterday): %v", err)
	}
	if err := store.SetDiaryEntry(ctx, "", "understanding", "A person who codes at night."); err != nil {
		t.Fatalf("SetDiaryEntry(understanding): %v", err)
	}

	var prompts []string
	brain := func(ctx context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		if len(prompts) == 1 {
			return "Today was about the diary seam.\n\nThe user built it all evening.", nil
		}
		return "A person who codes at night and now keeps a diary.", nil
	}
	var notes []notification
	s := New(store, brain, func(title, body string) { notes = append(notes, notification{title, body}) },
		config.ProactiveConfig{BriefHour: -1})
	s.closeHour = 0

	s.tick(ctx)

	today := time.Now().Format(dayFormat)
	entry, err := store.DiaryEntry(ctx, today, "day")
	if err != nil || entry != "Today was about the diary seam.\n\nThe user built it all evening." {
		t.Errorf("today's diary entry = %q, %v", entry, err)
	}
	u, err := store.DiaryEntry(ctx, "", "understanding")
	if err != nil || u != "A person who codes at night and now keeps a diary." {
		t.Errorf("understanding = %q, %v", u, err)
	}
	if len(prompts) != 2 {
		t.Fatalf("brain called %d times, want 2 (diary then understanding)", len(prompts))
	}
	for _, want := range []string{"Yesterday was quiet.", "Alex", "A person who codes at night."} {
		if !strings.Contains(prompts[0], want) {
			t.Errorf("diary prompt is missing %q", want)
		}
	}
	if !strings.Contains(prompts[1], "Today was about the diary seam.") {
		t.Errorf("understanding prompt is missing today's entry")
	}
	if len(notes) != 1 || notes[0].body != "Today was about the diary seam." {
		t.Errorf("notifications = %+v, want one carrying the entry's first line", notes)
	}

	// The entry now exists, so the close is done for today: another tick must not call the brain or notify again.
	s.tick(ctx)
	if len(prompts) != 2 || len(notes) != 1 {
		t.Errorf("second tick re-ran the close: %d brain calls, %d notifications", len(prompts), len(notes))
	}
}

// TestScheduler_Close_WaitsForCloseHour verifies the close does nothing before the configured hour. The hour gate is checked before any store read, so a fully fake clock is safe here.
func TestScheduler_Close_WaitsForCloseHour(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "working"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	calls := 0
	s := New(store, func(ctx context.Context, prompt string) (string, error) { calls++; return "entry", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: 22})
	now := time.Now()
	s.now = func() time.Time {
		return time.Date(now.Year(), now.Month(), now.Day(), 21, 30, 0, 0, now.Location())
	}

	s.tick(ctx)
	if calls != 0 {
		t.Errorf("brain called %d times before the close hour, want 0", calls)
	}
}

// TestScheduler_Close_RequiresActivityToday verifies a day with no episodes at all is not closed — there is nothing to write about, and an idle machine must not diary about it every evening.
func TestScheduler_Close_RequiresActivityToday(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)

	calls := 0
	s := New(store, func(ctx context.Context, prompt string) (string, error) { calls++; return "entry", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1})
	s.closeHour = 0

	s.tick(ctx)
	if calls != 0 {
		t.Errorf("brain called %d times with no activity today, want 0", calls)
	}
	if entry, _ := store.DiaryEntry(ctx, time.Now().Format(dayFormat), "day"); entry != "" {
		t.Errorf("diary entry written for an empty day: %q", entry)
	}
}

// TestScheduler_Close_RetriesAfterBrainFailure verifies a failed close leaves no partial state and succeeds on a later tick once the brain recovers and the backoff its failure earned has passed.
func TestScheduler_Close_RetriesAfterBrainFailure(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "working"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	failing := true
	s := New(store, func(ctx context.Context, prompt string) (string, error) {
		if failing {
			return "", fmt.Errorf("brain down")
		}
		return "The day, written.", nil
	}, func(string, string) {}, config.ProactiveConfig{BriefHour: -1})
	s.closeHour = 0
	n := time.Now()
	base := time.Date(n.Year(), n.Month(), n.Day(), 12, 0, 0, 0, n.Location())
	var offset time.Duration
	s.now = func() time.Time { return base.Add(offset) }
	today := base.Format(dayFormat)

	s.tick(ctx)
	if entry, _ := store.DiaryEntry(ctx, today, "day"); entry != "" {
		t.Fatalf("diary entry written despite brain failure: %q", entry)
	}

	failing = false
	offset = firstBackoff + time.Minute
	s.tick(ctx)
	if entry, _ := store.DiaryEntry(ctx, today, "day"); entry != "The day, written." {
		t.Errorf("diary entry after retry = %q, want the brain's answer", entry)
	}
}

// TestScheduler_Brief_DeliversOncePerDay is the tracer bullet for the morning brief: past the brief hour with fresh activity, one tick must deliver a brief built from the minutes and yesterday's entry, record its marker, and never deliver again that day.
func TestScheduler_Brief_DeliversOncePerDay(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)

	if _, err := store.LogEpisode(ctx, "code", "ora", "morning start"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	if _, err := store.LogNote(ctx, "Minutes: Alex to send the deck by Friday", "meeting"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format(dayFormat)
	if err := store.SetDiaryEntry(ctx, yesterday, "day", "Left the retrieval work half done."); err != nil {
		t.Fatalf("SetDiaryEntry(yesterday): %v", err)
	}

	var prompts []string
	var notes []notification
	s := New(store, func(ctx context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "Send the deck. Retrieval work is still half done.", nil
	}, func(title, body string) { notes = append(notes, notification{title, body}) },
		config.ProactiveConfig{CloseHour: -1})
	s.briefHour = 0

	s.tick(ctx)

	if len(notes) != 1 || notes[0].body != "Send the deck. Retrieval work is still half done." {
		t.Fatalf("notifications = %+v, want exactly the brief", notes)
	}
	if len(prompts) != 1 {
		t.Fatalf("brain called %d times, want 1", len(prompts))
	}
	for _, want := range []string{"send the deck by Friday", "Left the retrieval work half done."} {
		if !strings.Contains(prompts[0], want) {
			t.Errorf("brief prompt is missing %q", want)
		}
	}
	if marker, _ := store.DiaryEntry(ctx, time.Now().Format(dayFormat), "brief"); marker == "" {
		t.Error("brief marker row was not written")
	}

	s.tick(ctx)
	if len(notes) != 1 {
		t.Errorf("second tick delivered the brief again: %d notifications", len(notes))
	}
}

// TestScheduler_Brief_WaitsForUserActivity verifies the brief holds until an episode shows the user is actually present, so it lands when they sit down instead of at an empty desk.
func TestScheduler_Brief_WaitsForUserActivity(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)

	calls := 0
	s := New(store, func(ctx context.Context, prompt string) (string, error) { calls++; return "brief", nil },
		func(string, string) {}, config.ProactiveConfig{CloseHour: -1})
	s.briefHour = 0

	s.tick(ctx)
	if calls != 0 {
		t.Errorf("brain called %d times with no recent activity, want 0", calls)
	}
}

// lastSunday returns the most recent date on or before base that falls on a Sunday, keeping base's clock time. The weekly-study tests inject this as the scheduler's now: it must sit at or slightly before the real wall clock, because the activity gate compares the injected now against episode rows the store stamps with the real clock — a next-Sunday-in-the-future fake makes every fresh episode look days stale and the trigger never fires (that is exactly how these tests broke the first Monday they ran).
func lastSunday(base time.Time) time.Time {
	// Strictly before today, even when today is a Sunday. The freshness gate these tests rely on compares the fake clock against an episode logged at the real time, so a fake Sunday later in the day than the real clock reads as "that activity has not happened yet" and the pass never fires — which is what happened running these at 03:00 on Sunday 2026-09-13, against a fake clock pinned to 09:00.
	base = base.AddDate(0, 0, -1)
	for base.Weekday() != time.Sunday {
		base = base.AddDate(0, 0, -1)
	}
	// The hour is pinned to mid-morning so a test that advances its fake clock by minutes stays on the Sunday it started on; taking the wall clock's own hour made these tests fail when they ran late on a Sunday evening.
	return time.Date(base.Year(), base.Month(), base.Day(), 9, 0, 0, 0, base.Location())
}

// TestScheduler_WeeklyStudy_FiresOnceOnSunday is the tracer bullet for the Sunday trigger: past the brief hour, on a Sunday, with fresh activity, one tick must call the wired weeklyStudy func exactly once and write the once-per-Sunday marker, and a second tick must not call it again.
func TestScheduler_WeeklyStudy_FiresOnceOnSunday(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)

	sunday := lastSunday(time.Now())
	if _, err := store.LogEpisode(ctx, "code", "ora", "working"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	calls := 0
	var gotNow time.Time
	s := New(store, func(ctx context.Context, prompt string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.briefHour = 0
	s.now = func() time.Time { return sunday }
	s.SetWeeklyStudy(func(ctx context.Context, now time.Time) error {
		calls++
		gotNow = now
		return nil
	})

	s.tick(ctx)
	if calls != 1 {
		t.Fatalf("weeklyStudy called %d times, want 1", calls)
	}
	if !gotNow.Equal(sunday) {
		t.Errorf("weeklyStudy called with now=%v, want %v", gotNow, sunday)
	}
	marker, err := store.DiaryEntry(ctx, sunday.Format(dayFormat), "weekly-study")
	if err != nil || marker == "" {
		t.Errorf("weekly-study marker = %q, %v, want a non-empty marker written", marker, err)
	}

	s.tick(ctx)
	if calls != 1 {
		t.Errorf("weeklyStudy called %d times after a second tick, want still 1 (once-per-Sunday dedup)", calls)
	}
}

// TestScheduler_WeeklyStudy_NeverFiresOnANonSunday verifies the same hour/activity conditions on any other day of the week never call weeklyStudy — it is strictly a Sunday-only trigger.
func TestScheduler_WeeklyStudy_NeverFiresOnANonSunday(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)

	monday := lastSunday(time.Now()).AddDate(0, 0, 1)
	if _, err := store.LogEpisode(ctx, "code", "ora", "working"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	calls := 0
	s := New(store, func(ctx context.Context, prompt string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.briefHour = 0
	s.now = func() time.Time { return monday }
	s.SetWeeklyStudy(func(ctx context.Context, now time.Time) error { calls++; return nil })

	s.tick(ctx)
	if calls != 0 {
		t.Errorf("weeklyStudy called %d times on a Monday, want 0", calls)
	}
}

// TestScheduler_WeeklyStudy_BacksOffOnFailureWithoutMarking verifies a failing weeklyStudy does not write the once-per-Sunday marker, so the pass is tried again rather than recorded as done. It backs off like the close and the brief instead: the next tick inside the wait does nothing, and the first tick past it runs the study again.
func TestScheduler_WeeklyStudy_BacksOffOnFailureWithoutMarking(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)

	sunday := lastSunday(time.Now())
	if _, err := store.LogEpisode(ctx, "code", "ora", "working"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	calls := 0
	s := New(store, func(ctx context.Context, prompt string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.briefHour = 0
	now := sunday
	s.now = func() time.Time { return now }
	s.SetWeeklyStudy(func(ctx context.Context, now time.Time) error {
		calls++
		return fmt.Errorf("study pass failed")
	})

	s.tick(ctx)
	if calls != 1 {
		t.Fatalf("weeklyStudy called %d times, want 1", calls)
	}
	if marker, err := store.DiaryEntry(ctx, sunday.Format(dayFormat), "weekly-study"); err != nil || marker != "" {
		t.Errorf("weekly-study marker = %q, %v, want none written for a study that failed", marker, err)
	}

	now = sunday.Add(firstBackoff - time.Minute)
	s.tick(ctx)
	if calls != 1 {
		t.Errorf("weeklyStudy called %d times inside the backoff, want still 1", calls)
	}

	now = sunday.Add(firstBackoff + time.Minute)
	s.tick(ctx)
	if calls != 2 {
		t.Errorf("weeklyStudy called %d times once the backoff had passed, want 2", calls)
	}
}

// TestScheduler_WeeklyStudy_MarksDoneWhenNothingWasThereToRead verifies the "nothing to read" nil — cmd/daemon.go returns it when the machine has no replays and no dream traces — still writes the marker, since there is nothing to retry.
func TestScheduler_WeeklyStudy_MarksDoneWhenNothingWasThereToRead(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)

	sunday := lastSunday(time.Now())
	if _, err := store.LogEpisode(ctx, "code", "ora", "working"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	calls := 0
	s := New(store, func(ctx context.Context, prompt string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.briefHour = 0
	s.now = func() time.Time { return sunday }
	s.SetWeeklyStudy(func(ctx context.Context, now time.Time) error { calls++; return nil })

	s.tick(ctx)
	s.tick(ctx)
	if calls != 1 {
		t.Errorf("weeklyStudy called %d times, want 1: the marker records a pass that had nothing to read", calls)
	}
}

// openItem files one open action item raised daysAgo days ago and returns nothing — the brief is meant to find it by status, not by how recent its meeting was.
func openItem(t *testing.T, store *db.Store, owner, text string, priority string, daysAgo int) {
	t.Helper()
	_, err := store.AddActionItems(context.Background(), []memory.ActionItem{{
		Owner: owner, Text: text,
		Status: memory.StatusOpen, Priority: priority,
		Source: "md x mf tool", Raised: time.Now().AddDate(0, 0, -daysAgo),
	}})
	if err != nil {
		t.Fatalf("AddActionItems: %v", err)
	}
}

// An open action item reaches the brief however old the meeting that raised it is. This is the whole point of lifting action items out of the minutes: the minutes fall out of briefMinutesWindow after three days, and an owed task must not vanish with them.
func TestScheduler_Brief_CarriesOpenItemsPastTheMinutesWindow(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "morning start"); err != nil {
		t.Fatal(err)
	}
	openItem(t, store, "Me", "carry PR #13 through CI and merge.", memory.PriorityHigh, 9)

	var prompts []string
	s := New(store, func(ctx context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "brief", nil
	}, func(title, body string) {}, config.ProactiveConfig{CloseHour: -1})
	s.briefHour = 0
	s.tick(ctx)

	if len(prompts) != 1 {
		t.Fatalf("brain called %d times, want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], "carry PR #13 through CI and merge.") {
		t.Errorf("a nine-day-old open item never reached the brief prompt:\n%s", prompts[0])
	}
	if !strings.Contains(prompts[0], "Me") {
		t.Errorf("the brief prompt does not say who owes the item")
	}
}

// A closed item stops appearing. This is what a correction has to buy the user: saying something is done makes it go away.
func TestScheduler_Brief_DropsClosedItems(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "morning start"); err != nil {
		t.Fatal(err)
	}
	openItem(t, store, "Me", "finish the acme-essentials setup.", memory.PriorityNormal, 3)
	open, _ := store.OpenActionItems(ctx)
	if err := store.SetActionStatus(ctx, open[0].NoteID, memory.StatusDone); err != nil {
		t.Fatal(err)
	}

	var prompts []string
	s := New(store, func(ctx context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "brief", nil
	}, func(title, body string) {}, config.ProactiveConfig{CloseHour: -1})
	s.briefHour = 0
	s.tick(ctx)

	if strings.Contains(prompts[0], "acme-essentials") {
		t.Errorf("an item marked done still reached the brief:\n%s", prompts[0])
	}
}

// An item that has sat open for a while is asked about rather than restated: the user said they would rather be asked "were you able to make any progress here?" and answer, than be told the same thing every morning.
func TestScheduler_Brief_AsksAboutStaleItems(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "morning start"); err != nil {
		t.Fatal(err)
	}
	openItem(t, store, "Me", "reply on WhatsApp during his leave.", memory.PriorityLow, 12)
	openItem(t, store, "Me", "settle the payment.", memory.PriorityHigh, 1)

	var prompts []string
	s := New(store, func(ctx context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "brief", nil
	}, func(title, body string) {}, config.ProactiveConfig{CloseHour: -1})
	s.briefHour = 0
	s.tick(ctx)

	stale, fresh := "reply on WhatsApp during his leave.", "settle the payment."
	if !strings.Contains(prompts[0], stale) || !strings.Contains(prompts[0], fresh) {
		t.Fatalf("both items should reach the prompt:\n%s", prompts[0])
	}
	staleAt, freshAt := strings.Index(prompts[0], stale), strings.Index(prompts[0], fresh)
	if staleAt < freshAt {
		t.Errorf("the twelve-day-old item is not marked as one to ask about, it is listed alongside the fresh one:\n%s", prompts[0])
	}
}

// An item that has gone quiet is asked about in a notification the user can answer with one click, and the answer updates the item. This is the other half of "ask me and I'll tell you": the brief asks in words, this asks in a button.
func TestScheduler_Brief_AsksAboutAStaleItemAndAppliesTheAnswer(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "morning start"); err != nil {
		t.Fatal(err)
	}
	openItem(t, store, "Me", "improve capture resolution in the screen-frame tool.", memory.PriorityLow, 12)

	asked := make(chan string, 1)
	s := New(store, func(ctx context.Context, prompt string) (string, error) { return "brief", nil },
		func(title, body string) {}, config.ProactiveConfig{CloseHour: -1})
	s.briefHour = 0
	s.SetAsk(func(title, body string, actions []string) (string, error) {
		asked <- body
		return "done", nil
	})

	s.tick(ctx)

	select {
	case body := <-asked:
		if !strings.Contains(body, "improve capture resolution in the screen-frame tool.") {
			t.Errorf("the question does not name the item: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was ever asked about the twelve-day-old item")
	}

	// The answer has to actually land, or the click was theatre.
	deadline := time.Now().Add(2 * time.Second)
	for {
		open, err := store.OpenActionItems(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(open) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("answering \"done\" left the item open: %+v", open)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Nothing stale means no question — the notification only fires when there is something worth asking about.
func TestScheduler_Brief_NoQuestionWhenNothingIsStale(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "morning start"); err != nil {
		t.Fatal(err)
	}
	openItem(t, store, "Me", "settle the payment.", memory.PriorityNormal, 1)

	var asks int32
	s := New(store, func(ctx context.Context, prompt string) (string, error) { return "brief", nil },
		func(title, body string) {}, config.ProactiveConfig{CloseHour: -1})
	s.briefHour = 0
	s.SetAsk(func(title, body string, actions []string) (string, error) {
		atomic.AddInt32(&asks, 1)
		return "", nil
	})

	s.tick(ctx)
	time.Sleep(100 * time.Millisecond)

	if n := atomic.LoadInt32(&asks); n != 0 {
		t.Errorf("asked about %d items when nothing was stale", n)
	}
}

// An item filed under somebody else's name never reaches the user at all, brief or question: OpenActionItems already keeps only the user's own work, so this is not something deliverBrief has to guard against itself.
func TestScheduler_Brief_DropsSomebodyElsesItemEntirely(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "morning start"); err != nil {
		t.Fatal(err)
	}
	openItem(t, store, "Krish", "reply on WhatsApp during his leave.", memory.PriorityLow, 12)

	var asks int32
	var prompts []string
	s := New(store, func(ctx context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "brief", nil
	}, func(title, body string) {}, config.ProactiveConfig{CloseHour: -1})
	s.briefHour = 0
	s.SetAsk(func(title, body string, actions []string) (string, error) {
		atomic.AddInt32(&asks, 1)
		return "", nil
	})

	s.tick(ctx)
	time.Sleep(100 * time.Millisecond)

	if n := atomic.LoadInt32(&asks); n != 0 {
		t.Errorf("asked the user for progress on Krish's task %d times", n)
	}
	if strings.Contains(prompts[0], "reply on WhatsApp during his leave.") {
		t.Error("an item owed by somebody else reached the brief")
	}
}

// The four moments go to Ora's own card in the desktop window AND to the desktop notification, always: GNOME's banner cut every one of them off after two lines with nothing to click, but a window that has been closed all day still needs the notification's own buttons, and a notification answered from the message tray still needs the window's rail line to update — whichever surface the user is looking at has to work.

// TestScheduler_Brief_GoesToTheWindow checks the morning brief is handed to the window as a notice — title, body, the place a click opens and the moment it came from — and that no desktop notification is posted alongside it, since the window's card is the only surface while a window is up.
func TestScheduler_Brief_GoesToTheWindow(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "morning start"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	var notes []notification
	s := New(store, func(ctx context.Context, prompt string) (string, error) {
		return "Send the deck. Retrieval work is still half done.", nil
	}, func(title, body string) { notes = append(notes, notification{title, body}) },
		config.ProactiveConfig{CloseHour: -1})
	s.briefHour = 0

	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s.tick(ctx)

	want := Notice{Title: "Morning brief", Body: "Send the deck. Retrieval work is still half done.", Place: "days", ID: s.now().Format("2006-01-02"), Kind: "brief", Actions: noticeActions}
	if len(sent) != 1 || !reflect.DeepEqual(sent[0], want) {
		t.Errorf("notices = %+v, want exactly %+v", sent, want)
	}
	if len(notes) != 0 {
		t.Errorf("desktop notifications = %+v, want none while a window is up to draw the card", notes)
	}
}

// TestScheduler_Brief_FallsBackToNotifySend checks the brief still arrives through notify-send when no window is subscribed to the event stream, which is what the sender reports by returning false.
func TestScheduler_Brief_FallsBackToNotifySend(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "morning start"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	var notes []notification
	s := New(store, func(ctx context.Context, prompt string) (string, error) { return "One thing is still open.", nil },
		func(title, body string) { notes = append(notes, notification{title, body}) },
		config.ProactiveConfig{CloseHour: -1})
	s.briefHour = 0
	SetNoticeSender(func(n Notice) bool { return false })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s.tick(ctx)

	if len(notes) != 1 || notes[0].title != "Morning brief" || notes[0].body != "One thing is still open." {
		t.Errorf("notifications = %+v, want the brief through notify-send", notes)
	}
}

// TestScheduler_Close_GoesToTheWindow checks the evening close is handed over as a notice pointing at the day it just wrote, so clicking the card opens that day's page, and that no desktop notification is posted alongside it while a window is up.
func TestScheduler_Close_GoesToTheWindow(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "an evening on the diary seam"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	var notes []notification
	s := New(store, func(ctx context.Context, prompt string) (string, error) {
		return "Today was about the diary seam.\n\nThe user built it all evening.", nil
	}, func(title, body string) { notes = append(notes, notification{title, body}) },
		config.ProactiveConfig{BriefHour: -1})
	s.closeHour = 0

	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s.tick(ctx)

	want := Notice{Title: "Day's written down", Body: "Today was about the diary seam.", Place: "days", ID: time.Now().Format(dayFormat), Kind: "close", Actions: noticeActions}
	if len(sent) != 1 || !reflect.DeepEqual(sent[0], want) {
		t.Errorf("notices = %+v, want exactly %+v", sent, want)
	}
	if len(notes) != 0 {
		t.Errorf("desktop notifications = %+v, want none while a window is up to draw the card", notes)
	}
}

// TestNotify_PrefersTheWindow covers the meeting prep, which reaches this package through the recorder's own notifySend rather than through the scheduler: a package-level Notify offers itself to the window first, and only shells out to notify-send when no window took it. The kind is read from the icon, the one thing such a call carries that says what the moment is about.
//
// Every case here wires a sender that accepts, deliberately: a case that let the call fall through would run the real notify-send and put a banner on the screen of whoever is running the tests.
func TestNotify_PrefersTheWindow(t *testing.T) {
	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	Notify("audio-input-microphone", "Before you join: standup", "Last time you owed the deck.")
	Notify("x-office-calendar", "Recording saved", "Ora will transcribe it once you plug in.")

	// The card is drawn from the notice's own actions, so a moment with no task behind it has to carry its one Open button rather than leave the window to guess: a "Transcribing meeting" card that offered Done and the snoozes answered "Could not do that" when one was pressed.
	want := []Notice{
		{Title: "Before you join: standup", Body: "Last time you owed the deck.", Kind: "meeting", Actions: openOnlyActions},
		{Title: "Recording saved", Body: "Ora will transcribe it once you plug in.", Kind: "day", Actions: openOnlyActions},
	}
	if len(sent) != len(want) {
		t.Fatalf("notices = %+v, want %+v", sent, want)
	}
	for i := range want {
		if !reflect.DeepEqual(sent[i], want[i]) {
			t.Errorf("notice %d = %+v, want %+v", i, sent[i], want[i])
		}
	}
}

// A body long enough that notify-send would have cut it off still goes to the window whole: the length rule exists because GNOME truncates, and Ora's own card does not.
func TestNotify_SendsALongBodyToTheWindowWhole(t *testing.T) {
	long := strings.Repeat("a sentence about the day. ", 20)
	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	Notify("x-office-calendar", "Morning brief", long)

	if len(sent) != 1 || sent[0].Body != long {
		t.Errorf("notices = %+v, want the whole long body", sent)
	}
}

// GNOME cuts a notification body off after a few lines, so a morning brief or a meeting prep that runs long was never readable in full. A long body gets a button that opens the whole text; a short one stays a plain notification.
func TestNotifyArgs_LongBodyGetsReadAction(t *testing.T) {
	long := strings.Repeat("a sentence about the day. ", 20)
	args := notifyArgs("x-office-calendar", "Morning brief", long)
	if !strings.Contains(strings.Join(args, " "), "--action="+readAction+"=") {
		t.Errorf("long body should carry the read-in-full action, got %v", args)
	}
	args = notifyArgs("x-office-calendar", "Recording meeting", "Ora is recording.")
	if strings.Contains(strings.Join(args, " "), "--action=") {
		t.Errorf("short body should stay a plain notification, got %v", args)
	}
}

// stubStore wraps a store so one test can replace a single method. The real store cannot write a screen summary at a past time, and cannot be made to refuse one write while every other one still works. A nil field delegates to the wrapped store.
type stubStore struct {
	Store
	summaries func(ctx context.Context, since, until time.Time) ([]db.WindowSummary, error)
	setDiary  func(ctx context.Context, day, kind, content string) error
	setRun    func(ctx context.Context, id int64, when time.Time, answer string) error
}

// SummaryTimeline returns the stubbed timeline when one is set, otherwise the wrapped store's.
func (s *stubStore) SummaryTimeline(ctx context.Context, since, until time.Time) ([]db.WindowSummary, error) {
	if s.summaries != nil {
		return s.summaries(ctx, since, until)
	}
	return s.Store.SummaryTimeline(ctx, since, until)
}

// SetDiaryEntry writes through the stub when one is set, otherwise to the wrapped store.
func (s *stubStore) SetDiaryEntry(ctx context.Context, day, kind, content string) error {
	if s.setDiary != nil {
		return s.setDiary(ctx, day, kind, content)
	}
	return s.Store.SetDiaryEntry(ctx, day, kind, content)
}

// SetRoutineRun writes through the stub when one is set, otherwise to the wrapped store.
func (s *stubStore) SetRoutineRun(ctx context.Context, id int64, when time.Time, answer string) error {
	if s.setRun != nil {
		return s.setRun(ctx, id, when, answer)
	}
	return s.Store.SetRoutineRun(ctx, id, when, answer)
}

// TestTick_HungDutyDoesNotStopTheNext checks every duty gets its own deadline: a close whose brain call never returns must not stop the brief running on the same tick. A wedged Gemini call used to freeze every later duty for the life of the daemon.
func TestTick_HungDutyDoesNotStopTheNext(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "working"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	s := New(store, func(ctx context.Context, prompt string) (string, error) {
		if strings.HasPrefix(prompt, diaryInstruction) {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "Three things today.", nil
	}, func(string, string) {}, config.ProactiveConfig{})
	s.briefHour, s.closeHour = 0, 0
	s.dutyTimeout = 50 * time.Millisecond

	s.tick(ctx)

	if brief, _ := store.DiaryEntry(ctx, time.Now().Format(dayFormat), "brief"); brief != "Three things today." {
		t.Errorf("brief marker after a hung close = %q, want the brief delivered anyway", brief)
	}
}

// TestTick_StoreOnlyDutiesRunBeforeTheBrainOnes checks the cheap store-only duties are run first, so a "remind me in an hour" is not held up by a brain call that takes minutes.
func TestTick_StoreOnlyDutiesRunBeforeTheBrainOnes(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "working"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	if _, err := store.AddSnooze(ctx, "task", "42", "Still open", "Send the invoice", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("AddSnooze: %v", err)
	}

	var order []string
	SetNoticeSender(func(n Notice) bool { order = append(order, "notice:"+n.Kind); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s := New(store, func(context.Context, string) (string, error) {
		order = append(order, "brain")
		return "The day, written.", nil
	}, func(string, string) {}, config.ProactiveConfig{BriefHour: -1})
	s.closeHour = 0

	s.tick(ctx)

	if len(order) == 0 || order[0] != "notice:task" {
		t.Errorf("tick order = %v, want the due snooze raised before any brain call", order)
	}
}

// TestScheduler_Close_BacksOffAfterAFailure checks a close whose brain call failed is not retried on the very next tick: five minutes after the first failure, thirty after each one after that. Before this a broken brain cost one counted request a minute for the rest of the day.
func TestScheduler_Close_BacksOffAfterAFailure(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "ora", "working"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	calls := 0
	s := New(store, func(context.Context, string) (string, error) {
		calls++
		return "", fmt.Errorf("brain down")
	}, func(string, string) {}, config.ProactiveConfig{BriefHour: -1})
	s.closeHour = 0
	n := time.Now()
	base := time.Date(n.Year(), n.Month(), n.Day(), 12, 0, 0, 0, n.Location())
	var offset time.Duration
	s.now = func() time.Time { return base.Add(offset) }

	steps := []struct {
		after time.Duration
		want  int
	}{
		{0, 1},
		{time.Minute, 1},
		{firstBackoff + time.Minute, 2},
		{firstBackoff + 2*time.Minute, 2},
		{firstBackoff + laterBackoff + 2*time.Minute, 3},
	}
	for _, st := range steps {
		offset = st.after
		s.tick(ctx)
		if calls != st.want {
			t.Errorf("%s after the first failure: %d brain calls, want %d", st.after, calls, st.want)
		}
	}
}

// TestScheduler_Close_CatchesUpADaySleptThrough checks a day the machine was asleep through at the close hour still gets its diary entry on the first tick after the next day's close hour, written from that day's own timeline. The lost row is also the next day's "yesterday's entry" prompt input.
func TestScheduler_Close_CatchesUpADaySleptThrough(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	n := time.Now()
	today := time.Date(n.Year(), n.Month(), n.Day(), 23, 0, 0, 0, n.Location())
	yesterdayNoon := today.AddDate(0, 0, -1).Add(-11 * time.Hour)

	stub := &stubStore{Store: store, summaries: func(ctx context.Context, since, until time.Time) ([]db.WindowSummary, error) {
		if yesterdayNoon.Before(since) || yesterdayNoon.After(until) {
			return nil, nil
		}
		return []db.WindowSummary{{CreatedAt: yesterdayNoon, Content: "wrote the recorder"}}, nil
	}}

	var prompts []string
	s := New(stub, func(ctx context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "Yesterday was the recorder.", nil
	}, func(string, string) {}, config.ProactiveConfig{BriefHour: -1})
	s.closeHour = 22
	s.now = func() time.Time { return today }

	s.tick(ctx)

	if entry, _ := store.DiaryEntry(ctx, today.AddDate(0, 0, -1).Format(dayFormat), "day"); entry != "Yesterday was the recorder." {
		t.Errorf("yesterday's diary entry = %q, want it written from yesterday's own timeline", entry)
	}
	if entry, _ := store.DiaryEntry(ctx, today.Format(dayFormat), "day"); entry != "" {
		t.Errorf("today was closed with no activity recorded: %q", entry)
	}
	if len(prompts) == 0 || !strings.Contains(prompts[0], "wrote the recorder") {
		t.Errorf("the diary prompt did not carry yesterday's timeline: %v", prompts)
	}
}

// TestScheduler_Close_GivesEachDayItsOwnDeadline checks the catch-up day and today are closed under a deadline each, not both under one. Each close makes two brain calls, so four calls had to fit in one duty timeout on the first tick after a night the machine slept through, and the deadline expiring made yesterday's close eat the budget today's needed.
func TestScheduler_Close_GivesEachDayItsOwnDeadline(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	n := time.Now()
	today := time.Date(n.Year(), n.Month(), n.Day(), 23, 0, 0, 0, n.Location())

	// Both days have a timeline, so both are closed on this one tick.
	stub := &stubStore{Store: store, summaries: func(ctx context.Context, since, until time.Time) ([]db.WindowSummary, error) {
		return []db.WindowSummary{{CreatedAt: until.Add(-time.Hour), Content: "wrote the recorder"}}, nil
	}}

	// Every brain call spends most of one duty timeout and then checks whether it still has one. Under a single shared deadline the second day's calls are already past it.
	const call = 30 * time.Millisecond
	s := New(stub, func(ctx context.Context, prompt string) (string, error) {
		time.Sleep(call)
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "The day, written.", nil
	}, func(string, string) {}, config.ProactiveConfig{BriefHour: -1})
	s.closeHour = 22
	s.now = func() time.Time { return today }
	s.dutyTimeout = 3 * call

	s.tick(ctx)

	for _, day := range []time.Time{today.AddDate(0, 0, -1), today} {
		if entry, _ := store.DiaryEntry(ctx, day.Format(dayFormat), "day"); entry != "The day, written." {
			t.Errorf("%s diary entry = %q, want it written under its own deadline", day.Format(dayFormat), entry)
		}
	}
}

// TestDutyTimeoutFor_HoldsTwoBrainCallsWithHeadroom pins the deadline one duty gets against the brain timeout it has to cover. The evening close makes two brain calls, so ten minutes against a 300-second call ceiling was exactly the two calls with nothing left for the store reads and the prompt assembly around them.
func TestDutyTimeoutFor_HoldsTwoBrainCallsWithHeadroom(t *testing.T) {
	call := time.Duration(config.DefaultBrainTimeoutSeconds) * time.Second
	if got := dutyTimeoutFor(call); got <= 2*call {
		t.Errorf("dutyTimeoutFor(%v) = %v, want more than the two brain calls a close makes", call, got)
	}
	if got := dutyTimeoutFor(60 * time.Second); got >= dutyTimeoutFor(300*time.Second) {
		t.Error("the duty deadline does not follow the configured brain timeout")
	}
	if New(dbtest.Open(t), nil, nil, config.ProactiveConfig{}).dutyTimeout != dutyTimeoutFor(call) {
		t.Error("New's default duty timeout is not derived from the default brain timeout")
	}
}

// TestSetBrainTimeout_WidensTheDutyDeadline checks the daemon can hand the scheduler the brain timeout its config actually carries, so a machine that raised the CLI ceiling does not keep a deadline sized for the default.
func TestSetBrainTimeout_WidensTheDutyDeadline(t *testing.T) {
	s := New(dbtest.Open(t), nil, nil, config.ProactiveConfig{})
	s.SetBrainTimeout(20 * time.Minute)
	if got := s.dutyTimeout; got != dutyTimeoutFor(20*time.Minute) {
		t.Errorf("dutyTimeout = %v after SetBrainTimeout(20m), want %v", got, dutyTimeoutFor(20*time.Minute))
	}
}

// staleItem stores one stale action item and hands it back as the scheduler's own duty would read it, so a test can put the question straight to askAbout. Input: the test and the store. Output: the item, note id and all.
func staleItem(t *testing.T, store *db.Store) memory.ActionItem {
	t.Helper()
	openItem(t, store, "Me", "improve capture resolution in the screen-frame tool.", memory.PriorityLow, 12)
	items, err := store.OpenActionItems(context.Background())
	if err != nil {
		t.Fatalf("OpenActionItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("%d open items, want the one just stored", len(items))
	}
	return items[0]
}

// openCount is how many action items are still open on the store.
func openCount(t *testing.T, store *db.Store) int {
	t.Helper()
	open, err := store.OpenActionItems(context.Background())
	if err != nil {
		t.Fatalf("OpenActionItems: %v", err)
	}
	return len(open)
}

// The daily "Still open" question used to go straight to notify-send, which is why it kept appearing as a GNOME banner while every other moment had moved to Ora's own card. With a window up it is now a notice carrying its own three answers, and the answer comes back through the same POST /notices/{kind}/{id}/action route the card's other buttons use.
func TestAskAbout_WindowUp_AsksOnTheCardAndAppliesTheAnswer(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	item := staleItem(t, store)

	sent := make(chan Notice, 4)
	SetNoticeSender(func(n Notice) bool { sent <- n; return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s := New(store, nil, func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	var banners int32
	s.SetAsk(func(string, string, []string) (string, error) {
		atomic.AddInt32(&banners, 1)
		return "", nil
	})

	done := make(chan struct{})
	go func() { defer close(done); s.askAbout(ctx, item) }()

	var n Notice
	select {
	case n = <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("the stale-item question never reached the window")
	}
	if atomic.LoadInt32(&banners) != 0 {
		t.Error("the question went to notify-send as well as to the window's card")
	}
	if n.Kind != staleNoticeKind || n.ID != strconv.FormatInt(item.NoteID, 10) {
		t.Errorf("notice kind/id = %q/%q, want %q/%d", n.Kind, n.ID, staleNoticeKind, item.NoteID)
	}
	want := []Action{{"done", "Done"}, {"dropped", "Not happening"}, {"low", "Not urgent"}}
	if !reflect.DeepEqual(n.Actions, want) {
		t.Errorf("notice actions = %+v, want %+v", n.Actions, want)
	}

	if err := s.Act(ctx, n.Kind, n.ID, n.Title, n.Body, "dropped"); err != nil {
		t.Fatalf("Act with the card's own answer: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never reached the goroutine waiting on the question")
	}
	if got := openCount(t, store); got != 0 {
		t.Errorf("%d items still open, want the answered one dropped", got)
	}
}

// With no window listening the question stays exactly where it was: a notify-send banner carrying the same three buttons, answered the same way.
func TestAskAbout_NoWindow_KeepsTheNotifySendBanner(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	item := staleItem(t, store)

	SetNoticeSender(func(Notice) bool { return false })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s := New(store, nil, func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	var gotTitle, gotBody string
	var gotActions []string
	s.SetAsk(func(title, body string, actions []string) (string, error) {
		gotTitle, gotBody, gotActions = title, body, actions
		return "done", nil
	})

	s.askAbout(ctx, item)

	if gotTitle != "Still open" || !strings.Contains(gotBody, item.Text) {
		t.Errorf("banner asked %q / %q, want the stale item named under \"Still open\"", gotTitle, gotBody)
	}
	wantActions := []string{"done=Done", "dropped=Not happening", "low=Not urgent"}
	if !reflect.DeepEqual(gotActions, wantActions) {
		t.Errorf("banner actions = %v, want %v", gotActions, wantActions)
	}
	if got := openCount(t, store); got != 0 {
		t.Errorf("%d items still open, want the answered one done", got)
	}
}

// A card nobody answers in time changes nothing yet, and its buttons go on working: the question's goroutine has gone, so the press runs through what Ask registered against that notice instead, and the item it asked about is the one that changes. Pressing it used to be refused as an unknown action, and the card said "Could not do that".
func TestAskAbout_WindowUp_UnansweredKeepsItsButtonsWorking(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	item := staleItem(t, store)

	SetNoticeSender(func(Notice) bool { return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s := New(store, nil, func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.askWait = 20 * time.Millisecond
	s.SetAsk(func(string, string, []string) (string, error) {
		t.Error("an unanswered card fell back to the notify-send banner")
		return "", nil
	})

	s.askAbout(ctx, item)

	if got := openCount(t, store); got != 1 {
		t.Errorf("%d items open, want the unanswered one left alone", got)
	}
	if err := s.Act(ctx, staleNoticeKind, strconv.FormatInt(item.NoteID, 10), "Still open", "", "dropped"); err != nil {
		t.Fatalf("pressing the card's own button after the question timed out: %v", err)
	}
	if got := openCount(t, store); got != 0 {
		t.Errorf("%d items still open, want the one the late press dropped", got)
	}
}

// A key none of a notice's own buttons carries is still refused, so the route answers 400 for it exactly as it did before the registry existed.
func TestAct_UnknownKeyOnAWaitingNotice(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	item := staleItem(t, store)

	SetNoticeSender(func(Notice) bool { return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	s := New(store, nil, func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.askWait = 2 * time.Second
	go s.askAbout(ctx, item)
	time.Sleep(50 * time.Millisecond)

	id := strconv.FormatInt(item.NoteID, 10)
	if err := s.Act(ctx, staleNoticeKind, id, "Still open", "", "banana"); !errors.Is(err, ErrBadNoticeAction) {
		t.Errorf("Act with a key the card never offered = %v, want ErrBadNoticeAction", err)
	}
	if err := s.Act(ctx, "task", id, "Still open", "", "dropped"); !errors.Is(err, ErrBadNoticeAction) {
		t.Errorf("Act on another notice's kind = %v, want ErrBadNoticeAction — the registry is keyed by kind and id", err)
	}
}
