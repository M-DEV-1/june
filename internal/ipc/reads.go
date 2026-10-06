// reads.go holds the desktop window's read-only screens: what is on screen now, what is outstanding, what happened today, the meetings, a memory search, and the people. Every route is a GET, returns JSON, and never returns a null list — the window renders these shapes directly, so an absent value is an empty string or an empty list. The two exceptions are the DELETEs that take a meeting's write-up or a person off their screens.
package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"june/internal/db"
	"june/internal/memory"
	"june/internal/tracker"
	"june/internal/util"
)

// meetingNoteKind is the notes.kind a meeting's minutes are filed under by internal/recorder.
const meetingNoteKind = "meeting"

// The rune caps the window's screens are trimmed to: the live screen text on /context, and one timeline entry or one matter's detail on /today and /matters.
const (
	maxContextText = 600
	maxEntryText   = 200
)

// mattersCap, meetingsCap, timelineCap, searchCap, threadsCap and peopleCap bound each screen's list so one long-running store never hands the window a page it cannot draw.
const (
	mattersCap  = 30
	meetingsCap = 30
	timelineCap = 60
	searchCap   = 20
	threadsCap  = 5
	peopleCap   = 100
)

// peopleWindow is how far back GET /people reads meeting notes for names it has nothing else on. Ninety days because a name heard once in a meeting a season ago is not someone the user is working with now, and the read costs one FTS count query per name it finds.
const peopleWindow = 90 * 24 * time.Hour

// ContextView is GET /context: the window and text of the most recent capture.
type ContextView struct {
	App   string `json:"app"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// Matter is one outstanding thing on GET /matters. Kind is "action", "thread" or "meeting"; Status is "open", "watching" or "done"; When is RFC3339 or empty when nothing dated it.
type Matter struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	When   string `json:"when"`
	Detail string `json:"detail"`
}

// Entry is one line of today's timeline. Kind is "seen" (a summary of a stretch of screen time), "heard" (a meeting), "memory" (a note written today) or "task" (an action item closed today).
type Entry struct {
	When   string `json:"when"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Source string `json:"source"`
}

// TodayView is GET /today: the morning brief and the day's timeline, oldest first.
type TodayView struct {
	Brief    string  `json:"brief"`
	Timeline []Entry `json:"timeline"`
}

// Attendee is one person in a meeting. HeardOnly is true when the minutes marked the name as one the speech recogniser only heard, with nothing on screen to confirm it.
type Attendee struct {
	Name      string `json:"name"`
	HeardOnly bool   `json:"heard_only"`
}

// Meeting is one recorded meeting on GET /meetings.
type Meeting struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	When      string     `json:"when"`
	DurationS int        `json:"duration_s"`
	Minutes   string     `json:"minutes"`
	Attendees []Attendee `json:"attendees"`
}

// Fact is one hit on GET /memory/search. Archived is true for a row consolidation replaced, which is kept out of ordinary search and only ever surfaced here.
type Fact struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Text        string `json:"text"`
	SourceTitle string `json:"source_title"`
	When        string `json:"when"`
	Archived    bool   `json:"archived"`
}

// Person is one person on GET /people. Seen is true for someone with real evidence they were there; HeardOnly is true for a name the recogniser only heard in a meeting. Count is how many notes mention them.
type Person struct {
	Name      string `json:"name"`
	Seen      bool   `json:"seen"`
	HeardOnly bool   `json:"heard_only"`
	Note      string `json:"note"`
	Count     int    `json:"count"`
}

// fail writes an error status with the message as the body, for a request the store could not answer.
func fail(w http.ResponseWriter, err error, code int) {
	http.Error(w, err.Error(), code)
}

// maxJSONBody bounds how much of a request body DecodeJSON reads. The largest body any client sends is a question with a pasted page in it, far under this; without a bound the decoder read whatever arrived into memory, however large.
const maxJSONBody = 1 << 20

// DecodeJSON reads the request body into v, answering 400 with the decoder's own message when it will not parse, and 413 when the body runs past maxJSONBody. Input: the response writer, the request, and a pointer to the value to fill. Output: true when v was filled and the handler should carry on; false when the body was refused and a response has already been written, so the handler must return.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(v); err != nil {
		refuseBody(w, err)
		return false
	}
	return true
}

// decodeJSONOptional is DecodeJSON for a route where an empty body is an ordinary request rather than a mistake: v is left at its zero value and the handler carries on. Anything else that will not parse is still 400, and a body past maxJSONBody is still 413. Input and output as DecodeJSON.
func decodeJSONOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		refuseBody(w, err)
		return false
	}
	return true
}

// refuseBody answers a body the decoder would not take. Input: the response writer and the decoder's error. Output: none; 413 naming the limit when the body ran past maxJSONBody, and 400 with the decoder's own message for anything else.
func refuseBody(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		http.Error(w, "the request body is over "+strconv.FormatInt(tooBig.Limit, 10)+" bytes", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// rfc3339 renders t for the window, or "" for a zero time so the field is present but empty rather than showing year one.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// liveFocusTimeout bounds how long Context waits for a synchronous read of the window in focus right now, so a slow or hung accessibility call never delays the response — it just falls back to the buffer, the same as when no live reader is wired at all.
const liveFocusTimeout = 300 * time.Millisecond

// readFocused calls focused on its own goroutine and waits at most liveFocusTimeout for it to answer, or until ctx ends, whichever comes first — a request whose context is already done (the caller has moved on) is abandoned immediately rather than held open for the rest of the timeout. Input: the request's context, and the live focused-window reader, which may be nil. Output: the activity and true when focused answered in time and reported one, or the zero value and false when focused is nil, ctx ended, ran out of time, or found nothing. The spawned goroutine itself cannot be interrupted mid-call — the tracker has no cancellable read — so a hung accessibility call still runs to completion in the background; only the wait for it is cut short.
func readFocused(ctx context.Context, focused func(context.Context) (tracker.Activity, bool)) (tracker.Activity, bool) {
	if focused == nil {
		return tracker.Activity{}, false
	}
	result := make(chan tracker.Activity, 1)
	found := make(chan bool, 1)
	go func() {
		a, ok := focused(ctx)
		if ok {
			result <- a
		}
		found <- ok
	}()
	select {
	case ok := <-found:
		if !ok {
			return tracker.Activity{}, false
		}
		return <-result, true
	case <-time.After(liveFocusTimeout):
		return tracker.Activity{}, false
	case <-ctx.Done():
		return tracker.Activity{}, false
	}
}

// contextViewFrom builds the /context response for one captured activity: its app and title, and its text taken from the screen text, or the visible-text lines when there is no screen text, or the user-activity summary when there is neither, trimmed to 600 runes.
func contextViewFrom(a tracker.Activity) ContextView {
	text := a.ScreenText
	if text == "" {
		text = strings.Join(a.VisibleText, "\n")
	}
	if text == "" {
		text = a.UserActivity
	}
	return ContextView{App: a.App, Title: a.Title, Text: util.Runes(text, maxContextText)}
}

// Context handles GET /context: the app, window title and text of the window in focus right now. It reads the window in focus synchronously first (see readFocused), so the hotkey never names a stale window; that read applies the same refusals the tracker's own capture loop applies (see usersWindow), so the hotkey cannot hand the model a window the episode store would never have held. When the live read fails, times out, or names a window that is not the user's, it falls back to the last window the tracker saw the user in (see SetLastWindow), then to the tracker's live buffer — the same buffer /buffer serves — and then to the newest stored episode when the buffer has just been flushed or no tracker is wired at all. A window named by the live read or the tracker's last window carries no text of its own, so its text is taken from the newest capture of that same window (see contextViewWithText). Text is trimmed to 600 runes.
func (s *Server) Context(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a, ok := readFocused(ctx, s.focused); ok && usersWindow(a) {
		util.WriteJSON(w, s.contextViewWithText(ctx, a))
		return
	}

	// The hover takes the focus as it shows, so on the hotkey path the live read names June, and the window the user means is the one they were in just before. The tracker's loop records that window the moment the user arrives in it; the buffer below only holds a window once the dwell has passed and its capture has run, so a window the user spent ten seconds in before pressing the hotkey was never there, and an older one was named instead.
	if s.lastWindow != nil {
		if a, ok := s.lastWindow(); ok && usersWindow(a) {
			util.WriteJSON(w, s.contextViewWithText(ctx, a))
			return
		}
	}

	if s.screen != nil {
		// The newest capture is often the June window itself, since the tracker sees it the moment it takes focus; the context the user means is the newest capture of anything else.
		if a, ok := newestOtherThanJune(s.screen()); ok {
			util.WriteJSON(w, contextViewFrom(a))
			return
		}
	}

	// Several episodes back, not one: the tracker no longer files June's own window, but episodes recorded before that change are still in the store and answering with one of them tells the user what June was showing rather than what they were doing.
	eps, err := s.store.ListEpisodes(ctx, db.EpisodeQuery{Limit: 10, NewestFirst: true})
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, e := range eps {
		if tracker.IsJuneWindow(e.App, e.Title) {
			continue
		}
		util.WriteJSON(w, ContextView{App: e.App, Title: e.Title, Text: util.Runes(episodeText(e), maxContextText)})
		return
	}
	util.WriteJSON(w, ContextView{})
}

// usersWindow reports whether a window read is one the user is working in. Input: the read. Output: false for each window the tracker's own loop skips (skipReason in internal/tracker/daemon.go) — one nothing could name, a surface of the Windows shell, June's own window, and an application on the blocklist — so /context never names a window the episode store would not have held. The shell mattered on its own: with the taskbar, Start or the desktop in front, /context answered app "windows-shell", title "Unknown".
func usersWindow(a tracker.Activity) bool {
	switch {
	case a.App == "Unknown" && a.Title == "Unknown":
		return false
	case tracker.IsShell(a.App), tracker.IsJuneWindow(a.App, a.Title), tracker.Blocklisted(a.App):
		return false
	}
	return true
}

// contextTextLookback is how old a stored capture of a window may be and still lend its text to that window on /context. An hour is the longest stretch the compiler's buffer holds before it flushes, and past it a title as generic as "Untitled - Notepad" is as likely to be another document as the one on screen now, which is worse than sending no text: the model then reads the screen itself.
const contextTextLookback = time.Hour

// contextEpisodeScan is how many of an application's newest episodes are searched for one whose title matches the window /context names.
const contextEpisodeScan = 20

// contextViewWithText is contextViewFrom for a window read that may carry no text of its own. Input: the request's context and the window. Output: its app and title, with its own text when it has some, and otherwise the text of the newest capture of the same app and title — the compiler's buffer first, then the episodes stored in the last contextTextLookback — or no text when nothing has captured that window yet.
// The live read and the tracker's last window name a window by app and title alone, because neither reads the screen, which is the slow part. Answered as they came, every ask from the hover went out with no screen text, although the buffer and the store held the captured text of that very window.
func (s *Server) contextViewWithText(ctx context.Context, a tracker.Activity) ContextView {
	view := contextViewFrom(a)
	if view.Text != "" {
		return view
	}
	if s.screen != nil {
		buf := s.screen()
		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i].App != a.App || buf[i].Title != a.Title {
				continue
			}
			if captured := contextViewFrom(buf[i]); captured.Text != "" {
				view.Text = captured.Text
				return view
			}
		}
	}
	if s.store == nil {
		return view
	}
	// The app filter is a substring match, so the exact app and title are checked again here.
	eps, err := s.store.ListEpisodes(ctx, db.EpisodeQuery{Since: time.Now().Add(-contextTextLookback), App: a.App, Limit: contextEpisodeScan, NewestFirst: true})
	if err != nil {
		return view
	}
	for _, e := range eps {
		if e.App != a.App || e.Title != a.Title {
			continue
		}
		if text := episodeText(e); text != "" {
			view.Text = util.Runes(text, maxContextText)
			return view
		}
	}
	return view
}

// episodeText is the text /context shows for a stored episode: its screen text, or its visible-text lines when there is none, or its user-activity summary when there is neither, the same order contextViewFrom reads a live capture in. The visible text is stored as a JSON array (see memory.VisibleTextJSON), so it is read back into lines rather than shown as JSON.
func episodeText(e db.Episode) string {
	if e.ScreenText != "" {
		return e.ScreenText
	}
	if e.VisibleText != "" {
		var lines []string
		if json.Unmarshal([]byte(e.VisibleText), &lines) == nil {
			if text := strings.Join(lines, "\n"); text != "" {
				return text
			}
		} else {
			return e.VisibleText
		}
	}
	return e.UserActivity
}

// Matters handles GET /matters: everything outstanding, in the order the window shows it — open action items first, then the five most recently touched threads, then the meetings of the last seven days. Capped at 30 items.
func (s *Server) Matters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	matters := []Matter{}

	actions, err := s.store.OpenActionItems(ctx)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, a := range actions {
		detail := a.Owner
		if a.Source != "" {
			detail += " · " + a.Source
		}
		matters = append(matters, Matter{
			ID:     strconv.FormatInt(a.NoteID, 10),
			Title:  a.Text,
			Kind:   "action",
			Status: "open",
			When:   rfc3339(a.Raised),
			Detail: detail,
		})
	}

	threads, err := s.store.ActiveThreads(ctx, threadsCap)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, t := range threads {
		matters = append(matters, Matter{
			ID:     "thread-" + strconv.FormatInt(t.ID, 10),
			Title:  t.Subject,
			Kind:   "thread",
			Status: "watching",
			When:   rfc3339(t.LastSeen),
			Detail: util.Runes(t.State, maxEntryText),
		})
	}

	meetings, err := s.store.NotesOfKindSince(ctx, meetingNoteKind, time.Now().AddDate(0, 0, -7))
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, m := range meetings {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		matters = append(matters, Matter{
			ID:     "meeting-" + strconv.FormatInt(m.ID, 10),
			Title:  minutesTitle(m.Content),
			Kind:   "meeting",
			Status: "done",
			When:   rfc3339(m.CreatedAt),
			Detail: util.Runes(strings.TrimSpace(m.Content), maxEntryText),
		})
	}

	if len(matters) > mattersCap {
		matters = matters[:mattersCap]
	}
	util.WriteJSON(w, map[string]any{"matters": matters})
}

// Today handles GET /today: today's morning brief (or the latest daily digest when no brief was written) and the day's timeline, oldest first and capped at 60 entries. When there is more than that, the most recent 60 are kept.
func (s *Server) Today(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	since := db.DayStart(now)

	brief, err := s.store.DiaryEntry(ctx, now.Format("2006-01-02"), "brief")
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	if brief == "" {
		if brief, err = s.store.LatestDigest(ctx); err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
	}

	type dated struct {
		at    time.Time
		entry Entry
	}
	var rows []dated
	add := func(at time.Time, kind, text, source string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		rows = append(rows, dated{at: at, entry: Entry{When: rfc3339(at), Kind: kind, Text: util.Runes(text, maxEntryText), Source: source}})
	}

	summaries, err := s.store.SummaryTimeline(ctx, since, now)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, sum := range summaries {
		text, source := sum.Content, "June"
		var task memory.TaskSummary
		if json.Unmarshal([]byte(sum.Content), &task) == nil && task.Summary != "" {
			text, source = task.Summary, task.TaskName
		}
		add(sum.CreatedAt, "seen", text, source)
	}

	// Bounded by the same midnight the entries are then filtered against, rather than reading every note ever written on every poll of this screen. The store's own bound is on created_at or updated_at, which is what keeps an action item written last week and closed this morning on today's page.
	notes, err := s.store.NotesSince(ctx, since)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, n := range notes {
		switch n.Kind {
		case meetingNoteKind:
			if !n.CreatedAt.Before(since) {
				add(n.CreatedAt, "heard", minutesTitle(n.Content), "Meeting")
			}
		case memory.ActionNoteKind:
			// An action item's own row is when the work was closed, so a task entry keys off updated_at rather than created_at.
			a, ok := memory.ParseAction(n.Content)
			if ok && a.Status == memory.StatusDone && !n.UpdatedAt.Before(since) {
				add(n.UpdatedAt, "task", a.Text, a.Source)
			}
		default:
			if !n.CreatedAt.Before(since) {
				add(n.CreatedAt, "memory", n.Content, "Note")
			}
		}
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
	if len(rows) > timelineCap {
		rows = rows[len(rows)-timelineCap:]
	}
	timeline := make([]Entry, 0, len(rows))
	for _, r := range rows {
		timeline = append(timeline, r.entry)
	}
	util.WriteJSON(w, TodayView{Brief: brief, Timeline: timeline})
}

// Meetings handles GET /meetings: the last 30 recorded meetings, newest first, each with its minutes verbatim and the attendees read out of them.
func (s *Server) Meetings(w http.ResponseWriter, r *http.Request) {
	notes, err := s.store.NotesOfKindSince(r.Context(), meetingNoteKind, time.Time{})
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	// Notes arrive in the order they were filed, which is not the order the meetings ran in: a recording deferred to mains or recovered by the startup sweep is written up long after the call. Sorting before the cap makes this the 30 most recent meetings rather than the 30 most recently written up.
	sort.SliceStable(notes, func(i, j int) bool { return meetingStart(notes[i]).After(meetingStart(notes[j])) })
	if len(notes) > meetingsCap {
		notes = notes[:meetingsCap]
	}
	meetings := make([]Meeting, 0, len(notes))
	for _, n := range notes {
		minutes := minutesWithoutDurationMarker(n.Content)
		meetings = append(meetings, Meeting{
			ID:        strconv.FormatInt(n.ID, 10),
			Title:     minutesTitle(minutes),
			When:      rfc3339(meetingStart(n)),
			Minutes:   minutes,
			DurationS: meetingDurationSeconds(n.Content),
			Attendees: minutesAttendees(minutes),
		})
	}
	util.WriteJSON(w, map[string]any{"meetings": meetings})
}

// Meeting handles DELETE /meetings/{id}: it removes one meeting's write-up, for a call whose minutes are not worth keeping. Output: 204 with nothing, 404 for an id that names no meeting, 405 for any other method.
//
// The note's kind is checked before it is deleted, because meetings are notes and notes share one id space: without the check, an id typed or invented against this route would remove a diary entry or something the user told June to remember, and the meetings screen would have no way of knowing it had.
//
// The recording itself is left on disk. Deleting the write-up says the minutes were bad, not that the call never happened, and the audio is what a second attempt at them would have to read.
//
// The action items those minutes filed go with them while they are still open (see db.Store.DeleteMeetingActionItems): they are separate rows, linked to the minutes only by the meeting's name and day, so deleting the note alone left every task a rejected write-up invented on the Tasks screen. What the meeting taught personal context is not undone here: an entry the updater wrote may be one it rewrote rather than created, with nothing kept of what it said before, so it is removed by hand through DELETE /people/{name}.
func (s *Server) Meeting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "no such meeting", http.StatusNotFound)
		return
	}
	notes, err := s.store.NotesOfKindSince(r.Context(), meetingNoteKind, time.Time{})
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	at := slices.IndexFunc(notes, func(n db.Note) bool { return n.ID == id })
	if at < 0 {
		http.Error(w, "no such meeting", http.StatusNotFound)
		return
	}
	note := notes[at]
	if err := s.store.DeleteNote(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// After the note, so a failure here leaves the meeting gone and some of its tasks behind, which the user can still drop by hand, rather than tasks gone under a meeting that is still listed.
	if removed, err := s.store.DeleteMeetingActionItems(r.Context(), minutesWithoutDurationMarker(note.Content), meetingStart(note)); err != nil {
		slog.Error("meetings: could not remove the deleted meeting's open action items", "meeting", id, "removed", removed, "error", err)
	} else if removed > 0 {
		slog.Info("meetings: removed the deleted meeting's open action items", "meeting", id, "removed", removed)
	}
	w.WriteHeader(http.StatusNoContent)
}

// MemorySearch handles GET /memory/search?q=: 20 hybrid-search hits over notes, summaries, episodes and threads, filled up to that same total with archived notes whose text contains the query. A blank q is 400 — an empty search would otherwise read as "everything June knows".
func (s *Server) MemorySearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}

	hits, err := s.store.HybridSearch(ctx, q, "", searchCap)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	facts := make([]Fact, 0, len(hits))
	for _, h := range hits {
		title := h.Title
		if title == "" {
			title = h.App
		}
		facts = append(facts, Fact{
			ID:          h.Source + ":" + strconv.FormatInt(h.RefID, 10),
			Kind:        h.Source,
			Text:        util.Runes(h.Content, maxContextText),
			SourceTitle: title,
			When:        rfc3339(h.CreatedAt),
		})
	}

	// Archived notes are deliberately outside every index (see notes_archive in internal/db/store.go), so they are matched here by a plain substring scan rather than by search.
	archived, err := s.store.ArchivedNotes(ctx)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	lower := strings.ToLower(q)
	for _, a := range archived {
		// The whole page is bounded by searchCap, archive included: a query as short as "a" matches most of the archive, and each row carries up to 600 runes.
		if len(facts) >= searchCap {
			break
		}
		if !strings.Contains(strings.ToLower(a.Content), lower) {
			continue
		}
		facts = append(facts, Fact{
			ID:          "archive:" + strconv.FormatInt(a.NoteID, 10),
			Kind:        a.Kind,
			Text:        util.Runes(a.Content, maxContextText),
			SourceTitle: "archived note",
			When:        rfc3339(a.ArchivedAt),
			Archived:    true,
		})
	}
	util.WriteJSON(w, map[string]any{"facts": facts})
}

// People handles GET /people: the people personal context holds, then the names meetings of the last peopleWindow only ever heard, up to peopleCap in all. A personal-context entry is a person unless its subject is the user's own identity, the store's "unsure" bucket, or an area of preference.
func (s *Server) People(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	entries, err := s.store.PersonalContext(ctx)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}

	people := []Person{}
	known := map[string]bool{}
	for _, e := range entries {
		if !db.IsPersonSubject(e.Subject) {
			continue
		}
		name := db.PersonSubjectName(e.Subject)
		known[strings.ToLower(name)] = true
		count, err := s.store.MentionCount(ctx, name)
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		people = append(people, Person{Name: name, Seen: true, Note: strings.TrimSpace(e.Content), Count: count})
	}

	notes, err := s.store.NotesOfKindSince(ctx, meetingNoteKind, time.Now().Add(-peopleWindow))
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, n := range notes {
		for _, name := range heardNames(n.Content) {
			if len(people) >= peopleCap {
				break
			}
			if known[strings.ToLower(name)] {
				continue
			}
			known[strings.ToLower(name)] = true
			count, err := s.store.MentionCount(ctx, name)
			if err != nil {
				fail(w, err, http.StatusInternalServerError)
				return
			}
			people = append(people, Person{Name: name, HeardOnly: true, Count: count})
		}
	}
	util.WriteJSON(w, map[string]any{"people": people})
}

// Person handles DELETE /people/{name}: it removes one person personal context holds, named as GET /people names them ("Vexil Quorin") or by the entry's own subject ("vexil-quorin"). Output: 204 once removed; 404 for a name personal context holds no person under, which includes a name a meeting only heard, since nothing is stored for one; 405 for any other method.
// Personal context is read into every conversation, and the meeting updater writes people into it on its own judgement, so a person it got wrong was in every prompt from then on with no way to take them out from the window. The user's own identity and the preference entries are not people and cannot be removed through here.
func (s *Server) Person(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	entries, err := s.store.PersonalContext(r.Context())
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, e := range entries {
		if !db.IsPersonSubject(e.Subject) {
			continue
		}
		if !strings.EqualFold(db.PersonSubjectName(e.Subject), name) && !strings.EqualFold(e.Subject, name) {
			continue
		}
		if err := s.store.DeletePersonalContext(r.Context(), e.Subject); err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		slog.Info("people: removed a person from personal context", "subject", e.Subject)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "no such person", http.StatusNotFound)
}

// meetingDurationPrefix opens the machine-readable line internal/recorder appends to a meeting note's stored content, after the minutes text — see fileMinutes in internal/recorder/recorder.go. It carries the recording's actual wall-clock start and stop, the one thing on the note that could never be recovered from the minutes text itself.
const meetingDurationPrefix = "<!--june:duration "

// meetingStart is when a note's meeting actually ran. Input: one meeting note. Output: the recording's own start from its duration marker, falling back to when the note was filed for a note written before the marker existed — the window groups the meetings list by this, so a call written up the next morning would otherwise read as "today".
func meetingStart(n db.Note) time.Time {
	if start, _, ok := meetingDurationBounds(n.Content); ok {
		return start
	}
	return n.CreatedAt
}

// meetingDurationBounds reads the wall-clock start and stop a meeting note's marker carries. Input: the raw note content, marker included. Output: the two times and true, or two zero times and false for a note filed before this marker existed, one whose marker fails to parse, and one whose stop is not after its start.
func meetingDurationBounds(content string) (time.Time, time.Time, bool) {
	i := strings.Index(content, meetingDurationPrefix)
	if i < 0 {
		return time.Time{}, time.Time{}, false
	}
	line := content[i+len(meetingDurationPrefix):]
	if j := strings.Index(line, "-->"); j >= 0 {
		line = line[:j]
	}
	var startStr, stopStr string
	for _, field := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(field, "start="); ok {
			startStr = v
		} else if v, ok := strings.CutPrefix(field, "stop="); ok {
			stopStr = v
		}
	}
	start, err1 := time.Parse(time.RFC3339, startStr)
	stop, err2 := time.Parse(time.RFC3339, stopStr)
	if err1 != nil || err2 != nil || !stop.After(start) {
		return time.Time{}, time.Time{}, false
	}
	return start, stop, true
}

// meetingDurationSeconds reads a meeting note's recorded wall-clock length. Input: the raw note content, marker included. Output: how long the meeting ran in seconds, or 0 for a note filed before this marker existed, or one where it fails to parse.
func meetingDurationSeconds(content string) int {
	start, stop, ok := meetingDurationBounds(content)
	if !ok {
		return 0
	}
	return int(stop.Sub(start).Seconds())
}

// minutesWithoutDurationMarker strips the machine-readable duration line so the window never renders it as part of the minutes. Input: the raw note content. Output: the minutes text alone, or the input unchanged when it carries no marker.
func minutesWithoutDurationMarker(content string) string {
	i := strings.Index(content, meetingDurationPrefix)
	if i < 0 {
		return content
	}
	return strings.TrimRight(content[:i], "\n")
}

// minutesTitle names a set of minutes. Input: the minutes markdown. Output: the name on the bold line under the heading (what memory.MinutesLabel reads), the "# " heading when there is no such line, or "Meeting" when there is neither.
func minutesTitle(minutes string) string {
	if label := memory.MinutesLabel(minutes); label != "" {
		return label
	}
	for _, line := range strings.Split(minutes, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return "Meeting"
}

// heardMarker is how the minutes prompt asks a name the speech recogniser only heard to be written, for example: a contact (heard as "Oshveln").
const heardMarker = `heard as "`

// heardNames pulls every name the minutes marked as heard-only. Input: the minutes markdown. Output: the names inside each `heard as "…"` marker, in the order they appear, deduplicated.
func heardNames(minutes string) []string {
	var out []string
	seen := map[string]bool{}
	rest := minutes
	for {
		i := strings.Index(rest, heardMarker)
		if i < 0 {
			return out
		}
		rest = rest[i+len(heardMarker):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return out
		}
		name := strings.TrimSpace(rest[:j])
		rest = rest[j+1:]
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, name)
	}
}

// minutesAttendees reads who was actually in a meeting. Input: the minutes markdown. Output: one entry per bullet under the "In the meeting" list of the "## Attendees" section, in the order written, with HeardOnly set for a name carrying the heard-as marker; the "Mentioned or on screen only" list is skipped, and minutes without an attendee section give an empty, non-nil list.
func minutesAttendees(minutes string) []Attendee {
	out := []Attendee{}
	inSection, inList := false, false
	for _, line := range strings.Split(minutes, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			inSection = strings.EqualFold(trimmed, "## Attendees")
			inList = false
			continue
		}
		if !inSection {
			continue
		}
		if !strings.HasPrefix(trimmed, "- ") {
			// The two lists are introduced by their own bold lines; anything else between them is prose and changes nothing.
			label := strings.ToLower(strings.Trim(trimmed, "* "))
			if strings.HasPrefix(label, "in the meeting") {
				inList = true
			} else if strings.HasPrefix(label, "mentioned") {
				inList = false
			}
			continue
		}
		if !inList {
			continue
		}
		if a, ok := attendeeFromBullet(strings.TrimPrefix(trimmed, "- ")); ok {
			out = append(out, a)
		}
	}
	return out
}

// attendeeFromBullet reads one attendee bullet. Input: the bullet with its "- " marker stripped, in the "**Name** — what they did in this call" shape the minutes prompt asks for. Output: the attendee and true, or false for a bullet that carries no name at all. A bullet holding the heard-as marker takes its name from inside that marker and is reported as heard-only; otherwise the name is the text before the em dash, with bold markers and any "(recording)" style parenthetical removed.
func attendeeFromBullet(bullet string) (Attendee, bool) {
	if names := heardNames(bullet); len(names) > 0 {
		return Attendee{Name: names[0], HeardOnly: true}, true
	}
	name, _, ok := strings.Cut(bullet, "—")
	if !ok {
		name, _, _ = strings.Cut(bullet, ":")
		if name == "" {
			name = bullet
		}
	}
	name = strings.TrimSpace(strings.Trim(strings.TrimSpace(name), "*"))
	if i := strings.Index(name, "("); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	name = strings.TrimSpace(strings.Trim(name, "*"))
	if name == "" {
		return Attendee{}, false
	}
	return Attendee{Name: name}, true
}

// newestOtherThanJune returns the newest activity in buf whose app is not June's own window, and false when there is none. Input: the tracker buffer, oldest first. Output: the activity and whether one was found.
func newestOtherThanJune(buf []tracker.Activity) (tracker.Activity, bool) {
	for i := len(buf) - 1; i >= 0; i-- {
		if !tracker.IsJuneWindow(buf[i].App, buf[i].Title) {
			return buf[i], true
		}
	}
	return tracker.Activity{}, false
}
