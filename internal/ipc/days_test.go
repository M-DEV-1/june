package ipc

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"ora/internal/db"
	"ora/internal/memory"
)

// TestDaysListsActiveDays checks that a day with a meeting on it shows up, and that a day with a diary entry is marked as having a page.
func TestDaysListsActiveDays(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()
	storeWithMeeting(t, store)
	if _, err := store.WriteEpisode(ctx, db.EpisodeWrite{App: "Brave", Title: "a tab", ScreenText: "text"}); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
	if _, err := store.WriteEpisode(ctx, db.EpisodeWrite{App: "Brave", Title: "another tab", ScreenText: "text"}); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
	today := time.Now().Format("2006-01-02")
	if err := store.SetDiaryEntry(ctx, today, "day", "A long day.\nThe demo went out."); err != nil {
		t.Fatalf("seed diary: %v", err)
	}

	_, srv := newWindowServer(t, &fakeAsker{}, store)
	var list struct{ Days []DaySummary }
	getJSON(t, srv, "/days", &list)
	if list.Days == nil {
		t.Fatalf("days came back null, want an empty list")
	}
	if len(list.Days) != 1 || list.Days[0].Date != today {
		t.Fatalf("days = %+v, want just today", list.Days)
	}
	if !list.Days[0].HasPage {
		t.Errorf("today has a diary entry but has_page is false")
	}
	if list.Days[0].Title != "A long day." {
		t.Errorf("title = %q, want the first line of the day's page", list.Days[0].Title)
	}
	if list.Days[0].Seen != 2 {
		t.Errorf("seen = %d, want the two episodes recorded today", list.Days[0].Seen)
	}
	if list.Days[0].Meetings != 1 {
		t.Errorf("meetings = %d, want the one meeting filed today", list.Days[0].Meetings)
	}
	if list.Days[0].MeetingMinutes != 0 {
		t.Errorf("meeting_minutes = %d, want 0 — the seeded meeting carries no duration marker", list.Days[0].MeetingMinutes)
	}
}

// TestDaysOmitsEmptyDays checks that a day ActiveDays surfaces but that turns out to hold nothing at all is left off the list rather than shown as an empty day. A diary entry dated after today is exactly such a day: ActiveDays' union of the diary table has no upper bound, so it surfaces, but DiaryDays (bounded to [from, now]) never returns its page, and it has no episode or meeting of its own — seen, meetings and has_page all come back zero.
func TestDaysOmitsEmptyDays(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	if err := store.SetDiaryEntry(ctx, tomorrow, "day", "not really today yet"); err != nil {
		t.Fatalf("seed diary: %v", err)
	}
	today := time.Now().Format("2006-01-02")
	if err := store.SetDiaryEntry(ctx, today, "day", "A real day."); err != nil {
		t.Fatalf("seed diary: %v", err)
	}

	_, srv := newWindowServer(t, &fakeAsker{}, store)
	var list struct{ Days []DaySummary }
	getJSON(t, srv, "/days", &list)
	for _, d := range list.Days {
		if d.Date == tomorrow {
			t.Errorf("days = %+v, want the empty day left out", list.Days)
		}
	}
	if len(list.Days) != 1 || list.Days[0].Date != today {
		t.Fatalf("days = %+v, want just today", list.Days)
	}
}

// TestDayPage checks the day page itself: the diary text, the user's own turns that day, and the day's action items with their state.
func TestDayPage(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()
	today := time.Now().Format("2006-01-02")
	if err := store.SetDiaryEntry(ctx, today, "day", "The demo went out."); err != nil {
		t.Fatalf("seed diary: %v", err)
	}
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{{Owner: "Alex", Text: "send the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal}}); err != nil {
		t.Fatalf("seed action item: %v", err)
	}
	convID, err := store.CreateConversation(ctx, "the flight", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, convID, "you", "when does it leave", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	if _, err := store.AddTurn(ctx, convID, "ora", "half past four", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	_, srv := newWindowServer(t, &fakeAsker{}, store)
	var page DayView
	getJSON(t, srv, "/days/"+today, &page)
	if page.Date != today || page.Page != "The demo went out." {
		t.Errorf("day page = %+v, want today's diary entry", page)
	}
	if len(page.You) != 1 || page.You[0].Text != "when does it leave" {
		t.Errorf("you = %+v, want only the turns you spoke", page.You)
	}
	if page.You[0].When == "" {
		t.Errorf("a turn came back with no time")
	}
	if len(page.Tasks) != 1 || page.Tasks[0].Title != "send the deck" || page.Tasks[0].Done {
		t.Errorf("tasks = %+v, want the day's open action item", page.Tasks)
	}
}

// TestDayPageCarriesBriefAndClose checks that GET /days/{date} reports the morning brief and evening close as their own fields, alongside the existing page (which stays the close text, unchanged from before these fields existed).
func TestDayPageCarriesBriefAndClose(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()
	today := time.Now().Format("2006-01-02")
	if err := store.SetDiaryEntry(ctx, today, "day", "The demo went out."); err != nil {
		t.Fatalf("seed diary day: %v", err)
	}
	if err := store.SetDiaryEntry(ctx, today, "brief", "Ship the report — it's due today."); err != nil {
		t.Fatalf("seed diary brief: %v", err)
	}

	_, srv := newWindowServer(t, &fakeAsker{}, store)
	var page DayView
	getJSON(t, srv, "/days/"+today, &page)
	if page.Close != "The demo went out." {
		t.Errorf("close = %q, want the evening close entry", page.Close)
	}
	if page.Page != page.Close {
		t.Errorf("page = %q, want it to still carry the close text", page.Page)
	}
	if page.Brief != "Ship the report — it's due today." {
		t.Errorf("brief = %q, want the morning brief", page.Brief)
	}
}

// TestDayPageEmpty checks that a day nothing happened on answers with empty lists rather than an error.
func TestDayPageEmpty(t *testing.T) {
	store := newReadStore(t)
	_, srv := newWindowServer(t, &fakeAsker{}, store)
	var page DayView
	getJSON(t, srv, "/days/2020-01-01", &page)
	if page.Page != "" || page.You == nil || page.Tasks == nil {
		t.Errorf("empty day = %+v, want an empty page and empty lists", page)
	}
	if page.Heading != "" {
		t.Errorf("heading = %q, want empty for a day with nothing in it", page.Heading)
	}
}

// TestDayHeading checks the one-line summary GET /days/{date} adds: singular and plural both read right, and the meeting's recorded duration rounds to whole minutes.
func TestDayHeading(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()
	today := time.Now().Format("2006-01-02")

	for i := 0; i < 60; i++ {
		if _, err := store.WriteEpisode(ctx, db.EpisodeWrite{App: "Brave", Title: fmt.Sprintf("tab %d", i), ScreenText: "text"}); err != nil {
			t.Fatalf("seed episode %d: %v", i, err)
		}
	}
	start := time.Now().Add(-time.Hour)
	stop := start.Add(28 * time.Minute)
	minutesWithDuration := fmt.Sprintf("%s\n\n<!--ora:duration start=%s stop=%s-->\n", sampleMinutes, start.UTC().Format(time.RFC3339), stop.UTC().Format(time.RFC3339))
	if _, err := store.LogNote(ctx, minutesWithDuration, meetingNoteKind); err != nil {
		t.Fatalf("seed meeting: %v", err)
	}

	_, srv := newWindowServer(t, &fakeAsker{}, store)
	var page DayView
	getJSON(t, srv, "/days/"+today, &page)
	if page.Heading != "60 things seen · 1 call, 28 min" {
		t.Errorf("heading = %q, want the day's counts summarised in one line", page.Heading)
	}
}

// TestDayBadDate checks that a path that is not a date is refused instead of being read as one.
func TestDayBadDate(t *testing.T) {
	store := newReadStore(t)
	_, srv := newWindowServer(t, &fakeAsker{}, store)
	resp, err := http.Get(srv.URL + "/days/yesterday")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /days/yesterday = %d, want 400", resp.StatusCode)
	}
}

// TestDayHeadingOmitsZeroMinutes checks that a day whose recordings carry no duration says how many calls there were and nothing about minutes, instead of the "5 calls, 0 min" the user saw on 2026-09-05.
func TestDayHeadingOmitsZeroMinutes(t *testing.T) {
	if got := dayHeading(366, 5, 0); got != "366 things seen · 5 calls" {
		t.Errorf("heading = %q, want the minutes left out when none were recorded", got)
	}
	if got := dayHeading(0, 1, 12); got != "1 call, 12 min" {
		t.Errorf("heading = %q, want the minutes kept when they are known", got)
	}
}
