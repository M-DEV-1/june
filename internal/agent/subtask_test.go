package agent

import (
	"context"
	"errors"

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
