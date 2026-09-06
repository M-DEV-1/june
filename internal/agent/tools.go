package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"ora/internal/act"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/memory"
	oratext "ora/internal/text"
	"ora/internal/tracker"
	"ora/internal/window"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"google.golang.org/genai"
)

// maxToolRows caps how many rows any one tool result may carry. A tool result is prompt text, and action_items reads from a store that grows without bound — open action items never expire by design. query_memory has queryMemoryHits for the same reason.
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
			// The declarations from here to open_url ride on every round of every screen task (see screenRoundTools in ask.go), so each states its rule once and stops; the guidance a screen round also carries is in screenTaskGuidance, and TestScreenRoundDeclarations_StayShort holds the whole set to its byte budget.
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "observe_screen",
				Description: "List the window in front: its app and title, then every clickable or readable element showing, numbered, with its position. Call it before point_at and whenever asked what is on screen.",
				Parameters:  &genai.Schema{Type: genai.TypeObject},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "look",
				Description: "See the screen as a picture, for what observe_screen cannot list: video, photos, games, drawings, maps, charts. The result gives the picture's size and place; read points off it in its own coordinates. Two looks per turn.",
				Parameters:  &genai.Schema{Type: genai.TypeObject},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "point_at",
				Description: "Ring one element from the latest observe_screen list so the user can see which you mean.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"n":     {Type: genai.TypeNumber, Description: "Element number"},
						"label": {Type: genai.TypeString, Description: "Two or three words beside the ring"},
					},
					Required: []string{"n"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "show_marks",
				Description: "Draw a numbered mark over every element in the latest observe_screen list.",
				Parameters:  &genai.Schema{Type: genai.TypeObject},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "draw",
				Description: "Mark up the screen, all shapes in one call. arrow/line take from/to or points, path takes points, box/circle take on or rect. Nothing is clicked.",
				Parameters:  drawParameters(),
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "click",
				Description: "Press the numbered element from the latest observe_screen list through its own accessibility action, so no pointer moves. Call observe_screen after every action and read what changed. Never click anything that sends, pays, deletes or submits unless the user said go.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"n": {Type: genai.TypeNumber, Description: "Element number"},
					},
					Required: []string{"n"},
				},
			},
			{
				// Kept deliberately short: this declaration rides on every round of every screen task, and the four kinds fit in one line of the kind argument rather than needing an enum as well.
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "wait_for",
				Description: "Wait for the change an action was expected to make, instead of looking again. Polls to 5s and says what it found.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"kind":       {Type: genai.TypeString, Description: "title_contains, item_present, item_absent or field_holds"},
						"value":      {Type: genai.TypeString, Description: "the text to look for"},
						"timeout_ms": {Type: genai.TypeNumber, Description: "milliseconds, 5000 by default"},
					},
					Required: []string{"kind", "value"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "scroll_to",
				Description: "Scroll the numbered element from the latest observe_screen list into view.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"n": {Type: genai.TypeNumber, Description: "Element number"},
					},
					Required: []string{"n"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "type_text",
				Description: "Type into whatever has focus; click the field first. All text goes through here, and never a password, card number or other secret.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"text":  {Type: genai.TypeString, Description: "The text to type"},
						"enter": {Type: genai.TypeBoolean, Description: "Press Enter after the text"},
					},
					Required: []string{"text"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "press_key",
				Description: "Press a key or chord, for what no listed element can do; type_text is for text, press_key for keys. It lands where the keyboard focus is, so click the field or player first.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"keys": {Type: genai.TypeString, Description: "Enter, Escape, Tab, Space, Up, Down, Left, Right, Backspace, Delete, or a chord like Ctrl+L"},
					},
					Required: []string{"keys"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "click_at",
				Description: "Click a point with the pointer, for what the list has no element or no working action for. Coordinates come only off the picture the last look delivered; without one it is refused. Prefer click when the thing is listed.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"x": {Type: genai.TypeNumber, Description: "x in the look's picture"},
						"y": {Type: genai.TypeNumber, Description: "y in the look's picture"},
					},
					Required: []string{"x", "y"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "scroll_at",
				Description: "Scroll at a point, for a pane or player with no element in the list.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"x":  {Type: genai.TypeNumber, Description: "x in the look's picture"},
						"y":  {Type: genai.TypeNumber, Description: "y in the look's picture"},
						"dy": {Type: genai.TypeNumber, Description: "steps to scroll, positive is down"},
					},
					Required: []string{"x", "y", "dy"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "switch_window",
				Description: "Bring another application's window to the front and read back which came forward. Only when the request names that application; the task otherwise stays in the window it started in.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"app": {Type: genai.TypeString, Description: "The application to bring to the front"},
					},
					Required: []string{"app"},
				},
			},
			{
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "open_url",
				Description: "Open a URL in the default browser, only to reach a page not open yet, never a site's root over one of its own pages. It opens the page and reaches nothing on it; the screen tools do that.",
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
					"It searches Ora's record of what the user has already done: it is not a web search and it never reaches or opens anything, so it can no more find a page than remember one nobody visited. For anything on the screen now, or anywhere to get to, use observe_screen and the screen tools. " +
					"Moments (screen observations) rank with recency; facts/notes do not expire. " +
					"Use app to restrict to one application (Slack, Firefox, Code). " +
					"Whenever the question is anchored to a time — a day, a part of a day, a range — pass since/until: " +
					"the search then runs and ranks entirely inside that window, whereas without it the best matches can all come from the wrong day, and one busy stretch can drown out the rest of its own day. " +
					"A part of a day gets timestamp bounds, not the whole day: morning is roughly 06:00-12:00, afternoon 12:00-18:00, evening and night after that. " +
					"When a question narrows the time, run a fresh narrower query — do not answer a narrow question from a wider fetch you already have. " +
					"For pure day/timeline questions, or 'what was I just doing', use recall. " +
					"For what a meeting was about, what it decided, or who was in it, pass kind='meeting': that lists the minutes themselves for the window, newest first, instead of ranking — a ranked search finds screens of the user reading minutes before it finds the minutes.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"kind":   {Type: genai.TypeString, Description: "Optional. 'meeting' lists meeting minutes in the window (newest first, query ignored). Omit for a ranked search."},
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
				Name:     "query_store",
				Description: "Run one read-only SQL query straight against Ora's sqlite store, for structural and aggregate questions that a relevance-ranked search cannot answer — counts, group-bys, joins, \"which meetings did I attend today\", \"what hour do I usually stop working\". " +
					"query_memory searches by meaning and ranks by relevance; this reads the tables directly, so use it whenever the real answer is a COUNT, a GROUP BY, a MIN/MAX, or a join across tables rather than the ten most-relevant rows. " +
					"The connection itself is read-only — INSERT/UPDATE/DELETE/DROP/ALTER/PRAGMA-writes fail at the database, not by a filter on your text — so only SELECT, PRAGMA table_info(...), and EXPLAIN can do anything. " +
					"Exactly one statement per call, no trailing statements after a semicolon. " +
					"Every *_at/*_time column is UTC text — for anything the user would call \"today\" or an hour of day, wrap it: datetime(created_at,'localtime') BETWEEN ... " +
					"Results render as a header line of column names, then one line per row with values separated by a TAB — window titles routinely contain pipes and spaces, so a tab is the only separator that stays unambiguous. A query matching nothing says \"no rows matched\" plainly. Output is capped in rows and characters — add LIMIT or aggregate rather than pulling raw rows if you hit the cap. " +
					"Meetings are NOT episodes: an episode is a screen capture, so searching episodes for an app called Teams or Zoom finds the window and never the meeting. A meeting's minutes are a note with kind='meeting'. " +
					"An action note carries its state as a [state/priority] prefix at the start of content, so the things still owed are kind='action' AND content LIKE '[open/%'. " +
					"episodes_fts and memory_fts are full-text indexes: use \"episodes_fts MATCH 'word'\" and join its rowid to episodes.id, or \"memory_fts MATCH 'word'\" where source names the table ref_id points into. " +
					"Schema, read from the store itself. A column listed as \"is one of\" holds only those values:\n" + storeSchemaBlock(),
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"query": {Type: genai.TypeString, Description: "One read-only SQL statement, no trailing statements."},
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
				Behavior: genai.BehaviorNonBlocking,
				Name:     "branch",
				Description: "Resolve one open-ended question or research task that needs cross-referencing " +
					"several searches to build a complete answer (e.g. \"catch me up on everything about the " +
					"Riddler project\", or a question spanning multiple topics/timeframes) — instead of calling " +
					"query_memory/recall repeatedly yourself. THIS IS ALSO THE ONLY WAY TO REACH THE WEB: it is the one tool with live search, " +
					"so anything outside the user's own life — news, prices, documentation, a fact you are not certain of — goes here. " +
					"Never open a browser to answer a question; opening a page shows it to the user and tells you nothing. " +
					"Runs an internal multi-step search in the " +
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
				Behavior:    genai.BehaviorNonBlocking,
				Name:        "save_note",
				Description: "Save a durable fact the user tells you in conversation — identity, preferences, plans, relationships, projects. This is the only way something said in conversation reaches long-term memory. Not for task chatter.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"content": {Type: genai.TypeString, Description: "The fact to remember, as a durable statement, not a command."},
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
						"subject": {Type: genai.TypeString, Description: "Short key for the entry, lowercase and hyphenated: \"identity\", \"priya-shah\", \"preferences-communication\". Required for set and delete. Reuse an existing subject to edit it."},
						"content": {Type: genai.TypeString, Description: "For set: the whole entry, written as plain prose about the user or that person. It replaces the subject's previous content, so include what still holds, not just the new part."},
					},
					Required: []string{"action"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "revise",
				Description: "Fix or remove something Ora remembered wrong — a note, an action item, or a thread. " +
					"Look it up first with query_memory or recall to get its ref, the \"[note#N]\" or \"[thread#N]\" a result showed you, then call this. " +
					"Pass content to correct the text. For an action item, pass state (open, done, or dropped) instead — never leave a task the user says is done still open — and priority (high, normal, or low) when they say how much it matters. " +
					"Pass remove to delete a note entirely (a thread cannot be removed, only corrected). " +
					"Use this instead of just apologizing out loud and leaving the wrong fact in memory.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"ref":      {Type: genai.TypeString, Description: "The reference exactly as a result showed it: \"note#12\" or \"thread#3\", brackets optional."},
						"content":  {Type: genai.TypeString, Description: "Optional. The corrected text. Omit to leave it unchanged."},
						"state":    {Type: genai.TypeString, Description: "Optional. For an action item only: open, done, or dropped."},
						"priority": {Type: genai.TypeString, Description: "Optional. For an action item only: high, normal, or low. Everything starts normal, so set this only when the user says how much something matters."},
						"remove":   {Type: genai.TypeBoolean, Description: "Optional. Delete the row entirely — cannot be combined with content, state, or priority."},
					},
					Required: []string{"ref"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "action_items",
				Description: "List what the user still owes — the things they agreed to do in a meeting and have not " +
					"closed. Use this for any question about outstanding work, owed tasks, commitments, what is on " +
					"their plate, or what they need to do. Do not use query_memory for those: an action item's text " +
					"is the task itself and shares no words with the question, so searching for it finds meetings " +
					"about meetings instead. This reads the list directly. Each result carries its id, so revise " +
					"can close one straight afterwards.",
				Parameters: &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{}},
			},
			// The delegate tool is declared in delegate.go beside its handler; screen rounds trim to screenRoundTools, so it costs them nothing.
			delegateTool,
		},
	}}
}

// liveTools returns every tool exposed to the Live API session: ORA's own FunctionDeclarations (shell_exec, query_memory, save_note, etc.) plus Gemini's native GoogleSearch grounding tool, so Ora can look something up instead of guessing from memory.
// Verified live (2026-07-25) that both tool types work together on config.VoiceModel (gemini-2.5-flash-native-audio-preview-12-2025) — not guaranteed on every Gemini model/endpoint.
// GoogleSearch calls are grounded server-side by Gemini and never surface as a ToolCall, so they don't show up in the TUI's live tool status line the way the FunctionDeclarations tools do.
func liveTools() []*genai.Tool {
	return liveToolsFor(config.VoiceModel)
}

// liveToolsFor is liveTools for a named Live model. Google Search grounding rides beside the function tools on the 2.5 model; on the 3.x Live models the same pairing closes the session with "You exceeded your current quota" before the first word (probed on 2026-09-02, every other part of the handshake passes), so there it is left out and the model has no web search.
func liveToolsFor(model string) []*genai.Tool {
	tools := toolDefinitions()
	if strings.HasPrefix(model, "gemini-3") {
		return tools
	}
	return append(tools, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
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
	return oratext.Runes(oratext.OneLine(s), recallExcerpt)
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

// parseRef splits a "[note#12]" or "note#12" style reference — the shape query_memory, recall and action_items hand back — into its kind and numeric id, so revise can dispatch on it without the model having to know which table backs a result.
// Input: the ref as a result showed it, brackets optional. Output: the kind ("note" or "thread") and the id, or an error naming the shape a ref must have.
func parseRef(ref string) (string, int64, error) {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "[")
	ref = strings.TrimSuffix(ref, "]")
	kind, idStr, ok := strings.Cut(ref, "#")
	if !ok {
		return "", 0, fmt.Errorf(`ref must look like "note#12" or "thread#3"`)
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf(`ref must look like "note#12" or "thread#3"`)
	}
	return kind, id, nil
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

// brainRequiredTools are the tool names whose handler reads or writes through a.brain, the memory store. Widening the set of tools the ask gate can reach (askAllowedTools) opened a path where an agent built without a brain — a nil ContextReader, the shape both a not-yet-connected daemon and a test can produce — hit a.brain.SomeMethod(...) and panicked on the nil interface's own method dispatch. Checked once, up front, so every one of them fails the same way every other executeTool error path already does: a plain "error: ..." string, never a panic.
var brainRequiredTools = map[string]bool{
	"query_memory": true, "query_store": true, "recall": true,
	"save_note": true, "personal_context": true, "revise": true, "action_items": true,
}

// executeTool runs a tool and returns the result as a string
// maybe this can be seperated into /agent/tools altogether later and be compiled with OS specific code?
func (a *Agent) executeTool(ctx context.Context, name string, args map[string]any) string {
	if a.brain == nil && brainRequiredTools[name] {
		return toolError(storeUnavailable)
	}
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

	case "observe_screen":
		app, title, nodes, err := a.observe(ctx)
		if err != nil {
			slog.Warn("observe_screen failed", "error", err)
			return toolError("could not look at the screen: " + err.Error())
		}
		items := act.Filter(nodes)
		// Read before the new snapshot is stored, since the answer is this look against the one before it.
		previous := a.lastScreen(ctx)
		if len(items) == 0 {
			a.rememberScreen(ctx, items, screenSnapshot{app: app, title: title})
			return app + " · " + title + "\n(nothing actionable is showing)"
		}
		lines := strings.Split(act.Format(items), "\n")
		a.rememberScreen(ctx, items, screenSnapshot{app: app, title: title, lines: lines})
		return observeResult(app, title, lines, previous)

	case "look":
		if a.capture == nil {
			return toolError("this session cannot see the screen")
		}
		if !looksLeft(ctx) {
			return toolError(fmt.Sprintf("I have already looked at the screen %d times this turn; answer from what those pictures showed", maxLooksPerAsk))
		}
		shot, err := a.capture(ctx)
		if err != nil {
			slog.Warn("look failed", "error", err)
			return toolError("could not take a picture of the screen: " + err.Error())
		}
		recordLook(ctx, shot)
		return fmt.Sprintf("here is the picture, %d wide and %d high. Its top-left corner is %d,%d on the screen and one of its pixels is %.2f screen pixels, so point at things in it with its own coordinates and draw will put them back on the screen for you.", shot.W, shot.H, shot.X, shot.Y, shot.Scale)

	case "point_at":
		it, errText := a.seenItem(ctx, args)
		if errText != "" {
			return errText
		}
		if a.Point == nil {
			return toolError("this session cannot draw on the screen")
		}
		if errText := a.stillThere(ctx, it); errText != "" {
			return errText
		}
		x, y, w, h, errText := a.freshRect(ctx, it)
		if errText != "" {
			return errText
		}
		label, _ := args["label"].(string)
		remembered, hadRemembered := a.screenTarget()
		a.Point(x, y, w, h, label)
		a.rememberTarget(ScreenTarget{Label: it.Label, Role: it.Role, Window: a.currentWindow(ctx), HasRect: true, X: x, Y: y, W: w, H: h})
		note := ""
		if hadRemembered {
			note = targetMismatchNote(questionFrom(ctx), &remembered, it.Label)
		}
		return fmt.Sprintf("ringed [%d] %s %q", it.N, it.Role, it.Label) + note

	case "show_marks":
		items := a.seen(ctx)
		if items == nil {
			return toolError("call observe_screen first, then show_marks")
		}
		if a.Marks == nil {
			return toolError("this session cannot draw on the screen")
		}
		total := len(items)
		if total > 40 {
			items = items[:40]
		}
		// The marks are drawn where each element is now, not where the listing said it was: after a scroll the numbers would otherwise sit over whatever moved into those rectangles, and the numbers are what the model then clicks by. An element that is no longer showing gets no mark, the same rule point_at follows.
		fresh := make([]act.Item, 0, len(items))
		gone := 0
		for _, it := range items {
			x, y, w, h, errText := a.freshRect(ctx, it)
			if errText != "" {
				gone++
				continue
			}
			it.X, it.Y, it.W, it.H = x, y, w, h
			fresh = append(fresh, it)
		}
		a.Marks(fresh)
		switch {
		case gone > 0:
			return fmt.Sprintf("marked %d of %d elements on the screen; %d are no longer showing", len(fresh), total, gone)
		case len(fresh) < total:
			return fmt.Sprintf("marked %d of %d elements on the screen", len(fresh), total)
		}
		return fmt.Sprintf("marked %d element(s) on the screen", len(fresh))

	case "draw":
		if a.Draw == nil {
			return toolError("this session cannot draw on the screen")
		}
		shapes, errText := drawShapeList(args)
		remembered, hadRemembered := a.screenTarget()
		var drawn, refused []string
		var last *ScreenTarget
		// Shapes the stream already resolved — drawn or refused — are reported but never acted on again, and they are always the leading ones, because the stream hands them over in the order the call lists them; a refused one still holds its place so the shapes after it are not shifted onto the wrong entries.
		if early := streamDrawnFrom(ctx); len(early) > 0 {
			for _, d := range early {
				if d.Err != "" {
					refused = append(refused, fmt.Sprintf("shape %d — %s", len(drawn)+len(refused)+1, strings.TrimPrefix(d.Err, "error: ")))
					continue
				}
				drawn = append(drawn, d.Phrase)
				if d.Target != nil {
					last = d.Target
				}
			}
			if len(early) >= len(shapes) {
				shapes = nil
			} else {
				shapes = shapes[len(early):]
			}
		}
		// Read after the early block rather than returned bare, so shapes the stream already drew are still reported even when the finished call as a whole is over the cap or carries a bad entry.
		if errText != "" {
			refused = append(refused, strings.TrimPrefix(errText, "error: "))
		}
		for _, shape := range shapes {
			phrase, target, errText := a.drawOne(ctx, shape)
			if errText != "" {
				// One bad shape does not lose the rest of the drawing: the others are drawn and the model is told which entry failed, so it can send that one again rather than the whole diagram.
				refused = append(refused, fmt.Sprintf("shape %d — %s", len(drawn)+len(refused)+1, strings.TrimPrefix(errText, "error: ")))
				continue
			}
			drawn = append(drawn, phrase)
			if target != nil {
				last = target
			}
		}
		// Only the last shape drawn around a numbered element is remembered, so a later bare "draw a circle around it" resolves to the last thing this call drew around rather than to whichever entry happened to come first.
		note := ""
		if last != nil {
			a.rememberTarget(*last)
			if hadRemembered {
				note = targetMismatchNote(questionFrom(ctx), &remembered, last.Label)
			}
		}
		switch {
		case len(drawn) == 0:
			return toolError(strings.Join(refused, "; "))
		case len(drawn) == 1 && len(refused) == 0:
			return "drew " + drawn[0] + note
		default:
			result := fmt.Sprintf("drew %d shapes: %s", len(drawn), strings.Join(drawn, "; ")) + note
			if len(refused) > 0 {
				result += "\nnot drawn — " + strings.Join(refused, "; ")
			}
			return result
		}

	case "click":
		it, errText := a.seenItem(ctx, args)
		if errText != "" {
			return errText
		}
		if errText := a.frontWindowChanged(ctx); errText != "" {
			return errText
		}
		if errText := a.stillThere(ctx, it); errText != "" {
			return errText
		}
		window := a.currentWindow(ctx)
		if irreversible(it, window, false) && !consented(questionFrom(ctx), matchedVerb(it, window)) && !goAllowed(ctx) {
			return a.stopBeforeClick(ctx, it, window)
		}
		action, err := a.doAction(ctx, it.Ref)
		if err != nil {
			return toolError(fmt.Sprintf("could not click [%d] %s %q: %v", it.N, it.Role, it.Label, err))
		}
		a.rememberClick(ctx, it)
		a.rememberTarget(ScreenTarget{Label: it.Label, Role: it.Role, Window: window})
		clicked := fmt.Sprintf("clicked [%d] %s %q via %s", it.N, it.Role, it.Label, action)
		// Read the front window's title fresh, the same call observe_screen opens with, so the result says what the click actually did rather than what the stale pre-click list said. A click can resume, play or navigate to something other than what was asked, and the title is where that shows up first.
		_, title, _, err := a.observe(ctx)
		if err != nil || title == "" {
			return clicked + "; call observe_screen to see the result"
		}
		return fmt.Sprintf("%s; the window is now %q; check it matches what was asked, then call observe_screen if you need the list", clicked, title)

	case "wait_for":
		kind, _ := args["kind"].(string)
		value, _ := args["value"].(string)
		return a.waitFor(ctx, act.Check{Kind: kind, Value: value}, waitTimeout(args))

	case "scroll_to":
		it, errText := a.seenItem(ctx, args)
		if errText != "" {
			return errText
		}
		if err := a.scrollTo(ctx, it.Ref); err != nil {
			return toolError(fmt.Sprintf("could not scroll to [%d] %s %q: %v", it.N, it.Role, it.Label, err))
		}
		return fmt.Sprintf("scrolled to [%d] %s %q; call observe_screen to see the page now", it.N, it.Role, it.Label)

	case "type_text":
		text, ok := args["text"].(string)
		if !ok || text == "" {
			return toolError("type_text needs text")
		}
		enter, _ := args["enter"].(bool)
		// A newline, tab or backspace inside the text is a real Enter, Tab or BackSpace keystroke once it reaches the keyboard: the newline submits a chat box halfway through the message, and the tab moves the rest of the text into whatever field comes next. The enter argument above stays the one way to press Enter on purpose.
		if strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 }) {
			return toolError("type_text will not type control characters; use press_key for Enter, Tab or Backspace, or the enter argument to press Enter after the text")
		}
		// There is no accessibility read for which field actually has keyboard focus, so the field the last successful click acted on stands in for it — "click the field first" is the documented way to reach type_text anyway. Before any click, this is the zero act.Item, which matches neither check below.
		focused, known := a.focus(ctx)
		window := a.currentWindow(ctx)
		if !known && !blindConsent(questionFrom(ctx)) && !goAllowed(ctx) {
			return "Stopped before typing: I could not identify the field the text would go into, since the last click was at a point on the screen or a key has moved the focus since. Click the field by its number from observe_screen, or say \"yes, go ahead\" and I will type where the focus is."
		}
		if secretField(focused, window) {
			return fmt.Sprintf("Stopped before typing into %s %q in %q — I never type passwords, card numbers or other secrets, so say it yourself once the field is focused.", focused.Role, focused.Label, window)
		}
		if irreversible(focused, window, true) && !consented(questionFrom(ctx), matchedVerb(focused, window)) && !goAllowed(ctx) {
			return fmt.Sprintf("Stopped before typing into %s %q in %q. %s", focused.Role, focused.Label, window, consentPrompt(matchedVerb(focused, window)))
		}
		if enter {
			text += "\n"
		}
		// The text goes in through the portal keyboard, the same session press_key uses, into whatever has focus: the field the checks above looked at. There is no toolkit path to set a field's text on Wayland, so this is the one way in, opened on first use.
		dev, errText := a.inputDevice(ctx)
		if errText != "" {
			return errText
		}
		if err := dev.TypeText(text); err != nil {
			return toolError("could not type: " + err.Error())
		}
		return fmt.Sprintf("typed %d characters; call observe_screen to see the result", len([]rune(text)))

	case "press_key":
		keys, _ := args["keys"].(string)
		if keys == "" {
			return toolError("press_key needs keys, like \"Enter\" or \"Ctrl+L\"")
		}
		if pressesFocused(keys) {
			// Enter and Space press whatever has keyboard focus, which is a click by another name, so they go through the same stop line the click tool does — against the field or button the last click focused, since that is this session's only reading of where the keyboard is pointing.
			focused, known := a.focus(ctx)
			window := a.currentWindow(ctx)
			if !known && !blindConsent(questionFrom(ctx)) && !goAllowed(ctx) {
				return fmt.Sprintf("Stopped before pressing %s: I could not identify what has keyboard focus, since the last click was at a point on the screen or a key has moved the focus since. Click the control by its number from observe_screen, or say %q and I will press it.", keys, "yes, go ahead")
			}
			if irreversible(focused, window, false) && !consented(questionFrom(ctx), matchedVerb(focused, window)) && !goAllowed(ctx) {
				return fmt.Sprintf("Stopped before pressing %s on %s %q in %q. %s", keys, focused.Role, focused.Label, window, consentPrompt(matchedVerb(focused, window)))
			}
		}
		dev, errText := a.inputDevice(ctx)
		if errText != "" {
			return errText
		}
		if err := dev.PressKey(keys); err != nil {
			return toolError(fmt.Sprintf("could not press %s: %v", keys, err))
		}
		// Tab, Shift+Tab and the arrows move the keyboard off whatever the last click focused, and nothing here can say where to, so the next Enter or Space has to be refused rather than checked against a control this session can no longer vouch for.
		if movesFocus(keys) {
			a.focusLost(ctx)
		}
		return fmt.Sprintf("pressed %s; call observe_screen to see what it did", keys)

	case "click_at":
		x, y, errText := a.picturePoint(ctx, args)
		if errText != "" {
			return errText
		}
		window := a.frontWindow(ctx)
		// A point does have a name when the last observe_screen listed something covering it, and then the click is checked exactly as a numbered click on that item would be. Only a point no listed rectangle covers falls back to the window's own title.
		lands := ""
		if it, ok := itemAt(a.seen(ctx), x, y); ok {
			lands = fmt.Sprintf("; the point lands on [%d] %s %q", it.N, it.Role, it.Label)
			if secretField(it, window) {
				return fmt.Sprintf("Stopped before clicking %d,%d — the point lands on %s %q in %q, which holds a password or another secret. Click it yourself if you want it focused.", x, y, it.Role, it.Label, window)
			}
			if irreversible(it, window, false) && !consented(questionFrom(ctx), matchedVerb(it, window)) && !goAllowed(ctx) {
				return fmt.Sprintf("Stopped before clicking %d,%d — the point lands on [%d] %s %q in %q. %s", x, y, it.N, it.Role, it.Label, window, consentPrompt(matchedVerb(it, window)))
			}
		} else if verb := matchedVerb(act.Item{}, window); verb != "" && !consented(questionFrom(ctx), verb) && !goAllowed(ctx) {
			return fmt.Sprintf("Stopped before clicking %d,%d in %q. A point on the screen carries no label, so the window's own title is all I have to go on, and it names a %s. %s", x, y, window, verb, consentPrompt(verb))
		}
		dev, errText := a.inputDevice(ctx)
		if errText != "" {
			return errText
		}
		if err := dev.ClickAt(float64(x), float64(y)); err != nil {
			return toolError(fmt.Sprintf("could not click %d,%d: %v", x, y, err))
		}
		// The pointer has moved the keyboard somewhere this session cannot name, whatever was under the point, so type_text and a focused key press refuse until a fresh observe_screen or a numbered click says where the keyboard is again.
		a.focusLost(ctx)
		return fmt.Sprintf("clicked %d,%d on the screen%s; look or call observe_screen to see what it did", x, y, lands)

	case "scroll_at":
		x, y, errText := a.picturePoint(ctx, args)
		if errText != "" {
			return errText
		}
		dy, ok := args["dy"].(float64)
		if !ok || dy == 0 {
			return toolError("scroll_at needs dy, how many steps to scroll: positive is down")
		}
		dev, errText := a.inputDevice(ctx)
		if errText != "" {
			return errText
		}
		if err := dev.ScrollAt(float64(x), float64(y), int32(dy)); err != nil {
			return toolError(fmt.Sprintf("could not scroll at %d,%d: %v", x, y, err))
		}
		return fmt.Sprintf("scrolled %d steps at %d,%d; look or call observe_screen to see the page now", int(dy), x, y)

	case "switch_window":
		app, _ := args["app"].(string)
		return a.switchWindow(ctx, strings.TrimSpace(app))

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
		if msg := checkArgs(args, "query", "domain", "app", "since", "until", "kind"); msg != "" {
			return toolError(msg)
		}
		query, ok := args["query"].(string)
		if !ok {
			return toolError("query_memory needs something to search for")
		}
		if kind, _ := args["kind"].(string); kind == "meeting" {
			since, until, _, err := optionalWindow(args, time.Now())
			if err != nil {
				return toolError(dateHint)
			}
			return a.listMeetingNotes(ctx, since, until)
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
		slog.Debug("querying long-term memory", "query", query, "domain", domain, "since", since, "until", until)

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
			// Notes are the only source revise can follow up on, so they're the only hits that carry their ref_id in the surfaced line — the model needs it in hand to act on a correction.
			// The *WithSource variants append a {"source":{...}} tag ask.go's Evidence extraction reads back out — a caller can then show which stored row an answer leaned on, instead of a paraphrase nobody can trace.
			if h.Source == "note" {
				lines = append(lines, db.FormatNoteHitWithSource(h, 0))
			} else {
				lines = append(lines, db.FormatHitWithSource(h, 0))
			}
		}
		return strings.Join(lines, "\n")

	case "query_store":
		if msg := checkArgs(args, "query"); msg != "" {
			return toolError(msg)
		}
		query, ok := args["query"].(string)
		if !ok || strings.TrimSpace(query) == "" {
			return toolError("query_store needs a SQL statement to run")
		}
		slog.Info("running query_store", "query", query)
		result, err := a.brain.QueryStore(ctx, query, maxToolRows)
		if err != nil {
			// Every other tool hides its raw error behind toolError's fixed wording, because a Go error string means nothing to a model that can't fix it. Here the error IS the fix — "no such column: titel" or "only one SQL statement is allowed per call" tells the model exactly what to change and try again, so it's passed through instead of hidden.
			slog.Warn("query_store: rejected or failed", "query", query, "error", err)
			return toolError(err.Error())
		}
		return result

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

	case "revise":
		if msg := checkArgs(args, "ref", "content", "state", "priority", "remove"); msg != "" {
			return toolError(msg)
		}
		ref, ok := args["ref"].(string)
		if !ok || strings.TrimSpace(ref) == "" {
			return toolError("revise needs a ref — the \"note#N\" or \"thread#N\" a result showed you")
		}
		kind, id, err := parseRef(ref)
		if err != nil {
			return toolError(err.Error())
		}
		content, hasContent := args["content"].(string)
		hasContent = hasContent && strings.TrimSpace(content) != ""
		state, hasState := args["state"].(string)
		hasState = hasState && strings.TrimSpace(state) != ""
		priority, hasPriority := args["priority"].(string)
		hasPriority = hasPriority && strings.TrimSpace(priority) != ""
		remove, _ := args["remove"].(bool)
		if remove && (hasContent || hasState || hasPriority) {
			return toolError("revise can't remove and change something in the same call — pick one")
		}
		if !remove && !hasContent && !hasState && !hasPriority {
			return toolError("revise needs content, a state, a priority, or remove — say what changed")
		}

		switch kind {
		case "note":
			if remove {
				if err := a.brain.DeleteNote(ctx, id); err != nil {
					slog.Error("revise: delete failed", "id", id, "error", err)
					return toolError("nothing was there to delete — look it up again with query_memory and use the id it shows")
				}
				return "deleted"
			}
			// An action item's state lives as a "[state/priority]" prefix on the same notes row a plain note uses — SetActionStatus rewrites just that prefix and leaves the rest of the line alone.
			// Priority rides the same "[state/priority]" prefix as the status and is set the same way, so "make that one high priority" has somewhere to land.
			if hasPriority {
				p := strings.TrimSpace(priority)
				if !memory.ValidPriority(p) {
					return toolError("priority must be high, normal, or low")
				}
				if err := a.brain.SetActionPriority(ctx, id, p); err != nil {
					slog.Error("revise: priority write failed", "id", id, "error", err)
					return toolError("that priority didn't stick — look the item up again and use the id it shows")
				}
				if !hasState && !hasContent {
					return "updated"
				}
			}
			if hasState {
				s := strings.TrimSpace(state)
				if !memory.ValidStatus(s) {
					return toolError("state must be open, done, or dropped")
				}
				if err := a.brain.SetActionStatus(ctx, id, s); err != nil {
					slog.Error("revise: status write failed", "id", id, "error", err)
					return toolError("nothing was updated — look it up again with query_memory and use the id it shows")
				}
			}
			if hasContent {
				// An action item's content is a rendered "[state/priority] Owner — work (Meeting, date)" line, so its text is corrected through SetActionText, which re-renders the line: writing the model's prose straight over it would strip the prefix and drop the item out of every read that goes through ParseAction. An id that names an ordinary note is not an action item, and that one is written whole.
				err := a.brain.SetActionText(ctx, id, content)
				if errors.Is(err, db.ErrNotActionItem) {
					err = a.brain.UpdateNote(ctx, id, content)
				}
				if err != nil {
					slog.Error("revise: content write failed", "id", id, "error", err)
					return toolError("nothing was updated — look it up again with query_memory and use the id it shows")
				}
			}
			return "updated"
		case "thread":
			if remove {
				return toolError("a thread can't be removed — correct it with content instead")
			}
			if hasState || hasPriority {
				return toolError("a thread has no state or priority — those only apply to an action item")
			}
			if err := a.brain.UpdateThreadState(ctx, id, content); err != nil {
				slog.Error("revise: thread write failed", "id", id, "error", err)
				return toolError("nothing was fixed — look the thread up again with query_memory and use the id it shows")
			}
			return "fixed"
		default:
			return toolError(fmt.Sprintf("revise only handles note and thread refs, not %q", kind))
		}

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
			// The id leads so revise can close one without a second lookup, and the source meeting trails so the model can say where a task came from.
			fmt.Fprintf(&b, "[note#%d] %s\n", it.NoteID, it.Note())
		}
		return strings.TrimRight(b.String(), "\n")

	case "delegate":
		return delegateHandler(ctx, a, args)

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
	return fmt.Sprintf("%q", oratext.RunesEllipsis(s, toolArgSummaryRunes))
}

// toolActivitySummary pre-formats a tool call's primary argument into a short display literal for the UI (e.g. `"Riddler puzzles"` for query_memory), so the UI never needs to know each tool's arg-shape — that knowledge already lives here, next to executeTool/toolDefinitions.
// Unknown tools and no-arg tools (read_clipboard) summarize to "".
func toolActivitySummary(name string, args map[string]any) string {
	switch name {
	case "query_memory":
		if q, ok := args["query"].(string); ok {
			return quoteArg(q)
		}
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
	case "revise":
		if ref, ok := args["ref"].(string); ok {
			return quoteArg(ref)
		}
	case "personal_context":
		action, _ := args["action"].(string)
		if subject, ok := args["subject"].(string); ok && subject != "" {
			return fmt.Sprintf("%s %s", action, quoteArg(subject))
		}
		return quoteArg(action)
	case "branch":
		if task, ok := args["task"].(string); ok {
			return quoteArg(task)
		}
	case "query_store":
		if query, ok := args["query"].(string); ok {
			return quoteArg(query)
		}
	case "click", "scroll_to", "point_at":
		if n, ok := args["n"].(float64); ok {
			return fmt.Sprintf("element %d", int(n))
		}
	case "type_text":
		if text, ok := args["text"].(string); ok {
			return quoteArg(text)
		}
	}
	return ""
}

// resultSummary condenses a tool's raw result string into a short status word for the UI's transcript log line and for a "tool" event's Detail — "N hits" for the search-shaped tools, "failed" on any error result (executeTool always prefixes errors with "error"), "0 hits" for the known empty-result sentinels, the window line for observe_screen and the window a click landed on (never the accessibility listing itself, only that one line — see toolLogDetail for the separate rule that keeps screen content out of the server's own log file), "done" for any other success.
func resultSummary(name, result string) string {
	if strings.HasPrefix(result, "error") {
		return "failed"
	}
	if strings.HasPrefix(result, "Stopped before ") {
		return "stopped"
	}
	switch result {
	case "no memory matches", "no memory of that subject", "no episodes in that window", "no rows matched":
		return "0 hits"
	case "saved":
		return "saved"
	case "updated":
		return "updated"
	case "deleted":
		return "deleted"
	case "fixed":
		return "fixed"
	}
	if name == "query_memory" || name == "recall" {
		return fmt.Sprintf("%d hits", strings.Count(result, "\n")+1)
	}
	if name == "observe_screen" {
		line, _, _ := strings.Cut(result, "\n")
		return line
	}
	if name == "click" {
		if _, rest, ok := strings.Cut(result, `the window is now "`); ok {
			if title, _, ok := strings.Cut(rest, `"`); ok {
				return fmt.Sprintf("window now %q", title)
			}
		}
	}
	if name == "draw" {
		if label, ok := drawnItemLabel(result); ok {
			return fmt.Sprintf("drew around %q", label)
		}
	}
	return "done"
}

// drawnItemLabel reads the item name off a draw result that resolved a numbered item — "drew a circle around [3] push button \"Reload\"" (see the draw case's "box", "circle" branch) — so resultSummary can surface what was actually drawn around instead of the generic "done" a caller reading only the summary would otherwise see; a live "tool" event and a screen eval both read the summary, never the full result text. Output: the label and true, or "" and false for a draw that named no item — an arrow, a line, a path, or a box/circle drawn from a raw rectangle rather than "on" a numbered one.
func drawnItemLabel(result string) (string, bool) {
	_, rest, ok := strings.Cut(result, "] ")
	if !ok {
		return "", false
	}
	_, rest, ok = strings.Cut(rest, `"`)
	if !ok {
		return "", false
	}
	label, _, ok := strings.Cut(rest, `"`)
	return label, ok
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

// listMeetingNotes renders every meeting-minutes note whose creation time falls in [since, until] (a zero bound is open), newest first, each as "[note#ID] date — excerpt". It reads the notes table directly rather than ranking, because minutes are the answer to "what was the meeting about" and no query word reliably ranks them above the screens of the user reading them.
// maxMeetingNotesListed caps how many meetings query_memory kind=meeting lists in one answer; the rest are counted, and a narrower window or a real query reaches them.
const maxMeetingNotesListed = 30

func (a *Agent) listMeetingNotes(ctx context.Context, since, until time.Time) string {
	notes, err := a.brain.NotesOfKindSince(ctx, "meeting", since)
	if err != nil {
		slog.Error("query_memory: reading meeting notes failed", "error", err)
		return toolError(storeUnavailable)
	}
	var lines []string
	left := 0
	for _, n := range notes {
		if !until.IsZero() && n.CreatedAt.After(until) {
			continue
		}
		if len(lines) == maxMeetingNotesListed {
			left++
			continue
		}
		lines = append(lines, fmt.Sprintf("[note#%d] %s — %s", n.ID, n.CreatedAt.Format("Mon Jan 2 15:04"), db.FormatNoteHitWithSource(db.MemoryHit{Source: "note", RefID: n.ID, Content: n.Content, CreatedAt: n.CreatedAt}, 0)))
	}
	if len(lines) == 0 {
		if desc := filterDescription("", since, until); desc != "" {
			return "no meeting minutes " + desc
		}
		return "no meeting minutes saved yet"
	}
	if left > 0 {
		lines = append(lines, fmt.Sprintf("and %d more, older; narrow the window or ask about one", left))
	}
	return strings.Join(lines, "\n")
}

// screenSnapshot is one observe_screen answer kept for the next one to be compared against: the window it was of and the numbered lines it listed. It is only ever used to shorten what the model is told; the list the numbers resolve against is the ask's own seen items, which a fresh walk rewrites on every look.
type screenSnapshot struct {
	app, title string
	lines      []string
}

// ScreenTarget is the screen item a point_at, click or draw(on) call last acted on or pointed at: its label and role off the observe_screen listing that named it, the window it was in, and its rectangle in screen pixels when the hop had one. It is kept on the agent across every ask this session answers, so a bare "it" or "that" in a later ask can be resolved against what was actually done rather than a fresh screen read.
type ScreenTarget struct {
	Label, Role, Window string
	// HasRect is false for a target recorded from click, whose result text (frozen by an existing test) carries no rectangle.
	HasRect    bool
	X, Y, W, H int
}

// screenTarget returns the ScreenTarget this session last pointed at or acted on (see rememberTarget). Output: the target and true, or the zero value and false when nothing has been pointed at or acted on yet this session.
func (a *Agent) screenTarget() (ScreenTarget, bool) {
	t, ok := a.lastTarget.Load().(ScreenTarget)
	return t, ok
}

// rememberTarget stores t as the newest screen target, for a later ask's bare "it" to resolve against. Input: the target just acted on or pointed at. Output: none.
func (a *Agent) rememberTarget(t ScreenTarget) {
	a.lastTarget.Store(t)
}

// bareReferenceWords are the endings a follow-up on the same screen uses to mean "the thing I just pointed at or acted on" rather than naming something of its own: the question ends, trailing punctuation aside, in one of these — "ring it", "draw a circle around it", "do that again" — never "click the address bar", which already names its own noun after the pronoun or instead of one.
var bareReferenceWords = []string{"it", "that", "this", "again", "the same one"}

// isBareReference reports whether question is a follow-up naming nothing of its own to act on: it ends, once trailing punctuation is stripped, in one of bareReferenceWords. Deliberately simple — a question that names a noun after its pronoun ("this button") fails the check and gets no reminder, which only ever costs a helpful sentence, never a wrong one.
func isBareReference(question string) bool {
	q := strings.ToLower(strings.TrimRight(strings.TrimSpace(question), ".?! "))
	for _, w := range bareReferenceWords {
		if q == w || strings.HasSuffix(q, " "+w) {
			return true
		}
	}
	return false
}

// targetMismatchNote is the one line a point_at or draw(on) result adds when a bare follow-up ("ring it") resolved to a different item than the one remembered from before, so the model can correct itself within the same ask rather than the wrong item standing as what was drawn or pointed at. Input: the question, the target remembered before this call (nil when there was none) and the label the number just resolved to. Output: the note, with its own leading newline so a caller can concatenate it straight onto its result, or "" when there is nothing to flag — the question named its own target, there was nothing remembered yet, or the labels already agree.
func targetMismatchNote(question string, remembered *ScreenTarget, gotLabel string) string {
	if remembered == nil || !isBareReference(question) || remembered.Label == gotLabel {
		return ""
	}
	return fmt.Sprintf("\n(you were asked about %q, this is %q)", remembered.Label, gotLabel)
}

// unchangedScreenMarker opens the answer a repeat look gets. It is matched, not just printed: sameScreenAgain in ask.go reads it to know that a look which returned this text saw the screen it had already seen, so a round spent on it does not spend a step.
const unchangedScreenMarker = "(unchanged since the last look"

// maxChangedLines is how many lines may differ before a look is sent as a whole list instead of as the lines that changed. Twelve, because past that the page has moved rather than ticked over, and a model piecing a page together out of a dozen scattered corrections is worse off than one reading the page.
const maxChangedLines = 12

// observeResult renders one look at the screen against the look before it, so a page that has not moved is not paid for twice. Input: the window's app and title, the numbered lines this look produced, and the snapshot of the look before (zero value when there was none). Output: the whole numbered list for a first look, a different window, a list of a different length, or a page that changed too much to state line by line; a one-line "unchanged" answer when nothing moved; otherwise the window line and only the lines that changed.
// The numbers are the same numbers either way: the shorter answers are only ever sent when the list is the same length in the same order, so a number the model already has still points at the same row.
func observeResult(app, title string, lines []string, previous screenSnapshot) string {
	window := app + " · " + title
	full := window + "\n" + strings.Join(lines, "\n")
	if previous.app != app || previous.title != title || len(previous.lines) != len(lines) {
		return full
	}
	var changed []string
	for i, line := range lines {
		if line != previous.lines[i] {
			changed = append(changed, line)
		}
	}
	switch {
	case len(changed) == 0:
		return fmt.Sprintf("%s\n%s — the same %d items, and their numbers still stand)", window, unchangedScreenMarker, len(lines))
	case len(changed) <= maxChangedLines:
		return fmt.Sprintf("%s\n(the same %d items as the last look, with these changed:)\n%s", window, len(lines), strings.Join(changed, "\n"))
	}
	return full
}

// maxLooksPerAsk is how many pictures of the screen one ask may send. A picture is by far the most expensive thing a turn can carry — roughly 1,200 tokens for a 1280-wide screenful, against about 1,300 for a whole observe_screen listing — and two is what the question this was built for needs: one look at what is playing, and one more after something has moved.
const maxLooksPerAsk = 2

// lookTokenCost estimates what an image of this size costs the model to read. Input: the picture's width and height in pixels. Output: the token count, at one token per 750 pixels, which is what Anthropic documents and close to what the other two charge. No provider reports its input broken down by part, so this is an estimate on purpose — it is here so a turn that sent two screenfuls does not look, in the ledger, exactly like one that sent none.
func lookTokenCost(w, h int) int {
	return w * h / 750
}

// askLookState is what one ask's looks and screen tools leave behind: the newest picture, taken so draw can map coordinates the model reads off it back onto the screen; whether that picture has been handed to the model yet; how many pictures the ask has taken and what they are estimated to have cost; the list the last observe_screen produced and the answer it gave; and which control the last click focused. It is carried on the ask's own context (see withAskLookState), not on the shared Agent, so two asks running at once — a routine and a typed question — never share or clobber one screenshot, one numbered list, or one stop-line check.
type askLookState struct {
	mu sync.Mutex
	// look is the newest picture this ask took.
	look *tracker.Capture
	// lookUndelivered is true between a look being taken and the picture being handed to the model, so each picture is sent exactly once, with the tool result that produced it.
	lookUndelivered bool
	// looks is how many pictures this ask has taken (see maxLooksPerAsk) and lookTokens what they are estimated to have cost.
	looks      int
	lookTokens int
	// items is the []act.Item the last observe_screen listed, which a number from the model resolves against, and snap is the answer that look produced, so a repeat look can say what changed instead of sending the whole list again.
	items []act.Item
	snap  screenSnapshot
	// clicked is the item the last numbered click acted on, standing in for "the control that now has keyboard focus" — there is no accessibility read for actual focus, and click it first is the documented way to reach type_text anyway. focusUnknown says that stand-in cannot be trusted: a click at a bare coordinate or a key that moves focus has left this session unable to name what the keyboard is pointing at.
	clicked      act.Item
	focusUnknown bool
}

// streamDrawn is one shape that was resolved while the model was still writing the call it belongs to, whether it drew or not. The Codex stream hands over each shape the moment its object closes, so the ink starts before the round has finished rather than after it (see parseCodexStream); the phrase and the target are what the draw tool would have produced had it drawn the shape itself. Err carries the tool error when the shape failed instead — a shape refused during the stream still has to occupy its place in this slice, or the shapes it precedes in the finished call would be shifted onto the wrong entries.
type streamDrawn struct {
	Phrase string
	Target *ScreenTarget
	Err    string
}

// streamDrawnKey is the unexported context key withStreamDrawn stores one call's already-drawn shapes under.
type streamDrawnKey struct{}

// withStreamDrawn carries the shapes a draw call already drew off the stream into that call's own execution, so the tool draws only what is left rather than drawing everything a second time. Input: the ask's context and the shapes already drawn, in the order the call listed them. Output: a context for that one tool call.
func withStreamDrawn(ctx context.Context, drawn []streamDrawn) context.Context {
	if len(drawn) == 0 {
		return ctx
	}
	return context.WithValue(ctx, streamDrawnKey{}, drawn)
}

// streamDrawnFrom returns the leading shapes of this draw call that the stream already drew. Output: those shapes in call order, or nil when the call drew nothing early — which is every provider but Codex, and every Codex round whose backend sent no argument deltas.
func streamDrawnFrom(ctx context.Context) []streamDrawn {
	drawn, _ := ctx.Value(streamDrawnKey{}).([]streamDrawn)
	return drawn
}

// askLookStateKey is the unexported context key withAskLookState stores the per-ask look state under.
type askLookStateKey struct{}

// withAskLookState attaches a fresh, empty look state to ctx, one per ask, so the look allowance, the picture draw maps coordinates against and what it cost all belong to the ask now starting rather than to whatever ask ran before it. Input: the ask's own context. Output: a context carrying the new state, to use for every tool call the ask makes.
func withAskLookState(ctx context.Context) context.Context {
	return context.WithValue(ctx, askLookStateKey{}, &askLookState{})
}

// NewScreenScope gives one caller its own screen state — the numbered list observe_screen produced, the picture look took, how many looks it has taken and what they cost, and which control the last click focused — in place of the agent-wide state a directly driven tool call would otherwise read and write. A long-running computer-use job (internal/actjob) calls it once and makes every tool call of that job with the context it returns, so two jobs never resolve a number against each other's window and a job's screenshots count against its own look allowance. Input: the job's own context. Output: a context carrying fresh screen state.
func (a *Agent) NewScreenScope(ctx context.Context) context.Context {
	return withAskLookState(ctx)
}

// lookStateFrom reads the look state withAskLookState attached to ctx. Output: that state, or a throwaway empty one when ctx carries none, which only happens when a tool is driven directly rather than through an ask.
func lookStateFrom(ctx context.Context) *askLookState {
	if s, ok := ctx.Value(askLookStateKey{}).(*askLookState); ok {
		return s
	}
	return &askLookState{}
}

// askState is the screen state this tool call reads and writes: the one withAskLookState attached to ctx, or the agent's own when a tool is driven directly rather than through an ask, which is what a test and the eval entry points do. Input: the call's context. Output: the state, never nil.
func (a *Agent) askState(ctx context.Context) *askLookState {
	if s, ok := ctx.Value(askLookStateKey{}).(*askLookState); ok {
		return s
	}
	return &a.askScreen
}

// seen returns the items the last observe_screen of this ask listed, which is what a number the model gives resolves against. Output: nil when this ask has not observed the screen yet.
func (a *Agent) seen(ctx context.Context) []act.Item {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.items
}

// rememberScreen stores what an observe_screen call just produced: the items its numbers resolve against and the snapshot the next look is compared to. Observing also restores a known focus, since after it the model is naming elements again rather than pixels. Input: the call's context, the listed items and the snapshot. Output: none.
func (a *Agent) rememberScreen(ctx context.Context, items []act.Item, snap screenSnapshot) {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items, s.snap, s.focusUnknown = items, snap, false
}

// lastScreen returns the snapshot of this ask's last observe_screen answer, the zero value when it has not observed yet.
func (a *Agent) lastScreen(ctx context.Context) screenSnapshot {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// focus returns this session's stand-in for the control the keyboard is pointing at: the item the last numbered click acted on. Output: the item, and false when nothing can be vouched for — a click at a bare coordinate or a focus-moving key has happened since, and the caller must refuse rather than check the stale item.
func (a *Agent) focus(ctx context.Context) (act.Item, bool) {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clicked, !s.focusUnknown
}

// rememberClick records the item a numbered click just acted on as the control the keyboard is now pointing at. Input: the call's context and the clicked item. Output: none.
func (a *Agent) rememberClick(ctx context.Context, it act.Item) {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clicked, s.focusUnknown = it, false
}

// focusLost marks this session unable to say what the keyboard is pointing at, which a click at a bare coordinate and a focus-moving key both leave behind. type_text and a focused key press refuse while it stands. Input: the call's context. Output: none.
func (a *Agent) focusLost(ctx context.Context) {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clicked, s.focusUnknown = act.Item{}, true
}

// recordLook stores the picture a look just took: it becomes the one draw maps coordinates against, it is queued to be handed to the model, and it is counted against the ask's allowance and its cost. Input: the ask's context and the capture. Output: none.
func recordLook(ctx context.Context, c tracker.Capture) {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.look, s.lookUndelivered = &c, true
	s.looks++
	s.lookTokens += lookTokenCost(c.W, c.H)
}

// looksLeft reports whether this ask may take another picture.
func looksLeft(ctx context.Context) bool {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.looks < maxLooksPerAsk
}

// takeLook hands the newest picture to whichever brain is assembling the request, once. Output: the capture and true the first time it is called after a look, false every other time — the picture goes to the model with the tool result that produced it and never again.
func takeLook(ctx context.Context) (tracker.Capture, bool) {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.look == nil || !s.lookUndelivered {
		return tracker.Capture{}, false
	}
	s.lookUndelivered = false
	return *s.look, true
}

// seenLook returns the newest picture this ask took and actually showed the model, which is the one draw maps coordinates against. Output: the capture, and false when this ask has not looked or the picture has not been handed over yet.
// Delivery is the test, not the taking: a channel with no way to carry an image — the Live voice session, which takes tool results as text — leaves every picture undelivered, and a point read off a picture nobody saw is a guess like any other.
func seenLook(ctx context.Context) (tracker.Capture, bool) {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.look == nil || s.lookUndelivered {
		return tracker.Capture{}, false
	}
	return *s.look, true
}

// lookTokensSpent is what this ask's pictures are estimated to have cost, for the turn trace.
func lookTokensSpent(ctx context.Context) int {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookTokens
}

// needLookFirst is what draw says when it is given coordinates with no picture behind them. It names the tool to call, because the refusal is only useful if the model's next move is the look it should have made.
const needLookFirst = "I need to look at the screen first — call look, then give me points in that picture's own coordinates"

// toScreen turns a point the model read off the last look into the point on the screen it names. Input: the point in the picture's own pixels. Output: the screen point, or a tool error when this ask has not looked or the point is outside the picture, which is the shape a guessed coordinate takes.
func (a *Agent) toScreen(ctx context.Context, x, y int) (int, int, string) {
	c, ok := seenLook(ctx)
	if !ok {
		return 0, 0, toolError(needLookFirst)
	}
	if !c.Holds(x, y) {
		return 0, 0, toolError(fmt.Sprintf("%d,%d is not inside the picture the last look took, which is %d wide and %d high — give me a point in it", x, y, c.W, c.H))
	}
	sx, sy := c.ToScreen(x, y)
	return sx, sy, ""
}

// InputDevice is the keyboard and pointer press_key, click_at and scroll_at drive, and the one type_text falls back to when a field will not take text through the accessibility path: an *input.Session in production (see UsePortalInput), a fake in tests, which is the whole reason it is an interface — a test must record what was sent rather than press keys on the user's own screen.
type InputDevice interface {
	PressKey(name string) error
	TypeText(text string) error
	ClickAt(x, y float64) error
	ScrollAt(x, y float64, dy int32) error
}

// onceInput wraps an opener so the session behind it is opened at most once, on the first tool call that actually needs it. The portal puts a dialog on the user's screen asking them to allow remote control the first time a session opens, so opening at daemon start would ask someone who has not requested anything, and opening per call would ask again. Only a session that actually opened is remembered: the open blocks on that dialog, so an ask that gave up waiting for it must not cost this session its keyboard until the daemon restarts, and the next call asks again. Input: the opener. Output: an opener that hands back the one open session, retrying the open after every failure.
func onceInput(open func(context.Context) (InputDevice, error)) func(context.Context) (InputDevice, error) {
	var mu sync.Mutex
	var dev InputDevice
	return func(ctx context.Context) (InputDevice, error) {
		mu.Lock()
		defer mu.Unlock()
		if dev != nil {
			return dev, nil
		}
		opened, err := open(ctx)
		if err != nil {
			return nil, err
		}
		dev = opened
		return dev, nil
	}
}

// WindowRaiser brings an already-running window of another application to the front, which switch_window tries before it falls back to driving the shell's own search from the keyboard: a *window.Raiser in production, talking to the bundled GNOME Shell extension over D-Bus (see internal/window), a fake in tests. Available says whether that extension is loaded and enabled in the running shell right now; List reports every open window with its pid, WM_CLASS, title and focus state; ByPid, ByTitle and ByWmClass each activate a window by one key and report whether they found and raised one.
type WindowRaiser interface {
	Available(ctx context.Context) (bool, error)
	List(ctx context.Context) ([]window.Window, error)
	ByPid(ctx context.Context, pid uint32) (bool, error)
	ByTitle(ctx context.Context, substring string) (bool, error)
	ByWmClass(ctx context.Context, wmClass string) (bool, error)
}

// UseWindowRaiser gives this agent a way to raise another application's window through the bundled GNOME Shell extension. Input: the raiser, which switch_window asks first and falls back from when the extension is not installed, not enabled, or matched nothing.
func (a *Agent) UseWindowRaiser(r WindowRaiser) { a.raiser = r }

// UsePortalInput gives this agent a keyboard and pointer through the desktop portal, opened on the first tool call that needs one. Input: the directory the portal's restore token is kept in, so the user is asked to allow remote control once rather than on every restart.
func (a *Agent) UsePortalInput(dataDir string) {
	a.input = onceInput(func(ctx context.Context) (InputDevice, error) { return openPortalInput(ctx, dataDir) })
}

// inputDevice hands back this session's keyboard and pointer, opening it if this is the first call that needs it. Output: the device, or a tool error naming why there is none — nothing wired one up, or the portal refused, which is what a declined consent dialog looks like from here.
func (a *Agent) inputDevice(ctx context.Context) (InputDevice, string) {
	if a.input == nil {
		return nil, toolError("this session cannot reach the keyboard or the pointer")
	}
	dev, err := a.input(ctx)
	if err != nil {
		return nil, toolError("could not reach the keyboard and pointer: " + err.Error())
	}
	return dev, ""
}

// pressesFocused reports whether a key press acts on the control that has keyboard focus rather than moving around or editing text. Input: the key or chord press_key was given. Output: true for a bare Enter, Return or Space, which press what is focused and so go through the click's stop line; false for everything else, including a chord, since Ctrl+Enter is a different key to the application under it.
func pressesFocused(keys string) bool {
	switch strings.ToLower(strings.TrimSpace(keys)) {
	case "enter", "return", "space":
		return true
	}
	return false
}

// movesFocus reports whether a key press moves keyboard focus off whatever the last click focused: Tab, Shift+Tab and the arrows, whatever modifiers they carry. Input: the key or chord press_key was given. Output: true for those keys, which leave this session unable to say what is focused, false for everything else.
func movesFocus(keys string) bool {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(keys)), "+")
	switch strings.TrimSpace(parts[len(parts)-1]) {
	case "tab", "up", "down", "left", "right":
		return true
	}
	return false
}

// picturePoint reads the x and y a click_at or scroll_at was given and turns them into the point on the screen they name. Input: the tool's arguments, whose x and y are coordinates in the picture the last look took. Output: the screen point, or a tool error when either is missing or no look stands behind them.
func (a *Agent) picturePoint(ctx context.Context, args map[string]any) (int, int, string) {
	x, xOK := args["x"].(float64)
	y, yOK := args["y"].(float64)
	if !xOK || !yOK {
		return 0, 0, toolError("this tool needs x and y, read off the picture the last look took")
	}
	return a.toScreen(ctx, int(x), int(y))
}

// frontWindow names the window in front right now, read fresh rather than from the last observe_screen answer, because a click at a coordinate is checked against the window's own title and looks and clicks can have moved the front window on since that list was made. Output: the "app · title" form currentWindow uses, falling back to the last observed window when the walk fails.
func (a *Agent) frontWindow(ctx context.Context) string {
	app, title, _, err := a.observe(ctx)
	if err != nil || (app == "" && title == "") {
		return a.currentWindow(ctx)
	}
	if title == "" {
		return app
	}
	return app + " · " + title
}

// seenItem resolves the "n" argument of point_at, click and scroll_to to the item observe_screen listed under that number. Input: the tool's arguments. Output: the item, or "" and a tool error when n is missing, nothing has been observed yet, or the number is not in the last list.
func (a *Agent) seenItem(ctx context.Context, args map[string]any) (act.Item, string) {
	n, ok := args["n"].(float64)
	if !ok {
		return act.Item{}, toolError("this tool needs n, the element's number from observe_screen")
	}
	items := a.seen(ctx)
	return itemByNumber(items, int(n))
}

// itemAt finds the item the last observe_screen listed whose rectangle covers a point on the screen, so a click at a bare coordinate can be checked against the control it actually lands on. Input: the listed items, whose rectangles are in screen pixels, and the screen point. Output: the smallest item covering the point and true, the smallest so a button wins over the panel it sits in, or the zero item and false when nothing listed covers it — a canvas, a video, or a control the accessibility walk never saw.
func itemAt(items []act.Item, x, y int) (act.Item, bool) {
	var found act.Item
	ok := false
	for _, it := range items {
		if x < it.X || y < it.Y || x >= it.X+it.W || y >= it.Y+it.H {
			continue
		}
		if !ok || it.W*it.H < found.W*found.H {
			found, ok = it, true
		}
	}
	return found, ok
}

// itemByNumber resolves n to the item observe_screen listed under that number, the lookup seenItem and draw's from/to both use. Input: the items from the latest observe_screen list (nil when observe_screen has not run) and the number named. Output: the item, or "" and a tool error when items is nil or n is not in the list.
func itemByNumber(items []act.Item, n int) (act.Item, string) {
	if items == nil {
		return act.Item{}, toolError("call observe_screen first, then name one of its numbers")
	}
	if n < 1 || n > len(items) {
		return act.Item{}, toolError(fmt.Sprintf("there is no element %d in the last observe_screen list", n))
	}
	return items[n-1], ""
}

// drawShapeProperties are the fields one shape takes. The flat form of the draw tool and every entry of its shapes array share this one definition, so the two forms cannot drift apart and a model that learned one has learned the other.
func drawShapeProperties() map[string]*genai.Schema {
	return map[string]*genai.Schema{
		"shape": {Type: genai.TypeString, Enum: []string{"arrow", "line", "path", "box", "circle"}},
		"from":  {Type: genai.TypeNumber, Description: "start element"},
		"to":    {Type: genai.TypeNumber, Description: "end element"},
		"on":    {Type: genai.TypeNumber, Description: "element to surround (box, circle)"},
		"points": {
			Type:        genai.TypeArray,
			Description: "[[x,y],...] in look coordinates; two, three for path. Not box or circle.",
			Items:       &genai.Schema{Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeNumber}},
		},
		"rect": {
			Type:        genai.TypeObject,
			Description: "{x,y,w,h} in look coordinates (box, circle), instead of on",
			Properties: map[string]*genai.Schema{
				"x": {Type: genai.TypeNumber}, "y": {Type: genai.TypeNumber},
				"w": {Type: genai.TypeNumber}, "h": {Type: genai.TypeNumber},
			},
		},
		"label": {Type: genai.TypeString, Description: "A short label beside it"},
	}
}

// drawParameters are the draw tool's arguments: a list of shapes, always, even for one. Only the list is declared, because declaring a single-shape form beside it would send both field sets on every round of every screen task to say the same thing twice — and because a model shown a list draws a diagram in one call, where one shown both drew a shape per round and spent a whole turn's steps on it. The handler still accepts a bare shape (see drawShapeList), so a model that sends one anyway is answered rather than refused.
func drawParameters() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"shapes": {
				Type:        genai.TypeArray,
				Description: "All shapes in one call.",
				Items:       &genai.Schema{Type: genai.TypeObject, Properties: drawShapeProperties(), Required: []string{"shape"}},
			},
		},
		Required: []string{"shapes"},
	}
}

// maxDrawShapes bounds one draw call. The list comes from the model and every entry is resolved against the screen and broadcast to the overlay on its own, so it is worth a ceiling; thirty-two is far above any real diagram.
const maxDrawShapes = 32

// drawShapeList returns the shapes one draw call is asking for. Input: the tool's arguments. Output: the entries of "shapes" when it holds at least one, otherwise the arguments themselves as a single shape, or a tool error when shapes holds more than maxDrawShapes or an entry that is not an object. A call carrying both forms is read as the batch, because refusing it would cost a round to say something the batch already answers.
func drawShapeList(args map[string]any) ([]map[string]any, string) {
	raw, ok := args["shapes"].([]any)
	if !ok || len(raw) == 0 {
		// shapes was sent but is unusable as a batch — empty, or not an array at all — and the fallback below would read args itself as the one shape it names, which is only right when args actually carries "shape". Otherwise the fallback ends up refusing over a field ("shape") the model never meant to send, rather than the one it did.
		if _, present := args["shapes"]; present {
			if _, hasShape := args["shape"]; !hasShape {
				return nil, toolError("shapes must be a non-empty array of shape objects")
			}
		}
		return []map[string]any{args}, ""
	}
	if len(raw) > maxDrawShapes {
		return nil, toolError(fmt.Sprintf("draw takes at most %d shapes in one call, got %d", maxDrawShapes, len(raw)))
	}
	shapes := make([]map[string]any, len(raw))
	for i, entry := range raw {
		shape, ok := entry.(map[string]any)
		if !ok {
			return nil, toolError(fmt.Sprintf("shapes[%d] must be an object carrying shape and its own points, rect, on or from/to", i))
		}
		shapes[i] = shape
	}
	return shapes, ""
}

// drawOne draws one shape and says what it drew. Input: the ask's context and one shape's arguments — shape, then from/to, points, on or rect as that shape needs, and an optional label. Output: the phrase naming what was drawn, for the caller to put after "drew "; the target to remember when the shape was drawn around a numbered element, nil for every other form since there is nothing to resolve a later "it" against; and a tool error when the shape cannot be drawn, in which case nothing was drawn.
func (a *Agent) drawOne(ctx context.Context, args map[string]any) (string, *ScreenTarget, string) {
	shape, _ := args["shape"].(string)
	label, _ := args["label"].(string)
	switch shape {
	case "arrow", "line":
		points, errText := a.drawPoints(ctx, args, true, 2)
		if errText != "" {
			return "", nil, errText
		}
		if err := a.Draw(shape, points, 0, 0, 0, 0, label); err != nil {
			return "", nil, toolError(err.Error())
		}
		return shape + fmt.Sprintf(" through %d point(s)", len(points)) + labelNote(label), nil, ""
	case "path":
		points, errText := a.drawPoints(ctx, args, false, 3)
		if errText != "" {
			return "", nil, errText
		}
		if err := a.Draw(shape, points, 0, 0, 0, 0, label); err != nil {
			return "", nil, toolError(err.Error())
		}
		return fmt.Sprintf("a path through %d point(s)", len(points)) + labelNote(label), nil, ""
	case "box", "circle":
		x, y, w, h, it, errText := a.drawRect(ctx, args)
		if errText != "" {
			return "", nil, errText
		}
		if err := a.Draw(shape, nil, x, y, w, h, label); err != nil {
			return "", nil, toolError(err.Error())
		}
		// it is nil for the "rect" form (a point read off a picture, naming no observe_screen item), so there is nothing to remember or check a mismatch against — only the "on" form draws around a numbered item.
		if it == nil {
			return fmt.Sprintf("a %s around %d,%d %dx%d", shape, x, y, w, h) + labelNote(label), nil, ""
		}
		target := ScreenTarget{Label: it.Label, Role: it.Role, Window: a.currentWindow(ctx), HasRect: true, X: x, Y: y, W: w, H: h}
		return fmt.Sprintf("a %s around [%d] %s %q", shape, it.N, it.Role, it.Label) + labelNote(label), &target, ""
	case "":
		return "", nil, toolError("draw needs shape (arrow, line, path, box or circle), or shapes for several at once")
	default:
		return "", nil, toolError("draw needs shape to be arrow, line, path, box or circle")
	}
}

// drawPoints resolves the draw tool's arguments into the screen points to draw through, for shapes arrow, line and path. Input: the tool's arguments, carrying either "points" ([[x,y],...] read off the picture the last look took, at least min) or, when allowFromTo is set, "from" and "to" (element numbers from the latest observe_screen list, mapped to their rectangles' centres). Output: the points in screen coordinates, or nil and a tool error when points has fewer than min well-formed [x,y] entries, no look has been taken this turn, a point falls outside that picture, neither form is present or allowed, or an element number is not in the last list.
// The two forms are the only two ways a point can be known rather than guessed: an element the accessibility tree listed, or a place in a picture the model has actually seen. Inside a video, an image, a canvas or a game there are no elements, and on 2026-09-05 that is exactly where a run drew two labelled arrows at coordinates it had made up.
func (a *Agent) drawPoints(ctx context.Context, args map[string]any, allowFromTo bool, min int) ([][2]int, string) {
	if raw, ok := args["points"].([]any); ok {
		if len(raw) < min {
			return nil, toolError(fmt.Sprintf("draw needs at least %d points", min))
		}
		points := make([][2]int, len(raw))
		for i, p := range raw {
			pair, ok := p.([]any)
			if !ok || len(pair) != 2 {
				return nil, toolError("each point must be [x, y]")
			}
			x, xOK := pair[0].(float64)
			y, yOK := pair[1].(float64)
			if !xOK || !yOK {
				return nil, toolError("each point must be [x, y]")
			}
			sx, sy, errText := a.toScreen(ctx, int(x), int(y))
			if errText != "" {
				return nil, errText
			}
			points[i] = [2]int{sx, sy}
		}
		return points, ""
	}

	if !allowFromTo {
		return nil, toolError(fmt.Sprintf("draw needs points (at least %d)", min))
	}
	from, fromOK := args["from"].(float64)
	to, toOK := args["to"].(float64)
	if !fromOK || !toOK {
		return nil, toolError("draw needs from and to (element numbers from observe_screen), or points")
	}
	items := a.seen(ctx)
	fromItem, errText := itemByNumber(items, int(from))
	if errText != "" {
		return nil, errText
	}
	toItem, errText := itemByNumber(items, int(to))
	if errText != "" {
		return nil, errText
	}
	fromPoint, errText := a.livePoint(ctx, fromItem)
	if errText != "" {
		return nil, errText
	}
	toPoint, errText := a.livePoint(ctx, toItem)
	if errText != "" {
		return nil, errText
	}
	return [][2]int{fromPoint, toPoint}, ""
}

// livePoint returns the centre of an element as it is on the screen right now, after checking the number still points at the element observe_screen listed. Input: a context and the listed item. Output: the point to draw through, or the tool error to hand back when the element has moved out from under its number or cannot be read, in which case nothing is drawn — the same rule point_at follows, because a line drawn to where an element used to be points at whatever has taken its place.
func (a *Agent) livePoint(ctx context.Context, it act.Item) ([2]int, string) {
	if errText := a.stillThere(ctx, it); errText != "" {
		return [2]int{}, errText
	}
	x, y, w, h, errText := a.freshRect(ctx, it)
	if errText != "" {
		return [2]int{}, errText
	}
	return [2]int{x + w/2, y + h/2}, ""
}

// itemCentre returns the middle point of an observed item's rectangle, the point draw's from/to form draws through.
func itemCentre(it act.Item) [2]int {
	return [2]int{it.X + it.W/2, it.Y + it.H/2}
}

// drawRect resolves the draw tool's arguments into the rectangle to draw around, for shapes box and circle. Input: the tool's arguments, carrying either "on" (an element number from the latest observe_screen list) or "rect" ({"x":,"y":,"w":,"h":} read off the picture the last look took). Output: the rectangle in screen pixels, the item it was resolved from when "on" named one (nil for the "rect" form, which names no item), or all zero and a tool error when neither is present, the element number is not in the last list, rect is missing a numeric field, no look has been taken this turn, or the rectangle falls outside that picture.
func (a *Agent) drawRect(ctx context.Context, args map[string]any) (x, y, w, h int, it *act.Item, errText string) {
	if on, ok := args["on"].(float64); ok {
		items := a.seen(ctx)
		resolved, errText := itemByNumber(items, int(on))
		if errText != "" {
			return 0, 0, 0, 0, nil, errText
		}
		if errText := a.stillThere(ctx, resolved); errText != "" {
			return 0, 0, 0, 0, nil, errText
		}
		x, y, w, h, errText := a.freshRect(ctx, resolved)
		return x, y, w, h, &resolved, errText
	}
	rect, ok := args["rect"].(map[string]any)
	if !ok {
		return 0, 0, 0, 0, nil, toolError(`box and circle say where with rect {"x":..,"y":..,"w":..,"h":..} in the last look's coordinates, or with on (an element number from observe_screen); points is only for arrow, line and path`)
	}
	rx, xOK := rect["x"].(float64)
	ry, yOK := rect["y"].(float64)
	rw, wOK := rect["w"].(float64)
	rh, hOK := rect["h"].(float64)
	if !xOK || !yOK || !wOK || !hOK {
		return 0, 0, 0, 0, nil, toolError("rect needs x, y, w and h")
	}
	// Both corners are mapped, so a rectangle that starts inside the picture and runs off its edge is refused rather than drawn half over something the model never saw.
	x, y, errText = a.toScreen(ctx, int(rx), int(ry))
	if errText != "" {
		return 0, 0, 0, 0, nil, errText
	}
	far, low, errText := a.toScreen(ctx, int(rx)+int(rw), int(ry)+int(rh))
	if errText != "" {
		return 0, 0, 0, 0, nil, errText
	}
	return x, y, far - x, low - y, nil, ""
}

// labelNote formats the label suffix a draw result line carries, or "" when the model gave no label.
func labelNote(label string) string {
	if label == "" {
		return ""
	}
	return fmt.Sprintf(" labelled %q", label)
}

// stopBeforeClick refuses to click an element that trips the stop-line rule (irreversible) without the user's explicit go-ahead for this exact step. It rings the element first when this session can draw, so the user sees exactly what would have been clicked before being asked to say go — and the ring goes around where the element is now, since that ring is the whole of what the user is answering. Input: a context, the observed item that tripped irreversible, and the window it sits in. Output: a result beginning "Stopped before " naming the control and window plus the one-line question to unlock it; when the element cannot be read the refusal is replaced by the error telling the model to look again, and either way the click does not happen.
func (a *Agent) stopBeforeClick(ctx context.Context, it act.Item, window string) string {
	if a.Point != nil {
		x, y, w, h, errText := a.freshRect(ctx, it)
		if errText != "" {
			return errText
		}
		a.Point(x, y, w, h, it.Label)
	}
	return fmt.Sprintf("Stopped before clicking [%d] %s %q in %q. %s", it.N, it.Role, it.Label, window, consentPrompt(matchedVerb(it, window)))
}

// stillThere runs the staleness check on an element the model named by number, before anything is drawn around it or done to it. Input: a context and the item observe_screen listed. Output: "" when the element is still the role, label and rectangle the list showed, or the tool error to hand back when it is not.
func (a *Agent) stillThere(ctx context.Context, it act.Item) string {
	if a.verify == nil {
		return ""
	}
	if err := a.verify(ctx, it.Ref, it.Role, it.Label, it.X, it.Y, it.W, it.H); err != nil {
		return toolError(fmt.Sprintf("[%d] %s %q is not what observe_screen listed there, look again: %v", it.N, it.Role, it.Label, err))
	}
	return ""
}

// freshRect reads where an element is on the screen right now, so a ring goes around the element rather than around the rectangle it occupied when observe_screen made its list; the accessibility reference stays valid while the page scrolls under it, so the remembered rectangle can by now be over something else entirely. Input: a context and the item observe_screen listed. Output: the element's current rectangle, or an empty rectangle and the tool error to hand back when it cannot be read or has no size on screen, in which case nothing is drawn at all — a ring in the wrong place is what the user reads before saying go.
func (a *Agent) freshRect(ctx context.Context, it act.Item) (x, y, w, h int, errText string) {
	if a.extents == nil {
		return it.X, it.Y, it.W, it.H, ""
	}
	x, y, w, h, err := a.extents(ctx, it.Ref)
	if err != nil {
		return 0, 0, 0, 0, toolError(fmt.Sprintf("could not read where [%d] %s %q is on the screen, so I won't ring it; look again with observe_screen: %v", it.N, it.Role, it.Label, err))
	}
	if w <= 0 || h <= 0 {
		return 0, 0, 0, 0, toolError(fmt.Sprintf("[%d] %s %q is not showing on the screen now, so there is nothing to ring; look again with observe_screen", it.N, it.Role, it.Label))
	}
	return x, y, w, h, ""
}

// mentionsWindow reports whether question names the given window, so a click may follow the user across a window switch they actually asked for rather than one that just happened to grab focus. Input: the ask's own question text, and the front window's app and title. Output: true when the question contains the app name, or a title word of at least four letters, case-insensitively; false for an empty question, which never names anything.
func mentionsWindow(question, app, title string) bool {
	q := strings.ToLower(question)
	if q == "" {
		return false
	}
	if app != "" && strings.Contains(q, strings.ToLower(app)) {
		return true
	}
	for _, word := range strings.Fields(title) {
		word = strings.ToLower(strings.Trim(word, ".,:;·-\"'"))
		if len(word) >= 4 && strings.Contains(q, word) {
			return true
		}
	}
	return false
}

// frontWindowChanged refuses a click that would land in whatever grabbed focus since observe_screen built the list a number is being resolved against — a popup, a desktop overview, another application — unless the question that started this turn actually named where it should act (see WithQuestion, mentionsWindow). Input: a context carrying the ask's own question. Output: "" when nothing has been observed yet (seenItem already refuses that case), the front window still matches the one the list came from, or the question names the window now in front; otherwise the tool error to hand back, naming both windows.
// frontWindowNow reads the window in front right now in the "app · title" form currentWindow uses, for a check that has to be live rather than read off the last screen listing. Output: "" when the front cannot be read.
func (a *Agent) frontWindowNow(ctx context.Context) string {
	app, title, _, err := a.observe(ctx)
	if err != nil || app == "" {
		return ""
	}
	if title == "" {
		return app
	}
	return app + " · " + title
}

// How the window switch is paced. GNOME drops a keystroke sent before its own search field has been drawn, and again before the search has narrowed to a result, so the name is typed only once the overview has actually come up (see waitForOverview) and there is a wait after it is typed; both are variables so a test can set them to zero and cost no wall time. raiserTimeout bounds the extension's three D-Bus calls on their own, because they run inside gnome-shell and a busy shell must cost this tool two seconds rather than the whole turn.
var (
	switchKeyWait   = 400 * time.Millisecond
	switchVerifyFor = 2 * time.Second
	raiserTimeout   = 2 * time.Second
)

// windowSep is what currentWindow puts between a window's application and its title, and what namesApp splits on to match one side without the other.
const windowSep = " · "

// appWords splits text into its lowercase words, breaking on everything that is not a letter or a digit, so "brave-browser" is two words and a name can be matched a whole word at a time. Input: any text. Output: its words, lowercased.
func appWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// namesApp reports whether a piece of text names an application, case-insensitively and by whole words. Input: the text — a request, or a window read as "app · title" — and the application name, which may itself be several words. Output: false for an empty name, else whether the name's words appear in a row in the text, and in a window's reading in its application or in its title but never across the two.
// Whole words rather than any substring, and the two halves apart rather than blended: "gmail · Inbox" does not name Mail, and "Barcode Scanner" does not name Code, both of which used to read as the asked-for window already being in front.
func namesApp(text, app string) bool {
	want := appWords(app)
	if len(want) == 0 {
		return false
	}
	for _, part := range strings.Split(text, windowSep) {
		have := appWords(part)
		for i := 0; i+len(want) <= len(have); i++ {
			if slices.Equal(have[i:i+len(want)], want) {
				return true
			}
		}
	}
	return false
}

// shellApps are the names GNOME's own shell publishes itself under on the accessibility bus. A window read under one of these is the shell's own chrome — the overview, the dash, the top bar — not an application the user is in.
var shellApps = map[string]bool{"gnome-shell": true, "org.gnome.shell": true, "gjs": true}

// overviewOpen reports whether the shell's overview search is on the screen right now, read from the same accessibility tree observe_screen walks: while the overview is up the shell itself is what has focus, and it publishes a showing box to type the search into. Input: a context. Output: true only when the shell is what is in front and that box is showing; false whenever the tree cannot be read, so a switch refuses rather than typing into whatever else has focus.
func (a *Agent) overviewOpen(ctx context.Context) bool {
	app, _, nodes, err := a.observe(ctx)
	if err != nil || !shellApps[strings.ToLower(strings.TrimSpace(app))] {
		return false
	}
	for _, n := range nodes {
		if !n.Showing || (n.Role != "entry" && n.Role != "text") {
			continue
		}
		if n.Label == "" || strings.Contains(strings.ToLower(n.Label), "search") {
			return true
		}
	}
	return false
}

// waitForOverview polls until the shell's overview search is showing or the wait runs out, since the overview takes a moment to draw after Super and a name typed before it is drawn is dropped by GNOME or lands somewhere else. Input: a context. Output: true if the overview came up in time.
func (a *Agent) waitForOverview(ctx context.Context) bool {
	// Five times the pacing wait: enough for a shell mid-animation, and zero in tests, where one look is taken and that is the answer.
	deadline := time.Now().Add(5 * switchKeyWait)
	for {
		if a.overviewOpen(ctx) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-time.After(waitPollEvery):
		case <-ctx.Done():
			return false
		}
	}
}

// switchWindow brings another application's window to the front. It has two ways in and tries them in that order: the bundled GNOME Shell extension, which raises a window the way the shell itself does and puts nothing on the user's screen (see internal/window), and failing that the shell's own search driven through the portal keyboard — Super, the application's name, Enter — which is all an unprivileged daemon has on a GNOME Wayland session without that extension. Either way it then reads what actually came forward, since the search matches an installed application name rather than a window title and its top result can be something else entirely. Input: a context carrying the ask's own question (see WithQuestion) and the application to bring forward. Output: what happened, in the words the model reads back: the window it switched to, the window that is still in front, the window that came forward instead, or a tool error.
// It refuses unless the question itself names that application: a task carries the window it started in, and leaving that window is the user's decision, not the model's. Nothing but the given name is ever typed, and nothing is pressed or raised at all when the window asked for is already in front.
func (a *Agent) switchWindow(ctx context.Context, app string) string {
	if app == "" {
		return toolError("switch_window needs the app to bring to the front")
	}
	if !namesApp(questionFrom(ctx), app) {
		return toolError(fmt.Sprintf("I won't switch to %q: nothing in what was asked names it, and this task stays in the window it started in", app))
	}
	before := a.frontWindowNow(ctx)
	if namesApp(before, app) {
		return fmt.Sprintf("%q is already the window in front; nothing was pressed", before)
	}
	if ok, how := a.raiseWindow(ctx, app); ok {
		return a.switchOutcome(ctx, app, before, how, nil)
	}
	dev, errText := a.inputDevice(ctx)
	if errText != "" {
		return errText
	}
	// Super toggles the overview, so it is pressed only when the overview is not already up, and the name is typed only once the overview is actually showing: a Super the shell swallowed would otherwise put the name into whatever already had focus, the user's own document or compose box.
	if !a.overviewOpen(ctx) {
		if err := dev.PressKey("Super"); err != nil {
			return toolError("could not open the desktop search: " + err.Error())
		}
		if !a.waitForOverview(ctx) {
			return toolError("the desktop search never opened after Super, so nothing was typed and nothing moved")
		}
	}
	if err := dev.TypeText(app); err != nil {
		return toolError("could not type the app's name into the desktop search: " + err.Error())
	}
	time.Sleep(switchKeyWait)
	if err := dev.PressKey("Enter"); err != nil {
		return toolError("could not press Enter on the desktop search: " + err.Error())
	}
	return a.switchOutcome(ctx, app, before, "", dev)
}

// raiseWindow asks the GNOME Shell extension to bring the application's window forward. The pid of the process that owns a window is the most exact key there is, so List is read first and any open window whose class or title already names the app is raised by its pid; failing that (no extension, nothing in the list matched, or the pid activation itself did not land) it falls back to the looser text matches the extension does itself, WM_CLASS before title since a title is often a document name and not the app. Input: a context bounding the D-Bus calls and the application name. Output: true and the key that found it ("pid 1234", `wm_class "brave-browser"` or `title "Brave"`) when a window was raised; false and "" when there is no extension wired up, it is not loaded in the running shell, it matched nothing, or every call failed — every one of which means the keyboard path is what is left.
func (a *Agent) raiseWindow(ctx context.Context, app string) (bool, string) {
	if a.raiser == nil {
		return false, ""
	}
	// The extension answers from inside gnome-shell, so these calls get a bound of their own rather than the whole turn's: a shell busy redrawing costs this tool a couple of seconds and then the keyboard path, not the ask.
	ctx, cancel := context.WithTimeout(ctx, raiserTimeout)
	defer cancel()
	if ok, err := a.raiser.Available(ctx); err != nil || !ok {
		return false, ""
	}
	if windows, err := a.raiser.List(ctx); err == nil {
		for _, w := range windows {
			if !namesApp(w.WmClass, app) && !namesApp(w.Title, app) {
				continue
			}
			if ok, err := a.raiser.ByPid(ctx, w.Pid); err == nil && ok {
				return true, fmt.Sprintf("pid %d", w.Pid)
			}
		}
	}
	if ok, err := a.raiser.ByWmClass(ctx, app); err == nil && ok {
		return true, fmt.Sprintf("wm_class %q", app)
	}
	if ok, err := a.raiser.ByTitle(ctx, app); err == nil && ok {
		return true, fmt.Sprintf("title %q", app)
	}
	return false, ""
}

// switchOutcome reads which window is in front after a switch was attempted and says what happened in the words the model reads back. Input: a context, the application asked for, the window that was in front before, the key that raised it ("pid 1234" and so on, from raiseWindow) or "" when the keyboard did the switching, and the keyboard the search was typed on — nil when the extension did the raising and no search was ever opened. Output: the switch's result line, or a tool error when the front window cannot be read at all.
// A switch that did not land can leave the shell's search sitting over everything, so it is closed again with one Escape — but only when a live read says it is still up, since the same key sent at the user's own window discards whatever was in it. Whether that press lands changes nothing about the report, which is about the switch.
func (a *Agent) switchOutcome(ctx context.Context, app, before, how string, dev InputDevice) string {
	after := a.frontWindowAfterSwitch(ctx, app)
	// Escape only while the overview is actually still up: sent at anything else it is a keystroke into the user's own window, where it discards a draft or closes a dialog.
	if !namesApp(after, app) && dev != nil && a.overviewOpen(ctx) {
		_ = dev.PressKey("Escape")
		// The overview is not a window: it sits over the user's own rather than replacing it, so what to report is read again once it is gone, otherwise every miss reports the shell itself as the window in front.
		after = a.frontWindowAfterSwitch(ctx, app)
	}
	if namesApp(after, app) {
		if how != "" {
			return fmt.Sprintf("switched to %q, raised by %s; call observe_screen to see it", after, how)
		}
		return fmt.Sprintf("switched to %q; call observe_screen to see it", after)
	}
	switch {
	case after == "":
		return toolError("I could not read which window is in front after that, so I cannot say whether the switch worked")
	case after == before:
		return fmt.Sprintf("the front window is still %q; nothing called %q came forward", before, app)
	default:
		return fmt.Sprintf("the front window is now %q, not %q", after, app)
	}
}

// frontWindowAfterSwitch polls the window in front until it names the application asked for or switchVerifyFor runs out, since the shell takes a moment to raise a window after Enter. Input: a context and the application name. Output: the window in front in the "app · title" form, which is the last reading taken whether it matched or not.
func (a *Agent) frontWindowAfterSwitch(ctx context.Context, app string) string {
	deadline := time.Now().Add(switchVerifyFor)
	for {
		front := a.frontWindowNow(ctx)
		if namesApp(front, app) || !time.Now().Before(deadline) {
			return front
		}
		select {
		case <-time.After(waitPollEvery):
		case <-ctx.Done():
			return front
		}
	}
}

func (a *Agent) frontWindowChanged(ctx context.Context) string {
	last := a.lastScreen(ctx)
	if last.app == "" && last.title == "" {
		return ""
	}
	app, title, _, err := a.observe(ctx)
	if err != nil {
		// The real error, if there is one, surfaces from stillThere or the click itself right after this.
		return ""
	}
	if app == last.app && title == last.title {
		return ""
	}
	if mentionsWindow(questionFrom(ctx), app, title) {
		return ""
	}
	front, from := app, last.app
	if title != "" {
		front = app + " · " + title
	}
	if last.title != "" {
		from = last.app + " · " + last.title
	}
	return toolError(fmt.Sprintf("the window in front is now %q, not %q where observe_screen listed this element — call observe_screen again, or say which window to use", front, from))
}
