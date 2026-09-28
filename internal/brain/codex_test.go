package brain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"june/internal/agent"
	"june/internal/config"
)

// fakeAsker is a CodexAsker whose answer, or error, is fixed in advance, so FromAsker and FromConfig can be tested without a real ChatGPT login.
type fakeAsker struct {
	trace agent.TurnTrace
	err   error
}

func (f fakeAsker) AskText(ctx context.Context, question string) (agent.TurnTrace, error) {
	return f.trace, f.err
}

// FromAsker must hand back exactly the asker's answer text, and turn a failed or empty-answer ask into an error rather than an empty success.
func TestFromAsker(t *testing.T) {
	if got, err := FromAsker(fakeAsker{trace: agent.TurnTrace{Answer: "the answer"}})(context.Background(), "hi"); err != nil || got != "the answer" {
		t.Fatalf("got %q, err %v, want \"the answer\", nil", got, err)
	}
	if _, err := FromAsker(fakeAsker{err: errors.New("codex login: run `codex login` again")})(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("error = %v, want the asker's error to pass through", err)
	}
	if _, err := FromAsker(fakeAsker{trace: agent.TurnTrace{Answer: ""}})(context.Background(), "hi"); err == nil {
		t.Fatal("an empty answer must be an error, not an empty success")
	}
}

// FromConfig must route config.BrainCodex through the asker it is given instead of falling back to the Gemini API, which is the fix for POST /brains {"brain":"codex"} answering as gemini once a caller supplies one.
func TestFromConfig_CodexWithAsker(t *testing.T) {
	cfg := config.BrainConfig{Provider: config.BrainCodex}
	got, err := FromConfig(cfg, "", fakeAsker{trace: agent.TurnTrace{Answer: "from codex"}})(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "from codex" {
		t.Fatalf("got %q, want %q", got, "from codex")
	}
}
