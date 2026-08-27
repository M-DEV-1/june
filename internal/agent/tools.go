package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"ora/internal/db"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"google.golang.org/genai"
)

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
					"For pure day/timeline questions use recall. For 'what was I just doing' use get_recent.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"query":  {Type: genai.TypeString, Description: "What to search for — topic, project, show, person, etc. Never put a time word here ('today', 'yesterday', 'last week') — it will match text instead of dates; use since/until for that."},
						"domain": {Type: genai.TypeString, Description: "Optional. Restrict to 'work' or 'personal' memories only. Omit to search everything, weighted toward whichever domain you're currently in."},
						"app":    {Type: genai.TypeString, Description: "Optional. Restrict moments to this application name (case-insensitive substring, e.g. slack, firefox, code)."},
						"since":  {Type: genai.TypeString, Description: "Optional. Keep only results from this time onward. 'today', 'yesterday', a bare date (2026-07-05), or a timestamp (2026-07-05T09:30:00). Omit for no time limit."},
						"until":  {Type: genai.TypeString, Description: "Optional. Keep only results up to this time (same formats as 'since'; a bare date covers the whole day). Omit for no time limit."},
					},
					Required: []string{"query"},
				},
			},
			{
				Behavior: genai.BehaviorNonBlocking,
				Name:     "recall",
				Description: "Timeline or subject recall. Use since/until for chronological periods (yesterday, last Tuesday). " +
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

// recallSubjectLimit bounds how many lines RecallSubject contributes to the "recall" tool's subject path.
const recallSubjectLimit = 6

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

// appendOmitted adds one line accounting for a run of n consecutive idle episodes, so a mostly-empty window still reports that the time passed with nothing on screen instead of just returning fewer rows. n == 0 is a no-op.
// Input: the lines so far and the run length. Output: the lines with the marker appended.
func appendOmitted(lines []string, n int) []string {
	switch {
	case n <= 0:
		return lines
	case n == 1:
		return append(lines, "(1 idle/empty capture omitted)")
	default:
		return append(lines, fmt.Sprintf("(%d idle/empty captures omitted)", n))
	}
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
			return time.Time{}, err
		}
	}
	if endOfDay {
		return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc), nil
	}
	return d, nil
}

// dateFormatHint is the one error phrasing for a since/until that didn't parse, shared by recall and query_memory so the model gets the same list of accepted forms wherever it passes a date.
const dateFormatHint = `since/until must be "today", "yesterday", a bare date (2026-07-05), or an ISO-8601 timestamp (2026-07-05T00:00:00 or 2026-07-05T00:00:00Z)`

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

// checkArgs reports every argument name that isn't in valid, listing the valid ones so the model can correct itself on the next call.
// This exists because the model invents parameters — a real trace called recall with query_memory's "query" argument, which the recall handler ignored, then answered from the timeline branch with since defaulted to the start of today.
func checkArgs(args map[string]any, valid ...string) error {
	var unknown []string
	for name := range args {
		if !slices.Contains(valid, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.Sort(unknown)
	return fmt.Errorf("unknown argument(s) %s; valid arguments are %s",
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

// filterHitsByTime keeps hits whose CreatedAt falls inside [since, until]; a zero bound is unbounded on that side.
// A hit with no CreatedAt is kept: notes and summaries reach HybridSearch's lexical path without a timestamp, and dropping them would silently delete durable facts from every dated query.
func filterHitsByTime(hits []db.MemoryHit, since, until time.Time) []db.MemoryHit {
	out := hits[:0:0]
	for _, h := range hits {
		if h.CreatedAt.IsZero() {
			out = append(out, h)
			continue
		}
		if !since.IsZero() && h.CreatedAt.Before(since) {
			continue
		}
		if !until.IsZero() && h.CreatedAt.After(until) {
			continue
		}
		out = append(out, h)
	}
	return out
}

// queryMemoryHits is how many hits query_memory shows the model, and queryMemoryOverfetch multiplies what it asks the store for when a since/until window is set.
// The over-fetch is needed because HybridSearch ranks without any notion of the window: filtering its top 10 after the fact usually leaves nothing, since the whole point of a dated question is that the lexically best matches are from the wrong day.
const (
	queryMemoryHits      = 10
	queryMemoryOverfetch = 5
)

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
		return fmt.Sprintf("error reading clipboard: %v", err)
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
		return fmt.Sprintf("error: %v\noutput: %s", err, string(output))
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
		return "error: approval queue busy, request rejected"
	}

	select {
	case res := <-resChan:
		return res
	case <-ctx.Done():
		slog.Warn("HITL approval abandoned: session ended before user responded", "description", description)
		return "error: session ended before request was approved"
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
			return "error: command argument is required"
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
			return "error: path argument is required"
		}
		execute := func() string {
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Sprintf("error reading file: %v", err)
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
			return fmt.Sprintf("error listing directory: %v", err)
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
			return "error: url argument is required"
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
			return fmt.Sprintf("error opening url: %v", err)
		}
		return fmt.Sprintf("opened %s in browser", url)

	case "query_memory":
		if err := checkArgs(args, "query", "domain", "app", "since", "until"); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		query, ok := args["query"].(string)
		if !ok {
			return "error: query argument is required"
		}
		// domain is optional: a missing or wrong-typed arg silently becomes "" (search everything, weighted toward the current domain) rather than erroring — since/until below are stricter since a mis-parsed date changes which day the answer comes from.
		domain, _ := args["domain"].(string)
		since, until, timed, err := optionalWindow(args, time.Now())
		if errors.Is(err, errSinceAfterUntil) {
			return "error: since must not be after until"
		}
		if err != nil {
			return fmt.Sprintf("error: %s: %v", dateFormatHint, err)
		}
		slog.Info("querying long-term memory", "query", query, "domain", domain, "since", since, "until", until)

		// HybridSearch (FTS5 + vector, fused via reciprocal rank fusion) replaces the old two-call SearchMemory + RankedEpisodes merge — it covers episodes/summaries/notes/threads in one fused, domain-aware ranking.
		limit := queryMemoryHits
		if timed {
			limit *= queryMemoryOverfetch
		}
		hits, err := a.brain.HybridSearch(ctx, query, domain, limit)
		if err != nil {
			return fmt.Sprintf("error querying memory: %v", err)
		}
		if app, _ := args["app"].(string); strings.TrimSpace(app) != "" {
			hits = filterHitsByApp(hits, app)
		}
		// The store has no time-filtered search, so the window is applied here on the hits it returned — hence the over-fetch above.
		if timed {
			hits = filterHitsByTime(hits, since, until)
			if len(hits) > queryMemoryHits {
				hits = hits[:queryMemoryHits]
			}
		}
		if len(hits) == 0 {
			return "no memory matches"
		}
		lines := make([]string, 0, len(hits))
		for _, h := range hits {
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
		if err := checkArgs(args, "subject", "since", "until", "app"); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		if subject, ok := args["subject"].(string); ok && strings.TrimSpace(subject) != "" {
			// RecallSubject takes only a subject and a limit — it has no app filter and no date window — so
			// combining subject with either can't be honored. Say so rather than answering across every app
			// and all of time as if the filters had been applied.
			if hasArg(args, "app") || hasArg(args, "since") || hasArg(args, "until") {
				return "error: subject cannot be combined with app, since, or until; call recall with only subject, or drop subject and use since/until (with an optional app) for a time window"
			}
			slog.Info("recalling subject", "subject", subject)
			lines, err := a.brain.RecallSubject(ctx, subject, recallSubjectLimit)
			if err != nil {
				return fmt.Sprintf("error recalling subject: %v", err)
			}
			if len(lines) == 0 {
				return "no memory of that subject"
			}
			return strings.Join(lines, "\n")
		}

		sinceStr, sinceErr := stringArg(args, "since")
		untilStr, untilErr := stringArg(args, "until")
		if err := errors.Join(sinceErr, untilErr); err != nil {
			return fmt.Sprintf("error: %s: %v", dateFormatHint, err)
		}
		since, until, err := recallBounds(sinceStr, untilStr, time.Now())
		if errors.Is(err, errSinceAfterUntil) {
			return "error: since must not be after until"
		}
		if err != nil {
			return fmt.Sprintf("error: %s: %v", dateFormatHint, err)
		}
		app, _ := args["app"].(string)
		slog.Info("recalling timeline window", "since", since, "until", until, "app", app)

		// NewestFirst: real days exceed the 50-episode cap, so without it this returns the oldest 50 —
		// the start of the window — and silently stops there instead of covering the whole day.
		episodes, err := a.brain.ListEpisodes(ctx, db.EpisodeQuery{Since: since, Until: until, App: app, Limit: 50, NewestFirst: true})
		if err != nil {
			return fmt.Sprintf("error recalling timeline: %v", err)
		}
		if len(episodes) == 0 {
			return "no episodes in that window"
		}
		lines := make([]string, 0, len(episodes))
		idleRun := 0
		for _, e := range episodes {
			// Idle captures are dropped, but counted: a run of them collapses to one line so the model still sees that the window had a quiet stretch.
			if idleEpisode(e) {
				idleRun++
				continue
			}
			lines = appendOmitted(lines, idleRun)
			idleRun = 0
			excerpt := e.ScreenText
			if runes := []rune(excerpt); len(runes) > recallExcerpt {
				excerpt = string(runes[:recallExcerpt])
			}
			// Established timeline shape: "[Jan 2 15:04] app — title: …", converted to the user's local zone (episodes are stored in UTC) so what's shown matches their wall clock.
			lines = append(lines, fmt.Sprintf("[%s] %s — %s: %s",
				e.CreatedAt.In(time.Local).Format("Jan 2 15:04"), e.App, e.Title, excerpt))
		}
		lines = appendOmitted(lines, idleRun)
		return strings.Join(lines, "\n")

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
		episodes, err := a.brain.ListEpisodes(ctx, db.EpisodeQuery{App: app, Limit: limit, NewestFirst: true})
		if err != nil {
			return fmt.Sprintf("error getting recent moments: %v", err)
		}
		if len(episodes) == 0 {
			return "no recent episodes"
		}
		lines := make([]string, 0, len(episodes))
		idleRun := 0
		for _, e := range episodes {
			// Same idle-capture skip as recall above — get_recent formats the identical line from the identical rows.
			if idleEpisode(e) {
				idleRun++
				continue
			}
			lines = appendOmitted(lines, idleRun)
			idleRun = 0
			excerpt := e.ScreenText
			if e.UserActivity != "" {
				excerpt = e.UserActivity
				if e.ScreenText != "" && e.ScreenText != e.UserActivity {
					excerpt = e.UserActivity + " — " + e.ScreenText
				}
			}
			if runes := []rune(excerpt); len(runes) > recallExcerpt {
				excerpt = string(runes[:recallExcerpt])
			}
			line := fmt.Sprintf("[%s] %s — %s: %s",
				e.CreatedAt.In(time.Local).Format("Jan 2 15:04"), e.App, e.Title, excerpt)
			if e.ImagePath != "" {
				line += " [img]"
			}
			lines = append(lines, line)
		}
		lines = appendOmitted(lines, idleRun)
		return strings.Join(lines, "\n")

	case "branch":
		task, ok := args["task"].(string)
		if !ok || strings.TrimSpace(task) == "" {
			return "error: task argument is required"
		}
		if !a.tryReserveBranchSlot() {
			return fmt.Sprintf("error: branch call limit (%d) reached for this session", maxBranchesPerSession)
		}
		model, err := a.subtaskModelFactory()
		if err != nil {
			return fmt.Sprintf("error: branch failed to start: %v", err)
		}
		result, err := a.runSubtask(ctx, model, task)
		if err != nil {
			return fmt.Sprintf("error: branch failed: %v", err)
		}
		return result

	case "save_note":
		content, ok := args["content"].(string)
		if !ok || strings.TrimSpace(content) == "" {
			return "error: content argument is required"
		}
		if _, err := a.brain.LogNote(ctx, content, "fact"); err != nil {
			return fmt.Sprintf("error saving note: %v", err)
		}
		return "saved"

	case "update_note":
		idFloat, ok := args["id"].(float64)
		if !ok {
			return "error: id argument is required (get it from a [note#N] query_memory result)"
		}
		content, ok := args["content"].(string)
		if !ok || strings.TrimSpace(content) == "" {
			return "error: content argument is required"
		}
		if err := a.brain.UpdateNote(ctx, int64(idFloat), content); err != nil {
			return fmt.Sprintf("error updating note: %v", err)
		}
		return "updated"

	case "delete_note":
		idFloat, ok := args["id"].(float64)
		if !ok {
			return "error: id argument is required (get it from a [note#N] query_memory result)"
		}
		if err := a.brain.DeleteNote(ctx, int64(idFloat)); err != nil {
			return fmt.Sprintf("error deleting note: %v", err)
		}
		return "deleted"

	default:
		return fmt.Sprintf("unknown tool: %s", name)
	}
}

// toolActivitySummary pre-formats a tool call's primary argument into a short display literal for the UI (e.g. `"Riddler puzzles"` for query_memory), so the UI never needs to know each tool's arg-shape — that knowledge already lives here, next to executeTool/toolDefinitions.
// Unknown tools and no-arg tools (read_clipboard) summarize to "".
func toolActivitySummary(name string, args map[string]any) string {
	switch name {
	case "query_memory":
		if q, ok := args["query"].(string); ok {
			return fmt.Sprintf("%q", q)
		}
	case "get_recent":
		if app, ok := args["app"].(string); ok && strings.TrimSpace(app) != "" {
			return fmt.Sprintf("%q", app)
		}
		return "recent"
	case "recall":
		if subject, ok := args["subject"].(string); ok && strings.TrimSpace(subject) != "" {
			return fmt.Sprintf("%q", subject)
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
			return fmt.Sprintf("%q", cmd)
		}
	case "read_file":
		if path, ok := args["path"].(string); ok {
			return fmt.Sprintf("%q", path)
		}
	case "list_files":
		if path, ok := args["path"].(string); ok && path != "" {
			return fmt.Sprintf("%q", path)
		}
	case "open_url":
		if url, ok := args["url"].(string); ok {
			return fmt.Sprintf("%q", url)
		}
	case "save_note":
		if content, ok := args["content"].(string); ok {
			return fmt.Sprintf("%q", content)
		}
	case "update_note":
		if content, ok := args["content"].(string); ok {
			return fmt.Sprintf("%q", content)
		}
	case "delete_note":
		if id, ok := args["id"].(float64); ok {
			return fmt.Sprintf("#%d", int64(id))
		}
	case "branch":
		if task, ok := args["task"].(string); ok {
			return fmt.Sprintf("%q", task)
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
