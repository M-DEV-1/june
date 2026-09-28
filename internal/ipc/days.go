// days.go holds GET /days and GET /days/{date}: the window's history — which days have anything in them, and what one day held.
package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"june/internal/db"
	"june/internal/memory"
	"june/internal/util"
)

// daysBack is how far the history goes: the last sixty days that have anything in them.
const daysBack = 60

// dayTitleCap is the most runes of a day's title, which is the first non-empty line of its page with any leading "#" dropped, so the sidebar can draw it on one line.
const dayTitleCap = 80

// diaryDayKind is the diary kind the evening close writes one row of per day (see internal/proactive), which is the day's page when there is one.
const diaryDayKind = "day"

// diaryBriefKind is the diary kind the morning brief writes one row of per day (see internal/proactive.Scheduler.deliverBrief).
const diaryBriefKind = "brief"

// DaySummary is one row of GET /days. Title is the first line of the day's page, empty for a day June never wrote about; HasPage says whether such a page exists. Seen is the episodes recorded that day, Meetings the recordings held, and MeetingMinutes their total recorded length rounded to whole minutes.
type DaySummary struct {
	Date           string `json:"date"`
	Title          string `json:"title"`
	HasPage        bool   `json:"has_page"`
	Seen           int    `json:"seen"`
	Meetings       int    `json:"meetings"`
	MeetingMinutes int    `json:"meeting_minutes"`
}

// Said is one thing the user said on a day.
type Said struct {
	When string `json:"when"`
	Text string `json:"text"`
}

// DayTask is one action item raised on a day, with whether it has since been closed. It is the same row GET /tasks lists, and ID is the same id POST /tasks/{id}/done takes, so ticking it here and ticking it there are one act rather than two lists that drift apart.
// Status is the item's full state ("open", "done" or "dropped") since Done cannot tell a dropped item from an open one, and Owner is whose task it is ("me", "them" or "unclear").
type DayTask struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Done   bool   `json:"done"`
	Status string `json:"status"`
	Owner  string `json:"owner"`
}

// DayView is GET /days/{date}: what June wrote about the day, what the user asked that day, and the work the day raised. Heading is the day's activity summarised as one line, for example "60 things seen · 1 call, 28 min", or "" for a day with nothing in it. Brief is the morning brief delivered that day and Close the evening close entry, both "" when that day had none — Page carries the same close text when there is one, falling back to the day's digest when there is not, so a caller that only wants the reading page can keep using it unchanged.
type DayView struct {
	Date    string    `json:"date"`
	Page    string    `json:"page"`
	Brief   string    `json:"brief"`
	Close   string    `json:"close"`
	You     []Said    `json:"you"`
	Tasks   []DayTask `json:"tasks"`
	Heading string    `json:"heading"`
}

// Days handles GET /days: the last sixty days that have a capture, a meeting or a diary entry on them, newest first, each with the first line of its page.
func (s *Server) Days(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	from := now.AddDate(0, 0, -daysBack)

	dates, err := s.store.ActiveDays(ctx, from)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	pages, err := s.store.DiaryDays(ctx, from.Format("2006-01-02"), now.Format("2006-01-02"))
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	byDate := map[string]string{}
	for _, p := range pages {
		byDate[p.Day] = p.Content
	}

	seen, err := s.store.EpisodeCountsByDay(ctx, from, now)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	meetingNotes, err := s.store.NotesOfKindSince(ctx, meetingNoteKind, from)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	meetingCount, meetingSeconds := meetingsByDay(meetingNotes)

	days := make([]DaySummary, 0, len(dates))
	for _, date := range dates {
		page, has := byDate[date]
		summary := DaySummary{
			Date: date, Title: util.Runes(strings.TrimLeft(util.FirstLine(page), "# "), dayTitleCap), HasPage: has,
			Seen:           seen[date],
			Meetings:       meetingCount[date],
			MeetingMinutes: roundToMinutes(meetingSeconds[date]),
		}
		if summary.Seen == 0 && summary.Meetings == 0 && !summary.HasPage {
			// ActiveDays' own union has no upper bound on the diary table, so a diary row dated after "now" surfaces here with no page in range and nothing else on it either — left out rather than shown as an empty day.
			continue
		}
		days = append(days, summary)
	}
	util.WriteJSON(w, map[string]any{"days": days})
}

// Day handles GET /days/{date}, date as local 'YYYY-MM-DD'. Page is the diary entry June wrote that evening, or the day's digest when there is no entry, or empty. You are the user's own turns from that day's conversations, oldest first; Tasks are the action items the day raised, each with whether it is now closed. A date that does not parse is 400.
func (s *Server) Day(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	day, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	end := day.AddDate(0, 0, 1).Add(-time.Nanosecond)

	closeEntry, err := s.store.DiaryEntry(ctx, date, diaryDayKind)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	page := closeEntry
	if page == "" {
		if page, err = digestOfDay(ctx, s.store, day, end); err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
	}
	brief, err := s.store.DiaryEntry(ctx, date, diaryBriefKind)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}

	turns, err := s.store.TurnsBetween(ctx, day, end)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	you := []Said{}
	for _, t := range turns {
		if t.Role != "you" {
			continue
		}
		you = append(you, Said{When: rfc3339(t.When), Text: t.Text})
	}

	items, err := s.store.ActionItemsByOwner(ctx, allOwners)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	identity := s.store.Identity(ctx)
	tasks := []DayTask{}
	for _, a := range items {
		if a.Created.Before(day) || a.Created.After(end) {
			continue
		}
		tasks = append(tasks, DayTask{
			ID:     strconv.FormatInt(a.NoteID, 10),
			Title:  a.Text,
			Done:   a.Status == memory.StatusDone,
			Status: a.Status,
			Owner:  a.OwnerClass(identity),
		})
	}

	seen, err := s.store.EpisodeCountsByDay(ctx, day, end)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	meetingNotes, err := s.store.NotesOfKindSince(ctx, meetingNoteKind, day)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	meetingCount, meetingSeconds := 0, 0
	for _, n := range meetingNotes {
		if n.CreatedAt.After(end) {
			continue
		}
		meetingCount++
		meetingSeconds += meetingDurationSeconds(n.Content)
	}

	util.WriteJSON(w, DayView{
		Date: date, Page: page, Brief: brief, Close: closeEntry, You: you, Tasks: tasks,
		Heading: dayHeading(seen[date], meetingCount, roundToMinutes(meetingSeconds)),
	})
}

// meetingsByDay tallies meeting notes by the local calendar day they were filed on. Input: meeting notes, any order. Output: how many landed on each day, and their total recorded duration in seconds on each day — a note filed before the duration marker existed contributes to the count but adds zero seconds.
func meetingsByDay(notes []db.Note) (counts, seconds map[string]int) {
	counts, seconds = map[string]int{}, map[string]int{}
	for _, n := range notes {
		day := n.CreatedAt.Local().Format("2006-01-02")
		counts[day]++
		seconds[day] += meetingDurationSeconds(n.Content)
	}
	return counts, seconds
}

// roundToMinutes rounds a duration in seconds to the nearest whole minute.
func roundToMinutes(seconds int) int {
	return int(math.Round(float64(seconds) / 60))
}

// dayHeading summarises a day's activity as one line for the sidebar, or "" when the day had nothing at all. Input: episodes recorded, meetings held, and their total minutes. Output: for example "60 things seen · 1 call, 28 min".
func dayHeading(seen, meetings, minutes int) string {
	if seen == 0 && meetings == 0 {
		return ""
	}
	var parts []string
	if seen > 0 {
		parts = append(parts, countWord(seen, "thing seen", "things seen"))
	}
	if meetings > 0 {
		calls := countWord(meetings, "call", "calls")
		// A recording with no duration marker counts as a call but adds no minutes, and "0 min" would read as a claim that the calls were empty.
		if minutes > 0 {
			calls += fmt.Sprintf(", %d min", minutes)
		}
		parts = append(parts, calls)
	}
	return strings.Join(parts, " · ")
}

// countWord renders a count with its singular or plural noun, digits first.
func countWord(n int, singular, plural string) string {
	word := plural
	if n == 1 {
		word = singular
	}
	return strconv.Itoa(n) + " " + word
}

// digestOfDay is the fallback page for a day June never wrote a diary entry for: the daily digest the compaction pass wrote for that day. Input: the store and the day's bounds. Output: the digest text, or "" when the day has none. The summary timeline carries both the compiler's per-task summaries (marshalled JSON) and the digests (plain prose), and only the digest is prose — the same test Today() uses to tell them apart.
func digestOfDay(ctx context.Context, store *db.Store, from, to time.Time) (string, error) {
	rows, err := store.SummaryTimeline(ctx, from, to)
	if err != nil {
		return "", err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if json.Valid([]byte(rows[i].Content)) {
			continue
		}
		return rows[i].Content, nil
	}
	return "", nil
}
