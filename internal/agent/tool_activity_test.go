package agent

import (
	"context"
	"errors"

	"testing"
	"time"

	"google.golang.org/genai"
)

// --- pure helpers ---

// --- channel emission, driven through receiveLoop like the existing P1 tests ---

// TestReceiveLoop_ToolCall_EmitsStartedThenFinishedActivity proves runToolCall reports both lifecycle events on ToolActivityChan, in order, correlated by ID — the plumbing that lets the UI show a live "tool running" status and then collapse it into a permanent transcript entry.
func TestReceiveLoop_ToolCall_EmitsStartedThenFinishedActivity(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")

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
				{ID: "call-1", Name: "list_files", Args: map[string]any{"path": "."}},
			},
		},
	}

	var started, finished ToolActivity
	select {
	case started = <-a.ToolActivityChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the Started activity event")
	}
	if started.Phase != ToolStarted {
		t.Fatalf("expected first event to be ToolStarted, got %v", started.Phase)
	}
	if started.ID != "call-1" || started.Name != "list_files" {
		t.Fatalf("unexpected Started event: %+v", started)
	}
	if started.ArgsSummary != `"."` {
		t.Errorf("expected ArgsSummary %q, got %q", `"."`, started.ArgsSummary)
	}

	select {
	case finished = <-a.ToolActivityChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the Finished activity event")
	}
	if finished.Phase != ToolFinished {
		t.Fatalf("expected second event to be ToolFinished, got %v", finished.Phase)
	}
	if finished.ID != started.ID {
		t.Fatalf("Finished.ID %q does not correlate with Started.ID %q", finished.ID, started.ID)
	}
	if !finished.Started.Equal(started.Started) {
		t.Errorf("expected Finished.Started to echo Started.Started (%v), got %v", started.Started, finished.Started)
	}
	if finished.ResultSummary == "" {
		t.Error("expected a non-empty ResultSummary on the Finished event")
	}
	if finished.Err {
		t.Error("listing '.' should succeed, expected Err=false")
	}
}

// TestRunToolCall_ToolActivityChanFull_DoesNotBlockToolExecution proves the activity events use the same non-blocking-send-with-default pattern as TextResponseChan — a UI that isn't draining ToolActivityChan must never be able to stall a real tool call.
func TestRunToolCall_ToolActivityChanFull_DoesNotBlockToolExecution(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")

	// Fill the channel to capacity so any blocking send would hang forever.
	for i := 0; i < cap(a.ToolActivityChan); i++ {
		a.ToolActivityChan <- ToolActivity{ID: "filler"}
	}

	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 1),
		responses: make(chan genai.LiveSendToolResponseParameters, 1),
		closeErr:  errors.New("fake session closed"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	fs.msgCh <- &genai.LiveServerMessage{
		ToolCall: &genai.LiveServerToolCall{
			FunctionCalls: []*genai.FunctionCall{
				{ID: "call-2", Name: "list_files", Args: map[string]any{"path": "."}},
			},
		},
	}

	select {
	case resp := <-fs.responses:
		if resp.FunctionResponses[0].ID != "call-2" {
			t.Fatalf("unexpected response ID %q", resp.FunctionResponses[0].ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the tool response — a full ToolActivityChan appears to have blocked runToolCall")
	}
}
