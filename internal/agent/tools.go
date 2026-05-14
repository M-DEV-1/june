package agent

import (
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"

	"google.golang.org/genai"
)

// toolDefinitions returns the tools available to the model
func toolDefinitions() []*genai.Tool {
	return []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        "shell_exec",
				Description: "Execute a shell command on the user's system. ALWAYS ask for confirmation before running destructive commands.",
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
		},
	}}
}

// executeTool runs a tool and returns the result as a string
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
		// cap output to 2000 chars to avoid flooding the model
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

	default:
		return fmt.Sprintf("unknown tool: %s", name)
	}
}
