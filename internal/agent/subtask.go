package agent

import (
	"context"
	"fmt"
	"log/slog"

	"ora/internal/config"

	"google.golang.org/genai"
)

// maxSubtaskIterations bounds a branched subtask's own model round trips — a runaway loop otherwise costs unbounded Gemini calls, and this is background work the user can't see happening.
const maxSubtaskIterations = 6

// maxBranchesPerSession bounds how many times branch() can be invoked in one live session — mirrors the paper's own "max branches per task" cap, so one conversation can't quietly rack up unbounded background Gemini calls.
const maxBranchesPerSession = 3

// subtaskModel is the subset of genai.Models' behavior runSubtask depends on.
// *genai.Client's Models field (type genai.Models) satisfies this structurally, so tests can drive a fake instead of a real network call — the same seam liveSession (connect.go) provides for the live session.
type subtaskModel interface {
	GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
}

// subtaskAllowedTools is the read-only, stateless tool subset a branched subtask may call.
// Deliberately excludes shell_exec (depends on live-session-only state: AllowedCmds, ToolApprovalChan HITL), save_note, open_url, and branch itself (no recursive branching).
var subtaskAllowedTools = map[string]bool{
	"query_memory": true,
	"recall":       true,
}

// subtaskTools filters toolDefinitions() (the single source of truth for every tool's schema) down to subtaskAllowedTools, so the subtask model is only ever offered — not just guarded against calling — the safe subset.
// The declarations go through stripLiveOnlyFields because runSubtask calls generateContent, which refuses a Live-session-only field.
func subtaskTools() []*genai.Tool {
	var decls []*genai.FunctionDeclaration
	for _, tool := range toolDefinitions() {
		for _, decl := range tool.FunctionDeclarations {
			if subtaskAllowedTools[decl.Name] {
				decls = append(decls, decl)
			}
		}
	}
	decls = stripLiveOnlyFields(decls)
	// Google Search rides along here and not on the live session. Pairing it with the Live API on a gemini-3 model closes the session with a quota error before the first word, so liveToolsFor leaves it out — which quietly removed Ora's only way to look anything up, while the prompt went on telling it to search for prices and current events. It reached for open_url instead and opened the user's browser.
	// branch runs on the standard Gemini API, where that pairing is ordinary, so the capability comes back here without a new tool.
	return []*genai.Tool{{FunctionDeclarations: decls}, {GoogleSearch: &genai.GoogleSearch{}}}
}

// tryReserveBranchSlot atomically claims one of maxBranchesPerSession branch slots for the current session, returning false once they're exhausted.
// CompareAndSwap loop rather than a plain Add-then-check: a counter merely read after an unconditional Add could overshoot the cap under concurrent branch calls before any caller observes the excess.
func (a *Agent) tryReserveBranchSlot() bool {
	for {
		cur := a.branchCalls.Load()
		if cur >= maxBranchesPerSession {
			return false
		}
		if a.branchCalls.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// surfacePendingFolds fetches every branch() result that missed its original live session (see runToolCall's dead-session fallback in connect.go) and returns them as context lines for the next handshake, marking each consumed so it surfaces exactly once.
// Fetch errors are logged and degrade to no lines rather than failing the handshake — a missed fold surfacing late is better than a broken connect.
func (a *Agent) surfacePendingFolds(ctx context.Context) []string {
	folds, err := a.brain.UnconsumedFolds(ctx)
	if err != nil {
		slog.Warn("failed to fetch pending folds", "error", err)
		return nil
	}
	var lines []string
	for _, f := range folds {
		lines = append(lines, fmt.Sprintf("  [while you were away] %s: %s", f.Task, f.Result))
		if err := a.brain.ConsumeFold(ctx, f.ID); err != nil {
			slog.Warn("failed to mark fold consumed", "fold_id", f.ID, "error", err)
		}
	}
	return lines
}

// defaultSubtaskModel lazily builds a second, independent *genai.Client (no Live/v1alpha options — same recipe as memory.GeminiSummarizer) and returns its Models field, which satisfies subtaskModel.
// Built once and reused for the process lifetime; a real network client is safe for concurrent use alongside the live session's own client (GenerateContent is a stateless REST-style call, unrelated to the websocket session object).
func (a *Agent) defaultSubtaskModel() (subtaskModel, error) {
	a.subtaskClientOnce.Do(func() {
		a.subtaskClient, a.subtaskClientErr = genai.NewClient(context.Background(), &genai.ClientConfig{
			APIKey:  a.apiKey,
			Backend: genai.BackendGeminiAPI,
		})
	})
	if a.subtaskClientErr != nil {
		return nil, a.subtaskClientErr
	}
	return a.subtaskClient.Models, nil
}

// runSubtask resolves one bounded task via its own request/response loop against model, separate from the live session.
// It returns the model's final plain-text answer once it stops calling tools, dispatching any FunctionCall in between through the same executeTool switch every other tool call in ora goes through.
func (a *Agent) runSubtask(ctx context.Context, model subtaskModel, task string) (string, error) {
	contents := []*genai.Content{genai.NewContentFromText(task, genai.RoleUser)}
	cfg := &genai.GenerateContentConfig{Tools: subtaskTools()}

	for i := 0; i < maxSubtaskIterations; i++ {
		resp, err := model.GenerateContent(ctx, config.TextModel, contents, cfg)
		if err != nil {
			return "", fmt.Errorf("subtask: generate content (iteration %d): %w", i, err)
		}
		if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
			return "", fmt.Errorf("subtask: empty response from model (iteration %d)", i)
		}
		contents = append(contents, resp.Candidates[0].Content)

		calls := resp.FunctionCalls()
		if len(calls) == 0 {
			parts := resp.Candidates[0].Content.Parts
			if len(parts) == 0 {
				return "", fmt.Errorf("subtask: model returned no text and no tool calls (iteration %d)", i)
			}
			// resp.Text() joins every non-thought text part — a plain parts[0].Text would
			// return "" whenever thinking puts an empty THOUGHT part first.
			text := resp.Text()
			if text == "" {
				return "", fmt.Errorf("subtask: model returned no usable text and no tool calls (iteration %d)", i)
			}
			return text, nil
		}

		var parts []*genai.Part
		for _, fc := range calls {
			var result string
			if subtaskAllowedTools[fc.Name] {
				result = a.executeTool(ctx, fc.Name, fc.Args)
			} else {
				result = fmt.Sprintf("error: tool %q is not available in this context", fc.Name)
			}
			parts = append(parts, genai.NewPartFromFunctionResponse(fc.Name, map[string]any{"output": result}))
		}
		contents = append(contents, genai.NewContentFromParts(parts, genai.RoleUser))
	}

	return "", fmt.Errorf("subtask exceeded %d iterations without returning a final answer", maxSubtaskIterations)
}
