package agent

import (
	"context"
	"errors"

	"june/internal/db"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// TestExecuteTool_BranchFallsBackToARoutedWebAsk verifies that when the direct search call fails (no API key configured, or the provider errors), branch hands the task to a brain with its own web search instead of telling the live model the web is out of reach.
func TestExecuteTool_BranchFallsBackToARoutedWebAsk(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	a.webSearch = func(ctx context.Context, task string) (string, error) {
		return "", errors.New("web search: no search API configured")
	}
	asked := ""
	a.webAsk = func(ctx context.Context, task string) (string, error) {
		asked = task
		return "keyword spotting is a small model that listens for a fixed phrase", nil
	}
	got := a.executeTool(context.Background(), "branch", map[string]any{"task": "explain keyword spotting"})
	if asked != "explain keyword spotting" {
		t.Errorf("routed ask got %q, want the branch task", asked)
	}
	if got != "keyword spotting is a small model that listens for a fixed phrase" {
		t.Errorf("result = %q, want the routed answer", got)
	}
}

// TestReceiveLoop_BranchCall_DeliversLiveWhenSessionAlive drives receiveLoop with a real "branch" ToolCall (the same path a live Gemini session would take) and asserts the result is delivered back via SendToolResponse, matched by fc.ID/fc.Name — proving branch is dispatched end-to-end through receiveLoop, not just reachable via executeTool directly.
// Also asserts no fallback persistence happens on this happy path (pins the "only the dead-session fallback persists" design decision).
func TestReceiveLoop_BranchCall_DeliversLiveWhenSessionAlive(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "")
	a.webSearch = func(ctx context.Context, task string) (string, error) { return "Riddler kicked off last week", nil }

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

// TestRunToolCall_BranchDeadSession_PersistsFoldInsteadOfDropping is the load-bearing test for the dead-connection design decision: a branch search that's still running when its live session dies must not have its result silently dropped the way every other tool's does today (connect.go's ctx.Err() check) — it must persist via SaveFold so it surfaces at the next handshake instead.
func TestRunToolCall_BranchDeadSession_PersistsFoldInsteadOfDropping(t *testing.T) {
	brain := &toolTestBrain{foldSaved: make(chan struct{}, 1)}
	a := NewAgent(nil, nil, brain, nil, "")
	started := make(chan struct{})
	release := make(chan struct{})
	a.webSearch = func(ctx context.Context, task string) (string, error) {
		close(started)
		<-release
		return "Riddler kicked off last week", nil
	}

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
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the search call to start")
	}

	// The session dies while the search is still genuinely in flight.
	cancel()

	// Now let the search finish — well after the session is gone.
	close(release)

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
