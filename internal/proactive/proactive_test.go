package proactive

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"ora/internal/config"
	"ora/internal/db"
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
	if err := store.SetPersonalContext(ctx, "identity", "The user is Mahadevan."); err != nil {
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
	for _, want := range []string{"Yesterday was quiet.", "Mahadevan", "A person who codes at night."} {
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
	if _, err := store.LogNote(ctx, "Minutes: Mahadevan to send the deck by Friday", "meeting"); err != nil {
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
