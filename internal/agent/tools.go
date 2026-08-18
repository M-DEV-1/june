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

func toolDefinitions() []*genai.Tool {
	return []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
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
				Name:        "read_clipboard",
				Description: "Read the current contents of the user's clipboard",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
				},
			},
			{
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
				Name: "query_memory",
				Description: "Topical search over memory (moments, facts, arcs, period summaries). " +
					"Moments (screen observations) rank with recency; facts/notes do not expire. " +
					"Use app to restrict to one application (Slack, Firefox, Code). " +
					"For pure day/timeline questions use recall. For 'what was I just doing' use get_recent.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"query":  {Type: genai.TypeString, Description: "What to search for — topic, project, show, person, etc."},
						"domain": {Type: genai.TypeString, Description: "Optional. Restrict to 'work' or 'personal' memories only. Omit to search everything, weighted toward whichever domain you're currently in."},
						"app":    {Type: genai.TypeString, Description: "Optional. Restrict moments to this application name (case-insensitive substring, e.g. slack, firefox, code)."},
					},
					Required: []string{"query"},
				},
			},
			{
				Name: "recall",
				Description: "Timeline or subject recall. Use since/until for chronological periods (yesterday, last Tuesday). " +
					"Use subject for an ongoing arc. Use app to keep only that application's moments. " +
					"Returns short content+context lines, not raw screen dumps.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"subject": {Type: genai.TypeString, Description: "Optional. A subject/topic to recall (fuses the matching thread's arc with diverse episode specifics). Takes priority over the timeline."},
						"since":   {Type: genai.TypeString, Description: "Optional. Start of the timeline window as an ISO-8601 timestamp (2026-07-05T00:00:00Z) or bare date (2026-07-05). You know the current date/time — convert phrases like 'yesterday', 'July 5th', or 'last week' into a concrete date yourself. Defaults to the start of today."},
						"until":   {Type: genai.TypeString, Description: "Optional. End of the timeline window (same formats as 'since'). A bare date covers the whole day. Defaults to now. For a single day, set since and until to that same date."},
						"app":     {Type: genai.TypeString, Description: "Optional. Restrict moments to this application name (case-insensitive substring)."},
					},
				},
			},
			{
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
				Name: "branch",
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
				Name: "save_note",
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
				Name: "update_note",
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
				Name: "delete_note",
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

// recallBounds resolves the "recall" tool's since/until args into a concrete [since, until] range.
// The model, knowing the current date/time, converts any human phrase ("yesterday", "July 5th", "last week") into ISO-8601 bounds and passes them here — so this carries no hardcoded time vocabulary of its own.
func recallBounds(sinceStr, untilStr string, now time.Time) (time.Time, time.Time, error) {
	since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if strings.TrimSpace(sinceStr) != "" {
		parsed, err := parseInstant(sinceStr, now.Location(), false)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		since = parsed
	}
	until := now
	if strings.TrimSpace(untilStr) != "" {
		parsed, err := parseInstant(untilStr, now.Location(), true)
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

// parseInstant parses a full RFC3339 timestamp, or a bare calendar date (2006-01-02).
// A bare date anchors to the start of that day, or its end (23:59:59) when endOfDay is set — so a bare until date is inclusive of the whole day rather than a zero-width midnight instant.
func parseInstant(s string, loc *time.Location, endOfDay bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(s), loc)
	if err != nil {
		return time.Time{}, err
	}
	if endOfDay {
		return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc), nil
	}
	return d, nil
}

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
		query, ok := args["query"].(string)
		if !ok {
			return "error: query argument is required"
		}
		// domain is optional: a missing or wrong-typed arg silently becomes "" (search everything, weighted toward the current domain) rather than erroring — since/until below are stricter since that's a different tool.
		domain, _ := args["domain"].(string)
		slog.Info("querying long-term memory", "query", query, "domain", domain)

		// HybridSearch (FTS5 + vector, fused via reciprocal rank fusion) replaces the old two-call SearchMemory + RankedEpisodes merge — it covers episodes/summaries/notes/threads in one fused, domain-aware ranking.
		hits, err := a.brain.HybridSearch(ctx, query, domain, 10)
		if err != nil {
			return fmt.Sprintf("error querying memory: %v", err)
		}
		if app, _ := args["app"].(string); strings.TrimSpace(app) != "" {
			hits = filterHitsByApp(hits, app)
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
		if subject, ok := args["subject"].(string); ok && strings.TrimSpace(subject) != "" {
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

		var sinceStr, untilStr string
		if v, present := args["since"]; present {
			s, ok := v.(string)
			if !ok {
				return fmt.Sprintf("error: since/until must be ISO-8601 timestamps (e.g. 2026-07-05T00:00:00Z or 2026-07-05): since must be a string, got %T", v)
			}
			sinceStr = s
		}
		if v, present := args["until"]; present {
			s, ok := v.(string)
			if !ok {
				return fmt.Sprintf("error: since/until must be ISO-8601 timestamps (e.g. 2026-07-05T00:00:00Z or 2026-07-05): until must be a string, got %T", v)
			}
			untilStr = s
		}
		since, until, err := recallBounds(sinceStr, untilStr, time.Now())
		if errors.Is(err, errSinceAfterUntil) {
			return "error: since must not be after until"
		}
		if err != nil {
			return fmt.Sprintf("error: since/until must be ISO-8601 timestamps (e.g. 2026-07-05T00:00:00Z or 2026-07-05): %v", err)
		}
		app, _ := args["app"].(string)
		slog.Info("recalling timeline window", "since", since, "until", until, "app", app)

		episodes, err := a.brain.ListEpisodes(ctx, db.EpisodeQuery{Since: since, Until: until, App: app, Limit: 50})
		if err != nil {
			return fmt.Sprintf("error recalling timeline: %v", err)
		}
		if len(episodes) == 0 {
			return "no episodes in that window"
		}
		lines := make([]string, 0, len(episodes))
		for _, e := range episodes {
			excerpt := e.ScreenText
			if runes := []rune(excerpt); len(runes) > recallExcerpt {
				excerpt = string(runes[:recallExcerpt])
			}
			// Established timeline shape: "[Jan 2 15:04] app — title: …" (uses the timestamp's own location, so UTC fixtures stay UTC).
			lines = append(lines, fmt.Sprintf("[%s] %s — %s: %s",
				e.CreatedAt.Format("Jan 2 15:04"), e.App, e.Title, excerpt))
		}
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
		for _, e := range episodes {
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
				e.CreatedAt.Format("Jan 2 15:04"), e.App, e.Title, excerpt)
			if e.ImagePath != "" {
				line += " [img]"
			}
			lines = append(lines, line)
		}
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
