package brain

import (
	"context"

	"june/internal/agent"
)

// fakeAsker is a CodexAsker whose answer, or error, is fixed in advance, so FromAsker and FromConfig can be tested without a real ChatGPT login.
type fakeAsker struct {
	trace agent.TurnTrace
	err   error
}

func (f fakeAsker) AskText(ctx context.Context, question string) (agent.TurnTrace, error) {
	return f.trace, f.err
}
