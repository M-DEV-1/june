package proactive

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/memory"
)

// testStore opens a throwaway in-memory store that is closed when the test ends.
func testStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// notification is one captured notify call.
type notification struct {
	title, body string
}

// The tests that exercise a duty end to end zero its hour gate and keep the real clock, because the activity checks compare the clock against episode rows written at real wall time — a faked evening clock hours away from a just-written episode would trip them. The hour gate itself is pinned with a fake clock in its own test, where no store row is ever consulted.

// TestScheduler_Close_WritesDiaryUnderstandingAndNotifies is the tracer bullet for the evening close: with activity today, past the close hour, one tick must produce a diary entry grounded in the assembled material, a rewritten understanding doc, and exactly one notification carrying the entry's first line — and a second tick must do nothing more.
func TestScheduler_Close_WritesDiaryUnderstandingAndNotifies(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)

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
	store := testStore(t)
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
	store := testStore(t)

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

// TestScheduler_Close_RetriesNextTickAfterBrainFailure verifies a failed close leaves no partial state and succeeds on a later tick once the brain recovers.
func TestScheduler_Close_RetriesNextTickAfterBrainFailure(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
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
	today := time.Now().Format(dayFormat)

	s.tick(ctx)
	if entry, _ := store.DiaryEntry(ctx, today, "day"); entry != "" {
		t.Fatalf("diary entry written despite brain failure: %q", entry)
	}

	failing = false
	s.tick(ctx)
	if entry, _ := store.DiaryEntry(ctx, today, "day"); entry != "The day, written." {
		t.Errorf("diary entry after retry = %q, want the brain's answer", entry)
	}
}

// TestScheduler_Brief_DeliversOncePerDay is the tracer bullet for the morning brief: past the brief hour with fresh activity, one tick must deliver a brief built from the minutes and yesterday's entry, record its marker, and never deliver again that day.
func TestScheduler_Brief_DeliversOncePerDay(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)

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
	store := testStore(t)

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
	for base.Weekday() != time.Sunday {
		base = base.AddDate(0, 0, -1)
	}
	return base
}

// TestScheduler_WeeklyStudy_FiresOnceOnSunday is the tracer bullet for the Sunday trigger: past the brief hour, on a Sunday, with fresh activity, one tick must call the wired weeklyStudy func exactly once and write the once-per-Sunday marker, and a second tick must not call it again.
func TestScheduler_WeeklyStudy_FiresOnceOnSunday(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)

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
	store := testStore(t)

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

// TestScheduler_WeeklyStudy_MarksDoneEvenOnFailure verifies a failing weeklyStudy still writes the once-per-Sunday marker — the trigger logs and moves on rather than retrying every minute for the rest of the day, unlike the close/brief duties which do retry on failure.
func TestScheduler_WeeklyStudy_MarksDoneEvenOnFailure(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)

	sunday := lastSunday(time.Now())
	if _, err := store.LogEpisode(ctx, "code", "ora", "working"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	calls := 0
	s := New(store, func(ctx context.Context, prompt string) (string, error) { return "", nil },
		func(string, string) {}, config.ProactiveConfig{BriefHour: -1, CloseHour: -1})
	s.briefHour = 0
	s.now = func() time.Time { return sunday }
	s.SetWeeklyStudy(func(ctx context.Context, now time.Time) error {
		calls++
		return fmt.Errorf("study pass failed")
	})

	s.tick(ctx)
	if calls != 1 {
		t.Fatalf("weeklyStudy called %d times, want 1", calls)
	}
	if marker, err := store.DiaryEntry(ctx, sunday.Format(dayFormat), "weekly-study"); err != nil || marker == "" {
		t.Errorf("weekly-study marker = %q, %v, want a marker even though weeklyStudy failed", marker, err)
	}

	s.tick(ctx)
	if calls != 1 {
		t.Errorf("weeklyStudy called %d times after a second tick, want still 1", calls)
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
	store := testStore(t)
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
	store := testStore(t)
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
	store := testStore(t)
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
	store := testStore(t)
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
	store := testStore(t)
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
	store := testStore(t)
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
	store := testStore(t)
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

	want := Notice{Title: "Morning brief", Body: "Send the deck. Retrieval work is still half done.", Place: "tasks", Kind: "brief"}
	if len(sent) != 1 || sent[0] != want {
		t.Errorf("notices = %+v, want exactly %+v", sent, want)
	}
	if len(notes) != 0 {
		t.Errorf("desktop notifications = %+v, want none while a window is up to draw the card", notes)
	}
}

// TestScheduler_Brief_FallsBackToNotifySend checks the brief still arrives through notify-send when no window is subscribed to the event stream, which is what the sender reports by returning false.
func TestScheduler_Brief_FallsBackToNotifySend(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
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
	store := testStore(t)
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

	want := Notice{Title: "Day's written down", Body: "Today was about the diary seam.", Place: "days", ID: time.Now().Format(dayFormat), Kind: "close"}
	if len(sent) != 1 || sent[0] != want {
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

	want := []Notice{
		{Title: "Before you join: standup", Body: "Last time you owed the deck.", Kind: "meeting"},
		{Title: "Recording saved", Body: "Ora will transcribe it once you plug in.", Kind: "day"},
	}
	if len(sent) != len(want) {
		t.Fatalf("notices = %+v, want %+v", sent, want)
	}
	for i := range want {
		if sent[i] != want[i] {
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
