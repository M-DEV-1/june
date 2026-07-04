package agent

import (
	"context"
	"fmt"
	"log/slog"
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
				Name:        "query_memory",
				Description: "Search the user's historical semantic memory tasks. Use this when the user asks about something they did in the past that is not in the immediate context window.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"query": {Type: genai.TypeString, Description: "The keyword or phrase to search for in historical task summaries."},
					},
					Required: []string{"query"},
				},
			},
			{
				Name:        "recall",
				Description: "Recall your timeline for a period, or what you know about a subject, from your raw episode history.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"subject": {Type: genai.TypeString, Description: "Optional. A subject/topic to recall (fuses the matching thread's arc with diverse episode specifics). Takes priority over window."},
						"window":  {Type: genai.TypeString, Description: "Optional. A time period to recall as a chronological timeline: one of today|morning|afternoon|evening|week. Defaults to today."},
					},
				},
			},
		},
	}}
}

// recallSubjectLimit bounds how many lines RecallSubject contributes to the
// "recall" tool's subject path.
const recallSubjectLimit = 6

// recallExcerpt caps how much of an episode's screen_text is surfaced per
// line in the "recall" tool's window (timeline) path — shorter than
// maxEpisodeExcerpt since a whole day's timeline is many lines at once.
const recallExcerpt = 160

// recallWindowBounds computes [since, until] for the "recall" tool's window
// arg, relative to now:
//   - today: midnight -> now
//   - morning: 05:00 -> 12:00 (today)
//   - afternoon: 12:00 -> 17:00 (today)
//   - evening: 17:00 -> 23:59:59 (today)
//   - week: now-7d -> now
//   - anything else (including ""): defaults to today
func recallWindowBounds(window string, now time.Time) (time.Time, time.Time) {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch window {
	case "morning":
		return dayStart.Add(5 * time.Hour), dayStart.Add(12 * time.Hour)
	case "afternoon":
		return dayStart.Add(12 * time.Hour), dayStart.Add(17 * time.Hour)
	case "evening":
		return dayStart.Add(17 * time.Hour), dayStart.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
	case "week":
		return now.AddDate(0, 0, -7), now
	case "today":
		fallthrough
	default:
		return dayStart, now
	}
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

// executeTool runs a tool and returns the result as a string
// maybe this can be seperated into /agent/tools altogether later and be compiled with OS specific code?
func (a *Agent) executeTool(name string, args map[string]any) string {
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

		resChan := make(chan string, 1)
		select {
		case a.ToolApprovalChan <- ToolRequest{Command: command, ResultChan: resChan}:
		default:
			// TUI approval queue full — another tool is pending. Reject to unblock.
			return "error: approval queue busy, command rejected"
		}

		return <-resChan

	case "read_clipboard":
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

	case "read_file":
		path, ok := args["path"].(string)
		if !ok {
			return "error: path argument is required"
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("error reading file: %v", err)
		}
		result := string(data)
		if len(result) > 4000 {
			result = result[:4000] + "\n... (truncated, file too large)"
		}
		return result

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
		slog.Info("querying long-term memory", "query", query)

		hits, err := a.brain.SearchMemory(context.Background(), query)
		if err != nil {
			return fmt.Sprintf("error querying memory: %v", err)
		}
		// Episodes are raw screen-capture history and aren't covered by
		// SearchMemory (notes/summaries/threads), so the model couldn't
		// search for episode specifics until now. Merge in ranked episode
		// hits (recency+importance+relevance), clearly labeled.
		episodeHits, err := a.brain.RankedEpisodes(context.Background(), query, 5)
		if err != nil {
			return fmt.Sprintf("error querying memory: %v", err)
		}
		if len(hits) == 0 && len(episodeHits) == 0 {
			return "no memory matches"
		}
		lines := make([]string, 0, len(hits)+len(episodeHits))
		for _, h := range hits {
			lines = append(lines, fmt.Sprintf("[%s] %s", h.Source, h.Content))
		}
		for _, h := range episodeHits {
			lines = append(lines, fmt.Sprintf("[episode] %s", h.Content))
		}
		return strings.Join(lines, "\n")

	case "recall":
		if subject, ok := args["subject"].(string); ok && strings.TrimSpace(subject) != "" {
			slog.Info("recalling subject", "subject", subject)
			lines, err := a.brain.RecallSubject(context.Background(), subject, recallSubjectLimit)
			if err != nil {
				return fmt.Sprintf("error recalling subject: %v", err)
			}
			if len(lines) == 0 {
				return "no memory of that subject"
			}
			return strings.Join(lines, "\n")
		}

		window, _ := args["window"].(string)
		since, until := recallWindowBounds(window, time.Now())
		slog.Info("recalling timeline window", "window", window, "since", since, "until", until)

		episodes, err := a.brain.EpisodesInWindow(context.Background(), since, until, 50)
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
			lines = append(lines, fmt.Sprintf("[%s] %s — %s: %s", e.CreatedAt.Format("15:04"), e.App, e.Title, excerpt))
		}
		return strings.Join(lines, "\n")

	default:
		return fmt.Sprintf("unknown tool: %s", name)
	}
}
