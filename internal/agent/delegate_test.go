package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
)

// fakeRunner is a Runner stub: it records the cwd/systemPrompt/prompt it was called with and returns whatever result/err a test configured, or blocks until ctx is done when block is set (for the timeout test).
type fakeRunner struct {
	result string
	err    error
	block  bool

	gotCWD, gotSystemPrompt, gotPrompt string
}

func (f *fakeRunner) Run(ctx context.Context, cwd, systemPrompt, prompt string) (string, error) {
	f.gotCWD, f.gotSystemPrompt, f.gotPrompt = cwd, systemPrompt, prompt
	if f.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return f.result, f.err
}

// BuildBrief puts the goal, the thread rendered as "user:"/"ora:" lines, and the constraints/report footer into one page, in that order.
func TestBuildBrief_HasGoalThreadConstraintsAndReportLine(t *testing.T) {
	thread := []db.Turn{
		{Role: "you", Text: "can you fix the flaky test", Kind: "ask"},
		{Role: "ora", Text: "which one is flaky", Kind: "ask"},
	}
	brief := BuildBrief("fix the flaky test in store_test.go", thread, "")

	if !strings.Contains(brief, "Goal: fix the flaky test in store_test.go") {
		t.Errorf("brief missing goal: %s", brief)
	}
	if !strings.Contains(brief, "user: can you fix the flaky test") || !strings.Contains(brief, "ora: which one is flaky") {
		t.Errorf("brief missing thread lines: %s", brief)
	}
	if !strings.Contains(brief, "Constraints:") || !strings.Contains(brief, "do not send, publish, pay for or delete") {
		t.Errorf("brief missing constraints: %s", brief)
	}
	if !strings.Contains(brief, "Where to report:") {
		t.Errorf("brief missing report line: %s", brief)
	}
}

// An empty thread still gets a "nothing said yet" line rather than an empty section, and an empty personal block adds nothing.
func TestBuildBrief_EmptyThreadSaysSo(t *testing.T) {
	brief := BuildBrief("do the thing", nil, "")
	if !strings.Contains(brief, "(nothing said in this conversation yet)") {
		t.Errorf("brief = %s", brief)
	}
}

// A personal-context block, when given, is included in the brief verbatim.
func TestBuildBrief_IncludesPersonalContext(t *testing.T) {
	brief := BuildBrief("do the thing", nil, "Personal context — things known for certain about the user:\n  works on ora")
	if !strings.Contains(brief, "works on ora") {
		t.Errorf("brief = %s", brief)
	}
}

// A thread longer than delegateThreadBudget runes keeps only its newest lines, dropping the oldest first.
func TestBuildBrief_ThreadOverBudgetKeepsNewestOnly(t *testing.T) {
	oldLine := strings.Repeat("a", delegateThreadBudget)
	thread := []db.Turn{
		{Role: "you", Text: oldLine, Kind: "ask"},
		{Role: "ora", Text: "the newest line", Kind: "ask"},
	}
	brief := BuildBrief("goal", thread, "")
	if strings.Contains(brief, oldLine) {
		t.Errorf("old line over budget should have been dropped: %s", brief)
	}
	if !strings.Contains(brief, "the newest line") {
		t.Errorf("newest line should always be kept: %s", brief)
	}
}

// A thread line naming a secret — a password mention, or a credential file path — never reaches the brief.
func TestBuildBrief_RedactsSecretLines(t *testing.T) {
	thread := []db.Turn{
		{Role: "you", Text: "my password is hunter2", Kind: "ask"},
		{Role: "you", Text: "the key is at ~/.ssh/id_rsa", Kind: "ask"},
		{Role: "you", Text: "this line is fine", Kind: "ask"},
	}
	brief := BuildBrief("goal", thread, "")
	if strings.Contains(brief, "hunter2") {
		t.Errorf("password line leaked: %s", brief)
	}
	if strings.Contains(brief, "id_rsa") {
		t.Errorf("credential path leaked: %s", brief)
	}
	if !strings.Contains(brief, "this line is fine") {
		t.Errorf("non-secret line should survive: %s", brief)
	}
}

// A trailing error-kind turn (a failed answer) and an empty turn are both left out of the thread the same way HistoryFromTurns leaves them out.
func TestBuildBrief_DropsErrorAndEmptyTurns(t *testing.T) {
	thread := []db.Turn{
		{Role: "you", Text: "  ", Kind: "ask"},
		{Role: "ora", Text: "something went wrong", Kind: "error"},
		{Role: "you", Text: "the real question", Kind: "ask"},
	}
	brief := BuildBrief("goal", thread, "")
	if strings.Contains(brief, "something went wrong") {
		t.Errorf("error-kind turn should be dropped: %s", brief)
	}
	if !strings.Contains(brief, "the real question") {
		t.Errorf("real turn missing: %s", brief)
	}
}

// Delegate passes the delegation's CWD straight through to the runner, unchanged.
func TestDelegate_PassesCWDThrough(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{result: "done"}
	if _, err := a.delegate(t.Context(), run, Delegation{Brief: "do it", CWD: "/some/project"}, nil); err != nil {
		t.Fatal(err)
	}
	if run.gotCWD != "/some/project" {
		t.Errorf("cwd = %q, want /some/project", run.gotCWD)
	}
}

// Delegate's system prompt is the brief BuildBrief would have written for the same goal and thread, and the prompt is the goal itself.
func TestDelegate_SendsTheBriefAsSystemPromptAndGoalAsPrompt(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{result: "done"}
	thread := []db.Turn{{Role: "you", Text: "earlier thing", Kind: "ask"}}
	if _, err := a.delegate(t.Context(), run, Delegation{Brief: "fix the bug", CWD: "/repo"}, thread); err != nil {
		t.Fatal(err)
	}
	if run.gotPrompt != "fix the bug" {
		t.Errorf("prompt = %q", run.gotPrompt)
	}
	if !strings.Contains(run.gotSystemPrompt, "Goal: fix the bug") || !strings.Contains(run.gotSystemPrompt, "user: earlier thing") {
		t.Errorf("system prompt = %s", run.gotSystemPrompt)
	}
}

// Delegate returns the runner's result trimmed.
func TestDelegate_ReturnsTheRunnerResult(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{result: "  the answer  "}
	result, err := a.delegate(t.Context(), run, Delegation{Brief: "do it"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != "the answer" {
		t.Errorf("result = %q", result)
	}
}

// Delegate with no brief fails before ever calling the runner.
func TestDelegate_NoBriefIsAnError(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{result: "done"}
	if _, err := a.delegate(t.Context(), run, Delegation{Brief: "  "}, nil); err == nil {
		t.Fatal("expected an error for an empty brief")
	}
	if run.gotPrompt != "" {
		t.Errorf("runner should never have been called")
	}
}

// A runner error surfaces as-is, except when the context's own deadline is what ended the run, which is reported as a timeout naming delegateTimeout.
func TestDelegate_RunnerErrorSurfaces(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{err: errors.New("boom")}
	_, err := a.delegate(t.Context(), run, Delegation{Brief: "do it"}, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
}

// A delegate call that outlives its wall budget ends in a timeout error rather than hanging forever — driven by a context whose own deadline is already shorter than delegateTimeout, so the test does not wait ten minutes for it.
func TestDelegate_TimesOut(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{block: true}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := a.delegate(ctx, run, Delegation{Brief: "do it"}, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want a timeout error", err)
	}
}

// delegateHandler with no brief argument returns a toolError sentence rather than calling the runner or panicking.
func TestDelegateHandler_MissingBriefReturnsToolError(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	got := delegateHandler(t.Context(), a, map[string]any{})
	if !strings.HasPrefix(got, "error: ") {
		t.Errorf("got = %q, want an error: prefixed message", got)
	}
}

// delegateHandler with a brief that is only whitespace is treated the same as a missing one.
func TestDelegateHandler_BlankBriefReturnsToolError(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	got := delegateHandler(t.Context(), a, map[string]any{"brief": "   "})
	if !strings.HasPrefix(got, "error: ") {
		t.Errorf("got = %q, want an error: prefixed message", got)
	}
}
