package agent

import (
	"context"
	"path/filepath"

	"os"
	"strings"
	"testing"
	"time"
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

// A cwd that does not exist, or is a regular file rather than a directory, fails before the runner is ever started.
func TestDelegate_RejectsMissingCWD(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{filepath.Join(dir, "no", "such", "dir"), file} {
		a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
		run := &fakeRunner{result: "done"}
		if _, err := a.delegate(t.Context(), run, Delegation{Brief: "do it", CWD: cwd}, nil); err == nil {
			t.Errorf("cwd %q: no error, want the bad cwd refused", cwd)
		}
		if run.gotPrompt != "" {
			t.Errorf("cwd %q: runner should never have been called", cwd)
		}
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
