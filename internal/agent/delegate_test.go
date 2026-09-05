package agent

import (
	"context"
	"errors"
	"os"
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

// A secret in the goal itself — not just the thread — never reaches the brief.
func TestBuildBrief_RedactsGoalLine(t *testing.T) {
	brief := BuildBrief("my password is hunter2", nil, "")
	if strings.Contains(brief, "hunter2") {
		t.Errorf("secret goal leaked: %s", brief)
	}
}

// A secret line inside the personal-context block never reaches the brief, while the rest of the block survives.
func TestBuildBrief_RedactsPersonalContextLine(t *testing.T) {
	personal := "Personal context — things known for certain about the user:\n  password: hunter2\n  works on ora"
	brief := BuildBrief("goal", nil, personal)
	if strings.Contains(brief, "hunter2") {
		t.Errorf("secret personal-context line leaked: %s", brief)
	}
	if !strings.Contains(brief, "works on ora") {
		t.Errorf("non-secret personal-context line should survive: %s", brief)
	}
}

// A "to" other than "" or "claude" is refused before the runner is ever called, rather than silently run as claude.
func TestDelegate_RejectsUnknownTo(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{result: "done"}
	_, err := a.delegate(t.Context(), run, Delegation{To: "codex", Brief: "do it"}, nil)
	if err == nil || !strings.Contains(err.Error(), `"codex" is not a delegate Ora can run`) {
		t.Errorf("err = %v", err)
	}
	if run.gotPrompt != "" {
		t.Errorf("runner should never have been called for an unsupported target")
	}
}

// delegateHandler's error message carries the underlying error, not a generic line — so a made-up cwd tells the model the path was wrong instead of "try again". The cwd-existence check (tested below at the delegate level) means this never has to start the real claude binary to see an error.
func TestDelegateHandler_ErrorIncludesUnderlyingError(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	got := delegateHandler(t.Context(), a, map[string]any{"brief": "do it", "cwd": "/home/x/proj-does-not-exist"})
	if !strings.Contains(got, "/home/x/proj-does-not-exist") {
		t.Errorf("got = %q, want the underlying error naming the bad cwd", got)
	}
}

// A cwd that does not exist, or is not a directory, fails before the runner is ever started, naming the problem.
func TestDelegate_RejectsMissingCWD(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{result: "done"}
	_, err := a.delegate(t.Context(), run, Delegation{Brief: "do it", CWD: "/no/such/project/dir"}, nil)
	if err == nil || !strings.Contains(err.Error(), "/no/such/project/dir") {
		t.Errorf("err = %v, want it to name the missing cwd", err)
	}
	if run.gotPrompt != "" {
		t.Errorf("runner should never have been called for a missing cwd")
	}
}

// A regular file given as cwd is rejected the same way a missing one is, rather than being handed to the runner.
func TestDelegate_RejectsCWDThatIsAFile(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{result: "done"}
	file := t.TempDir() + "/not-a-dir"
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := a.delegate(t.Context(), run, Delegation{Brief: "do it", CWD: file}, nil)
	if err == nil {
		t.Fatal("expected an error for a cwd that is a file")
	}
}

// The delegate's own child process runs in its own process group so cmd.Cancel can kill the whole group, not just the direct child, once the wall-clock budget or caller context ends the run.
func TestNewDelegateCmd_RunsInOwnProcessGroupAndCancelKillsIt(t *testing.T) {
	cmd := newDelegateCmd(context.Background(), "", "/tmp/does-not-matter")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Errorf("cmd.SysProcAttr = %+v, want Setpgid true", cmd.SysProcAttr)
	}
	if cmd.Cancel == nil {
		t.Errorf("cmd.Cancel is nil, want a group-kill on cancel")
	}
	if cmd.WaitDelay != 2*time.Second {
		t.Errorf("cmd.WaitDelay = %s, want 2s kept", cmd.WaitDelay)
	}
}

// Delegate passes the delegation's CWD straight through to the runner, unchanged.
func TestDelegate_PassesCWDThrough(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{result: "done"}
	dir := t.TempDir()
	if _, err := a.delegate(t.Context(), run, Delegation{Brief: "do it", CWD: dir}, nil); err != nil {
		t.Fatal(err)
	}
	if run.gotCWD != dir {
		t.Errorf("cwd = %q, want %q", run.gotCWD, dir)
	}
}

// Delegate's system prompt is the brief BuildBrief would have written for the same goal and thread, and the prompt is the goal itself.
func TestDelegate_SendsTheBriefAsSystemPromptAndGoalAsPrompt(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{result: "done"}
	thread := []db.Turn{{Role: "you", Text: "earlier thing", Kind: "ask"}}
	if _, err := a.delegate(t.Context(), run, Delegation{Brief: "fix the bug", CWD: t.TempDir()}, thread); err != nil {
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
