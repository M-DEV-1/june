package agent

import (
	"context"
	"errors"
	"fmt"
	"ora/internal/db"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// fakeSubtaskModel is a minimal subtaskModel for testing runSubtask without a real network call — same trick as fakeLiveSession in connect_test.go.
// responses/errs are consumed in order, one per GenerateContent call; calls records the contents passed each time so tests can assert on conversation state (e.g. that a FunctionResponse was appended before the next call).
type fakeSubtaskModel struct {
	responses []*genai.GenerateContentResponse
	errs      []error
	calls     [][]*genai.Content
	configs   []*genai.GenerateContentConfig
}

func (f *fakeSubtaskModel) GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	i := len(f.calls)
	f.calls = append(f.calls, contents)
	f.configs = append(f.configs, config)
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i >= len(f.responses) {
		return nil, fmt.Errorf("fakeSubtaskModel: no response queued for call %d", i)
	}
	return f.responses[i], nil
}

func textResponse(text string) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{Content: genai.NewContentFromText(text, genai.RoleModel)}},
	}
}

func functionCallResponse(name string, args map[string]any) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: genai.NewContentFromParts([]*genai.Part{genai.NewPartFromFunctionCall(name, args)}, genai.RoleModel),
		}},
	}
}

// TestRunSubtask_ReturnsPlainTextWhenNoToolCallsNeeded is the tracer bullet: the model answers the branched task directly with no tool calls at all.
// runSubtask must make exactly one GenerateContent call and return the model's text verbatim — proving the loop's exit path works end-to-end before any tool-dispatch behavior is added.
func TestRunSubtask_ReturnsPlainTextWhenNoToolCallsNeeded(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	model := &fakeSubtaskModel{
		responses: []*genai.GenerateContentResponse{
			textResponse("the answer is 42"),
		},
	}

	got, err := a.runSubtask(context.Background(), model, "what is the answer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "the answer is 42" {
		t.Errorf("runSubtask result = %q, want %q", got, "the answer is 42")
	}
	if len(model.calls) != 1 {
		t.Errorf("GenerateContent called %d times, want exactly 1", len(model.calls))
	}
}

// TestRunSubtask_SkipsThoughtPartAndReturnsRealAnswer verifies runSubtask does not blindly return parts[0].Text: with thinking enabled, Gemini routinely puts an empty THOUGHT part first, and the real answer in a later part. Returning parts[0].Text here would silently return "" as a successful answer.
func TestRunSubtask_SkipsThoughtPartAndReturnsRealAnswer(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: genai.NewContentFromParts([]*genai.Part{
				{Text: "", Thought: true},
				{Text: "real answer"},
			}, genai.RoleModel),
		}},
	}
	model := &fakeSubtaskModel{responses: []*genai.GenerateContentResponse{resp}}

	got, err := a.runSubtask(context.Background(), model, "what is the answer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "real answer" {
		t.Errorf("runSubtask result = %q, want %q", got, "real answer")
	}
}

// TestRunSubtask_JoinsMultiPartTextResponse verifies a response split across several non-thought text parts is joined in full, not truncated to the first part.
func TestRunSubtask_JoinsMultiPartTextResponse(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: genai.NewContentFromParts([]*genai.Part{
				{Text: "first half. "},
				{Text: "second half."},
			}, genai.RoleModel),
		}},
	}
	model := &fakeSubtaskModel{responses: []*genai.GenerateContentResponse{resp}}

	got, err := a.runSubtask(context.Background(), model, "what is the answer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "first half. second half." {
		t.Errorf("runSubtask result = %q, want %q", got, "first half. second half.")
	}
}

// TestRunSubtask_DispatchesFunctionCallThroughExecuteToolAndLoops verifies the core multi-hop behavior: when the model asks for a tool, runSubtask must actually run it (via the same executeTool dispatcher every other tool uses — proven here by asserting the brain's HybridSearch was really called) and feed the result back for a second round trip, rather than stopping or fabricating an answer.
func TestRunSubtask_DispatchesFunctionCallThroughExecuteToolAndLoops(t *testing.T) {
	brain := &toolTestBrain{
		hybridHits: []db.MemoryHit{{Source: "note", Content: "Riddler kicked off last week"}},
	}
	a := NewAgent(nil, nil, brain, nil, "")
	model := &fakeSubtaskModel{
		responses: []*genai.GenerateContentResponse{
			functionCallResponse("query_memory", map[string]any{"query": "Riddler"}),
			textResponse("Riddler kicked off last week, per your notes"),
		},
	}

	got, err := a.runSubtask(context.Background(), model, "catch me up on Riddler")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Riddler kicked off last week, per your notes" {
		t.Errorf("runSubtask result = %q", got)
	}
	if brain.capturedDomain != "" {
		t.Errorf("expected HybridSearch to have been invoked with empty domain, capturedDomain=%q — was it called at all?", brain.capturedDomain)
	}
	if len(model.calls) != 2 {
		t.Fatalf("GenerateContent called %d times, want exactly 2", len(model.calls))
	}

	// Second call's contents must carry a FunctionResponse part so the model
	// actually sees the tool's output, not just a bare repeat of the prompt.
	secondCallContents := model.calls[1]
	var sawFunctionResponse bool
	for _, c := range secondCallContents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "query_memory" {
				sawFunctionResponse = true
				if out, _ := p.FunctionResponse.Response["output"].(string); out == "" {
					t.Errorf("FunctionResponse output was empty")
				}
			}
		}
	}
	if !sawFunctionResponse {
		t.Errorf("second GenerateContent call did not include a query_memory FunctionResponse part")
	}
}

// TestRunSubtask_RespectsIterationCap verifies a subtask that never stops calling tools terminates with a clean error at maxSubtaskIterations, instead of looping (and burning Gemini calls) forever.
func TestRunSubtask_RespectsIterationCap(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")

	responses := make([]*genai.GenerateContentResponse, 0, maxSubtaskIterations+2)
	for i := 0; i < maxSubtaskIterations+2; i++ {
		responses = append(responses, functionCallResponse("query_memory", map[string]any{"query": "x"}))
	}
	model := &fakeSubtaskModel{responses: responses}

	_, err := a.runSubtask(context.Background(), model, "never-ending task")
	if err == nil {
		t.Fatal("expected an error when the model never stops calling tools, got nil")
	}
	if len(model.calls) != maxSubtaskIterations {
		t.Errorf("GenerateContent called %d times, want exactly maxSubtaskIterations (%d)", len(model.calls), maxSubtaskIterations)
	}
}

// TestRunSubtask_RejectsDisallowedToolName guards against a hypothetical model deviation: the subtask schema only advertises query_memory/recall, but nothing stops a model from emitting a FunctionCall for a tool outside that set anyway.
// shell_exec specifically must never reach executeTool from here — it depends on live-session-only state (AllowedCmds, ToolApprovalChan HITL) that doesn't exist in this background loop; running it for real would either hang forever waiting for an approval nobody can give, or silently execute a shell command the user never approved.
func TestRunSubtask_RejectsDisallowedToolName(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	model := &fakeSubtaskModel{
		responses: []*genai.GenerateContentResponse{
			functionCallResponse("shell_exec", map[string]any{"command": "rm -rf /"}),
			textResponse("done"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := a.runSubtask(ctx, model, "some task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "done" {
		t.Errorf("runSubtask result = %q, want %q", got, "done")
	}

	select {
	case req := <-a.ToolApprovalChan:
		t.Fatalf("shell_exec should never have been dispatched from a subtask, but got HITL request: %+v", req)
	default:
	}
}

// TestRunSubtask_PropagatesModelError verifies a GenerateContent failure surfaces as a clean error, not a panic or a swallowed empty result.
func TestRunSubtask_PropagatesModelError(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	wantErr := fmt.Errorf("gemini: rate limited")
	model := &fakeSubtaskModel{errs: []error{wantErr}}

	_, err := a.runSubtask(context.Background(), model, "some task")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error = %v, want it to wrap the underlying model error", err)
	}
}

// TestRunSubtask_HandlesEmptyCandidates verifies a response with zero Candidates (a real, documented possibility per the SDK, e.g. on a content filter block) returns a clean error instead of panicking on an out-of-range index into Candidates[0].
func TestRunSubtask_HandlesEmptyCandidates(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	model := &fakeSubtaskModel{
		responses: []*genai.GenerateContentResponse{{Candidates: nil}},
	}

	_, err := a.runSubtask(context.Background(), model, "some task")
	if err == nil {
		t.Fatal("expected an error for an empty-candidates response, got nil")
	}
}

// TestRunSubtask_HandlesEmptyPartsInFinalResponse verifies a response with a non-nil Content but zero Parts (e.g. a safety-filtered finish with no text) returns a clean error instead of panicking on an out-of-range index into Parts[0] — the same class of defensive check as the empty-Candidates case above, for the one Parts access that check doesn't cover.
func TestRunSubtask_HandlesEmptyPartsInFinalResponse(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	model := &fakeSubtaskModel{
		responses: []*genai.GenerateContentResponse{
			{Candidates: []*genai.Candidate{{Content: &genai.Content{Role: genai.RoleModel}}}},
		},
	}

	_, err := a.runSubtask(context.Background(), model, "some task")
	if err == nil {
		t.Fatal("expected an error for a response with empty Parts, got nil")
	}
}

// TestRunSubtask_TimesOutViaContext verifies a subtask that's still "thinking" when its context is cancelled returns promptly with an error, rather than blocking forever on a model call that will never return.
func TestRunSubtask_TimesOutViaContext(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	model := &blockingSubtaskModel{unblocked: make(chan struct{})}
	defer close(model.unblocked)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := a.runSubtask(ctx, model, "some task")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when the context times out mid-call, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("runSubtask took %v to return after a 50ms context timeout — it isn't respecting ctx", elapsed)
	}
}

// TestRunSubtask_OffersOnlyQueryMemoryAndRecallToTheModel verifies the model is actually given the query_memory/recall tool schemas to call — without this, a real Gemini model has no way to invoke them at all, silently defeating the whole multi-hop mechanism.
// Also asserts shell_exec/save_note/branch are NOT offered — this pins that they're not even offerable in the first place, on top of the earlier test guarding against them ever being called.
func TestRunSubtask_OffersOnlyQueryMemoryAndRecallToTheModel(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	model := &fakeSubtaskModel{
		responses: []*genai.GenerateContentResponse{textResponse("done")},
	}

	if _, err := a.runSubtask(context.Background(), model, "some task"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(model.configs) != 1 || model.configs[0] == nil || len(model.configs[0].Tools) == 0 {
		t.Fatalf("expected GenerateContent to be called with a non-empty Tools config, got %+v", model.configs)
	}
	var gotNames []string
	for _, tool := range model.configs[0].Tools {
		for _, decl := range tool.FunctionDeclarations {
			gotNames = append(gotNames, decl.Name)
		}
	}
	want := map[string]bool{"query_memory": true, "recall": true, "get_recent": true}
	if len(gotNames) != len(want) {
		t.Fatalf("offered tools = %v, want exactly %v", gotNames, want)
	}
	for _, name := range gotNames {
		if !want[name] {
			t.Errorf("offered disallowed tool %q to the subtask model", name)
		}
	}
}

// blockingSubtaskModel simulates a GenerateContent call that hangs until the caller's context is done (mirroring a real network call blocked on a slow server), so TestRunSubtask_TimesOutViaContext can assert on ctx-driven cancellation without a real timeout race.
type blockingSubtaskModel struct {
	unblocked chan struct{}
}

func (m *blockingSubtaskModel) GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.unblocked:
		return nil, fmt.Errorf("test cleanup: model unblocked without a real response")
	}
}

// TestToolDefinitions_IncludesBranchWithRequiredTask verifies the branch tool is actually offered to the live model, with "task" required — the live model's own dispatch path (branch → runSubtask) is meaningless if the live model never sees the tool exists at all.
func TestToolDefinitions_IncludesBranchWithRequiredTask(t *testing.T) {
	for _, tool := range toolDefinitions() {
		for _, decl := range tool.FunctionDeclarations {
			if decl.Name != "branch" {
				continue
			}
			if decl.Parameters == nil || len(decl.Parameters.Required) != 1 || decl.Parameters.Required[0] != "task" {
				t.Errorf("branch tool Required = %v, want exactly [\"task\"]", decl.Parameters.Required)
			}
			return
		}
	}
	t.Fatal("toolDefinitions() does not include a \"branch\" tool")
}

// TestExecuteTool_BranchRequiresTaskArg verifies the same "error: ..." string convention every other tool uses for a missing required arg.
func TestExecuteTool_BranchRequiresTaskArg(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")

	want := "error: branch needs the question to work on"
	got := a.executeTool(context.Background(), "branch", map[string]any{})
	if got != want {
		t.Errorf("executeTool(branch, {}) = %q, want %q", got, want)
	}
}

// TestExecuteTool_BranchDispatchesThroughSubtaskModelFactory verifies executeTool's "branch" case actually runs the side-call loop (via the injectable subtaskModelFactory seam) and returns its folded result verbatim — proving the live-model-facing tool is wired to runSubtask end-to-end, not just validating its args.
func TestExecuteTool_BranchDispatchesThroughSubtaskModelFactory(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fake := &fakeSubtaskModel{responses: []*genai.GenerateContentResponse{textResponse("folded answer")}}
	a.subtaskModelFactory = func() (subtaskModel, error) { return fake, nil }

	got := a.executeTool(context.Background(), "branch", map[string]any{"task": "catch me up"})
	if got != "folded answer" {
		t.Errorf("executeTool(branch, ...) = %q, want %q", got, "folded answer")
	}
}

// TestExecuteTool_BranchRejectsCallsPastPerSessionCap verifies a live session can't rack up unbounded background Gemini calls through branch — mirroring the paper's own "max branches per task" bound. The (cap+1)th call must be rejected before it ever reaches the subtask model.
func TestExecuteTool_BranchRejectsCallsPastPerSessionCap(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	responses := make([]*genai.GenerateContentResponse, maxBranchesPerSession)
	for i := range responses {
		responses[i] = textResponse(fmt.Sprintf("answer %d", i))
	}
	fake := &fakeSubtaskModel{responses: responses}
	a.subtaskModelFactory = func() (subtaskModel, error) { return fake, nil }

	for i := 0; i < maxBranchesPerSession; i++ {
		got := a.executeTool(context.Background(), "branch", map[string]any{"task": "task"})
		if strings.HasPrefix(got, "error") {
			t.Fatalf("call %d: unexpected rejection before the cap: %q", i, got)
		}
	}

	got := a.executeTool(context.Background(), "branch", map[string]any{"task": "one too many"})
	if !strings.HasPrefix(got, "error") {
		t.Errorf("call past cap = %q, want an error", got)
	}
	if len(fake.calls) != maxBranchesPerSession {
		t.Errorf("subtask model invoked %d times, want exactly maxBranchesPerSession (%d) — the capped call must never reach the model", len(fake.calls), maxBranchesPerSession)
	}
}

// TestAgentConnect_ResetsBranchCallCounterAtSessionStart verifies the per-session branch cap is actually per-session, not permanent for the process's lifetime — a fresh Connect() (reconnect or new session) must reset the counter so branch isn't silently dead for the rest of the program after one long conversation used up its slots.
func TestAgentConnect_ResetsBranchCallCounterAtSessionStart(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "FAKE_API_KEY")
	a.branchCalls.Store(int32(maxBranchesPerSession))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = a.Connect(ctx, nil) // expected to fail fast on a bad key; the reset must happen before that failure

	if got := a.branchCalls.Load(); got != 0 {
		t.Errorf("branchCalls after Connect = %d, want 0 — the session-scoped counter must reset on every Connect()", got)
	}
}

// TestReceiveLoop_BranchCall_DeliversLiveWhenSessionAlive drives receiveLoop with a real "branch" ToolCall (the same path a live Gemini session would take) and asserts the folded result is delivered back via SendToolResponse, matched by fc.ID/fc.Name — proving branch is dispatched end-to-end through receiveLoop, not just reachable via executeTool directly.
// Also asserts no fallback persistence happens on this happy path (pins the "only the dead-session fallback persists" design decision).
func TestReceiveLoop_BranchCall_DeliversLiveWhenSessionAlive(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "")
	fake := &fakeSubtaskModel{responses: []*genai.GenerateContentResponse{textResponse("Riddler kicked off last week")}}
	a.subtaskModelFactory = func() (subtaskModel, error) { return fake, nil }

	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 2),
		responses: make(chan genai.LiveSendToolResponseParameters, 2),
		closeErr:  errors.New("fake session closed"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{
		ToolCall: &genai.LiveServerToolCall{
			FunctionCalls: []*genai.FunctionCall{
				{ID: "call-branch", Name: "branch", Args: map[string]any{"task": "catch me up on Riddler"}},
			},
		},
	}

	select {
	case resp := <-fs.responses:
		fr := resp.FunctionResponses[0]
		if fr.ID != "call-branch" || fr.Name != "branch" {
			t.Fatalf("expected ID=call-branch Name=branch, got ID=%q Name=%q", fr.ID, fr.Name)
		}
		if fr.Response["output"] != "Riddler kicked off last week" {
			t.Fatalf("unexpected delivered output: %v", fr.Response["output"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the branch call's response")
	}

	if brain.savedFoldTask != "" {
		t.Errorf("expected no fallback persistence on the live-delivery happy path, got savedFoldTask=%q", brain.savedFoldTask)
	}
}

// blockUntilReleasedModel signals started once GenerateContent is entered, then blocks until release is closed before returning text — lets a test deterministically kill the session while a branch subtask is genuinely still in flight, without racing a real timing window.
type blockUntilReleasedModel struct {
	started chan struct{}
	release chan struct{}
	text    string
}

func (m *blockUntilReleasedModel) GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	close(m.started)
	<-m.release
	return textResponse(m.text), nil
}

// TestRunToolCall_BranchDeadSession_PersistsFoldInsteadOfDropping is the load-bearing test for the dead-connection design decision: a branch subtask that's still running when its live session dies must not have its result silently dropped the way every other tool's does today (connect.go's ctx.Err() check) — it must persist via SaveFold so it surfaces at the next handshake instead.
func TestRunToolCall_BranchDeadSession_PersistsFoldInsteadOfDropping(t *testing.T) {
	brain := &toolTestBrain{foldSaved: make(chan struct{}, 1)}
	a := NewAgent(nil, nil, brain, nil, "")
	model := &blockUntilReleasedModel{
		started: make(chan struct{}),
		release: make(chan struct{}),
		text:    "Riddler kicked off last week",
	}
	a.subtaskModelFactory = func() (subtaskModel, error) { return model, nil }

	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 2),
		responses: make(chan genai.LiveSendToolResponseParameters, 2),
		closeErr:  errors.New("fake session closed"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{
		ToolCall: &genai.LiveServerToolCall{
			FunctionCalls: []*genai.FunctionCall{
				{ID: "call-branch", Name: "branch", Args: map[string]any{"task": "catch me up on Riddler"}},
			},
		},
	}

	select {
	case <-model.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the subtask model call to start")
	}

	// The session dies while the subtask is still genuinely in flight.
	cancel()

	// Now let the subtask finish — well after the session is gone.
	close(model.release)

	select {
	case resp := <-fs.responses:
		t.Fatalf("expected no live delivery for a dead session, got %+v", resp)
	case <-time.After(500 * time.Millisecond):
	}

	select {
	case <-brain.foldSaved:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fallback SaveFold call")
	}
	if brain.savedFoldTask != "catch me up on Riddler" || brain.savedFoldResult != "Riddler kicked off last week" {
		t.Errorf("fallback persistence wrong: task=%q result=%q", brain.savedFoldTask, brain.savedFoldResult)
	}
}

// TestReceiveLoop_BranchCall_InnerToolCallsDoNotEmitToolActivity pins the visibility contract: a branch subtask's inner query_memory/recall calls are synthetic (no real live fc.ID to correlate a Started/Finished pair against) and must never appear on ToolActivityChan — only the outer branch call itself, once, correlated to the real live fc.ID.
// Otherwise the UI would see the messy multi-hop internals this whole mechanism exists to hide.
func TestReceiveLoop_BranchCall_InnerToolCallsDoNotEmitToolActivity(t *testing.T) {
	brain := &toolTestBrain{hybridHits: []db.MemoryHit{{Source: "note", Content: "x"}}}
	a := NewAgent(nil, nil, brain, nil, "")
	fake := &fakeSubtaskModel{
		responses: []*genai.GenerateContentResponse{
			functionCallResponse("query_memory", map[string]any{"query": "a"}),
			functionCallResponse("query_memory", map[string]any{"query": "b"}),
			textResponse("done"),
		},
	}
	a.subtaskModelFactory = func() (subtaskModel, error) { return fake, nil }

	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 2),
		responses: make(chan genai.LiveSendToolResponseParameters, 2),
		closeErr:  errors.New("fake session closed"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{
		ToolCall: &genai.LiveServerToolCall{
			FunctionCalls: []*genai.FunctionCall{
				{ID: "call-branch", Name: "branch", Args: map[string]any{"task": "multi-hop task"}},
			},
		},
	}

	select {
	case <-fs.responses:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the branch call to complete")
	}

	var events []ToolActivity
	drain := time.After(200 * time.Millisecond)
loop:
	for {
		select {
		case ev := <-a.ToolActivityChan:
			events = append(events, ev)
		case <-drain:
			break loop
		}
	}

	if len(events) != 2 {
		t.Fatalf("ToolActivityChan events = %+v, want exactly 2 (Started+Finished for branch only)", events)
	}
	for _, ev := range events {
		if ev.ID != "call-branch" || ev.Name != "branch" {
			t.Errorf("unexpected event leaked onto ToolActivityChan: %+v", ev)
		}
	}
	if events[0].Phase != ToolStarted || events[1].Phase != ToolFinished {
		t.Errorf("expected Started then Finished, got phases %v, %v", events[0].Phase, events[1].Phase)
	}
}

// TestSurfacePendingFolds_ReturnsLinesAndMarksConsumed is the tracer bullet for next-session surfacing: unconsumed folds must come back as human-readable context lines AND be marked consumed, so a branch result that missed its original session surfaces exactly once at the next one.
func TestSurfacePendingFolds_ReturnsLinesAndMarksConsumed(t *testing.T) {
	brain := &toolTestBrain{
		unconsumedFolds: []db.Fold{
			{ID: 7, Task: "catch me up on Riddler", Result: "kicked off last week"},
			{ID: 9, Task: "find the blocker", Result: "waiting on review"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "")

	lines := a.surfacePendingFolds(context.Background())

	if len(lines) != 2 {
		t.Fatalf("surfacePendingFolds returned %d lines, want 2: %v", len(lines), lines)
	}
	for _, want := range []string{"catch me up on Riddler", "kicked off last week", "find the blocker", "waiting on review"} {
		var found bool
		for _, l := range lines {
			if strings.Contains(l, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no line contained %q, got %v", want, lines)
		}
	}

	if len(brain.consumedFoldIDs) != 2 || brain.consumedFoldIDs[0] != 7 || brain.consumedFoldIDs[1] != 9 {
		t.Errorf("consumedFoldIDs = %v, want [7 9]", brain.consumedFoldIDs)
	}
}

// TestExecuteTool_BranchSurfacesSubtaskFailureAsErrorString verifies a failed side-call (e.g. the model API erroring) surfaces through executeTool's usual "error: ..." string convention rather than as a bare Go error reaching the model, matching resultSummary/ToolActivity.Err's strings.HasPrefix(result, "error") check.
func TestExecuteTool_BranchSurfacesSubtaskFailureAsErrorString(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fake := &fakeSubtaskModel{errs: []error{fmt.Errorf("gemini: unavailable")}}
	a.subtaskModelFactory = func() (subtaskModel, error) { return fake, nil }

	got := a.executeTool(context.Background(), "branch", map[string]any{"task": "catch me up"})
	if !strings.HasPrefix(got, "error") {
		t.Errorf("executeTool(branch, ...) = %q, want it to start with %q", got, "error")
	}
}

// TestSubtaskTools_StripsBehavior covers why branch() has been silently dead: the declarations it reuses from toolDefinitions() carry Behavior=NON_BLOCKING, which only BidiGenerateContent accepts. generateContent — the API runSubtask actually calls — rejects the whole request with "FunctionDeclaration.behavior only supported by BidiGenerateContent", so every branch call failed before it ran a single search.
func TestSubtaskTools_StripsBehavior(t *testing.T) {
	tools := subtaskTools()
	if len(tools) == 0 || len(tools[0].FunctionDeclarations) == 0 {
		t.Fatal("expected the subtask tool subset to be non-empty")
	}
	for _, decl := range tools[0].FunctionDeclarations {
		if decl.Behavior != "" {
			t.Errorf("declaration %q carries Behavior %q, which generateContent rejects", decl.Name, decl.Behavior)
		}
	}
	// The live session's own declarations must keep it — NON_BLOCKING there is what stops a memory lookup from freezing the conversation.
	for _, tool := range toolDefinitions() {
		for _, decl := range tool.FunctionDeclarations {
			if decl.Behavior != genai.BehaviorNonBlocking {
				t.Errorf("live declaration %q lost its NON_BLOCKING behavior", decl.Name)
			}
		}
	}
}
