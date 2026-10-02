package proactive

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"june/internal/config"
	"june/internal/db"
	"june/internal/db/dbtest"
	"june/internal/memory"
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

	if _, err := store.LogEpisode(ctx, "code", "june — diary.go", "building the diary seam"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	if err := store.SetPersonalContext(ctx, "identity", "The user is Zemna."); err != nil {
		t.Fatalf("SetPersonalContext: %v", err)
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format(time.DateOnly)
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

	today := time.Now().Format(time.DateOnly)
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
	for _, want := range []string{"Yesterday was quiet.", "Zemna", "A person who codes at night."} {
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

// TestScheduler_Close_RetriesAfterBrainFailure verifies a failed close leaves no partial state and succeeds on a later tick once the brain recovers and the backoff its failure earned has passed.
func TestScheduler_Close_RetriesAfterBrainFailure(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "june", "working"); err != nil {
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
	today := base.Format(time.DateOnly)

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

	if _, err := store.LogEpisode(ctx, "code", "june", "morning start"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	if _, err := store.LogNote(ctx, "Minutes: Zemna to send the deck by Friday", "meeting"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format(time.DateOnly)
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
	if marker, _ := store.DiaryEntry(ctx, time.Now().Format(time.DateOnly), "brief"); marker == "" {
		t.Error("brief marker row was not written")
	}

	s.tick(ctx)
	if len(notes) != 1 {
		t.Errorf("second tick delivered the brief again: %d notifications", len(notes))
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
	if _, err := store.LogEpisode(ctx, "code", "june", "working"); err != nil {
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
	marker, err := store.DiaryEntry(ctx, sunday.Format(time.DateOnly), "weekly-study")
	if err != nil || marker == "" {
		t.Errorf("weekly-study marker = %q, %v, want a non-empty marker written", marker, err)
	}

	s.tick(ctx)
	if calls != 1 {
		t.Errorf("weeklyStudy called %d times after a second tick, want still 1 (once-per-Sunday dedup)", calls)
	}
}

// openItem files one open action item raised daysAgo days ago and returns nothing — the brief is meant to find it by status, not by how recent its meeting was.
func openItem(t *testing.T, store *db.Store, owner, text string, priority string, daysAgo int) {
	t.Helper()
	_, err := store.AddActionItems(context.Background(), []memory.ActionItem{{
		Owner: owner, Text: text,
		Status: memory.StatusOpen, Priority: priority,
		Source: "vq x zb tool", Raised: time.Now().AddDate(0, 0, -daysAgo),
	}})
	if err != nil {
		t.Fatalf("AddActionItems: %v", err)
	}
}

// An open action item reaches the brief however old the meeting that raised it is. This is the whole point of lifting action items out of the minutes: the minutes fall out of briefMinutesWindow after three days, and an owed task must not vanish with them.
func TestScheduler_Brief_CarriesOpenItemsPastTheMinutesWindow(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "june", "morning start"); err != nil {
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

// An item that has gone quiet is asked about in a notification the user can answer with one click, and the answer updates the item. This is the other half of "ask me and I'll tell you": the brief asks in words, this asks in a button.
func TestScheduler_Brief_AsksAboutAStaleItemAndAppliesTheAnswer(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.LogEpisode(ctx, "code", "june", "morning start"); err != nil {
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

// The four moments go to June's own card in the desktop window AND to the desktop notification, always: GNOME's banner cut every one of them off after two lines with nothing to click, but a window that has been closed all day still needs the notification's own buttons, and a notification answered from the message tray still needs the window's rail line to update — whichever surface the user is looking at has to work.

// TestNotify_PrefersTheWindow covers the meeting prep, which reaches this package through the recorder's own notifySend rather than through the scheduler: a package-level Notify offers itself to the window first, and only shells out to notify-send when no window took it. The kind is read from the icon, the one thing such a call carries that says what the moment is about.
//
// Every case here wires a sender that accepts, deliberately: a case that let the call fall through would run the real notify-send and put a banner on the screen of whoever is running the tests.
func TestNotify_PrefersTheWindow(t *testing.T) {
	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	Notify("audio-input-microphone", "Before you join: standup", "Last time you owed the deck.")
	Notify("x-office-calendar", "Recording saved", "June will transcribe it once you plug in.")

	// The card is drawn from the notice's own actions, so a moment with no task behind it has to carry its one Open button rather than leave the window to guess: a "Transcribing meeting" card that offered Done and the snoozes answered "Could not do that" when one was pressed.
	want := []Notice{
		{Title: "Before you join: standup", Body: "Last time you owed the deck.", Kind: "meeting", Actions: openOnlyActions},
		{Title: "Recording saved", Body: "June will transcribe it once you plug in.", Kind: "day", Actions: openOnlyActions},
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
	if _, err := store.LogEpisode(ctx, "code", "june", "working"); err != nil {
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

	if brief, _ := store.DiaryEntry(ctx, time.Now().Format(time.DateOnly), "brief"); brief != "Three things today." {
		t.Errorf("brief marker after a hung close = %q, want the brief delivered anyway", brief)
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

	if entry, _ := store.DiaryEntry(ctx, today.AddDate(0, 0, -1).Format(time.DateOnly), "day"); entry != "Yesterday was the recorder." {
		t.Errorf("yesterday's diary entry = %q, want it written from yesterday's own timeline", entry)
	}
	if entry, _ := store.DiaryEntry(ctx, today.Format(time.DateOnly), "day"); entry != "" {
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

	// Every brain call spends most of one duty timeout and then checks whether it still has one. Under a single shared deadline the second day's calls are already past it. The call is long enough that a slow SQLite write on a Windows runner still fits in what is left of each day's deadline; at 30 ms it did not.
	const call = 200 * time.Millisecond
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
		if entry, _ := store.DiaryEntry(ctx, day.Format(time.DateOnly), "day"); entry != "The day, written." {
			t.Errorf("%s diary entry = %q, want it written under its own deadline", day.Format(time.DateOnly), entry)
		}
	}
}
