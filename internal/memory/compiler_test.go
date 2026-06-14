package memory_test

import (
	"context"
	"fmt"
	"ora/internal/memory"
	"ora/internal/tracker"
	"strings"
	"sync"
	"testing"
)

type mockSummarizer struct {
	callCount int
}

func (m *mockSummarizer) Summarize(ctx context.Context, activities []tracker.Activity, currentTask string) (*memory.TaskSummary, error) {
	m.callCount++
	return &memory.TaskSummary{
		SameTask: true,
		TaskName: "mock task",
		Summary:  "mock summary",
	}, nil
}

type mockStorage struct {
	callCount int
}

func (m *mockStorage) LogSemanticNode(ctx context.Context, summary memory.TaskSummary) error {
	m.callCount++
	return nil
}

func (m *mockStorage) LogNote(ctx context.Context, content, kind string) (int64, error) {
	return 0, nil
}

func TestCompiler_BuffersWithoutFlushing(t *testing.T) {
	llm := &mockSummarizer{}
	store := &mockStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "db.go"})
	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "agent.go"})

	if llm.callCount > 0 {
		t.Errorf("expected 0 LLM calls, got %d", llm.callCount)
	}
	if compiler.BufferSize() != 3 {
		t.Errorf("expected buffer size 3, got %d", compiler.BufferSize())
	}
}

func TestCompiler_FlushesOnAppChange(t *testing.T) {
	llm := &mockSummarizer{}
	store := &mockStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "Google"})

	if llm.callCount != 1 {
		t.Errorf("expected 1 LLM call, got %d", llm.callCount)
	}
	if compiler.BufferSize() != 1 {
		t.Errorf("expected buffer size 1 (the new app), got %d", compiler.BufferSize())
	}
}

func TestCompiler_FlushesOnWordCountLimit(t *testing.T) {
	llm := &mockSummarizer{}
	store := &mockStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	// 300 words each × 5 = 1500 words → should trigger flush on the 5th ingest
	screenText := strings.Repeat("word ", 300)
	for i := 0; i < 5; i++ {
		compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go", ScreenText: screenText})
	}

	if llm.callCount != 1 {
		t.Errorf("expected 1 flush at word limit, got %d", llm.callCount)
	}
	if store.callCount != 1 {
		t.Errorf("expected 1 store call, got %d", store.callCount)
	}
}

func TestCompiler_NoFlushBelowWordLimit(t *testing.T) {
	llm := &mockSummarizer{}
	store := &mockStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	// 100 words each × 5 = 500 words → under limit, no flush
	screenText := strings.Repeat("word ", 100)
	for i := 0; i < 5; i++ {
		compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go", ScreenText: screenText})
	}

	if llm.callCount != 0 {
		t.Errorf("expected no flush under word limit, got %d LLM calls", llm.callCount)
	}
}

type capturingSummarizer struct {
	received []tracker.Activity
}

func (m *capturingSummarizer) Summarize(ctx context.Context, activities []tracker.Activity, currentTask string) (*memory.TaskSummary, error) {
	m.received = activities
	return &memory.TaskSummary{SameTask: true, TaskName: "t", Summary: "s"}, nil
}

func TestCompiler_PassesScreenTextToSummarizer(t *testing.T) {
	llm := &capturingSummarizer{}
	store := &mockStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "auth.go", ScreenText: "func validateToken"})
	compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "Google"}) // triggers flush on app change

	if len(llm.received) == 0 {
		t.Fatal("summarizer was not called")
	}
	if llm.received[0].ScreenText != "func validateToken" {
		t.Errorf("expected ScreenText passed to summarizer, got %q", llm.received[0].ScreenText)
	}
}

type errorSummarizer struct {
	callCount int
}

func (m *errorSummarizer) Summarize(ctx context.Context, activities []tracker.Activity, currentTask string) (*memory.TaskSummary, error) {
	m.callCount++
	return nil, fmt.Errorf("api rate limit reached")
}

type capturingStorage struct {
	stored memory.TaskSummary
}

func (m *capturingStorage) LogSemanticNode(ctx context.Context, summary memory.TaskSummary) error {
	m.stored = summary
	return nil
}

func (m *capturingStorage) LogNote(ctx context.Context, content, kind string) (int64, error) {
	return 0, nil
}

func TestCompiler_FallbackIncludesScreenText(t *testing.T) {
	llm := &errorSummarizer{}
	store := &capturingStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "auth.go", ScreenText: "func validateToken"})
	compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "Google"})

	if store.stored.Summary == "" {
		t.Fatal("expected fallback summary to be stored")
	}
	if !strings.Contains(store.stored.Summary, "func validateToken") {
		t.Errorf("fallback summary missing ScreenText, got: %q", store.stored.Summary)
	}
}

func TestCompiler_FallbackOnAPIFailure(t *testing.T) {
	llm := &errorSummarizer{}
	store := &mockStorage{}
	compiler := memory.NewCompiler(llm, store)

	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "StackOverflow"})

	if store.callCount != 1 {
		t.Errorf("Expected 1 call to Database Storage (fallback), got %d", store.callCount)
	}

	if compiler.BufferSize() != 1 {
		t.Errorf("Expected buffer size 1 after fallback flush, got %d", compiler.BufferSize())
	}
}

type notesSummarizer struct{}

func (n *notesSummarizer) Summarize(ctx context.Context, activities []tracker.Activity, currentTask string) (*memory.TaskSummary, error) {
	return &memory.TaskSummary{
		SameTask: true,
		TaskName: "notes task",
		Summary:  "did stuff",
		Notes:    []string{"user prefers terse responses", "user is debugging the React PR"},
	}, nil
}

type notesStorage struct {
	mu        sync.Mutex
	nodesCalls []memory.TaskSummary
	noteCalls  []struct{ content, kind string }
}

func (s *notesStorage) LogSemanticNode(ctx context.Context, summary memory.TaskSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodesCalls = append(s.nodesCalls, summary)
	return nil
}

func (s *notesStorage) LogNote(ctx context.Context, content, kind string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteCalls = append(s.noteCalls, struct{ content, kind string }{content, kind})
	return int64(len(s.noteCalls)), nil
}

func TestCompiler_AutoExtractsNotes(t *testing.T) {
	llm := &notesSummarizer{}
	store := &notesStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.ForceFlush(ctx)

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.nodesCalls) != 1 {
		t.Fatalf("expected 1 LogSemanticNode call, got %d", len(store.nodesCalls))
	}
	if len(store.noteCalls) != 2 {
		t.Fatalf("expected 2 LogNote calls, got %d", len(store.noteCalls))
	}
	if store.noteCalls[0].content != "user prefers terse responses" {
		t.Errorf("unexpected first note: %q", store.noteCalls[0].content)
	}
	if store.noteCalls[1].content != "user is debugging the React PR" {
		t.Errorf("unexpected second note: %q", store.noteCalls[1].content)
	}
	for _, c := range store.noteCalls {
		if c.kind != "fact" {
			t.Errorf("expected kind 'fact', got %q", c.kind)
		}
	}
}
