package ipc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"june/internal/db"
	"june/internal/db/dbtest"
	"june/internal/memory"
	"june/internal/tracker"
)

// getJSON performs a GET against the test server and decodes the body into out, failing the test on any status other than 200.
func getJSON(t *testing.T, srv *httptest.Server, path string, out any) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
}

// sampleMinutes is one meeting's minutes in the shape internal/recorder's prompt asks for: a title line, an attendee section split into who was really there and who was only mentioned, and one attendee the recogniser only heard.
const sampleMinutes = `# Lodestone sync
**Lodestone sync — Wed 3 Sep 2026 10:00 to 10:30**

## What the meeting covered
- The route planning demo.

## Attendees
**In the meeting**
- **Zemna Braxen (recording)** — ran the demo
- **Vexil Quorin** — asked about shipping factors
- a contact (heard as "Oshveln") — spoke twice near the end

**Mentioned or on screen only**
- Sorrek — owns the TDL task
`

func TestContext_ReadsTheLatestCapture(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	if _, err := store.WriteEpisode(ctx, db.EpisodeWrite{App: "Brave", Title: "an old tab", ScreenText: "older text"}); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	cases := []struct {
		name              string
		screen            func() []tracker.Activity
		wantApp, wantText string
	}{
		{
			name: "the live buffer wins when the tracker has something",
			screen: func() []tracker.Activity {
				return []tracker.Activity{{App: "Slack", Title: "a", ScreenText: "first"}, {App: "Ghostty", Title: "june", ScreenText: strings.Repeat("x", 900)}}
			},
			wantApp:  "Ghostty",
			wantText: strings.Repeat("x", 600),
		},
		{
			name: "the window itself is never the context: the newest capture that is not June wins",
			screen: func() []tracker.Activity {
				return []tracker.Activity{{App: "Slack", Title: "a", ScreenText: "slack text"}, {App: "June", Title: "June", ScreenText: "Ask, or hold space"}}
			},
			wantApp:  "Slack",
			wantText: "slack text",
		},
		{
			name: "the XWayland frame of the window, named by its title, is June too",
			screen: func() []tracker.Activity {
				return []tracker.Activity{{App: "Brave", Title: "docs", ScreenText: "doc text"}, {App: "mutter-x11-frames", Title: "June", ScreenText: "Ask"}}
			},
			wantApp:  "Brave",
			wantText: "doc text",
		},
		{
			name:     "a buffer holding only June falls back to the newest stored episode",
			screen:   func() []tracker.Activity { return []tracker.Activity{{App: "june", Title: "June", ScreenText: "Ask"}} },
			wantApp:  "Brave",
			wantText: "older text",
		},
		{
			name:     "an empty buffer falls back to the newest stored episode",
			screen:   func() []tracker.Activity { return nil },
			wantApp:  "Brave",
			wantText: "older text",
		},
		{
			name:     "no tracker at all is not an error",
			screen:   nil,
			wantApp:  "Brave",
			wantText: "older text",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newTestServer(t, &fakeAsker{}, store, c.screen, nil)
			var got ContextView
			getJSON(t, srv, "/context", &got)
			if got.App != c.wantApp {
				t.Errorf("app = %q, want %q", got.App, c.wantApp)
			}
			if got.Text != c.wantText {
				t.Errorf("text = %q (len %d), want %q (len %d)", got.Text, len(got.Text), c.wantText, len(c.wantText))
			}
		})
	}
}

// TestContext_LiveFocusWinsOverBuffer covers the reported lag: the hotkey must name the window in focus right now, even when the tracker's sampled buffer still holds an older, different window.
func TestContext_LiveFocusWinsOverBuffer(t *testing.T) {
	store := dbtest.Open(t)
	screen := func() []tracker.Activity {
		return []tracker.Activity{{App: "Slack", Title: "a", ScreenText: "slack text"}}
	}
	focused := func(context.Context) (tracker.Activity, bool) {
		return tracker.Activity{App: "Ghostty", Title: "june repo", ScreenText: "live text"}, true
	}
	srv := newTestServer(t, &fakeAsker{}, store, screen, focused)
	var got ContextView
	getJSON(t, srv, "/context", &got)
	if got.App != "Ghostty" || got.Text != "live text" {
		t.Errorf("got %+v, want the live focused window, not the buffer", got)
	}
}

func TestMatters_ActionsThenThreadsThenMeetings(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()

	if _, err := store.AddActionItems(ctx, []memory.ActionItem{
		{Owner: "Me", Text: "send the invoice", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Lodestone sync", Raised: time.Now().Add(-24 * time.Hour)},
		{Owner: "Me", Text: "share the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal},
	}); err != nil {
		t.Fatalf("seed action items: %v", err)
	}
	for _, subject := range []string{"route planning", "june window"} {
		if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: subject, Kind: "work", State: "in flight"}); err != nil {
			t.Fatalf("seed thread: %v", err)
		}
	}
	if _, err := store.LogNote(ctx, sampleMinutes, "meeting"); err != nil {
		t.Fatalf("seed meeting: %v", err)
	}

	srv := newTestServer(t, &fakeAsker{}, store, nil, nil)
	var got struct {
		Matters []Matter `json:"matters"`
	}
	getJSON(t, srv, "/matters", &got)

	var kinds []string
	for _, m := range got.Matters {
		kinds = append(kinds, m.Kind)
		if m.ID == "" || m.Title == "" {
			t.Errorf("matter with an empty id or title: %+v", m)
		}
	}
	want := []string{"action", "action", "thread", "thread", "meeting"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if got.Matters[0].Status != "open" || got.Matters[2].Status != "watching" || got.Matters[4].Status != "done" {
		t.Errorf("statuses = %q/%q/%q, want open/watching/done", got.Matters[0].Status, got.Matters[2].Status, got.Matters[4].Status)
	}
	if got.Matters[4].Title != "Lodestone sync" {
		t.Errorf("meeting title = %q, want %q", got.Matters[4].Title, "Lodestone sync")
	}
}

func TestToday_BriefAndTimelineOldestFirst(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	day := time.Now().Format("2006-01-02")

	if err := store.SetDiaryEntry(ctx, day, "brief", "Two things are waiting on you."); err != nil {
		t.Fatalf("seed brief: %v", err)
	}
	if err := store.LogSemanticNode(ctx, memory.TaskSummary{TaskName: "GitHub mail", Summary: "read the review comments"}); err != nil {
		t.Fatalf("seed summary: %v", err)
	}
	if _, err := store.LogNote(ctx, sampleMinutes, "meeting"); err != nil {
		t.Fatalf("seed meeting: %v", err)
	}
	if _, err := store.LogNote(ctx, "The user prefers short answers", "fact"); err != nil {
		t.Fatalf("seed note: %v", err)
	}
	id, err := store.LogNote(ctx, memory.ActionItem{Owner: "Zemna", Text: "send the invoice", Status: memory.StatusOpen, Priority: memory.PriorityNormal}.Note(), memory.ActionNoteKind)
	if err != nil {
		t.Fatalf("seed action: %v", err)
	}
	if err := store.SetActionStatus(ctx, id, memory.StatusDone); err != nil {
		t.Fatalf("close action: %v", err)
	}

	srv := newTestServer(t, &fakeAsker{}, store, nil, nil)
	var got TodayView
	getJSON(t, srv, "/today", &got)

	if got.Brief != "Two things are waiting on you." {
		t.Errorf("brief = %q, want today's morning brief", got.Brief)
	}
	seen := map[string]bool{}
	var last time.Time
	for _, e := range got.Timeline {
		seen[e.Kind] = true
		when, err := time.Parse(time.RFC3339, e.When)
		if err != nil {
			t.Fatalf("entry when = %q, not RFC3339: %v", e.When, err)
		}
		if when.Before(last) {
			t.Errorf("timeline is not oldest first: %s came after %s", e.When, last.Format(time.RFC3339))
		}
		last = when
		if len([]rune(e.Text)) > 200 {
			t.Errorf("entry text is %d runes, want at most 200", len([]rune(e.Text)))
		}
	}
	for _, kind := range []string{"seen", "heard", "memory", "task"} {
		if !seen[kind] {
			t.Errorf("timeline has no %q entry: %+v", kind, got.Timeline)
		}
	}
}

func TestMeetings_NewestFirstWithAttendees(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	if _, err := store.LogNote(ctx, "# Older meeting\n**Older meeting — Tue 2 Sep 2026 09:00 to 09:20**\n", "meeting"); err != nil {
		t.Fatalf("seed older meeting: %v", err)
	}
	if _, err := store.LogNote(ctx, sampleMinutes, "meeting"); err != nil {
		t.Fatalf("seed meeting: %v", err)
	}

	srv := newTestServer(t, &fakeAsker{}, store, nil, nil)
	var got struct {
		Meetings []Meeting `json:"meetings"`
	}
	getJSON(t, srv, "/meetings", &got)

	if len(got.Meetings) != 2 {
		t.Fatalf("got %d meetings, want 2", len(got.Meetings))
	}
	if got.Meetings[0].Title != "Lodestone sync" || got.Meetings[1].Title != "Older meeting" {
		t.Fatalf("titles = %q, %q; want the newest meeting first", got.Meetings[0].Title, got.Meetings[1].Title)
	}
	if got.Meetings[1].Attendees == nil {
		t.Errorf("a meeting with no attendee section must still carry an empty list, not null")
	}
	want := []Attendee{
		{Name: "Zemna Braxen", HeardOnly: false},
		{Name: "Vexil Quorin", HeardOnly: false},
		{Name: "Oshveln", HeardOnly: true},
	}
	if len(got.Meetings[0].Attendees) != len(want) {
		t.Fatalf("attendees = %+v, want %+v", got.Meetings[0].Attendees, want)
	}
	for i := range want {
		if got.Meetings[0].Attendees[i] != want[i] {
			t.Errorf("attendee %d = %+v, want %+v", i, got.Meetings[0].Attendees[i], want[i])
		}
	}
	if got.Meetings[0].Minutes != sampleMinutes {
		t.Errorf("minutes were not returned verbatim")
	}
	// Neither note carries a duration marker (internal/recorder didn't exist to write one when these were filed), so both must read as 0 rather than error.
	if got.Meetings[0].DurationS != 0 || got.Meetings[1].DurationS != 0 {
		t.Errorf("duration_s = %d, %d; want 0 for notes filed with no marker", got.Meetings[0].DurationS, got.Meetings[1].DurationS)
	}
}

func TestMemorySearch(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	if _, err := store.LogNote(ctx, "Vexil wants the invoice before Friday", "fact"); err != nil {
		t.Fatalf("seed note: %v", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO notes_archive(note_id, content, kind, created_at, archived_at) VALUES(7, 'the invoice was raised in July', 'fact', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed archived note: %v", err)
	}
	srv := newTestServer(t, &fakeAsker{}, store, nil, nil)

	var got struct {
		Facts []Fact `json:"facts"`
	}
	getJSON(t, srv, "/memory/search?q=invoice", &got)

	var live, archived int
	for _, f := range got.Facts {
		if f.ID == "" || f.Text == "" {
			t.Errorf("fact with an empty id or text: %+v", f)
		}
		if f.Archived {
			archived++
		} else {
			live++
		}
	}
	if live == 0 {
		t.Errorf("no live hit for %q: %+v", "invoice", got.Facts)
	}
	if archived != 1 {
		t.Errorf("archived hits = %d, want 1: %+v", archived, got.Facts)
	}
}

func TestPeople_PersonalEntriesThenHeardOnlyNames(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	for _, e := range [][2]string{
		{"identity", "The user is Zemna Braxen."},
		{"vexil-quorin", "Leads the route planning work."},
		{"preferences-communication", "Prefers short answers."},
	} {
		if err := store.SetPersonalContext(ctx, e[0], e[1]); err != nil {
			t.Fatalf("seed personal context: %v", err)
		}
	}
	if _, err := store.LogNote(ctx, sampleMinutes, "meeting"); err != nil {
		t.Fatalf("seed meeting: %v", err)
	}
	if _, err := store.LogNote(ctx, "Vexil Quorin is reviewing the deck", "fact"); err != nil {
		t.Fatalf("seed note: %v", err)
	}

	srv := newTestServer(t, &fakeAsker{}, store, nil, nil)
	var got struct {
		People []Person `json:"people"`
	}
	getJSON(t, srv, "/people", &got)

	if len(got.People) != 2 {
		t.Fatalf("people = %+v, want the one stored person and the one heard-only name", got.People)
	}
	first, second := got.People[0], got.People[1]
	if first.Name != "Vexil Quorin" || !first.Seen || first.HeardOnly {
		t.Errorf("first person = %+v, want a seen Vexil Quorin", first)
	}
	if first.Count != 2 {
		t.Errorf("Vexil Quorin count = %d, want 2 (the meeting and the fact note)", first.Count)
	}
	if first.Note == "" {
		t.Errorf("a stored person should carry their personal-context entry as the note")
	}
	if second.Name != "Oshveln" || second.Seen || !second.HeardOnly {
		t.Errorf("second person = %+v, want a heard-only Oshveln", second)
	}
}

// TestContext_StoredEpisodeFallbackSkipsJune covers the last resort: no live focus, no buffer, and the newest episodes in the store are June's own window, filed before the tracker learned to skip it. The answer must be the newest episode that is some other window.
func TestContext_StoredEpisodeFallbackSkipsJune(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	for _, e := range []db.EpisodeWrite{
		{App: "Brave", Title: "docs", ScreenText: "doc text"},
		{App: "june", Title: "June", ScreenText: "Ask, or hold space"},
		{App: "mutter-x11-frames", Title: "June", ScreenText: "Ask"},
	} {
		if _, err := store.WriteEpisode(ctx, e); err != nil {
			t.Fatalf("seed episode: %v", err)
		}
	}

	focused := func(context.Context) (tracker.Activity, bool) { return tracker.Activity{}, false }
	srv := newTestServer(t, &fakeAsker{}, store, func() []tracker.Activity { return nil }, focused)
	var got ContextView
	getJSON(t, srv, "/context", &got)
	if got.App != "Brave" || got.Text != "doc text" {
		t.Errorf("got %+v, want the newest stored episode that is not June's own window", got)
	}
}

// The hotkey's live read must refuse the same windows the tracker refuses. /context filtered June's own window and nothing else, so pressing the hotkey with a password manager in front answered {"app":"1Password","title":"Vault — Personal"} and that is what the window fed into the model's prompt — a row the episode store would never hold, because the tracker's own skip drops it before it is written.
func TestContext_LiveFocusOnTheBlocklistFallsThroughToBuffer(t *testing.T) {
	tracker.SetBlocklist([]string{"1password"})
	t.Cleanup(func() { tracker.SetBlocklist(nil) })

	store := dbtest.Open(t)
	screen := func() []tracker.Activity {
		return []tracker.Activity{{App: "Slack", Title: "a", ScreenText: "slack text"}}
	}
	focused := func(context.Context) (tracker.Activity, bool) {
		return tracker.Activity{App: "1Password", Title: "Vault — Personal", ScreenText: "the master password list"}, true
	}
	srv := newTestServer(t, &fakeAsker{}, store, screen, focused)
	var got ContextView
	getJSON(t, srv, "/context", &got)
	if got.App != "Slack" || got.Text != "slack text" {
		t.Errorf("got %+v, want the buffer's window since the live read named a blocked application", got)
	}
}

// A meeting whose write-up is not worth keeping is deleted from the list it appears in, and the note behind it goes with it.
func TestMeeting_DeleteRemovesTheWriteUp(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	id, err := store.LogNote(ctx, sampleMinutes, "meeting")
	if err != nil {
		t.Fatalf("seed meeting: %v", err)
	}
	srv := newTestServer(t, &fakeAsker{}, store, nil, nil)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/meetings/"+strconv.FormatInt(id, 10), nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE answered %d, want 204", resp.StatusCode)
	}

	var got struct {
		Meetings []Meeting `json:"meetings"`
	}
	getJSON(t, srv, "/meetings", &got)
	if len(got.Meetings) != 0 {
		t.Errorf("%d meetings left, want the deleted one gone", len(got.Meetings))
	}
}

// Only a meeting can be deleted through this route. Notes share one id space, so an id that names a diary entry or a memory note must not be removed by a request the meetings screen made.
func TestMeeting_DeleteRefusesANoteThatIsNotAMeeting(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	id, err := store.LogNote(ctx, "Yendric's daughter is called Omvex.", "memory")
	if err != nil {
		t.Fatalf("seed note: %v", err)
	}
	srv := newTestServer(t, &fakeAsker{}, store, nil, nil)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/meetings/"+strconv.FormatInt(id, 10), nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE of a note that is not a meeting answered %d, want 404", resp.StatusCode)
	}
	notes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Errorf("%d notes left, want the note that is not a meeting untouched", len(notes))
	}
}
