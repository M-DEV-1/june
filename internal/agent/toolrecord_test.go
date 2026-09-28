package agent

import (
	"fmt"
	"testing"
)

// The record is only worth keeping if "this tool is broken" and "this tool was never allowed near the job" can be told apart afterwards, so the classing is checked against strings the real producers write rather than against copies of their wording.
func TestToolOutcome_TellsARefusalFromABreakage(t *testing.T) {
	cases := []struct {
		name   string
		result string
		want   string
	}{
		{"a tool's own words", "added to the task list: buy milk", OutcomeOK},
		{"a screen listing", "1 button \"Send\"\n2 link \"Home\"", OutcomeOK},
		{"a tool that ran and failed", toolError("that task didn't save"), OutcomeError},
		{"an approval nobody could give", refuseApproval("shell: rm -rf /"), OutcomeRefused},
		{"a tool the ask loop never offered", fmt.Sprintf("error: tool %q %s", "shell_exec", askGateMark), OutcomeRefused},
		{"the stop line before a click", "Stopped before clicking [3] button \"Send\" in \"Mail\". " + consentPrompt("send"), OutcomeRefused},
		{"the stop line before an order", "Stopped before clicking 40,50 in \"Shop\". " + consentPrompt("place order"), OutcomeRefused},
	}
	for _, tc := range cases {
		if got := toolOutcome(tc.result); got != tc.want {
			t.Errorf("%s: toolOutcome(%q) = %q, want %q", tc.name, tc.result, got, tc.want)
		}
	}
}
