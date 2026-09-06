package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"ora/internal/db"
	"ora/internal/util"
)

// recallSubjectLimit bounds how many lines RecallSubject contributes to the "recall" tool's subject path.
const recallSubjectLimit = 6

// recallEpisodeCap is how many raw episodes one recall window may return, and recallSummaryCap how many summary lines. Together they implement one rule with no span constants in it: a window is answered from the finest tier whose entire content fits — episodes when they all fit, every task summary when those fit, and a per-day-per-task rollup when even the summaries overflow. Coverage is by construction at every tier; nothing is ever cut to the newest slice.
const (
	recallEpisodeCap = 50
	recallSummaryCap = 60
)

// summaryTimeline renders the summary tier for a recall window, oldest first. Returns nil when the window has no summaries, which sends the caller back to raw episodes.
func (a *Agent) summaryTimeline(ctx context.Context, since, until time.Time) []string {
	sums, err := a.brain.SummaryTimeline(ctx, since, until)
	if err != nil {
		slog.Warn("recall: summary tier read failed, falling back to episodes", "error", err)
		return nil
	}
	kept := make([]db.WindowSummary, 0, len(sums))
	for _, s := range sums {
		if task, _ := parseTaskSummary(s.Content); task == "Raw Activity Log" {
			// The compiler's fallback bucket for windows it could not read — noise, not a stretch of work.
			continue
		}
		kept = append(kept, s)
	}
	if len(kept) == 0 {
		return nil
	}
	if len(kept) <= recallSummaryCap {
		lines := make([]string, 0, len(kept))
		for _, s := range kept {
			lines = append(lines, "["+s.CreatedAt.Local().Format("Jan 2 15:04")+"] "+summaryLine(s.Content))
		}
		return lines
	}
	return rollupByDayAndTask(kept)
}

// parseTaskSummary reads the compiler's JSON summary shape. ok is false for anything else — a digest's plain prose, or a malformed row.
func parseTaskSummary(content string) (task string, summary string) {
	var s struct {
		Task    string `json:"task_name"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(content), &s); err == nil {
		return s.Task, s.Summary
	}
	return "", ""
}

// summaryLine renders one summary node as prose: the compiler's JSON becomes "task — what happened", a digest's plain prose passes through untouched.
func summaryLine(content string) string {
	if task, summary := parseTaskSummary(content); task != "" {
		return task + " — " + oneLineExcerpt(summary)
	}
	return oneLineExcerpt(content)
}

// rollupByDayAndTask collapses an over-long summary timeline to one line per task per day, carrying how many stretches it covered and the first stretch's description — the tier above task summaries, computed at read time because stored digests only exist once compaction has retired a day's summaries.
func rollupByDayAndTask(sums []db.WindowSummary) []string {
	type slot struct {
		day, task, first string
		count            int
		order            int
	}
	slots := map[string]*slot{}
	var ordered []*slot
	for _, s := range sums {
		task, summary := parseTaskSummary(s.Content)
		if task == "" {
			task, summary = oneLineExcerpt(s.Content), ""
		}
		day := s.CreatedAt.Local().Format("Jan 2")
		key := day + "\x00" + task
		if sl, ok := slots[key]; ok {
			sl.count++
			continue
		}
		sl := &slot{day: day, task: task, first: oneLineExcerpt(summary), count: 1, order: len(ordered)}
		slots[key] = sl
		ordered = append(ordered, sl)
	}
	lines := make([]string, 0, len(ordered))
	for _, sl := range ordered {
		line := "[" + sl.day + "] " + sl.task
		if sl.count > 1 {
			line += fmt.Sprintf(" (%d stretches)", sl.count)
		}
		if sl.first != "" {
			line += " — " + sl.first
		}
		lines = append(lines, line)
	}
	return lines
}

// recallExcerpt caps how much of an episode's screen_text is surfaced per line in the "recall" tool's window (timeline) path — shorter than maxEpisodeExcerpt since a whole day's timeline is many lines at once.
const recallExcerpt = 160

// blankField reports whether an episode field carries no information. Empty counts, and so does the literal "Unknown" the tracker writes when it cannot read the focused window's app or title (see the "activity tracked" log lines) — the string is a placeholder, not a value.
func blankField(s string) bool {
	t := strings.TrimSpace(s)
	return t == "" || strings.EqualFold(t, "unknown")
}

// idleEpisode reports whether an episode has nothing to say: no app, no title, no screen text and no described activity. A recall over a night at an idle machine returned 25 such rows out of 40, each rendering as "Unknown — Unknown: Unknown" and each costing tokens the real rows needed.
func idleEpisode(e db.Episode) bool {
	return blankField(e.App) && blankField(e.Title) && blankField(e.ScreenText) && blankField(e.UserActivity)
}

// idleGapPrefix opens the line standing in for a run of idle captures, so a caller can tell an accounting line from a real moment.
const idleGapPrefix = "nothing on screen for"

// humanSpan renders a duration the way a person says it out loud: "2h 10m", "12m", or "a moment" for anything under a minute.
func humanSpan(d time.Duration) string {
	if d < time.Minute {
		return "a moment"
	}
	if h := int(d.Hours()); h > 0 {
		return fmt.Sprintf("%dh %dm", h, int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// appendIdleGap adds one line saying how long a run of idle captures covered, so a mostly-empty window reads as time passing rather than as a count of rows the tool threw away — "(19 idle omitted)" is what made a real user answer "are you serious?". An empty run is a no-op.
// Input: the lines so far and the consecutive idle episodes. Output: the lines with the gap line appended.
func appendIdleGap(lines []string, run []db.Episode) []string {
	if len(run) == 0 {
		return lines
	}
	// The run is a contiguous slice in whatever order the caller got its episodes (newest-first, in practice), so the span is the distance between its ends either way round.
	span := run[0].CreatedAt.Sub(run[len(run)-1].CreatedAt)
	if span < 0 {
		span = -span
	}
	return append(lines, idleGapPrefix+" "+humanSpan(span))
}

// wrapperProcesses are process names that own a window without being the program the user actually sees: mutter-x11-frames is the compositor's own frame around an X11 client, gnome-terminal-server hosts every GNOME terminal window. Printing them as the application tells the model nothing about what was on screen.
var wrapperProcesses = map[string]bool{"mutter-x11-frames": true, "gnome-terminal-server": true}

// displayAppTitle picks the app name and title to print for one capture. For a wrapper process the real program name is the tail of the window title — "portfolio_vulnerability_scores.xlsx — LibreOffice Calc" is LibreOffice Calc showing that file — so the tail becomes the app and the head stays the title. A title with no such tail keeps the process name, since a wrong guess is worse than an ugly one.
// Input: the capture's app and title. Output: the app name and title to print.
func displayAppTitle(app, title string) (string, string) {
	if !wrapperProcesses[app] {
		return app, title
	}
	for _, sep := range []string{" — ", " - "} {
		if i := strings.LastIndex(title, sep); i > 0 {
			return strings.TrimSpace(title[i+len(sep):]), strings.TrimSpace(title[:i])
		}
	}
	return app, title
}

// oneLineExcerpt collapses every run of whitespace in captured screen text to a single space, then caps it at recallExcerpt runes. A capture carries the newlines and column padding of whatever was on screen, which turns one timeline row into a dozen lines and spends the rune cap on layout instead of content.
func oneLineExcerpt(s string) string {
	return util.Runes(util.OneLine(s), recallExcerpt)
}

// formatSpan renders how long a run of consecutive captures of the same window covered, as "1h04m" or "12m". Anything under a minute returns "" so a single capture prints no duration at all.
func formatSpan(d time.Duration) string {
	if d < time.Minute {
		return ""
	}
	if h := int(d.Hours()); h > 0 {
		return fmt.Sprintf("%dh%02dm", h, int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// episodeHandle splits one capture into the name its row leads with and the machine names that follow it in a parenthetical, so the model reads a row about the work rather than a row about the software.
// The handle is what the user would call the thing: the capture's own one-phrase activity, since nothing in the store links an episode to the thread it was attributed to. With no activity phrase the app name from displayAppTitle leads instead and only the window title stays in the parenthetical.
// Input: one episode. Output: the leading handle and the parenthetical contents, either of which can be "".
func episodeHandle(e db.Episode) (string, string) {
	app, title := displayAppTitle(e.App, e.Title)
	machine := make([]string, 0, 2)
	for _, s := range []string{app, title} {
		if !blankField(s) {
			machine = append(machine, strings.TrimSpace(s))
		}
	}
	handle := strings.TrimSpace(e.UserActivity)
	if handle == "" && len(machine) > 0 {
		handle, machine = machine[0], machine[1:]
	}
	return handle, strings.Join(machine, ", ")
}

// formatEpisodeTimeline renders episodes (newest first) as one line per window. A run of idle captures collapses to one "nothing on screen for …" line, and consecutive captures of the same app and title collapse to a single line carrying how long that window stayed up — the same window sampled every minute used to print once per sample.
// Input: the episodes and a function producing the excerpt for one of them (recall shows screen text). Output: the formatted lines in the order the episodes came in.
func formatEpisodeTimeline(episodes []db.Episode, excerptFor func(db.Episode) string) []string {
	var lines []string
	idleStart := 0
	idleRun := 0
	for i := 0; i < len(episodes); {
		if idleEpisode(episodes[i]) {
			if idleRun == 0 {
				idleStart = i
			}
			idleRun++
			i++
			continue
		}
		lines = appendIdleGap(lines, episodes[idleStart:idleStart+idleRun])
		idleRun = 0

		j := i + 1
		for j < len(episodes) && !idleEpisode(episodes[j]) && episodes[j].App == episodes[i].App && episodes[j].Title == episodes[i].Title {
			j++
		}
		oldest := episodes[j-1]
		handle, machine := episodeHandle(episodes[i])
		if machine != "" {
			handle += " (" + machine + ")"
		}
		span := formatSpan(episodes[i].CreatedAt.Sub(oldest.CreatedAt))
		if span != "" {
			span += " "
		}
		excerpt := excerptFor(episodes[i])
		if excerpt == "" {
			// Nothing captured beyond the window itself: end the row at the handle rather than on a dangling colon.
			lines = append(lines, fmt.Sprintf("[%s] %s%s",
				oldest.CreatedAt.In(time.Local).Format("Jan 2 15:04"), span, handle))
			i = j
			continue
		}
		// Timeline shape: "[Jan 2 15:04] 1h04m the vulnerability scoring (LibreOffice Calc, portfolio.xlsx): …", converted to the user's local zone (episodes are stored in UTC) so what's shown matches their wall clock. The stamp is the run's start, so the duration reads forward from it.
		lines = append(lines, fmt.Sprintf("[%s] %s%s: %s",
			oldest.CreatedAt.In(time.Local).Format("Jan 2 15:04"), span, handle, excerpt))
		i = j
	}
	return appendIdleGap(lines, episodes[idleStart:idleStart+idleRun])
}

// recallBounds resolves the "recall" tool's since/until args into a concrete [since, until] range.
// The model, knowing the current date/time, converts any human phrase ("July 5th", "last week") into ISO-8601 bounds and passes them here; parseInstant additionally accepts the bare words "today" and "yesterday", which the model passes straight through often enough to be worth handling.
func recallBounds(sinceStr, untilStr string, now time.Time) (time.Time, time.Time, error) {
	since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if strings.TrimSpace(sinceStr) != "" {
		parsed, err := parseInstant(sinceStr, now, false)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		since = parsed
	}
	until := now
	if strings.TrimSpace(untilStr) != "" {
		parsed, err := parseInstant(untilStr, now, true)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		until = parsed
	}
	if since.After(until) {
		return time.Time{}, time.Time{}, errSinceAfterUntil
	}
	return since, until, nil
}

// errSinceAfterUntil is a sentinel so callers can distinguish "the range is backwards" from "the timestamp didn't parse" — leading a reversed-range error with the ISO-8601 format hint would be misleading when the format was fine.
var errSinceAfterUntil = errors.New("since must not be after until")

// parseInstant parses a full RFC3339 timestamp, a zoneless datetime (2006-01-02T15:04:05, read in now's zone), a bare calendar date (2006-01-02), or the words "today" and "yesterday" resolved against now.
// A bare date or word anchors to the start of that day, or its end (23:59:59) when endOfDay is set — so a bare until date is inclusive of the whole day rather than a zero-width midnight instant.
// The words are here because the user says them out loud and the model passes them straight through; without them the call errors, or worse, the word reaches the search as a search term and matches things like "India Today".
func parseInstant(s string, now time.Time, endOfDay bool) (time.Time, error) {
	loc := now.Location()
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// A zoneless datetime carries a time of day already, so endOfDay doesn't apply to it.
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, loc); err == nil {
		return t, nil
	}
	d, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		switch strings.ToLower(s) {
		case "today":
			d = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		case "yesterday":
			d = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -1)
		default:
			n, ok := parseDaysAgo(s)
			if !ok {
				return time.Time{}, err
			}
			d = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -n)
		}
	}
	if endOfDay {
		return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc), nil
	}
	return d, nil
}

// parseDaysAgo reads the "N days ago" phrasing the user says out loud and the model passes straight through to since/until. Without it the call fails with a raw Go time-parse error, which a real session read out loud to the user.
// Input: the raw argument. Output: how many days back it means, and whether it was that shape at all.
func parseDaysAgo(s string) (int, bool) {
	f := strings.Fields(strings.ToLower(s))
	if len(f) != 3 || f[2] != "ago" || (f[1] != "day" && f[1] != "days") {
		return 0, false
	}
	n, err := strconv.Atoi(f[0])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// dateHint is the one phrasing for a since/until the tool could not read, shared by recall and query_memory so the model gets the same list of forms that work wherever it passes a date.
const dateHint = "I can only search by a real date — try 'today', 'yesterday', or a date like 2026-07-05"
