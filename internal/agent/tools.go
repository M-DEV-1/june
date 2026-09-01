package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"ora/internal/db"
	"ora/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genai"
)

// maxToolRows caps how many rows any one tool result may carry. A tool result is prompt text, and action_items and thread_evidence both read from stores that grow without bound — open action items never expire by design, and a long-running thread accumulates captures forever. query_memory has queryMemoryHits for the same reason.
const maxToolRows = 40

func shellName() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "sh"
}

// toolDefinitions returns ORA's own function declarations for the Live API.
// Every declaration is NON_BLOCKING. An unset Behavior means BLOCKING, which tells the Live API to freeze the conversation for the whole duration of a tool call — the model stops speaking and stops listening until the result lands, so a two-second memory lookup becomes two seconds of dead air on a voice call. NON_BLOCKING keeps the model talking and listening while the call runs; the result is folded back in later, at the moment picked by toolResponseScheduling in connect.go. Ora's own tool execution was already off the receive loop (see runToolCall), so this changes nothing about the transport — only the model-level contract.
func toolDefinitions() []*genai.Tool {
	return []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Behavior: genai.BehaviorNonBlocking,
				// TODO: need an approve/suggest feature for these tools
				Name:        "shell_exec",
				Description: "Execute a shell command on the user's system. Use powershell syntax on windows, sh on linux/mac. ALWAYS ask for confirmation before running destructive commands (rm, del, format, etc).",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"command": {Type: genai.TypeString, Description: "The shell command to execute"},
					},
					Required: []string{"command"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "read_clipboard",
				Description: "Read the current contents of the user's clipboard",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "read_file",
				Description: "Read the contents of a file on the user's filesystem. Use this to inspect code, configs, or any text file.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"path": {Type: genai.TypeString, Description: "Absolute or relative file path to read"},
					},
					Required: []string{"path"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "list_files",
				Description: "List files and directories at a given path. Returns names with [dir] or [file] prefix.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"path": {Type: genai.TypeString, Description: "Directory path to list. Defaults to current directory if empty."},
					},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "open_url",
				Description: "Open a URL in the user's default browser.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"url": {Type: genai.TypeString, Description: "The URL to open"},
					},
					Required: []string{"url"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "query_memory",
				Description: "Topical search over memory (moments, facts, arcs, period summaries). " +
					"Moments (screen observations) rank with recency; facts/notes do not expire. " +
					"Use app to restrict to one application (Slack, Firefox, Code). " +
					"Whenever the question is anchored to a time — a day, a part of a day, a range — pass since/until: " +
					"the search then runs and ranks entirely inside that window, whereas without it the best matches can all come from the wrong day, and one busy stretch can drown out the rest of its own day. " +
					"A part of a day gets timestamp bounds, not the whole day: morning is roughly 06:00-12:00, afternoon 12:00-18:00, evening and night after that. " +
					"When a question narrows the time, run a fresh narrower query — do not answer a narrow question from a wider fetch you already have. " +
					"For pure day/timeline questions use recall. For 'what was I just doing' use get_recent.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"query":  {Type: genai.TypeString, Description: "What to search for — topic, project, show, person, etc. Never put a time word here ('today', 'yesterday', 'last week') — it will match text instead of dates; use since/until for that."},
						"domain": {Type: genai.TypeString, Description: "Optional. Restrict to 'work' or 'personal' memories only. Omit to search everything, weighted toward whichever domain you're currently in."},
						"app":    {Type: genai.TypeString, Description: "Optional. Restrict moments to this application name (case-insensitive substring, e.g. slack, firefox, code)."},
						"since":  {Type: genai.TypeString, Description: "Optional. Start of the time window results must fall in. 'today', 'yesterday', a bare date (2026-07-05) meaning its start, or a timestamp (2026-07-05T09:30:00). You know the current date/time — convert other phrases into a concrete date yourself. Omit for no lower bound."},
						"until":  {Type: genai.TypeString, Description: "Optional. End of the time window (same formats as 'since'; a bare date covers through the end of that day). Omit to mean up to now. For a single day, set since and until to that same date."},
					},
					Required: []string{"query"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "recall",
				Description: "Timeline or subject recall. Use since/until for chronological periods (yesterday, last Tuesday). " +
					"A part of a day gets timestamp bounds rather than the whole day — morning roughly 06:00-12:00, afternoon 12:00-18:00, evening and night after that — and a question that narrows the time deserves a fresh narrower call, not an answer read off a wider fetch. " +
					"Use subject for an ongoing arc. Use app to keep only that application's moments. " +
					"Returns short content+context lines, not raw screen dumps.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"subject": {Type: genai.TypeString, Description: "Optional. A subject/topic to recall (fuses the matching thread's arc with diverse episode specifics). Cannot be combined with app, since, or until — use one or the other."},
						"since":   {Type: genai.TypeString, Description: "Optional. Start of the timeline window: 'today', 'yesterday', a bare date (2026-07-05), or a timestamp (2026-07-05T00:00:00). You know the current date/time — convert other phrases like 'July 5th' or 'last week' into a concrete date yourself. Defaults to the start of today."},
						"until":   {Type: genai.TypeString, Description: "Optional. End of the timeline window (same formats as 'since'). A bare date covers the whole day. Defaults to now. For a single day, set since and until to that same date."},
						"app":     {Type: genai.TypeString, Description: "Optional. Restrict moments to this application name (case-insensitive substring)."},
					},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "get_recent",
				Description: "The most recent screen moments, newest first. Use for 'what was I just doing', 'what have I been looking at', or the last few captures in an app. Not a topical search.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"limit": {Type: genai.TypeInteger, Description: "How many moments to return (default 10, max 50)."},
						"app":   {Type: genai.TypeString, Description: "Optional. Restrict to this application name (case-insensitive substring)."},
					},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "branch",
				Description: "Resolve one open-ended question or research task that needs cross-referencing " +
					"several searches to build a complete answer (e.g. \"catch me up on everything about the " +
					"Riddler project\", or a question spanning multiple topics/timeframes) — instead of calling " +
					"query_memory/recall repeatedly yourself. Runs an internal multi-step search in the " +
					"background and returns only the final synthesized answer; you will not see, and must not " +
					"need, its intermediate steps. Prefer query_memory/recall directly for a single simple lookup.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"task": {Type: genai.TypeString, Description: "The open-ended question or research task to resolve."},
					},
					Required: []string{"task"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "save_note",
				Description: "Save a durable fact the user tells you directly in conversation — identity, " +
					"preferences, plans, relationships, ongoing projects. Use this the moment they say something " +
					"worth remembering long-term (\"remember I have a dentist appointment Friday\", \"I prefer " +
					"terse replies\"). This is the ONLY way something said in conversation reaches long-term " +
					"memory — screen activity is captured separately and automatically, but nothing spoken or " +
					"typed to you here is remembered unless you save it. Don't use it for transient task chatter.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"content": {Type: genai.TypeString, Description: "The fact to remember, written as a durable statement, not a command to you."},
					},
					Required: []string{"content"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "personal_context",
				Description: "The small store of things known for CERTAIN about the user: who they are, the people in their life " +
					"(family, colleagues, friends), and preferences they have stated. Every entry goes into every conversation you " +
					"have with them, so it stays small and it stays true. " +
					"action \"set\" is only for something the user said about themselves, or confirmed when you asked them. Never " +
					"put in something you inferred, guessed, or read off their screen — an observation belongs in save_note instead. " +
					"Call action \"view\" before writing: subjects you already have come back with it, and if one of them covers what " +
					"you were about to add, edit that subject rather than making a near-duplicate. " +
					"action \"delete\" is for an entry the user says is wrong or no longer true.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"action":  {Type: genai.TypeString, Description: "\"view\" to read everything stored, \"set\" to write or edit one subject, \"delete\" to remove one."},
						"subject": {Type: genai.TypeString, Description: "Short key for the entry, lowercase and hyphenated: \"identity\", \"trupti-hosmani\", \"preferences-communication\". Required for set and delete. Reuse an existing subject to edit it."},
						"content": {Type: genai.TypeString, Description: "For set: the whole entry, written as plain prose about the user or that person. It replaces the subject's previous content, so include what still holds, not just the new part."},
					},
					Required: []string{"action"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "update_note",
				Description: "Correct a previously saved note whose content was wrong (misheard, misunderstood, " +
					"or the user says it's outdated) — look the note up first with query_memory to get its id " +
					"from the \"[note#N]\" prefix, then call this with the corrected content. Use this instead of " +
					"just apologizing out loud and leaving the wrong fact in memory.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"id":      {Type: genai.TypeInteger, Description: "The note's id, from a \"[note#N]\" query_memory result."},
						"content": {Type: genai.TypeString, Description: "The corrected fact, written as a durable statement."},
					},
					Required: []string{"id", "content"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "update_action",
				Description: "Mark something the user owes as done or dropped, or change how much it matters. " +
					"Action items are the things somebody agreed to do in a meeting; they are what the morning " +
					"brief leads with. Look one up with query_memory to get its id from the \"[note#N]\" prefix, " +
					"then call this. Use it whenever the user says a task is finished, is not happening, or is " +
					"more or less urgent than you implied — never leave a task the user says is done still open.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"id":       {Type: genai.TypeInteger, Description: "The action item's id, from a \"[note#N]\" result."},
						"status":   {Type: genai.TypeString, Description: "open, done, or dropped. Omit to leave the status alone."},
						"priority": {Type: genai.TypeString, Description: "high, normal, or low. Omit to leave the priority alone."},
					},
					Required: []string{"id"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "action_items",
				Description: "List what the user still owes — the things they agreed to do in a meeting and have not " +
					"closed. Use this for any question about outstanding work, owed tasks, commitments, what is on " +
					"their plate, or what they need to do. Do not use query_memory for those: an action item's text " +
					"is the task itself and shares no words with the question, so searching for it finds meetings " +
					"about meetings instead. This reads the list directly. Each result carries its id, so update_action " +
					"can close one straight afterwards.",
				Parameters: &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{}},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "thread_evidence",
				Description: "Show the captures behind an ongoing thread — the actual screens, in order, that the " +
					"thread was summarised from. A thread's state is one line; this is what it was written from. " +
					"Use it whenever the user asks for detail a thread only gestures at: what the findings actually " +
					"were, what the error said, which files were touched. Get the id from a \"[thread#N]\" result.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"id":    {Type: genai.TypeInteger, Description: "The thread's id, from a \"[thread#N]\" result."},
						"limit": {Type: genai.TypeInteger, Description: "Optional. How many captures to show, newest first. Defaults to 10."},
					},
					Required: []string{"id"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "fix_thread",
				Description: "Correct an ongoing thread whose summary is wrong — it merged two unrelated things, " +
					"or records a fact the user says is not true. Look it up first with query_memory or recall to " +
					"get its id from the \"[thread#N]\" prefix, then call this with what the thread actually is. " +
					"Threads are separate from notes: update_note cannot reach them.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"id":         {Type: genai.TypeInteger, Description: "The thread's id, from a \"[thread#N]\" result."},
						"correction": {Type: genai.TypeString, Description: "What this thread actually is, written as a durable one-line summary — it replaces the wrong one."},
					},
					Required: []string{"id", "correction"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "delete_note",
				Description: "Permanently remove a previously saved note the user says is wrong, irrelevant, or " +
					"should be forgotten — look the note up first with query_memory to get its id from the " +
					"\"[note#N]\" prefix, then call this. Use this instead of just apologizing out loud and " +
					"leaving the wrong fact in memory.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"id": {Type: genai.TypeInteger, Description: "The note's id, from a \"[note#N]\" query_memory result."},
					},
					Required: []string{"id"},
				},
			},
		},
	}}
}

// liveTools returns every tool exposed to the Live API session: ORA's own FunctionDeclarations (shell_exec, query_memory, save_note, etc.) plus Gemini's native GoogleSearch grounding tool, so Ora can look something up instead of guessing from memory.
// Verified live (2026-07-25) that both tool types work together on config.VoiceModel (gemini-2.5-flash-native-audio-preview-12-2025) — not guaranteed on every Gemini model/endpoint.
// GoogleSearch calls are grounded server-side by Gemini and never surface as a ToolCall, so they don't show up in the TUI's live tool status line the way the FunctionDeclarations tools do.
func liveTools() []*genai.Tool {
	tools := toolDefinitions()
	tools = append(tools, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
	return tools
}

// ToolDeclarations returns the function declarations the live session exposes, the same list liveTools builds. Gemini's native search tool is not included because it has no declaration to hand a non-live model. The trajectory eval in evals/ uses it to give a text-mode model the identical tool surface the voice session has. Input: none. Output: the declarations, in the order the live session sends them.
func ToolDeclarations() []*genai.FunctionDeclaration {
	tools := liveTools()
	if len(tools) == 0 {
		return nil
	}
	return tools[0].FunctionDeclarations
}

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

// idleGapPrefix opens the line standing in for a run of idle captures. capRealRows keys on it to tell an accounting line from a real moment.
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
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > recallExcerpt {
		return string(runes[:recallExcerpt])
	}
	return s
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
// Input: the episodes and a function producing the excerpt for one of them (recall shows screen text; get_recent folds in the frame marker). Output: the formatted lines in the order the episodes came in.
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

// capRealRows truncates lines once limit real moment lines have been emitted, ignoring the "nothing on screen for …" lines — those are accounting, not moments the user asked for.
// Input: the formatted lines and how many moments the caller asked for. Output: the prefix holding at most that many moments.
func capRealRows(lines []string, limit int) []string {
	n := 0
	for i, l := range lines {
		if strings.HasPrefix(l, idleGapPrefix) {
			continue
		}
		n++
		if n > limit {
			return lines[:i]
		}
	}
	return lines
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

// toolError renders a failed tool call as one plain sentence. Every failure path in executeTool goes through it, so a Go error string (a time-parse dump, a type name, a wrapped sqlite message) never reaches the model and, from there, never gets read out loud. The real error goes to the log instead.
// Input: a plain-words sentence saying what went wrong and, where the model can fix it, what to try instead. Output: the tool result string, prefixed "error: " — which is what the model and resultSummary both read as "this call failed".
func toolError(msg string) string {
	return "error: " + msg
}

// storeUnavailable is what every failed memory read says. The model can only ever do one thing about it, so naming the store's own error would add nothing it can act on.
const storeUnavailable = "I couldn't reach your memory just now — try that again in a moment"

// hasArg reports whether name was supplied with a value that isn't an empty string. A non-string value counts as supplied — the model meant something by it, and treating it as absent is how a filter gets dropped without anyone noticing.
func hasArg(args map[string]any, name string) bool {
	v, present := args[name]
	if !present {
		return false
	}
	s, ok := v.(string)
	return !ok || strings.TrimSpace(s) != ""
}

// stringArg returns args[name] as a string. A missing argument is "" with no error; one present but not a string is an error, since silently ignoring it is how a wrong-typed date turned into "the start of today".
func stringArg(args map[string]any, name string) (string, error) {
	v, present := args[name]
	if !present {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %T", name, v)
	}
	return s, nil
}

// checkArgs names every argument that isn't in valid, listing the ones that are so the model can correct itself on the next call. Returns "" when every argument is known.
// This exists because the model invents parameters — a real trace called recall with query_memory's "query" argument, which the recall handler ignored, then answered from the timeline branch with since defaulted to the start of today.
func checkArgs(args map[string]any, valid ...string) string {
	var unknown []string
	for name := range args {
		if !slices.Contains(valid, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	slices.Sort(unknown)
	return fmt.Sprintf("I don't take %s here — what I do take is %s",
		strings.Join(unknown, ", "), strings.Join(valid, ", "))
}

// optionalWindow parses since/until when either is given, for tools where no dates means no time filter at all (unlike recall, whose timeline branch defaults to today — see recallBounds).
// Output: the bounds, whether any filtering should happen, and a parse error. A zero bound means unbounded on that side.
func optionalWindow(args map[string]any, now time.Time) (time.Time, time.Time, bool, error) {
	sinceStr, sinceErr := stringArg(args, "since")
	untilStr, untilErr := stringArg(args, "until")
	if err := errors.Join(sinceErr, untilErr); err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	var since, until time.Time
	var err error
	if strings.TrimSpace(sinceStr) != "" {
		if since, err = parseInstant(sinceStr, now, false); err != nil {
			return time.Time{}, time.Time{}, false, err
		}
	}
	if strings.TrimSpace(untilStr) != "" {
		if until, err = parseInstant(untilStr, now, true); err != nil {
			return time.Time{}, time.Time{}, false, err
		}
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return time.Time{}, time.Time{}, false, errSinceAfterUntil
	}
	return since, until, !since.IsZero() || !until.IsZero(), nil
}

// queryMemoryHits is how many hits query_memory shows the model. The since/until window is enforced inside HybridSearchWindow (SQL-side, before top-k), so a windowed call asks for the same limit as a plain one.
const queryMemoryHits = 10

// sensitivePathSubstrings/sensitivePathSuffixes gate read_file behind HITL approval — credentials, SSH/GPG/cloud keys, and ora's own IPC token, all of which the model could otherwise read and ship to the Gemini API with zero user involvement. Matched against the path as given plus its absolute form, so both a relative "id_rsa" and "~/.ssh/id_rsa" (which filepath.Abs can't expand "~" in, but still contains the ".ssh/" substring literally) get caught.
var sensitivePathSubstrings = []string{".ssh/", ".gnupg/", ".aws/", ".env", "id_rsa", "id_ed25519", "credentials", "shadow", "ora-db/ipc-token"}
var sensitivePathSuffixes = []string{".pem", ".key"}

// isSensitivePath reports whether path matches one of the patterns above.
func isSensitivePath(path string) bool {
	candidates := []string{path}
	if abs, err := filepath.Abs(path); err == nil {
		candidates = append(candidates, filepath.Clean(abs))
	}
	for _, c := range candidates {
		for _, sub := range sensitivePathSubstrings {
			if strings.Contains(c, sub) {
				return true
			}
		}
		for _, suffix := range sensitivePathSuffixes {
			if strings.HasSuffix(c, suffix) {
				return true
			}
		}
	}
	return false
}

// readClipboard reads the system clipboard. Extracted so it can be passed as a ToolRequest.Execute closure — read_clipboard always requires HITL approval (see executeTool), since a password manager routinely puts secrets there.
func readClipboard() string {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-Command", "Get-Clipboard")
	} else {
		cmd = exec.Command("xclip", "-selection", "clipboard", "-o")
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("clipboard read failed", "error", err)
		return toolError("I couldn't read the clipboard")
	}
	return string(output)
}

func RunShellCommand(command string) string {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-Command", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		// The command's own output is the answer to why it failed and the model needs it; only Go's exit-status wrapper is dropped.
		return toolError("that command didn't run cleanly") + "\noutput: " + string(output)
	}
	result := string(output)
	if len(result) > 2000 {
		result = result[:2000] + "\n... (truncated)"
	}
	return result
}

// requestApproval sends a generic HITL approval request through ToolApprovalChan and blocks for the TUI's result — or until ctx is cancelled (the live session ended before the user responded; Connect's sessCancel via receiveLoop -> runToolCall). Callers check AllowedCmds themselves before calling this — allowKey/editableCommand are only carried through for the TUI to act on ("Allow for session" storage, "Suggest changes" pre-fill), not re-checked here.
func (a *Agent) requestApproval(ctx context.Context, allowKey, description string, execute func() string, editableCommand string) string {
	resChan := make(chan string, 1)
	req := ToolRequest{
		Description:     description,
		Execute:         execute,
		ResultChan:      resChan,
		AllowKey:        allowKey,
		EditableCommand: editableCommand,
	}
	select {
	case a.ToolApprovalChan <- req:
	default:
		// TUI approval queue full — another tool is pending. Reject to unblock.
		return toolError("I'm already waiting on another approval — ask again in a moment")
	}

	select {
	case res := <-resChan:
		return res
	case <-ctx.Done():
		slog.Warn("HITL approval abandoned: session ended before user responded", "description", description)
		return toolError("that ended before it was approved")
	}
}

// ExecuteTool is just executeTool but exported, so eval tests outside this package can call the real tool (query_memory, recall, etc) the same way the model does.
func (a *Agent) ExecuteTool(ctx context.Context, name string, args map[string]any) string {
	return a.executeTool(ctx, name, args)
}

// executeTool runs a tool and returns the result as a string
// maybe this can be seperated into /agent/tools altogether later and be compiled with OS specific code?
func (a *Agent) executeTool(ctx context.Context, name string, args map[string]any) string {
	switch name {
	case "shell_exec":
		command, ok := args["command"].(string)
		if !ok {
			return toolError("shell_exec needs a command to run")
		}

		// Check session allowlist
		if _, allowed := a.AllowedCmds.Load(command); allowed {
			slog.Info("executing auto-allowed shell command", "command", command)
			return RunShellCommand(command)
		}

		slog.Warn("intercepting shell command for HITL", "command", command)
		return a.requestApproval(ctx, command, "shell: "+command, func() string { return RunShellCommand(command) }, command)

	case "read_clipboard":
		// Always gated — a password manager routinely leaves a secret sitting in the clipboard, and there's no way to distinguish that from a benign copy ahead of time.
		if _, allowed := a.AllowedCmds.Load("read_clipboard"); allowed {
			return readClipboard()
		}
		slog.Warn("intercepting clipboard read for HITL")
		return a.requestApproval(ctx, "read_clipboard", "read the clipboard", readClipboard, "")

	case "read_file":
		path, ok := args["path"].(string)
		if !ok {
			return toolError("read_file needs a path")
		}
		execute := func() string {
			data, err := os.ReadFile(path)
			if err != nil {
				slog.Warn("read_file failed", "path", path, "error", err)
				return toolError("I couldn't read that file — check the path")
			}
			result := string(data)
			if len(result) > 4000 {
				result = result[:4000] + "\n... (truncated, file too large)"
			}
			return result
		}
		if !isSensitivePath(path) {
			return execute()
		}
		allowKey := "read_file:" + path
		if _, allowed := a.AllowedCmds.Load(allowKey); allowed {
			return execute()
		}
		slog.Warn("intercepting sensitive file read for HITL", "path", path)
		return a.requestApproval(ctx, allowKey, "read file: "+path, execute, "")

	case "list_files":
		path, _ := args["path"].(string)
		if path == "" {
			path = "."
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			slog.Warn("list_files failed", "path", path, "error", err)
			return toolError("I couldn't list that folder — check the path")
		}
		var lines []string
		for _, e := range entries {
			prefix := "[file]"
			if e.IsDir() {
				prefix = "[dir] "
			}
			lines = append(lines, fmt.Sprintf("%s %s", prefix, filepath.Join(path, e.Name())))
		}
		if len(lines) > 100 {
			lines = lines[:100]
			lines = append(lines, "... (truncated, too many entries)")
		}
		return strings.Join(lines, "\n")

	case "open_url":
		url, ok := args["url"].(string)
		if !ok {
			return toolError("open_url needs a url")
		}
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "windows":
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
		case "darwin":
			cmd = exec.Command("open", url)
		default:
			cmd = exec.Command("xdg-open", url)
		}
		if err := cmd.Start(); err != nil {
			slog.Warn("open_url failed", "url", url, "error", err)
			return toolError("I couldn't open that link")
		}
		return fmt.Sprintf("opened %s in browser", url)

	case "query_memory":
		if msg := checkArgs(args, "query", "domain", "app", "since", "until"); msg != "" {
			return toolError(msg)
		}
		query, ok := args["query"].(string)
		if !ok {
			return toolError("query_memory needs something to search for")
		}
		// domain is optional: a missing or wrong-typed arg silently becomes "" (search everything, weighted toward the current domain) rather than erroring — since/until below are stricter since a mis-parsed date changes which day the answer comes from.
		domain, _ := args["domain"].(string)
		since, until, timed, err := optionalWindow(args, time.Now())
		if errors.Is(err, errSinceAfterUntil) {
			return toolError("that range runs backwards — the start has to come before the end")
		}
		if err != nil {
			slog.Warn("query_memory: unreadable date", "error", err)
			return toolError(dateHint)
		}
		slog.Info("querying long-term memory", "query", query, "domain", domain, "since", since, "until", until)

		// HybridSearchWindow (FTS5 + vector, fused via reciprocal rank fusion) covers episodes/summaries/notes/threads in one fused, domain-aware ranking, with the since/until window enforced store-side — inside the SQL and the vector candidate pool, before any top-k — so a sparse window still yields its items instead of the old over-fetch-and-post-filter returning nothing.
		hits, err := a.brain.HybridSearchWindow(ctx, query, domain, since, until, queryMemoryHits)
		if err != nil {
			slog.Error("query_memory: search failed", "error", err)
			return toolError(storeUnavailable)
		}
		found := len(hits)
		app, _ := args["app"].(string)
		if strings.TrimSpace(app) != "" {
			hits = filterHitsByApp(hits, app)
		}
		if len(hits) == 0 {
			// "Nothing exists" and "the filters removed everything" are different answers and the model has to be able to tell them apart — answering the second as the first is how a question about episodes watched today got a flat no while the rows sat in the store.
			desc := filterDescription(app, since, until)
			if desc == "" {
				return "no memory matches"
			}
			total := found
			if total == 0 && timed {
				// The window emptied the search inside the store, so what exists outside it takes one unwindowed call to see.
				if unfiltered, uerr := a.brain.HybridSearchWindow(ctx, query, domain, time.Time{}, time.Time{}, queryMemoryHits); uerr == nil {
					total = len(unfiltered)
				}
			}
			if total > 0 {
				return fmt.Sprintf("%d matches, none %s", total, desc)
			}
			return "no memory matches"
		}
		lines := make([]string, 0, len(hits))
		// A minute-sampled screen produces runs of byte-identical captures, and ten copies of one row crowd nine real answers out of the result. Dedupe on the content itself (not the whole line — the same text five minutes apart is still the same information).
		seen := make(map[string]bool, len(hits))
		for _, h := range hits {
			if key := strings.TrimSpace(h.Content); key != "" {
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			// Content is excerpted via db.FormatHit/FormatNoteHit like every other read path — an unformatted hit can inject tens of KB from a single oversized row (see RetrieveRelevant/RecallSubject, which already do this).
			// Notes are the only source with an update_note/delete_note follow-up tool, so they're the only hits that carry their ref_id in the surfaced line — the model needs it in hand to act on a correction.
			if h.Source == "note" {
				lines = append(lines, db.FormatNoteHit(h, 0))
			} else {
				lines = append(lines, db.FormatHit(h, 0))
			}
		}
		return strings.Join(lines, "\n")

	case "recall":
		if msg := checkArgs(args, "subject", "since", "until", "app"); msg != "" {
			return toolError(msg)
		}
		if subject, ok := args["subject"].(string); ok && strings.TrimSpace(subject) != "" {
			slog.Info("recalling subject", "subject", subject)
			lines, err := a.brain.RecallSubject(ctx, subject, recallSubjectLimit)
			if err != nil {
				slog.Error("recall: subject lookup failed", "subject", subject, "error", err)
				return toolError(storeUnavailable)
			}
			if len(lines) == 0 {
				return "no memory of that subject"
			}
			// RecallSubject takes only a subject and a limit — it has no app filter and no date window — so combining subject with either can't be honored. Erroring taught the model nothing (it made the same call twice in one session) and cost the turn; the answer plus a line saying which filter was dropped is what it was after.
			var ignored []string
			if hasArg(args, "since") || hasArg(args, "until") {
				ignored = append(ignored, "the date window")
			}
			if hasArg(args, "app") {
				ignored = append(ignored, "the app filter")
			}
			if len(ignored) > 0 {
				lines = append(lines, fmt.Sprintf("(note: subject recall ignores %s)", strings.Join(ignored, " and ")))
			}
			return strings.Join(lines, "\n")
		}

		sinceStr, sinceErr := stringArg(args, "since")
		untilStr, untilErr := stringArg(args, "until")
		if err := errors.Join(sinceErr, untilErr); err != nil {
			slog.Warn("recall: date argument was not text", "error", err)
			return toolError(dateHint)
		}
		since, until, err := recallBounds(sinceStr, untilStr, time.Now())
		if errors.Is(err, errSinceAfterUntil) {
			return toolError("that range runs backwards — the start has to come before the end")
		}
		if err != nil {
			slog.Warn("recall: unreadable date", "error", err)
			return toolError(dateHint)
		}
		app, _ := args["app"].(string)
		slog.Info("recalling timeline window", "since", since, "until", until, "app", app)

		// One more than the cap, so overflow is detectable: a window whose episodes all fit is answered from them raw, and a bigger window climbs to the summary tier instead of silently returning whichever slice of itself is newest.
		episodes, err := a.brain.ListEpisodes(ctx, db.EpisodeQuery{Since: since, Until: until, App: app, Limit: recallEpisodeCap + 1, NewestFirst: true})
		if err != nil {
			slog.Error("recall: timeline read failed", "error", err)
			return toolError(storeUnavailable)
		}
		if len(episodes) == 0 {
			return "no episodes in that window"
		}
		// Summaries carry no app attribution, so an app-filtered recall stays on episodes whatever the size.
		if len(episodes) > recallEpisodeCap && app == "" {
			if lines := a.summaryTimeline(ctx, since, until); len(lines) > 0 {
				return strings.Join(lines, "\n")
			}
		}
		if len(episodes) > recallEpisodeCap {
			episodes = episodes[:recallEpisodeCap]
		}
		return strings.Join(formatEpisodeTimeline(episodes, func(e db.Episode) string {
			return oneLineExcerpt(e.ScreenText)
		}), "\n")

	case "get_recent":
		limit := 10
		if v, ok := args["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		if limit > 50 {
			limit = 50
		}
		app, _ := args["app"].(string)
		slog.Info("recalling recent moments", "limit", limit, "app", app)
		// Over-fetch, then cap after the idle rows are dropped: the newest rows in the store are routinely idle captures, so asking for exactly limit rows is how "the last six hours" came back as one real moment plus "(19 idle omitted)".
		episodes, err := a.brain.ListEpisodes(ctx, db.EpisodeQuery{App: app, Limit: min(limit*4, 200), NewestFirst: true})
		if err != nil {
			slog.Error("get_recent: read failed", "error", err)
			return toolError(storeUnavailable)
		}
		if len(episodes) == 0 {
			return "no recent episodes"
		}
		lines := formatEpisodeTimeline(episodes, func(e db.Episode) string {
			// The tracked activity is the row's handle now (see episodeHandle), so the excerpt is just the screen text — printing the activity here too said the same phrase twice on every line.
			excerpt := oneLineExcerpt(e.ScreenText)
			if e.ImagePath != "" {
				excerpt = strings.TrimSpace(excerpt + " [img]")
			}
			return excerpt
		})
		return strings.Join(capRealRows(lines, limit), "\n")

	case "branch":
		task, ok := args["task"].(string)
		if !ok || strings.TrimSpace(task) == "" {
			return toolError("branch needs the question to work on")
		}
		if !a.tryReserveBranchSlot() {
			return toolError("I've already run all the background searches I get this session — use query_memory or recall instead")
		}
		model, err := a.subtaskModelFactory()
		if err != nil {
			slog.Error("branch: could not start", "error", err)
			return toolError("that background search couldn't start — use query_memory or recall instead")
		}
		result, err := a.runSubtask(ctx, model, task)
		if err != nil {
			slog.Error("branch: failed", "error", err)
			return toolError("that background search didn't come back — use query_memory or recall instead")
		}
		return result

	case "save_note":
		content, ok := args["content"].(string)
		if !ok || strings.TrimSpace(content) == "" {
			return toolError("save_note needs the fact to remember")
		}
		if _, err := a.brain.LogNote(ctx, content, "fact"); err != nil {
			slog.Error("save_note: write failed", "error", err)
			return toolError("that didn't save — try saying it again")
		}
		return "saved"

	case "personal_context":
		action, _ := args["action"].(string)
		subject, _ := args["subject"].(string)
		content, _ := args["content"].(string)
		switch strings.ToLower(strings.TrimSpace(action)) {
		case "view":
			entries, err := a.brain.PersonalContext(ctx)
			if err != nil {
				slog.Error("personal_context: read failed", "error", err)
				return toolError(storeUnavailable)
			}
			if len(entries) == 0 {
				return "nothing in personal context yet"
			}
			var b strings.Builder
			for _, e := range entries {
				fmt.Fprintf(&b, "%s: %s\n", e.Subject, e.Content)
			}
			return strings.TrimRight(b.String(), "\n")
		case "set":
			if strings.TrimSpace(subject) == "" {
				return toolError("personal_context needs a subject to file this under, like \"identity\" or the person's name")
			}
			if strings.TrimSpace(content) == "" {
				return toolError("personal_context needs the content to save under that subject")
			}
			if err := a.brain.SetPersonalContext(ctx, subject, content); err != nil {
				slog.Error("personal_context: write failed", "subject", subject, "error", err)
				return toolError("that didn't save — try saying it again")
			}
			return "saved"
		case "delete":
			if strings.TrimSpace(subject) == "" {
				return toolError("personal_context needs the subject to remove — view it first to see which ones there are")
			}
			if err := a.brain.DeletePersonalContext(ctx, subject); err != nil {
				slog.Error("personal_context: delete failed", "subject", subject, "error", err)
				return toolError("nothing was removed — view it first to see which subjects there are")
			}
			return "deleted"
		default:
			return toolError("personal_context takes view, set or delete")
		}

	case "update_note":
		idFloat, ok := args["id"].(float64)
		if !ok {
			return toolError("update_note needs the note's id — the number in a [note#N] query_memory result")
		}
		content, ok := args["content"].(string)
		if !ok || strings.TrimSpace(content) == "" {
			return toolError("update_note needs the corrected fact")
		}
		if err := a.brain.UpdateNote(ctx, int64(idFloat), content); err != nil {
			slog.Error("update_note: write failed", "id", int64(idFloat), "error", err)
			return toolError("nothing was updated — look the note up again with query_memory and use the id it shows")
		}
		return "updated"

	case "thread_evidence":
		idFloat, ok := args["id"].(float64)
		if !ok || idFloat <= 0 {
			return toolError("thread_evidence needs the thread's id — the number in a [thread#N] result")
		}
		limit := 10
		if l, ok := args["limit"].(float64); ok && l > 0 {
			limit = int(l)
		}
		// A tool result is prompt text. query_memory caps at queryMemoryHits for the same reason: without a ceiling a model that asks for a thousand captures gets them, each now rendered with the raised excerpt budget.
		if limit > maxToolRows {
			limit = maxToolRows
		}
		eps, err := a.brain.EpisodesForThread(ctx, int64(idFloat), limit)
		if err != nil {
			slog.Error("thread_evidence: read failed", "id", int64(idFloat), "error", err)
			return toolError("could not read this thread's captures")
		}
		if len(eps) == 0 {
			// Threads attributed before the compiler began recording the edge have none, and saying so plainly stops the model reading an empty result as "nothing happened".
			return "no captures are linked to that thread — it was summarised before Ora started recording which screens a thread came from"
		}
		var b strings.Builder
		for _, e := range eps {
			fmt.Fprintf(&b, "%s  %s — %s\n", e.CreatedAt.Local().Format("Mon Jan 2 15:04"), e.App, e.Title)
			if txt := strings.TrimSpace(e.ScreenText); txt != "" {
				fmt.Fprintf(&b, "    %s\n", db.FormatHit(db.MemoryHit{Source: "episode", Content: txt}, 0))
			}
		}
		return strings.TrimRight(b.String(), "\n")

	case "action_items":
		items, err := a.brain.OpenActionItems(ctx)
		if err != nil {
			slog.Error("action_items: read failed", "error", err)
			return toolError("could not read the action items")
		}
		if len(items) == 0 {
			return "nothing outstanding — no open action items"
		}
		var b strings.Builder
		// Open items never expire by design — "an owed task does not stop being owed" — so the list only grows, and it is prompt text like any other tool result.
		if len(items) > maxToolRows {
			items = items[:maxToolRows]
		}
		for _, it := range items {
			// The id leads so update_action can close one without a second lookup, and the source meeting trails so the model can say where a task came from.
			fmt.Fprintf(&b, "[note#%d] %s\n", it.NoteID, it.Note())
		}
		return strings.TrimRight(b.String(), "\n")

	case "update_action":
		idFloat, ok := args["id"].(float64)
		if !ok {
			return toolError("update_action needs the action item's id — the number in a [note#N] query_memory result")
		}
		status, _ := args["status"].(string)
		priority, _ := args["priority"].(string)
		if strings.TrimSpace(status) == "" && strings.TrimSpace(priority) == "" {
			return toolError("update_action needs a status (open, done, dropped) or a priority (high, normal, low) to change")
		}
		if s := strings.TrimSpace(status); s != "" {
			if !memory.ValidStatus(s) {
				return toolError("status must be open, done, or dropped")
			}
			if err := a.brain.SetActionStatus(ctx, int64(idFloat), s); err != nil {
				slog.Error("update_action: status write failed", "id", int64(idFloat), "error", err)
				return toolError("nothing was updated — look the item up again with query_memory and use the id it shows")
			}
		}
		if p := strings.TrimSpace(priority); p != "" {
			if !memory.ValidPriority(p) {
				return toolError("priority must be high, normal, or low")
			}
			if err := a.brain.SetActionPriority(ctx, int64(idFloat), p); err != nil {
				slog.Error("update_action: priority write failed", "id", int64(idFloat), "error", err)
				return toolError("nothing was updated — look the item up again with query_memory and use the id it shows")
			}
		}
		return "updated"

	case "fix_thread":
		idFloat, ok := args["id"].(float64)
		if !ok {
			return toolError("fix_thread needs the thread's id — the number in a [thread#N] result")
		}
		correction, ok := args["correction"].(string)
		if !ok || strings.TrimSpace(correction) == "" {
			return toolError("fix_thread needs what the thread actually is")
		}
		if err := a.brain.UpdateThreadState(ctx, int64(idFloat), correction); err != nil {
			slog.Error("fix_thread: write failed", "id", int64(idFloat), "error", err)
			return toolError("nothing was fixed — look the thread up again with query_memory and use the id it shows")
		}
		return "fixed"

	case "delete_note":
		idFloat, ok := args["id"].(float64)
		if !ok {
			return toolError("delete_note needs the note's id — the number in a [note#N] query_memory result")
		}
		if err := a.brain.DeleteNote(ctx, int64(idFloat)); err != nil {
			slog.Error("delete_note: write failed", "id", int64(idFloat), "error", err)
			return toolError("nothing was deleted — look the note up again with query_memory and use the id it shows")
		}
		return "deleted"

	default:
		slog.Warn("unknown tool called", "tool", name)
		return toolError("there's no tool by that name")
	}
}

// toolArgSummaryRunes bounds how much of an argument the UI's tool line shows. A shell command or a note body runs arbitrarily long, and the live status row is a single line.
const toolArgSummaryRunes = 48

// quoteArg renders an argument value as a quoted display literal, cut to toolArgSummaryRunes with an ellipsis when it is longer.
// Input: the raw argument string. Output: the quoted, possibly-truncated literal, e.g. `"ls -la"`.
func quoteArg(s string) string {
	if r := []rune(s); len(r) > toolArgSummaryRunes {
		s = string(r[:toolArgSummaryRunes]) + "\u2026"
	}
	return fmt.Sprintf("%q", s)
}

// toolActivitySummary pre-formats a tool call's primary argument into a short display literal for the UI (e.g. `"Riddler puzzles"` for query_memory), so the UI never needs to know each tool's arg-shape — that knowledge already lives here, next to executeTool/toolDefinitions.
// Unknown tools and no-arg tools (read_clipboard) summarize to "".
func toolActivitySummary(name string, args map[string]any) string {
	switch name {
	case "query_memory":
		if q, ok := args["query"].(string); ok {
			return quoteArg(q)
		}
	case "get_recent":
		if app, ok := args["app"].(string); ok && strings.TrimSpace(app) != "" {
			return quoteArg(app)
		}
		return "recent"
	case "recall":
		if subject, ok := args["subject"].(string); ok && strings.TrimSpace(subject) != "" {
			return quoteArg(subject)
		}
		since, _ := args["since"].(string)
		until, _ := args["until"].(string)
		switch {
		case since != "" && until != "":
			return fmt.Sprintf("since %s until %s", since, until)
		case since != "":
			return fmt.Sprintf("since %s", since)
		case until != "":
			return fmt.Sprintf("until %s", until)
		}
	case "shell_exec":
		if cmd, ok := args["command"].(string); ok {
			return quoteArg(cmd)
		}
	case "read_file":
		if path, ok := args["path"].(string); ok {
			return quoteArg(path)
		}
	case "list_files":
		if path, ok := args["path"].(string); ok && path != "" {
			return quoteArg(path)
		}
	case "open_url":
		if url, ok := args["url"].(string); ok {
			return quoteArg(url)
		}
	case "save_note":
		if content, ok := args["content"].(string); ok {
			return quoteArg(content)
		}
	case "update_note":
		if content, ok := args["content"].(string); ok {
			return quoteArg(content)
		}
	case "personal_context":
		action, _ := args["action"].(string)
		if subject, ok := args["subject"].(string); ok && subject != "" {
			return fmt.Sprintf("%s %s", action, quoteArg(subject))
		}
		return quoteArg(action)
	case "delete_note":
		if id, ok := args["id"].(float64); ok {
			return fmt.Sprintf("#%d", int64(id))
		}
	case "branch":
		if task, ok := args["task"].(string); ok {
			return quoteArg(task)
		}
	}
	return ""
}

// resultSummary condenses a tool's raw result string into a short status word for the UI's transcript log line — "N hits" for the search-shaped tools, "failed" on any error result (executeTool always prefixes errors with "error"), "0 hits" for the known empty-result sentinels, "done" for any other success.
func resultSummary(name, result string) string {
	if strings.HasPrefix(result, "error") {
		return "failed"
	}
	switch result {
	case "no memory matches", "no memory of that subject", "no episodes in that window", "no recent episodes":
		return "0 hits"
	case "saved":
		return "saved"
	case "updated":
		return "updated"
	case "deleted":
		return "deleted"
	}
	if name == "query_memory" || name == "recall" || name == "get_recent" {
		return fmt.Sprintf("%d hits", strings.Count(result, "\n")+1)
	}
	return "done"
}

// filterDescription names the post-filters query_memory applied, in the words the model can repeat back: "in slack since Aug 28 00:00". Returns "" when no filter was set, so the caller can fall back to the plain no-matches answer.
func filterDescription(app string, since, until time.Time) string {
	var parts []string
	if strings.TrimSpace(app) != "" {
		parts = append(parts, "in "+strings.TrimSpace(app))
	}
	if !since.IsZero() {
		parts = append(parts, "since "+since.In(time.Local).Format("Jan 2 15:04"))
	}
	if !until.IsZero() {
		parts = append(parts, "until "+until.In(time.Local).Format("Jan 2 15:04"))
	}
	return strings.Join(parts, " ")
}

// filterHitsByApp keeps episode hits whose App contains filter (case-insensitive) and drops other sources. A missing App on an episode hit is dropped rather than guessed.
func filterHitsByApp(hits []db.MemoryHit, app string) []db.MemoryHit {
	app = strings.ToLower(strings.TrimSpace(app))
	if app == "" {
		return hits
	}
	out := hits[:0:0]
	for _, h := range hits {
		if h.Source != "episode" {
			continue
		}
		if strings.Contains(strings.ToLower(h.App), app) {
			out = append(out, h)
		}
	}
	return out
}
