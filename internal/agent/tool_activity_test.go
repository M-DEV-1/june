package agent

import (
	"context"
	"errors"

	"testing"
	"time"

	"google.golang.org/genai"
)

// --- pure helpers ---

func TestResultSummary(t *testing.T) {
	cases := []struct {
		name, result, want string
	}{
		{"query_memory", "error: query argument is required", "failed"},
		{"query_memory", "no memory matches", "0 hits"},
		{"recall", "no memory of that subject", "0 hits"},
		{"recall", "no episodes in that window", "0 hits"},
		{"query_memory", "[episode] foo\n[note] bar\n[summary] baz", "3 hits"},
		{"recall", "[Jan 2 15:04] Chrome — reddit: something", "1 hits"},
		{"shell_exec", "total 0\ndrwxr-xr-x", "done"},
		{"shell_exec", "error: command argument is required", "failed"},
		{"read_file", "file contents here", "done"},
		{"save_note", "saved", "saved"},
		{"save_note", "error: content argument is required", "failed"},
		{"revise", "updated", "updated"},
		{"revise", "fixed", "fixed"},
		{"branch", "Riddler kicked off last week, blocked on X", "done"},
		{"branch", "error: branch failed: gemini: unavailable", "failed"},
		// observe_screen and click carry the window line — never the accessibility listing itself — so a pass rule or the UI's live progress can see what window a look or a click actually landed on.
		{"observe_screen", "brave · PR #13 · GitHub\n[1] push button \"Merge\"", "brave · PR #13 · GitHub"},
		{"observe_screen", "brave · Inbox\n(nothing actionable is showing)", "brave · Inbox"},
		{"click", `clicked [1] push button "Merge" via press; the window is now "PR #13 · GitHub"; check it matches what was asked, then call observe_screen if you need the list`, `window now "PR #13 · GitHub"`},
		{"click", `clicked [1] push button "Merge" via press; call observe_screen to see the result`, "done"},
		{"click_at", `clicked 120,340 on the screen; the point lands on [2] link "Docs"; look or call observe_screen to see what it did`, "clicked 120,340 on the screen"},
		{"scroll_at", `scrolled 3 steps at 50,60; look or call observe_screen to see the page now`, "scrolled 3 steps at 50,60"},
		{"click_at", "error: could not click 120,340: no display", "failed"},
	}
	for _, c := range cases {
		got := resultSummary(c.name, c.result)
		if got != c.want {
			t.Errorf("resultSummary(%q, %q) = %q, want %q", c.name, c.result, got, c.want)
		}
	}
}

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

// TestRunToolCall_ToolActivityChanFull_DoesNotBlockToolExecution proves the activity events use the same non-blocking-send-with-default pattern as every other Agent channel (TextResponseChan, ErrorChan) — a UI that isn't draining ToolActivityChan must never be able to stall a real tool call.
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
