package ipc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
	"ora/internal/memory"
	"ora/internal/tracker"
)

// newReadStore opens a throwaway in-memory store the same way internal/db's own tests do, closed when the test ends.
func newReadStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

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
- The value chain demo.

## Attendees
**In the meeting**
- **Alex Rivera (recording)** — ran the demo
- **Priya Shah** — asked about emission factors
- a contact (heard as "Ashar") — spoke twice near the end

**Mentioned or on screen only**
- Sneha — owns the PFP task
`

func TestContext_ReadsTheLatestCapture(t *testing.T) {
	store := newReadStore(t)
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
				return []tracker.Activity{{App: "Slack", Title: "a", ScreenText: "first"}, {App: "Ghostty", Title: "ora", ScreenText: strings.Repeat("x", 900)}}
			},
			wantApp:  "Ghostty",
			wantText: strings.Repeat("x", 600),
		},
		{
			name: "the window itself is never the context: the newest capture that is not Ora wins",
			screen: func() []tracker.Activity {
				return []tracker.Activity{{App: "Slack", Title: "a", ScreenText: "slack text"}, {App: "Ora", Title: "Ora", ScreenText: "Ask, or hold space"}}
			},
			wantApp:  "Slack",
			wantText: "slack text",
		},
		{
			name: "the XWayland frame of the window, named by its title, is Ora too",
			screen: func() []tracker.Activity {
				return []tracker.Activity{{App: "Brave", Title: "docs", ScreenText: "doc text"}, {App: "mutter-x11-frames", Title: "Ora", ScreenText: "Ask"}}
			},
			wantApp:  "Brave",
			wantText: "doc text",
		},
		{
			name:     "a buffer holding only Ora falls back to the newest stored episode",
			screen:   func() []tracker.Activity { return []tracker.Activity{{App: "ora", Title: "Ora", ScreenText: "Ask"}} },
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
	store := newReadStore(t)
	screen := func() []tracker.Activity {
		return []tracker.Activity{{App: "Slack", Title: "a", ScreenText: "slack text"}}
	}
	focused := func(context.Context) (tracker.Activity, bool) {
		return tracker.Activity{App: "Ghostty", Title: "ora repo", ScreenText: "live text"}, true
	}
	srv := newTestServer(t, &fakeAsker{}, store, screen, focused)
	var got ContextView
	getJSON(t, srv, "/context", &got)
	if got.App != "Ghostty" || got.Text != "live text" {
		t.Errorf("got %+v, want the live focused window, not the buffer", got)
	}
}

// TestContext_LiveFocusOraFallsThroughToBuffer covers the moment the hotkey itself takes focus: the live read names Ora, so /context must still fall through to the buffer's newest non-Ora capture, exactly as it does for a stale buffer entry.
func TestContext_LiveFocusOraFallsThroughToBuffer(t *testing.T) {
	store := newReadStore(t)
	screen := func() []tracker.Activity {
		return []tracker.Activity{{App: "Slack", Title: "a", ScreenText: "slack text"}}
	}
	focused := func(context.Context) (tracker.Activity, bool) {
		return tracker.Activity{App: "Ora", Title: "Ora", ScreenText: "Ask, or hold space"}, true
	}
	srv := newTestServer(t, &fakeAsker{}, store, screen, focused)
	var got ContextView
	getJSON(t, srv, "/context", &got)
	if got.App != "Slack" || got.Text != "slack text" {
		t.Errorf("got %+v, want the buffer's window since the live read named Ora", got)
	}
}

// TestContext_LiveFocusFailureFallsThroughToBuffer covers a live reader that finds nothing (no accessibility bus, no focused window): /context must behave exactly as it did before a live reader existed.
func TestContext_LiveFocusFailureFallsThroughToBuffer(t *testing.T) {
	store := newReadStore(t)
	screen := func() []tracker.Activity {
		return []tracker.Activity{{App: "Slack", Title: "a", ScreenText: "slack text"}}
	}
	focused := func(context.Context) (tracker.Activity, bool) { return tracker.Activity{}, false }
	srv := newTestServer(t, &fakeAsker{}, store, screen, focused)
	var got ContextView
	getJSON(t, srv, "/context", &got)
	if got.App != "Slack" || got.Text != "slack text" {
		t.Errorf("got %+v, want the buffer's window since the live read found nothing", got)
	}
}

// TestContext_LiveFocusTimeoutDoesNotDelayResponse covers a hung accessibility read: /context must fall back to the buffer rather than wait for it, and the whole request must still finish quickly rather than blocking for as long as the reader takes.
func TestContext_LiveFocusTimeoutDoesNotDelayResponse(t *testing.T) {
	store := newReadStore(t)
	screen := func() []tracker.Activity {
		return []tracker.Activity{{App: "Slack", Title: "a", ScreenText: "slack text"}}
	}
	focused := func(context.Context) (tracker.Activity, bool) {
		time.Sleep(2 * time.Second)
		return tracker.Activity{App: "Ghostty", Title: "too late", ScreenText: "too late"}, true
	}
	srv := newTestServer(t, &fakeAsker{}, store, screen, focused)

	start := time.Now()
	var got ContextView
	getJSON(t, srv, "/context", &got)
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("GET /context took %s, want at most ~400ms even with a slow focused reader", elapsed)
	}
	if got.App != "Slack" || got.Text != "slack text" {
		t.Errorf("got %+v, want the buffer's window since the live read did not answer in time", got)
	}
}

// TestReadFocused_AbandonedWhenRequestContextEnds covers a request whose context ends well before liveFocusTimeout (300ms) would fire on its own: readFocused must give up as soon as ctx is done, not keep waiting out the fixed timer while the caller has already moved on. The focused reader itself blocks far longer than either bound, and is left running — readFocused has no way to interrupt it, only to stop waiting on it.
func TestReadFocused_AbandonedWhenRequestContextEnds(t *testing.T) {
	blocked := make(chan struct{})
	focused := func(context.Context) (tracker.Activity, bool) {
		<-blocked
		return tracker.Activity{App: "late"}, true
	}
	defer close(blocked)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, ok := readFocused(ctx, focused)
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("readFocused took %s, want it abandoned around the 30ms context deadline, well short of the 300ms liveFocusTimeout", elapsed)
	}
	if ok {
		t.Error("readFocused reported a result even though its context ended first")
	}
}

func TestMatters_ActionsThenThreadsThenMeetings(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()

	if _, err := store.AddActionItems(ctx, []memory.ActionItem{
		{Owner: "Me", Text: "send the invoice", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Lodestone sync", Raised: time.Now().Add(-24 * time.Hour)},
		{Owner: "Me", Text: "share the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal},
	}); err != nil {
		t.Fatalf("seed action items: %v", err)
	}
	for _, subject := range []string{"value chain", "ora window"} {
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

func TestMatters_EmptyStoreGivesAnEmptyList(t *testing.T) {
	srv := newTestServer(t, &fakeAsker{}, newReadStore(t), nil, nil)
	resp, err := http.Get(srv.URL + "/matters")
	if err != nil {
		t.Fatalf("GET /matters: %v", err)
	}
	defer resp.Body.Close()
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	if got := strings.TrimSpace(string(body[:n])); got != `{"matters":[]}` {
		t.Fatalf("body = %s, want an empty list rather than null", got)
	}
}

func TestToday_BriefAndTimelineOldestFirst(t *testing.T) {
	store := newReadStore(t)
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
	id, err := store.LogNote(ctx, memory.ActionItem{Owner: "Alex", Text: "send the invoice", Status: memory.StatusOpen, Priority: memory.PriorityNormal}.Note(), memory.ActionNoteKind)
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

func TestToday_BriefFallsBackToTheLatestDigest(t *testing.T) {
	store := newReadStore(t)
	if _, err := store.DB().Exec(`INSERT INTO nodes(parent_id, type, content) VALUES(NULL, 'digest', 'yesterday in one paragraph')`); err != nil {
		t.Fatalf("seed digest: %v", err)
	}
	srv := newTestServer(t, &fakeAsker{}, store, nil, nil)
	var got TodayView
	getJSON(t, srv, "/today", &got)
	if got.Brief != "yesterday in one paragraph" {
		t.Errorf("brief = %q, want the latest digest", got.Brief)
	}
}

func TestMeetings_NewestFirstWithAttendees(t *testing.T) {
	store := newReadStore(t)
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
		{Name: "Alex Rivera", HeardOnly: false},
		{Name: "Priya Shah", HeardOnly: false},
		{Name: "Ashar", HeardOnly: true},
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

// A note internal/recorder filed carries a trailing duration marker after the minutes text. GET /meetings must turn that into duration_s and never let the marker itself leak into the minutes text the window renders.
func TestMeetings_ReadsDurationMarkerAndStripsIt(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()
	minutes := "# Standup\n\n## Key points\n- shipped it.\n"
	// This is the exact shape internal/recorder's fileMinutes stores: the minutes text, then a blank line and the marker, matching withMeetingDuration in internal/recorder/recorder.go.
	withMarker := minutes + "\n\n<!--ora:duration start=2026-09-04T10:00:00Z stop=2026-09-04T10:41:00Z-->\n"
	if _, err := store.LogNote(ctx, withMarker, "meeting"); err != nil {
		t.Fatalf("seed meeting: %v", err)
	}

	srv := newTestServer(t, &fakeAsker{}, store, nil, nil)
	var got struct {
		Meetings []Meeting `json:"meetings"`
	}
	getJSON(t, srv, "/meetings", &got)

	if len(got.Meetings) != 1 {
		t.Fatalf("got %d meetings, want 1", len(got.Meetings))
	}
	if got.Meetings[0].DurationS != 41*60 {
		t.Errorf("duration_s = %d, want %d (41 minutes)", got.Meetings[0].DurationS, 41*60)
	}
	if strings.Contains(got.Meetings[0].Minutes, "ora:duration") {
		t.Errorf("duration marker leaked into the rendered minutes: %q", got.Meetings[0].Minutes)
	}
	if want := strings.TrimRight(minutes, "\n"); got.Meetings[0].Minutes != want {
		t.Errorf("minutes = %q, want the marker stripped back to %q", got.Meetings[0].Minutes, want)
	}
}

func TestMemorySearch(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()
	if _, err := store.LogNote(ctx, "Priya wants the invoice before Friday", "fact"); err != nil {
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

func TestMemorySearch_EmptyQueryIs400(t *testing.T) {
	srv := newTestServer(t, &fakeAsker{}, newReadStore(t), nil, nil)
	for _, path := range []string{"/memory/search", "/memory/search?q=", "/memory/search?q=%20"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, want 400", path, resp.StatusCode)
		}
	}
}

func TestPeople_PersonalEntriesThenHeardOnlyNames(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()
	for _, e := range [][2]string{
		{"identity", "The user is Alex Rivera."},
		{"priya-shah", "Leads the value chain work."},
		{"preferences-communication", "Prefers short answers."},
	} {
		if err := store.SetPersonalContext(ctx, e[0], e[1]); err != nil {
			t.Fatalf("seed personal context: %v", err)
		}
	}
	if _, err := store.LogNote(ctx, sampleMinutes, "meeting"); err != nil {
		t.Fatalf("seed meeting: %v", err)
	}
	if _, err := store.LogNote(ctx, "Priya Shah is reviewing the deck", "fact"); err != nil {
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
	if first.Name != "Priya Shah" || !first.Seen || first.HeardOnly {
		t.Errorf("first person = %+v, want a seen Priya Shah", first)
	}
	if first.Count != 2 {
		t.Errorf("Priya Shah count = %d, want 2 (the meeting and the fact note)", first.Count)
	}
	if first.Note == "" {
		t.Errorf("a stored person should carry their personal-context entry as the note")
	}
	if second.Name != "Ashar" || second.Seen || !second.HeardOnly {
		t.Errorf("second person = %+v, want a heard-only Ashar", second)
	}
}

// TestContext_StoredEpisodeFallbackSkipsOra covers the last resort: no live focus, no buffer, and the newest episodes in the store are Ora's own window, filed before the tracker learned to skip it. The answer must be the newest episode that is some other window.
func TestContext_StoredEpisodeFallbackSkipsOra(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()
	for _, e := range []db.EpisodeWrite{
		{App: "Brave", Title: "docs", ScreenText: "doc text"},
		{App: "ora", Title: "Ora", ScreenText: "Ask, or hold space"},
		{App: "mutter-x11-frames", Title: "Ora", ScreenText: "Ask"},
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
		t.Errorf("got %+v, want the newest stored episode that is not Ora's own window", got)
	}
}

// A meeting is dated by when it ran, not by when its write-up was filed. A recording deferred to mains, or one the startup sweep recovered after a crash, is filed hours or days after the call — and the window groups the list by this field, so the call would sit under the wrong day and read as "today".
func TestMeetings_DatedByTheRecordingsStartNotTheNote(t *testing.T) {
	store := newReadStore(t)
	ctx := context.Background()
	withMarker := "# Standup\n\n## Key points\n- shipped it.\n\n<!--ora:duration start=2026-09-04T16:10:00Z stop=2026-09-04T16:41:00Z-->\n"
	if _, err := store.LogNote(ctx, withMarker, "meeting"); err != nil {
		t.Fatalf("seed meeting: %v", err)
	}
	if _, err := store.LogNote(ctx, "# Unmarked\n\n**Unmarked — Tue 2 Sep 2026 09:00 to 09:20**\n", "meeting"); err != nil {
		t.Fatalf("seed unmarked meeting: %v", err)
	}

	srv := newTestServer(t, &fakeAsker{}, store, nil, nil)
	var got struct {
		Meetings []Meeting `json:"meetings"`
	}
	getJSON(t, srv, "/meetings", &got)

	if len(got.Meetings) != 2 {
		t.Fatalf("got %d meetings, want 2", len(got.Meetings))
	}
	var marked, unmarked Meeting
	for _, m := range got.Meetings {
		if m.Title == "Standup" {
			marked = m
		} else {
			unmarked = m
		}
	}
	when, err := time.Parse(time.RFC3339, marked.When)
	if err != nil {
		t.Fatalf("when = %q: %v", marked.When, err)
	}
	if !when.Equal(time.Date(2026, 9, 4, 16, 10, 0, 0, time.UTC)) {
		t.Errorf("when = %s, want the recording's start 2026-09-04T16:10:00Z", marked.When)
	}
	// A note filed before the marker existed has nothing else to go on, so it keeps the date it was filed.
	if unmarked.When == "" {
		t.Errorf("a meeting with no duration marker must still carry the date its note was filed")
	}
	// The list is ordered by when the meetings ran, so the one recorded in September sits below the one filed just now rather than above it on the strength of being filed second.
	if got.Meetings[0].Title != "Unmarked" {
		t.Errorf("meetings are ordered %q then %q; want the most recently run first", got.Meetings[0].Title, got.Meetings[1].Title)
	}
}

// The hotkey's live read must refuse the same windows the tracker refuses. /context filtered Ora's own window and nothing else, so pressing the hotkey with a password manager in front answered {"app":"1Password","title":"Vault — Personal"} and that is what the window fed into the model's prompt — a row the episode store would never hold, because the tracker's own skip drops it before it is written.
func TestContext_LiveFocusOnTheBlocklistFallsThroughToBuffer(t *testing.T) {
	tracker.SetBlocklist([]string{"1password"})
	t.Cleanup(func() { tracker.SetBlocklist(nil) })

	store := newReadStore(t)
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
