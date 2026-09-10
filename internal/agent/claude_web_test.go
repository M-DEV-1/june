package agent

import (
	"slices"
	"strings"
	"testing"
)

// Claude answers on the user's own subscription, and that subscription includes a web search Ora was throwing away: --tools "" dropped the whole built-in set, so an ask that fell back to Claude had no way to look anything up and said so ("I don't actually have web access wired up right now", 2026-09-07).
// Verified against the real CLI on 2026-09-07: `claude -p --restricted --tools WebSearch --allowed-tools WebSearch` searched and answered with source URLs.
func TestClaudeArgs_KeepsWebSearch(t *testing.T) {
	args := claudeArgs("sonnet", "/tmp/mcp.json", "/tmp/prompt.txt", []string{"recall", "save_note"})

	tools := flagValue(t, args, "--tools")
	if !slices.Contains(strings.Split(tools, ","), "WebSearch") {
		t.Errorf("--tools = %q, want it to name WebSearch", tools)
	}
	allowed := strings.Split(flagValue(t, args, "--allowed-tools"), ",")
	if !slices.Contains(allowed, "WebSearch") {
		t.Errorf("--allowed-tools = %v, want WebSearch among them", allowed)
	}
	// Ora's own tools still have to be there, each under the MCP server's prefix.
	if !slices.Contains(allowed, "mcp__"+claudeMCPServerName+"__recall") {
		t.Errorf("--allowed-tools = %v, want Ora's own recall among them", allowed)
	}
	// --restricted still drops the command and code-running tools and WebFetch, which is what keeps a prompt carrying unvetted text from running anything on this machine.
	if !slices.Contains(args, "--restricted") {
		t.Error("--restricted is gone, so the built-in command and code tools are back")
	}
}

// flagValue returns the argument following name.
func flagValue(t *testing.T, args []string, name string) string {
	t.Helper()
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("%s is not in %v", name, args)
	}
	return args[i+1]
}
