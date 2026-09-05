// codex.go adapts an already-built agent.CodexBrain into this package's Brain shape, so config.BrainCodex can answer for real once a caller has one to hand FromConfig.
package brain

import (
	"context"
	"fmt"
	"log/slog"

	"ora/internal/agent"
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

// WithCodexFallback returns a Brain that answers with primary and, when primary fails in a way no Gemini model can fix — 429 because the day's free-tier request allowance is spent, or 503 because every model is overloaded — asks fallback the same prompt instead. Input: the primary Brain and the Codex one to hand over to, which may be nil when the machine has no ChatGPT login. Output: primary's answer, or fallback's when primary could not answer at all.
// This is the same rule agent.AskText already applies to the questions the user asks and waits on, extended to the unattended jobs so a spent quota degrades them to Codex rather than losing the day's summaries and minutes outright.
// The rule that a fallback never happens after a tool has run is kept by construction: a Brain is one prompt in and one answer out with no tools in the seam, so there is never a tool call to repeat.
// A primary failure of any other kind is returned untouched, since it would fail the same way on any provider. When fallback fails too, the returned error names both, because the original quota failure is the one that explains the day.
func WithCodexFallback(primary, fallback Brain) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		reply, err := primary(ctx, prompt)
		if err == nil {
			return reply, nil
		}
		if fallback == nil || !agent.GeminiCannotAnswer(err) {
			return "", err
		}
		slog.Warn("background job handing over to codex", "error", err)
		fallbackReply, fallbackErr := fallback(ctx, prompt)
		if fallbackErr != nil {
			return "", fmt.Errorf("gemini could not answer (%v) and codex failed too: %w", err, fallbackErr)
		}
		return fallbackReply, nil
	}
}
