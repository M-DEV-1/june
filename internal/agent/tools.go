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
		},
	}}
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
		slog.Warn("executing shell command", "command", command)

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

		results, err := a.brain.QueryMemory(context.Background(), query)
		if err != nil {
			return fmt.Sprintf("error querying memory: %v", err)
		}
		if len(results) == 0 {
			return "No memories found matching that query."
		}
		return "Found memories:\n" + strings.Join(results, "\n")

	default:
		return fmt.Sprintf("unknown tool: %s", name)
	}
}
