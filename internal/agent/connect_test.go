package agent

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/genai"
)

// TestTurnContext_CarriesFreshTime verifies the per-turn context injected before each user message carries the CURRENT time — the system prompt is frozen at handshake, so "now" has to ride in every turn or a long conversation drifts (stuck at connect time, wrong date past midnight).
func TestTurnContext_CarriesFreshTime(t *testing.T) {
	ist := time.FixedZone("IST", int(5.5*3600))
	now := time.Date(2026, 7, 6, 14, 30, 0, 0, ist) // a Monday afternoon

	got := turnContext(now, nil)

	for _, want := range []string{"Monday", "14:30"} {
		if !strings.Contains(got, want) {
			t.Errorf("turnContext(no recalls) = %q, missing %q", got, want)
		}
	}
}

// TestTurnContext_IncludesRecalls verifies that when relevant memory is retrieved for a turn, it rides in the same context payload as the time.
func TestTurnContext_IncludesRecalls(t *testing.T) {
	now := time.Date(2026, 7, 6, 14, 30, 0, 0, time.UTC)

	got := turnContext(now, []string{"working on the ora recall tool"})

	if !strings.Contains(got, "working on the ora recall tool") {
		t.Errorf("turnContext = %q, missing recall content", got)
	}
	if !strings.Contains(got, "14:30") {
		t.Errorf("turnContext = %q, should still carry the time alongside recalls", got)
	}
}

// TestBuildTurnContent_SingleTurnWithContextAndText verifies buildTurnContent packs the turn context and user text into ONE Content/turn as two Parts, not two separate SendClientContent calls — the old two-call shape let the model reply to the bare context line, doubling Live API round-trips.
func TestBuildTurnContent_SingleTurnWithContextAndText(t *testing.T) {
	now := time.Date(2026, 7, 6, 14, 30, 0, 0, time.UTC)
	recalls := []string{"working on the ora recall tool"}

	turns := buildTurnContent(now, recalls, "what time is it")

	if len(turns) != 1 {
		t.Fatalf("expected exactly one Content/turn, got %d", len(turns))
	}
	parts := turns[0].Parts
	if len(parts) != 2 {
		t.Fatalf("expected exactly 2 parts (context + user text) in the single turn, got %d: %+v", len(parts), parts)
	}
	if !strings.Contains(parts[0].Text, "14:30") || !strings.Contains(parts[0].Text, "working on the ora recall tool") {
		t.Errorf("expected first part to carry the turnContext (time + recalls), got %q", parts[0].Text)
	}
	if parts[1].Text != "what time is it" {
		t.Errorf("expected second part to be the verbatim user text, got %q", parts[1].Text)
	}
}

// TestNowAnchor_EncodesCurrentMoment verifies nowAnchor renders the weekday, calendar date, wall-clock time, and timezone — the temporal anchor injected into the system prompt so the model isn't blind to "now" (it was previously seen confusing the date and deriving IST by hand).
func TestNowAnchor_EncodesCurrentMoment(t *testing.T) {
	ist := time.FixedZone("IST", int(5.5*3600))
	now := time.Date(2026, 7, 6, 12, 44, 0, 0, ist) // a Monday

	anchor := nowAnchor(now)

	for _, want := range []string{"Monday", "2026", "12:44", "IST"} {
		if !strings.Contains(anchor, want) {
			t.Errorf("nowAnchor(%v) = %q, missing %q", now, anchor, want)
		}
	}
}

// fakeLiveSession is a minimal liveSession for testing receiveLoop's concurrency behavior without a live websocket.
// msgCh feeds messages in order; Receive blocks until one is available and returns closeErr once msgCh is closed (mirrors session.Receive() erroring after the connection drops).
// SendToolResponse records what would have been sent back to the model.
type fakeLiveSession struct {
	msgCh     chan *genai.LiveServerMessage
	responses chan genai.LiveSendToolResponseParameters
	closeErr  error
}

func (f *fakeLiveSession) Receive() (*genai.LiveServerMessage, error) {
	msg, ok := <-f.msgCh
	if !ok {
		return nil, f.closeErr
	}
	return msg, nil
}

func (f *fakeLiveSession) SendToolResponse(p genai.LiveSendToolResponseParameters) error {
	f.responses <- p
	return nil
}

// TestReceiveLoop_ToolCallDoesNotBlockReceivePath proves a slow/blocking tool call (e.g. HITL shell_exec awaiting approval) doesn't stall session.Receive() and drop mic frames — executeTool used to run synchronously inline in receiveLoop, blocking the whole receive path for as long as the tool took.
//
// Drives a real blocking shell_exec (not in the allowlist, so it genuinely blocks on ToolApprovalChan/ResultChan) followed immediately by a fast tool call, and asserts the fast call's result comes back first. Under the old synchronous code this would hang until the 2s timeout instead.
func TestReceiveLoop_ToolCallDoesNotBlockReceivePath(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")

	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 4),
		responses: make(chan genai.LiveSendToolResponseParameters, 4),
		closeErr:  errors.New("fake session closed"),
	}

	errChan := make(chan error, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		a.receiveLoop(ctx, fs, "test-model", errChan)
		close(done)
	}()

	// 1. Slow call: shell_exec on an unapproved command blocks in executeTool
	// on <-resChan until the test sends an approval result below.
	fs.msgCh <- &genai.LiveServerMessage{
		ToolCall: &genai.LiveServerToolCall{
			FunctionCalls: []*genai.FunctionCall{
				{ID: "call-slow", Name: "shell_exec", Args: map[string]any{"command": "echo never-approved"}},
			},
		},
	}

	// 2. Fast call, sent right after with no delay. Must complete without
	// waiting on the slow call above.
	fs.msgCh <- &genai.LiveServerMessage{
		ToolCall: &genai.LiveServerToolCall{
			FunctionCalls: []*genai.FunctionCall{
				{ID: "call-fast", Name: "list_files", Args: map[string]any{"path": "."}},
			},
		},
	}

	select {
	case resp := <-fs.responses:
		got := resp.FunctionResponses[0].ID
		if got != "call-fast" {
			t.Fatalf("expected the fast call's response to arrive first, got ID %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fast tool call's response - receiveLoop appears blocked by the still-pending slow tool call")
	}

	// Now resolve the slow call's HITL approval.
	var req ToolRequest
	select {
	case req = <-a.ToolApprovalChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the slow tool call's HITL approval request")
	}
	if req.Command != "echo never-approved" {
		t.Fatalf("unexpected approval request command %q", req.Command)
	}
	req.ResultChan <- "ok: approved and ran"

	select {
	case resp := <-fs.responses:
		fr := resp.FunctionResponses[0]
		if fr.ID != "call-slow" || fr.Name != "shell_exec" {
			t.Fatalf("expected slow call's response (ID=call-slow, Name=shell_exec), got ID=%q Name=%q", fr.ID, fr.Name)
		}
		if fr.Response["output"] != "ok: approved and ran" {
			t.Fatalf("expected slow call's approved result to be delivered, got %v", fr.Response["output"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the slow tool call's response after approval")
	}

	close(fs.msgCh)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("receiveLoop did not return after the session closed")
	}
}

// TestRunToolCall_SessionEndsBeforeApproval_GoroutineExitsInsteadOfLeaking covers a HITL-blocked runToolCall goroutine that used to leak forever if the session disconnected before the user approved/rejected — fixed by threading ctx through the `<-resChan` wait so it can also select on ctx.Done() (Connect()'s sessCancel() is what fires it in production).
//
// Drives a real blocking HITL call, confirms it's genuinely parked awaiting approval, then cancels the session context WITHOUT resolving the approval — the goroutine must exit on its own and must not try to deliver a response into a session that's gone.
func TestRunToolCall_SessionEndsBeforeApproval_GoroutineExitsInsteadOfLeaking(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")

	fs := &fakeLiveSession{
		msgCh:     make(chan *genai.LiveServerMessage, 2),
		responses: make(chan genai.LiveSendToolResponseParameters, 2),
		closeErr:  errors.New("fake session closed"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go a.receiveLoop(ctx, fs, "test-model", make(chan error, 2))

	runtime.GC()
	baseline := runtime.NumGoroutine()

	fs.msgCh <- &genai.LiveServerMessage{
		ToolCall: &genai.LiveServerToolCall{
			FunctionCalls: []*genai.FunctionCall{
				{ID: "call-abandoned", Name: "shell_exec", Args: map[string]any{"command": "echo never-approved-2"}},
			},
		},
	}

	// Confirm it's genuinely blocked awaiting HITL approval (not already
	// finished) before cutting the session out from under it.
	select {
	case <-a.ToolApprovalChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the abandoned call's HITL approval request")
	}

	// Simulate the session dying before the user ever responds — never send
	// anything on ResultChan.
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if runtime.NumGoroutine() <= baseline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count stayed above pre-call baseline %d (currently %d) 2s after session cancellation — the HITL goroutine leaked", baseline, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case resp := <-fs.responses:
		t.Fatalf("expected no response delivered for a session that ended before approval, got: %+v", resp)
	default:
	}
}

// TestTruncateUTF8_SplitsExactlyOnMultiByteRuneBoundary verifies truncating mid multi-byte UTF-8 char (e.g. the 3-byte U+FFFC object-replacement char a11y capture is full of) backs off to the last complete rune instead of returning a broken half-character — the exact shape of data a real tool result contains.
func TestTruncateUTF8_SplitsExactlyOnMultiByteRuneBoundary(t *testing.T) {
	// "ab" (2 bytes) + U+FFFC (3 bytes) == 5 bytes total. Cutting at byte 4 lands one byte into the 3-byte rune (bytes 2,3,4 of the string).
	s := "ab￼"

	got := truncateUTF8(s, 4)

	if got != "ab" {
		t.Errorf("truncateUTF8(%q, 4) = %q, want %q (the split rune dropped entirely)", s, got, "ab")
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncateUTF8(%q, 4) = %q is not valid UTF-8", s, got)
	}
}

// TestTruncateUTF8_ShorterThanLimitReturnsUnchanged verifies the common case (no truncation needed) is a no-op, not an off-by-one truncation.
func TestTruncateUTF8_ShorterThanLimitReturnsUnchanged(t *testing.T) {
	s := "short"
	if got := truncateUTF8(s, 100); got != s {
		t.Errorf("truncateUTF8(%q, 100) = %q, want unchanged %q", s, got, s)
	}
}
