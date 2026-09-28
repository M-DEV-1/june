package ipc

import (
	"context"
	"testing"
	"time"

	"ora/internal/agent"
	"ora/internal/db/dbtest"
)

// Until this was wired, nothing anywhere kept what the model reached for: act_runs only ever held the runs that touched the screen, the activity feed is thrown away when the window closes, and the voice session recorded nothing at all — so a voice turn that used look and draw was, in the store, indistinguishable from one that used no tools (2026-09-07). This drives one real tool through the real agent with the daemon's own recorder on the context and reads the row back out.
func TestToolRecorder_FilesWhatTheModelReachedForAndWhatItWasOffered(t *testing.T) {
	store := dbtest.Open(t)
	ag := agent.NewAgent(nil, nil, store, nil, "")

	ctx := agent.WithToolRecorder(context.Background(), toolRecorder(store, "ask", 7))
	ctx = agent.WithOffered(ctx, []string{"add_task", "save_note"})
	since := time.Now().Add(-time.Minute)

	if res := ag.ExecuteTool(ctx, "add_task", map[string]any{"title": "buy milk"}); res == "" {
		t.Fatal("add_task returned nothing")
	}
	// A tool that runs and fails must be filed too, and filed as a failure, or a broken tool reads as one nobody chose.
	ag.ExecuteTool(ctx, "add_task", map[string]any{"title": "   "})

	calls, err := store.ToolCallsSince(context.Background(), since)
	if err != nil {
		t.Fatalf("reading the tool calls back: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("filed %d tool calls, want 2", len(calls))
	}

	first := calls[0]
	if first.Path != "ask" || first.ConversationID != 7 || first.Name != "add_task" {
		t.Errorf("row = path %q conversation %d name %q, want ask/7/add_task", first.Path, first.ConversationID, first.Name)
	}
	if first.Outcome != agent.OutcomeOK {
		t.Errorf("outcome = %q, want %q", first.Outcome, agent.OutcomeOK)
	}
	if len(first.Offered) != 2 || first.Offered[0] != "add_task" || first.Offered[1] != "save_note" {
		t.Errorf("offered = %v, want the two tools the round was handed", first.Offered)
	}
	if calls[1].Outcome != agent.OutcomeError {
		t.Errorf("a tool that ran and failed was filed as %q, want %q", calls[1].Outcome, agent.OutcomeError)
	}
}
