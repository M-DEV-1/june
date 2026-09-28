// codex.go adapts an already-built agent.CodexBrain into this package's Brain shape, so config.BrainCodex can answer for real once a caller has one to hand FromConfig.
package brain

import (
	"context"
	"fmt"

	"june/internal/agent"
)

// CodexAsker is what an already-built *agent.Agent offers through its CodexBrain wrapper: ask one question, get back the full turn trace. FromConfig accepts one of these as an optional argument because building an *agent.Agent needs a microphone, a speaker and a memory compiler that FromConfig's own inputs (a BrainConfig and a Gemini API key) do not carry — only a caller that already holds an *Agent, such as the daemon, can supply one.
type CodexAsker interface {
	AskText(ctx context.Context, question string) (agent.TurnTrace, error)
}

// FromAsker wraps a CodexAsker as a Brain, discarding everything but the answer text so it matches what every other constructor in this package returns. Output: the answer, or an error when the ask failed or came back with no text.
func FromAsker(a CodexAsker) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		tr, err := a.AskText(ctx, prompt)
		if err != nil {
			return "", err
		}
		if tr.Answer == "" {
			return "", fmt.Errorf("codex returned no text")
		}
		return tr.Answer, nil
	}
}
