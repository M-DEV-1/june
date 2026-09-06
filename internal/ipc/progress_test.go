package ipc

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"ora/internal/agent"
	"ora/internal/db/dbtest"
)

// observingAsker is a test double for Asker whose AskText plays out the same two-call shape askText's real tool loop makes around one tool call: agent.ObserveTool once before the tool would run, with its argument summary, and once after, with its result summary — reading the observer off ctx exactly the way run()'s wiring hands it in (see agent.WithToolObserver in ipc.go). It returns a trace whose own ToolHops name that tool, so run()'s post-answer bookkeeping (recordActRun, the tool-name list stored on the turn) still has something to read once AskText is done.
type observingAsker struct {
	toolName string
	before   string
	after    string
	answer   string
}

func (o *observingAsker) AskText(ctx context.Context, question string) (agent.TurnTrace, error) {
	agent.ObserveTool(ctx, o.toolName, o.before)
	agent.ObserveTool(ctx, o.toolName, o.after)
	return agent.TurnTrace{
		ToolHops: []agent.ToolHop{{Name: o.toolName}},
		Answer:   o.answer,
	}, nil
}

// Feature: live tool progress. Before this, run() only learned about a turn's tool calls after AskText returned, and broadcast one "tool" event per hop then — so a long screen task showed nothing on the window until it was all over. run() now wraps the ask context with an observer that broadcasts a "tool" event the moment askText's own loop reports one, live, and no longer re-broadcasts the same hops again once the trace comes back. This test wants exactly the two tool events the observer fired, both landing before the answer, carrying their summaries as Detail and the tool name as Text — and no extra tool events after them (a fix that kept both the live broadcast and the old post-hoc loop would show four events instead of two).
func TestRun_ToolEventsArriveLiveNotDuplicated(t *testing.T) {
	asker := &observingAsker{toolName: "click", before: "clicking Reload", after: "done", answer: "reloaded the page"}
	srv := newTestServer(t, asker, dbtest.Open(t), nil, nil)

	ch, closeFn := readSSE(t, srv)
	defer closeFn()

	resp, err := http.Post(srv.URL+"/ask", "application/json", strings.NewReader(`{"question":"reload the page"}`))
	if err != nil {
		t.Fatalf("POST /ask: %v", err)
	}
	resp.Body.Close()

	wantTypes := []string{"status", "tool", "tool", "answer", "done"}
	var toolDetails []string
	for i, want := range wantTypes {
		ev := mustEvent(t, ch)
		if ev.Type != want {
			t.Fatalf("event %d: type = %q, want %q", i, ev.Type, want)
		}
		if ev.Type == "tool" {
			if ev.Text != "click" {
				t.Errorf("tool event %d: Text = %q, want the tool name", i, ev.Text)
			}
			toolDetails = append(toolDetails, ev.Detail)
		}
	}
	if len(toolDetails) != 2 || toolDetails[0] != "clicking Reload" || toolDetails[1] != "done" {
		t.Errorf("tool event details = %v, want [clicking Reload done]", toolDetails)
	}
}

// The request's "go" field must reach the ask context (agent.WithGo, see run() in ipc.go) so a guarded click or Enter this exact question is asking for can actually run; observingAsker doesn't read it back, but a bad-JSON "go" (wrong type) must not break decoding or the rest of the turn.
func TestAsk_GoFieldDecodesAndDoesNotBreakTheTurn(t *testing.T) {
	asker := &observingAsker{toolName: "click", before: "clicking Send", after: "done", answer: "sent"}
	srv := newTestServer(t, asker, dbtest.Open(t), nil, nil)

	ch, closeFn := readSSE(t, srv)
	defer closeFn()

	resp, err := http.Post(srv.URL+"/ask", "application/json", strings.NewReader(`{"question":"send it","go":true}`))
	if err != nil {
		t.Fatalf("POST /ask: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	resp.Body.Close()

	wantTypes := []string{"status", "tool", "tool", "answer", "done"}
	for i, want := range wantTypes {
		if ev := mustEvent(t, ch); ev.Type != want {
			t.Fatalf("event %d: type = %q, want %q", i, ev.Type, want)
		}
	}
}
