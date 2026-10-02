package proactive

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// A notice's title and body come from a brain and from meeting titles, so they must reach the PowerShell toast as data: carried in the environment, never spliced into the script PowerShell parses.
func TestToastCommand_CarriesTextInTheEnvironmentNotTheScript(t *testing.T) {
	title := `'); Remove-Item -Recurse C:\ ; ('`
	body := "$(Stop-Computer) \"quoted\" `backtick`"
	cmd := toastCommand(context.Background(), title, body)
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "Remove-Item") || strings.Contains(arg, "Stop-Computer") {
			t.Errorf("argument %q carries the notice's text, want it only in the environment", arg)
		}
	}
	if !slices.Contains(cmd.Env, toastTitleEnv+"="+title) || !slices.Contains(cmd.Env, toastBodyEnv+"="+body) {
		t.Errorf("environment does not carry the title and body verbatim")
	}
}
