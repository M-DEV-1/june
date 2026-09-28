package ipc

import (
	"context"
	"errors"
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

// TestRun_RecordsAGeminiTurnUnderItsOwnProvider checks the other shape of model slug: the Gemini paths name a bare model with no provider in front of it, so the provider is the one the trace's usage names.
func TestRun_RecordsAGeminiTurnUnderItsOwnProvider(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	convID, err := store.CreateConversation(ctx, "cost", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	trace := agent.TurnTrace{
		Model:    "gemini-3-flash",
		Question: "who is vexil",
		Answer:   "a colleague",
		Usage:    agent.TokenUsage{Provider: agent.ProviderGemini, InputTokens: 900, OutputTokens: 60, TotalTokens: 960},
	}
	s := New(&fakeAsker{trace: trace}, store, nil, nil)
	s.run(s.asker, "ask-1", convID, "who is vexil", "", false, nil)

	uses, err := store.TokenUseRecent(ctx, 10)
	if err != nil {
		t.Fatalf("TokenUseRecent: %v", err)
	}
	if len(uses) != 1 {
		t.Fatalf("TokenUseRecent = %d rows, want 1", len(uses))
	}
	if uses[0].Provider != "gemini" || uses[0].Model != "gemini-3-flash" {
		t.Errorf("provider/model = %q/%q, want gemini and the bare model name", uses[0].Provider, uses[0].Model)
	}
}

// TestRun_RecordsACallThatFailedAndOneThatCountedNothing checks the two rows that must still be written rather than skipped: an ask the model failed, and an ask whose provider reported no counts. Both are calls the user may be charged for, and a missing row is one they cannot see; the counts stay at zero rather than being guessed at.
func TestRun_RecordsACallThatFailedAndOneThatCountedNothing(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	convID, err := store.CreateConversation(ctx, "cost", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	failed := agent.TurnTrace{Model: "codex/gpt-5.5", Question: "the one that failed", Usage: agent.TokenUsage{Provider: "codex"}}
	s := New(&fakeAsker{trace: failed, err: errors.New("the backend refused")}, store, nil, nil)
	s.run(s.asker, "ask-1", convID, "the one that failed", "", false, nil)

	quiet := agent.TurnTrace{Model: "gemini-3-flash", Question: "the quiet one", Answer: "here", Usage: agent.TokenUsage{Provider: agent.ProviderGemini}}
	s2 := New(&fakeAsker{trace: quiet}, store, nil, nil)
	s2.run(s2.asker, "ask-2", convID, "the quiet one", "", false, nil)

	uses, err := store.TokenUseRecent(ctx, 10)
	if err != nil {
		t.Fatalf("TokenUseRecent: %v", err)
	}
	if len(uses) != 2 {
		t.Fatalf("TokenUseRecent = %d rows, want 2 (a failed call and a silent one are both calls)", len(uses))
	}
	for _, u := range uses {
		if u.TotalTokens != 0 || u.InputTokens != 0 || u.OutputTokens != 0 {
			t.Errorf("%q was counted as %+v, want zeroes", u.Question, u)
		}
		if u.Provider == "" {
			t.Errorf("%q was filed under no provider", u.Question)
		}
	}
}

// echoQuestionAsker is an Asker whose trace carries back the question it was handed, the way the real ask path fills TurnTrace.Question, so a test can see exactly what the recorders file.
type echoQuestionAsker struct{ hops []agent.ToolHop }

func (a *echoQuestionAsker) AskText(ctx context.Context, question string) (agent.TurnTrace, error) {
	return agent.TurnTrace{Question: question, Answer: "done", ToolHops: a.hops}, nil
}

// TestRun_RecordsTheBareQuestionWhenScreenContextWasAttached checks that the "On screen: ..." prefix run() puts in front of the question for the model stays out of both ledgers: act_runs.question and token_use.question hold the question the user actually asked, since GET /usage shows the first 120 runes of that field and the screen text is neither the question nor something to keep.
func TestRun_RecordsTheBareQuestionWhenScreenContextWasAttached(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()

	asker := &echoQuestionAsker{hops: []agent.ToolHop{{Name: "observe_screen", Result: "1. button Save"}}}
	s := New(asker, store, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	s.run(s.asker, "ask-1", 0, "click save", "Bank of Somewhere — balance 12,431.02", false, nil)

	runs, err := store.ActRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ActRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("ActRuns = %d, want 1", len(runs))
	}
	if runs[0].Question != "click save" {
		t.Errorf("act run question = %q, want the bare question", runs[0].Question)
	}

	uses, err := store.TokenUseRecent(ctx, 10)
	if err != nil {
		t.Fatalf("TokenUseRecent: %v", err)
	}
	if len(uses) != 1 {
		t.Fatalf("TokenUseRecent = %d, want 1", len(uses))
	}
	if uses[0].Question != "click save" {
		t.Errorf("token use question = %q, want the bare question", uses[0].Question)
	}
}
