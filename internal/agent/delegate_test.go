package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// TestBuildBrief drives BuildBrief once per row and checks what that row's shape of goal/thread/personal-block must do to the page it returns: the goal, the thread rendered as "user:"/"ora:" lines, and the constraints/report footer all land in it, in that order; an empty thread gets a "nothing said yet" line rather than an empty section; a thread over delegateThreadBudget runes keeps its newest lines and drops the oldest; a trailing error-kind turn and an empty turn are left out the way HistoryFromTurns leaves them out; and a secret — in the thread, in the goal, or in the personal-context block — never reaches the brief while the rest of whichever block it was in survives.
func TestBuildBrief(t *testing.T) {
	oldLine := strings.Repeat("a", delegateThreadBudget)
	cases := []struct {
		name            string
		goal            string
		thread          []db.Turn
		personal        string
		wantContains    []string
		wantNotContains []string
	}{
		{
			name: "goal, thread, constraints and report line",
			goal: "fix the flaky test in store_test.go",
			thread: []db.Turn{
				{Role: "you", Text: "can you fix the flaky test", Kind: "ask"},
				{Role: "ora", Text: "which one is flaky", Kind: "ask"},
			},
			wantContains: []string{
				"Goal: fix the flaky test in store_test.go",
				"user: can you fix the flaky test", "ora: which one is flaky",
				"Constraints:", "do not send, publish, pay for or delete",
				"Where to report:",
			},
		},
		{
			name:         "empty thread says so",
			goal:         "do the thing",
			wantContains: []string{"(nothing said in this conversation yet)"},
		},
		{
			name:         "personal-context block included verbatim",
			goal:         "do the thing",
			personal:     "Personal context — things known for certain about the user:\n  works on ora",
			wantContains: []string{"works on ora"},
		},
		{
			name: "a thread over budget keeps only its newest lines",
			goal: "goal",
			thread: []db.Turn{
				{Role: "you", Text: oldLine, Kind: "ask"},
				{Role: "ora", Text: "the newest line", Kind: "ask"},
			},
			wantContains:    []string{"the newest line"},
			wantNotContains: []string{oldLine},
		},
		{
			name: "secret thread lines are redacted, ordinary ones survive",
			goal: "goal",
			thread: []db.Turn{
				{Role: "you", Text: "my password is hunter2", Kind: "ask"},
				{Role: "you", Text: "the key is at ~/.ssh/id_rsa", Kind: "ask"},
				{Role: "you", Text: "this line is fine", Kind: "ask"},
			},
			wantContains:    []string{"this line is fine"},
			wantNotContains: []string{"hunter2", "id_rsa"},
		},
		{
			name: "error-kind and empty turns are dropped",
			goal: "goal",
			thread: []db.Turn{
				{Role: "you", Text: "  ", Kind: "ask"},
				{Role: "ora", Text: "something went wrong", Kind: "error"},
				{Role: "you", Text: "the real question", Kind: "ask"},
			},
			wantContains:    []string{"the real question"},
			wantNotContains: []string{"something went wrong"},
		},
		{
			name:            "a secret in the goal itself is redacted",
			goal:            "my password is hunter2",
			wantNotContains: []string{"hunter2"},
		},
		{
			name:            "a secret personal-context line is redacted, the rest survives",
			goal:            "goal",
			personal:        "Personal context — things known for certain about the user:\n  password: hunter2\n  works on ora",
			wantContains:    []string{"works on ora"},
			wantNotContains: []string{"hunter2"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			brief := BuildBrief(c.goal, c.thread, c.personal)
			for _, want := range c.wantContains {
				if !strings.Contains(brief, want) {
					t.Errorf("brief missing %q: %s", want, brief)
				}
			}
			for _, unwanted := range c.wantNotContains {
				if strings.Contains(brief, unwanted) {
					t.Errorf("brief leaked %q: %s", unwanted, brief)
				}
			}
		})
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

// A cwd that does not exist, or is a regular file rather than a directory, fails before the runner is ever started, naming the problem.
func TestDelegate_RejectsMissingCWD(t *testing.T) {
	file := t.TempDir() + "/not-a-dir"
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{"/no/such/project/dir", file} {
		a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
		run := &fakeRunner{result: "done"}
		_, err := a.delegate(t.Context(), run, Delegation{Brief: "do it", CWD: cwd}, nil)
		if err == nil || !strings.Contains(err.Error(), cwd) {
			t.Errorf("cwd %q: err = %v, want it to name the bad cwd", cwd, err)
		}
		if run.gotPrompt != "" {
			t.Errorf("cwd %q: runner should never have been called", cwd)
		}
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

// One delegate call has to get four unrelated things right at once: the CWD passes straight through to the runner unchanged, the system prompt is the brief BuildBrief would have written for the same goal and thread, the prompt is the goal itself, and the runner's result comes back trimmed.
func TestDelegate_PassesCWDThrough(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{result: "  the answer  "}
	dir := t.TempDir()
	thread := []db.Turn{{Role: "you", Text: "earlier thing", Kind: "ask"}}
	result, err := a.delegate(t.Context(), run, Delegation{Brief: "fix the bug", CWD: dir}, thread)
	if err != nil {
		t.Fatal(err)
	}
	if run.gotCWD != dir {
		t.Errorf("cwd = %q, want %q", run.gotCWD, dir)
	}
	if run.gotPrompt != "fix the bug" {
		t.Errorf("prompt = %q", run.gotPrompt)
	}
	if !strings.Contains(run.gotSystemPrompt, "Goal: fix the bug") || !strings.Contains(run.gotSystemPrompt, "user: earlier thing") {
		t.Errorf("system prompt = %s", run.gotSystemPrompt)
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

// A runner error surfaces as-is, except when the context's own deadline is what ended the run, which is reported as a timeout saying how long it ran.
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

// delegateHandler with no brief argument, or one that is only whitespace, returns a toolError sentence rather than calling the runner or panicking; a blank brief is treated the same as a missing one.
func TestDelegateHandler_MissingBriefReturnsToolError(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	for _, args := range []map[string]any{{}, {"brief": "   "}} {
		got := delegateHandler(t.Context(), a, args)
		if !strings.HasPrefix(got, "error: ") {
			t.Errorf("args %v: got = %q, want an error: prefixed message", args, got)
		}
	}
}

// The goal reaches the delegate twice, in the brief and as the prompt on stdin, and a secret in it is stripped from both.
func TestDelegate_RedactsTheGoalOnStdin(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := &fakeRunner{result: "done"}
	if _, err := a.delegate(t.Context(), run, Delegation{Brief: "my password is hunter2, fix the migration", CWD: t.TempDir()}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(run.gotPrompt, "hunter2") {
		t.Errorf("secret goal reached the delegate's stdin: %q", run.gotPrompt)
	}
	if strings.Contains(run.gotSystemPrompt, "hunter2") {
		t.Errorf("secret goal reached the system prompt: %s", run.gotSystemPrompt)
	}
}

// A run a short deadline ended says how long it actually ran, in milliseconds, never "0s".
func TestDelegate_TimeoutNamesTheRealRunTime(t *testing.T) {
	if got := ranFor(20 * time.Millisecond); got.String() != "20ms" {
		t.Errorf("ranFor(20ms) = %s", got)
	}
	if got := ranFor(90*time.Second + 400*time.Millisecond); got.String() != "1m30s" {
		t.Errorf("ranFor(90.4s) = %s", got)
	}
}

// TestDelegate_CapsTheRunnerResult checks a chatty delegate run is cut to delegateResultBudget runes before it becomes a tool result, on a rune boundary and with the same marker the other tools use. Uncapped, the whole of `claude -p`'s stdout went into the next model prompt.
func TestDelegate_CapsTheRunnerResult(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	run := &fakeRunner{result: strings.Repeat("é", delegateResultBudget+500)}

	got, err := a.delegate(t.Context(), run, Delegation{Brief: "do it"}, nil)

	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if !utf8.ValidString(got) {
		t.Error("the truncated delegate result is not valid UTF-8")
	}
	if !strings.HasSuffix(got, "\n... (truncated)") {
		t.Fatal("expected the truncation marker on a result over the budget")
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "\n... (truncated)")); n != delegateResultBudget {
		t.Errorf("kept %d runes, want %d", n, delegateResultBudget)
	}
}
