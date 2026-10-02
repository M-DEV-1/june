package ipc

import (
	"context"
	"testing"
	"time"

	"june/internal/agent"
	"june/internal/db/dbtest"
)

// TestRun_RecordsAnActRunWhenAScreenToolRan checks run() files a screen-tool trace as an act run with outcome ok and its step names, and leaves no act run behind for a memory-only trace.
func TestRun_RecordsAnActRunWhenAScreenToolRan(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	convID, err := store.CreateConversation(ctx, "smoke", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	trace := agent.TurnTrace{
		Answer: "clicked it",
		ToolHops: []agent.ToolHop{
			{Name: "observe_screen", Result: "1. button Save"},
			{Name: "click", Args: map[string]any{"n": float64(1)}, Result: "clicked [1] button \"Save\""},
		},
	}
	s := New(&fakeAsker{trace: trace}, store, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	s.run(s.asker, "ask-1", convID, "click save", "", false, nil)

	runs, err := store.ActRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ActRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("ActRuns = %d, want 1", len(runs))
	}
	if runs[0].Outcome != "ok" {
		t.Errorf("Outcome = %q, want ok", runs[0].Outcome)
	}
	if len(runs[0].Steps) != 2 || runs[0].Steps[1].Name != "click" {
		t.Fatalf("Steps = %+v, want observe_screen then click", runs[0].Steps)
	}

	// A memory-only trace leaves no act run.
	convID2, err := store.CreateConversation(ctx, "smoke2", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	memTrace := agent.TurnTrace{Answer: "hi", ToolHops: []agent.ToolHop{{Name: "query_memory", Result: "nothing"}}}
	s2 := New(&fakeAsker{trace: memTrace}, store, nil, nil)
	ch2 := s2.hub.subscribe()
	defer s2.hub.unsubscribe(ch2)
	s2.run(s2.asker, "ask-2", convID2, "what did I say", "", false, nil)

	runsAfter, err := store.ActRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ActRuns: %v", err)
	}
	if len(runsAfter) != 1 {
		t.Fatalf("ActRuns after memory-only ask = %d, want still 1", len(runsAfter))
	}
}

// TestRun_RecordsWhatTheTurnCostInTokens checks that a finished ask files its token counts alongside its answer, which is what lets the user see what every model provider is costing them. The provider and model come from the trace's model slug — "codex/gpt-5.5" splits into the two — the channel is "text" because this is /ask, and the counts and duration are the trace's own.
func TestRun_RecordsWhatTheTurnCostInTokens(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	convID, err := store.CreateConversation(ctx, "cost", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	trace := agent.TurnTrace{
		Model:    "codex/gpt-5.5",
		Question: "what did I do today",
		Answer:   "you wrote code",
		Duration: 2 * time.Second,
		Usage:    agent.TokenUsage{Provider: "codex", InputTokens: 1200, OutputTokens: 340, TotalTokens: 1540},
	}
	s := New(&fakeAsker{trace: trace}, store, nil, nil)
	s.run(s.asker, "ask-1", convID, "what did I do today", "", false, nil)

	uses, err := store.TokenUseRecent(ctx, 10)
	if err != nil {
		t.Fatalf("TokenUseRecent: %v", err)
	}
	if len(uses) != 1 {
		t.Fatalf("TokenUseRecent = %d rows, want 1", len(uses))
	}
	u := uses[0]
	if u.Provider != "codex" || u.Model != "gpt-5.5" {
		t.Errorf("provider/model = %q/%q, want codex/gpt-5.5 split in two", u.Provider, u.Model)
	}
	if u.Channel != "text" {
		t.Errorf("channel = %q, want text", u.Channel)
	}
	if u.InputTokens != 1200 || u.OutputTokens != 340 || u.TotalTokens != 1540 {
		t.Errorf("counts = %d in, %d out, %d total, want 1200/340/1540", u.InputTokens, u.OutputTokens, u.TotalTokens)
	}
	if u.DurationMS != 2000 {
		t.Errorf("duration = %d ms, want 2000", u.DurationMS)
	}
	if u.Question != "what did I do today" {
		t.Errorf("question = %q", u.Question)
	}
}
