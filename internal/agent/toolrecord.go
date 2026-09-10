// toolrecord.go carries the record of one tool call out of the agent, the same way toolObserverKey carries live progress out of it: through the context, so the agent states what happened without knowing there is a store on the other end.
// It is separate from ToolObserver on purpose. The observer is a progress hook for a window watching a turn happen, fires twice per call, and is allowed to be lossy. This is the durable record a later pass reads, fires once per call, and carries what the observer throws away — how long it took, how it ended, and what else the model could have called instead.
package agent

import (
	"context"
	"strings"
	"time"

	"google.golang.org/genai"
)

// ToolRecord is one finished tool call. Args and Result are the short summaries the activity feed already renders, never the raw text: one screen listing is over a thousand tokens and keeping it twice buys nothing. Offered is every tool the model could have called on this round, which is what lets a reader tell a tool that was never shown from one that was shown and passed over.
type ToolRecord struct {
	Name     string
	Args     string
	Outcome  string
	Result   string
	Duration time.Duration
	Offered  []string
}

// ToolRecorder is handed each finished tool call. Input: the record. Output: none — a recorder that cannot write must swallow its own error, because filing the record must never fail the tool.
type ToolRecorder func(ToolRecord)

type toolRecorderKey struct{}
type offeredKey struct{}

// WithToolRecorder attaches fn to ctx so every tool run under it is filed. Input: the parent context and the recorder, which may be nil. Output: a context carrying it.
func WithToolRecorder(ctx context.Context, fn ToolRecorder) context.Context {
	return context.WithValue(ctx, toolRecorderKey{}, fn)
}

// recorderFrom reads the recorder attached to ctx. Output: the recorder, or nil when there is none, which is the case in every test and on any path the daemon has not wired.
func recorderFrom(ctx context.Context) ToolRecorder {
	fn, _ := ctx.Value(toolRecorderKey{}).(ToolRecorder)
	return fn
}

// WithOffered names the tools the model was given on this round. Input: the parent context and the names. Output: a context carrying them.
// It is set where the declarations are chosen rather than where a tool is run, because that is the only place that knows what was left out — and what was left out is the whole point: a screen round is handed a trimmed set (see screenRoundTools), so a memory tool missing from a screen turn's record means the harness withheld it, not that the model did not want it.
func WithOffered(ctx context.Context, names []string) context.Context {
	return context.WithValue(ctx, offeredKey{}, names)
}

// toolNames lists every tool a request's tool set actually declares. Input: the tools as they go out to the backend. Output: their names, in declaration order.
func toolNames(tools []*genai.Tool) []string {
	var names []string
	for _, t := range tools {
		for _, d := range t.FunctionDeclarations {
			names = append(names, d.Name)
		}
	}
	return names
}

// declNames is toolNames for the loops that hold the declarations themselves rather than a tool set built from them. Input: the declarations. Output: their names, in order.
func declNames(decls []*genai.FunctionDeclaration) []string {
	names := make([]string, len(decls))
	for i, d := range decls {
		names[i] = d.Name
	}
	return names
}

// offeredFrom reads back the offered set. Output: the names, or nil when the round did not say.
func offeredFrom(ctx context.Context) []string {
	names, _ := ctx.Value(offeredKey{}).([]string)
	return names
}

// The three outcome classes a call is filed under. They exist so a later pass can ask a question the raw result string cannot answer: "refused" is the harness saying no, "error" is the tool trying and failing, and telling them apart is the difference between "this tool does not work" and "this tool was not allowed".
const (
	OutcomeOK      = "ok"
	OutcomeError   = "error"
	OutcomeRefused = "refused"
)

// The three fragments a refusal is built from, each one used by the code that writes the refusal so the two cannot drift apart. Classing by prose would go quietly wrong the first time someone reworded a sentence; classing by a fragment the sentence is assembled from cannot, because rewording it means editing the constant the producer itself uses.
const (
	// refuseApprovalMark is in every refusal from a tool that needed an approval nobody could give (see refuseApproval).
	refuseApprovalMark = "there's no way to ask for it here"
	// askGateMark is in the refusal a tool gets when the ask loop was never allowed to offer it (see ask.go).
	askGateMark = "is not available in an ask"
	// consentMark opens the sentence every stop-line refusal ends with (see consentPrompt), which is the one thing all of them share: the click, the keypress and the typed text each name a different action, but all of them ask for the same word back.
	consentMark = `Say "yes, `
)

// toolOutcome classes one tool's result string. Input: the string executeTool is about to return. Output: OutcomeRefused when the harness stopped the call, OutcomeError when the tool ran and failed, OutcomeOK otherwise.
// Refused is checked before error because a refusal is also an error string, and the two answer different questions: error means the tool is broken, refused means the tool was never let near the thing it was asked to do. Folding them together would read as a broken tool and send someone to fix code that works.
func toolOutcome(result string) string {
	switch {
	case strings.Contains(result, refuseApprovalMark), strings.Contains(result, askGateMark), strings.Contains(result, consentMark):
		return OutcomeRefused
	case strings.HasPrefix(result, "error: "):
		return OutcomeError
	}
	return OutcomeOK
}
