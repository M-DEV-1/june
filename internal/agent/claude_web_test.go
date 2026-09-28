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

// TestStripSourcesBlock_WithSourcesBlock checks that a trailing "Sources:" block — what Claude's built-in WebSearch tool appends, since it runs outside Ora's own MCP server and so leaves no source tag in the tool hops evidenceFromToolHops reads — is removed from the answer text and turned into one Evidence entry per link, in the order they appeared.
func TestStripSourcesBlock_WithSourcesBlock(t *testing.T) {
	answer := "The RTX 3050 is the laptop GPU in that model.\n\nSources:\n- [GPU specs](https://example.com/gpu)\n- [Laptop review](https://example.com/review)"

	got, evidence := stripSourcesBlock(answer)

	if got != "The RTX 3050 is the laptop GPU in that model." {
		t.Errorf("stripSourcesBlock text = %q, want the Sources block and the blank line before it removed", got)
	}
	if strings.Contains(got, "Sources:") {
		t.Errorf("stripSourcesBlock text = %q, want no trace of the Sources block", got)
	}
	if len(evidence) != 2 {
		t.Fatalf("evidence = %+v, want one entry per link", evidence)
	}
	if evidence[0].Kind != "web" || evidence[0].Title != "GPU specs" || evidence[0].Excerpt != "https://example.com/gpu" {
		t.Errorf("evidence[0] = %+v, want kind web, the link's title, and its url as the excerpt", evidence[0])
	}
	if evidence[1].Title != "Laptop review" || evidence[1].Excerpt != "https://example.com/review" {
		t.Errorf("evidence[1] = %+v, want the second link", evidence[1])
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
