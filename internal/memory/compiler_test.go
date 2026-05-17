package memory_test

import (
	"context"
	"fmt"
	"ora/internal/memory"
	"ora/internal/tracker"
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

func TestCompiler_FlushesOnMaxBuffer(t *testing.T) {
	llm := &mockSummarizer{}
	store := &mockStorage{}
	compiler := memory.NewCompiler(llm, store)

	ctx := context.Background()

	// spam it with 10 items to fill it up
	for i := 0; i < 10; i++ {
		compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "Reading article"})
	}

	// this one overflows it, trigger flushhhhh
	compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "Still reading"})

	// let's see if it actually called the llm
	if llm.callCount != 1 {
		t.Errorf("Expected 1 call to LLM Summarizer, got %d", llm.callCount)
	}
	if store.callCount != 1 {
		t.Errorf("Expected 1 call to Database Storage, got %d", store.callCount)
	}

	if compiler.BufferSize() != 1 {
		t.Errorf("Expected buffer size to be 1 after flush, got %d", compiler.BufferSize())
	}
}

type errorSummarizer struct {
	callCount int
}

func (m *errorSummarizer) Summarize(ctx context.Context, activities []tracker.Activity, currentTask string) (*memory.TaskSummary, error) {
	m.callCount++
	return nil, fmt.Errorf("api rate limit reached")
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
