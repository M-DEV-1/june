package ipc

import (
	"context"
	"net/http"
	"testing"
	"time"

	"june/internal/db"
	"june/internal/db/dbtest"
	"june/internal/memory"
)

// TestDaysListsActiveDays checks that a day with a meeting on it shows up, and that a day with a diary entry is marked as having a page.
func TestDaysListsActiveDays(t *testing.T) {
	store := dbtest.Open(t)
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

// TestDayPage checks the day page itself: the diary text, the user's own turns that day, and the day's action items with their state.
func TestDayPage(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	today := time.Now().Format("2006-01-02")
	if err := store.SetDiaryEntry(ctx, today, "day", "The demo went out."); err != nil {
		t.Fatalf("seed diary: %v", err)
	}
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{{Owner: "Zemna", Text: "send the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal}}); err != nil {
		t.Fatalf("seed action item: %v", err)
	}
	convID, err := store.CreateConversation(ctx, "the flight", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, convID, "you", "when does it leave", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	if _, err := store.AddTurn(ctx, convID, "june", "half past four", "ask", nil, nil); err != nil {
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
	// The day's list and the tasks page have to be the same rows, or ticking one leaves the other stale: the id is what POST /tasks/{id}/done is called with, and the status and owner are what the row is drawn from.
	if page.Tasks[0].ID == "" {
		t.Errorf("a raised item came back with no id, so the window cannot tick it: %+v", page.Tasks[0])
	}
	if page.Tasks[0].Status != memory.StatusOpen {
		t.Errorf("status = %q, want %q", page.Tasks[0].Status, memory.StatusOpen)
	}
	if page.Tasks[0].Owner == "" {
		t.Errorf("a raised item came back with no owner: %+v", page.Tasks[0])
	}
	if code := postJSON(t, srv, "/tasks/"+page.Tasks[0].ID+"/done", `{"done":true}`, nil); code != http.StatusOK {
		t.Fatalf("POST /tasks/%s/done = %d, want the day's id to be the tasks page's id", page.Tasks[0].ID, code)
	}
	getJSON(t, srv, "/days/"+today, &page)
	if len(page.Tasks) != 1 || !page.Tasks[0].Done {
		t.Errorf("after ticking it through /tasks the day still shows %+v", page.Tasks)
	}
}
