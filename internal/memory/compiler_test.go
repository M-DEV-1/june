package memory_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"ora/internal/memory"
	"ora/internal/tracker"
	"strings"
	"sync"
	"testing"
	"time"
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

func (m *mockSummarizer) ReconcileNotes(ctx context.Context, existing []memory.NoteRef, candidates []string) ([]memory.NoteOp, error) {
	ops := make([]memory.NoteOp, len(candidates))
	for i, c := range candidates {
		ops[i] = memory.NoteOp{Action: "add", Content: c}
	}
	return ops, nil
}

func (m *mockSummarizer) AttributeThreads(ctx context.Context, activities []tracker.Activity, existing []memory.Thread) (*memory.ThreadAttribution, error) {
	m.callCount++
	return &memory.ThreadAttribution{
		Threads: []memory.ThreadUpdate{{Subject: "mock task", Kind: "work", State: "mock state", Summary: "mock summary"}},
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

func (m *mockStorage) ExistingNotes(ctx context.Context) ([]memory.NoteRef, error) {
	return nil, nil
}

func (m *mockStorage) UpdateNote(ctx context.Context, id int64, content string) error {
	return nil
}

func (m *mockStorage) UpsertThread(ctx context.Context, u memory.ThreadUpdate) (int64, error) {
	return 0, nil
}

func (m *mockStorage) ThreadsForAttribution(ctx context.Context, limit int) ([]memory.Thread, error) {
	return nil, nil
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

func (m *capturingSummarizer) ReconcileNotes(ctx context.Context, existing []memory.NoteRef, candidates []string) ([]memory.NoteOp, error) {
	return nil, nil
}

func (m *capturingSummarizer) AttributeThreads(ctx context.Context, activities []tracker.Activity, existing []memory.Thread) (*memory.ThreadAttribution, error) {
	m.received = activities
	return &memory.ThreadAttribution{
		Threads: []memory.ThreadUpdate{{Subject: "t", Kind: "work", State: "s", Summary: "s"}},
	}, nil
}

func TestCompiler_PassesScreenTextToSummarizer(t *testing.T) {
	llm := &capturingSummarizer{}
	store := &mockStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	// Pad screen text to exceed minFlushWords so the flush is not discarded.
	screenText := "func validateToken " + strings.Repeat("word ", 30)
	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "auth.go", ScreenText: screenText})
	compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "Google"}) // triggers flush on app change

	if len(llm.received) == 0 {
		t.Fatal("summarizer was not called")
	}
	if !strings.HasPrefix(llm.received[0].ScreenText, "func validateToken") {
		t.Errorf("expected ScreenText passed to summarizer to start with 'func validateToken', got %q", llm.received[0].ScreenText)
	}
}

type errorSummarizer struct {
	callCount int
}

func (m *errorSummarizer) Summarize(ctx context.Context, activities []tracker.Activity, currentTask string) (*memory.TaskSummary, error) {
	m.callCount++
	return nil, fmt.Errorf("api rate limit reached")
}

func (m *errorSummarizer) ReconcileNotes(ctx context.Context, existing []memory.NoteRef, candidates []string) ([]memory.NoteOp, error) {
	return nil, nil
}

func (m *errorSummarizer) AttributeThreads(ctx context.Context, activities []tracker.Activity, existing []memory.Thread) (*memory.ThreadAttribution, error) {
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

func (m *capturingStorage) ExistingNotes(ctx context.Context) ([]memory.NoteRef, error) {
	return nil, nil
}

func (m *capturingStorage) UpdateNote(ctx context.Context, id int64, content string) error {
	return nil
}

func (m *capturingStorage) UpsertThread(ctx context.Context, u memory.ThreadUpdate) (int64, error) {
	return 0, nil
}

func (m *capturingStorage) ThreadsForAttribution(ctx context.Context, limit int) ([]memory.Thread, error) {
	return nil, nil
}

func TestCompiler_FallbackIncludesScreenText(t *testing.T) {
	llm := &errorSummarizer{}
	store := &capturingStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	// Pad screen text to exceed minFlushWords so the flush is not discarded.
	screenText := "func validateToken " + strings.Repeat("word ", 30)
	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "auth.go", ScreenText: screenText})
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

type notesSummarizer struct {
	reconcileOps []memory.NoteOp
	reconcileErr error
}

func (n *notesSummarizer) Summarize(ctx context.Context, activities []tracker.Activity, currentTask string) (*memory.TaskSummary, error) {
	return &memory.TaskSummary{
		SameTask: true,
		TaskName: "notes task",
		Summary:  "did stuff",
		Notes:    []string{"user prefers terse responses", "user is debugging the React PR"},
	}, nil
}

func (n *notesSummarizer) ReconcileNotes(ctx context.Context, existing []memory.NoteRef, candidates []string) ([]memory.NoteOp, error) {
	if n.reconcileErr != nil {
		return nil, n.reconcileErr
	}
	if n.reconcileOps != nil {
		return n.reconcileOps, nil
	}
	ops := make([]memory.NoteOp, len(candidates))
	for i, c := range candidates {
		ops[i] = memory.NoteOp{Action: "add", Content: c}
	}
	return ops, nil
}

// AttributeThreads emits a single thread plus identity facts so the note
// reconciliation path (which now operates on attr.Identity) is exercised.
func (n *notesSummarizer) AttributeThreads(ctx context.Context, activities []tracker.Activity, existing []memory.Thread) (*memory.ThreadAttribution, error) {
	return &memory.ThreadAttribution{
		Threads:  []memory.ThreadUpdate{{Subject: "notes task", Kind: "work", State: "did stuff", Summary: "did stuff"}},
		Identity: []string{"user prefers terse responses", "user is debugging the React PR"},
	}, nil
}

type notesStorage struct {
	mu          sync.Mutex
	nodesCalls  []memory.TaskSummary
	noteCalls   []struct{ content, kind string }
	updateCalls []struct {
		id      int64
		content string
	}
	existingNotes []memory.NoteRef
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

func (s *notesStorage) ExistingNotes(ctx context.Context) ([]memory.NoteRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.existingNotes, nil
}

func (s *notesStorage) UpdateNote(ctx context.Context, id int64, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateCalls = append(s.updateCalls, struct {
		id      int64
		content string
	}{id, content})
	return nil
}

func (s *notesStorage) UpsertThread(ctx context.Context, u memory.ThreadUpdate) (int64, error) {
	return 0, nil
}

func (s *notesStorage) ThreadsForAttribution(ctx context.Context, limit int) ([]memory.Thread, error) {
	return nil, nil
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

func TestCompiler_ReconcileUpdate(t *testing.T) {
	llm := &notesSummarizer{
		reconcileOps: []memory.NoteOp{
			{Action: "update", ID: 7, Content: "user prefers terse and concise responses"},
			{Action: "skip"},
		},
	}
	store := &notesStorage{
		existingNotes: []memory.NoteRef{{ID: 7, Content: "user prefers terse responses"}},
	}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.ForceFlush(ctx)

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.noteCalls) != 0 {
		t.Errorf("expected 0 LogNote calls for update/skip ops, got %d", len(store.noteCalls))
	}
	if len(store.updateCalls) != 1 {
		t.Fatalf("expected 1 UpdateNote call, got %d", len(store.updateCalls))
	}
	if store.updateCalls[0].id != 7 {
		t.Errorf("expected UpdateNote id=7, got %d", store.updateCalls[0].id)
	}
	if store.updateCalls[0].content != "user prefers terse and concise responses" {
		t.Errorf("unexpected UpdateNote content: %q", store.updateCalls[0].content)
	}
}

func TestCompiler_ReconcileAdd(t *testing.T) {
	llm := &notesSummarizer{
		reconcileOps: []memory.NoteOp{
			{Action: "add", Content: "user works in Go"},
		},
	}
	store := &notesStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.ForceFlush(ctx)

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.noteCalls) != 1 {
		t.Fatalf("expected 1 LogNote call for add op, got %d", len(store.noteCalls))
	}
	if store.noteCalls[0].content != "user works in Go" {
		t.Errorf("unexpected note content: %q", store.noteCalls[0].content)
	}
	if len(store.updateCalls) != 0 {
		t.Errorf("expected 0 UpdateNote calls for add op, got %d", len(store.updateCalls))
	}
}

func TestCompiler_ReconcileErrorFallback(t *testing.T) {
	llm := &notesSummarizer{
		reconcileErr: fmt.Errorf("llm timeout"),
	}
	store := &notesStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.ForceFlush(ctx)

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.noteCalls) != 2 {
		t.Fatalf("expected 2 LogNote calls on fallback, got %d", len(store.noteCalls))
	}
	if store.noteCalls[0].content != "user prefers terse responses" {
		t.Errorf("unexpected first fallback note: %q", store.noteCalls[0].content)
	}
	if store.noteCalls[1].content != "user is debugging the React PR" {
		t.Errorf("unexpected second fallback note: %q", store.noteCalls[1].content)
	}
	if len(store.updateCalls) != 0 {
		t.Errorf("expected 0 UpdateNote calls on fallback, got %d", len(store.updateCalls))
	}
}

// --- Salience gate tests ---

func TestIsSalient(t *testing.T) {
	cases := []struct {
		name     string
		act      tracker.Activity
		wantSalient bool
	}{
		{
			name:        "empty title and empty screen text",
			act:         tracker.Activity{App: "Explorer", Title: "", ScreenText: ""},
			wantSalient: false,
		},
		{
			name:        "whitespace-only title and empty screen text",
			act:         tracker.Activity{App: "Explorer", Title: "   ", ScreenText: ""},
			wantSalient: false,
		},
		{
			name:        "new tab lowercase",
			act:         tracker.Activity{App: "Chrome", Title: "new tab", ScreenText: ""},
			wantSalient: false,
		},
		{
			name:        "New Tab mixed case",
			act:         tracker.Activity{App: "Chrome", Title: "New Tab", ScreenText: ""},
			wantSalient: false,
		},
		{
			name:        "untitled",
			act:         tracker.Activity{App: "Notepad", Title: "Untitled", ScreenText: ""},
			wantSalient: false,
		},
		{
			name:        "desktop",
			act:         tracker.Activity{App: "Explorer", Title: "Desktop", ScreenText: ""},
			wantSalient: false,
		},
		{
			name:        "trivial title but non-trivial screen text",
			act:         tracker.Activity{App: "Chrome", Title: "New Tab", ScreenText: "package main\n\nfunc main() {}"},
			wantSalient: true,
		},
		{
			name:        "real title",
			act:         tracker.Activity{App: "VSCode", Title: "compiler.go", ScreenText: ""},
			wantSalient: true,
		},
		{
			name:        "real title and screen text",
			act:         tracker.Activity{App: "VSCode", Title: "auth.go", ScreenText: "func validateToken(tok string) bool"},
			wantSalient: true,
		},
		{
			name:        "empty title but has screen text",
			act:         tracker.Activity{App: "Terminal", Title: "", ScreenText: "go build ./..."},
			wantSalient: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := memory.IsSalient(tc.act)
			if got != tc.wantSalient {
				t.Errorf("IsSalient(%+v) = %v, want %v", tc.act, got, tc.wantSalient)
			}
		})
	}
}

func TestCompiler_NonSalientActivitiesNotBuffered(t *testing.T) {
	llm := &mockSummarizer{}
	store := &mockStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "New Tab", ScreenText: ""})
	compiler.Ingest(ctx, tracker.Activity{App: "Explorer", Title: "Desktop", ScreenText: ""})
	compiler.Ingest(ctx, tracker.Activity{App: "Notepad", Title: "Untitled", ScreenText: ""})

	if compiler.BufferSize() != 0 {
		t.Errorf("expected buffer size 0 for non-salient activities, got %d", compiler.BufferSize())
	}
	if llm.callCount != 0 {
		t.Errorf("expected 0 LLM calls for non-salient activities, got %d", llm.callCount)
	}
	if store.callCount != 0 {
		t.Errorf("expected 0 store calls for non-salient activities, got %d", store.callCount)
	}
}

// TestCompiler_ThinTitleActivityFlushes verifies that a salient activity with
// only a window title (no screen text) is summarized — social/gaming/meeting
// sessions produce sparse screen content but are still worth remembering.
func TestCompiler_ThinTitleActivityFlushes(t *testing.T) {
	llm := &mockSummarizer{}
	store := &mockStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "Discord", Title: "General (voice)", ScreenText: ""})
	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go", ScreenText: ""})

	if llm.callCount != 1 {
		t.Errorf("expected 1 LLM call for thin-title flush, got %d", llm.callCount)
	}
	if store.callCount != 1 {
		t.Errorf("expected 1 store call for thin-title flush, got %d", store.callCount)
	}
}

func TestCompiler_SalientTitleAndScreenTextBothFlush(t *testing.T) {
	llm := &mockSummarizer{}
	store := &mockStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go", ScreenText: "func main() {}"})
	compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "GitHub"})

	if llm.callCount != 1 {
		t.Errorf("expected 1 LLM call, got %d", llm.callCount)
	}
	if store.callCount != 1 {
		t.Errorf("expected 1 store call, got %d", store.callCount)
	}
}

// ─── Thread attribution + flush recording fakes ───────────────────────────────

// flushRecordingStorage records every Storage method call so tests can assert
// exactly what flush writes and the order it does it in.
type flushRecordingStorage struct {
	mu             sync.Mutex
	semanticCalls  []memory.TaskSummary
	upsertCalls    []memory.ThreadUpdate
	noteCalls      []struct{ content, kind string }
	updateCalls    []struct {
		id      int64
		content string
	}
	existingCalled int
}

func (s *flushRecordingStorage) LogSemanticNode(_ context.Context, summary memory.TaskSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.semanticCalls = append(s.semanticCalls, summary)
	return nil
}

func (s *flushRecordingStorage) LogNote(_ context.Context, content, kind string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteCalls = append(s.noteCalls, struct{ content, kind string }{content, kind})
	return int64(len(s.noteCalls)), nil
}

func (s *flushRecordingStorage) ExistingNotes(_ context.Context) ([]memory.NoteRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.existingCalled++
	return nil, nil
}

func (s *flushRecordingStorage) UpdateNote(_ context.Context, id int64, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateCalls = append(s.updateCalls, struct {
		id      int64
		content string
	}{id, content})
	return nil
}

func (s *flushRecordingStorage) UpsertThread(_ context.Context, u memory.ThreadUpdate) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertCalls = append(s.upsertCalls, u)
	return u.ID, nil // return provided ID (0 is fine — compiler ignores the return value)
}

func (s *flushRecordingStorage) ThreadsForAttribution(_ context.Context, _ int) ([]memory.Thread, error) {
	return nil, nil
}

// fixedAttribSummarizer returns a caller-configured ThreadAttribution (or error).
// Summarize and ReconcileNotes are stubs that are never invoked by flush.
type fixedAttribSummarizer struct {
	attr *memory.ThreadAttribution
	err  error
}

func (s *fixedAttribSummarizer) Summarize(_ context.Context, _ []tracker.Activity, _ string) (*memory.TaskSummary, error) {
	return nil, nil
}

func (s *fixedAttribSummarizer) ReconcileNotes(_ context.Context, _ []memory.NoteRef, _ []string) ([]memory.NoteOp, error) {
	return nil, nil
}

func (s *fixedAttribSummarizer) AttributeThreads(_ context.Context, _ []tracker.Activity, _ []memory.Thread) (*memory.ThreadAttribution, error) {
	return s.attr, s.err
}

// identityTrackingSummarizer is a Summarizer that records ReconcileNotes calls
// and returns a configurable identity list from AttributeThreads.
type identityTrackingSummarizer struct {
	identity        []string
	reconcileCalled int
}

func (s *identityTrackingSummarizer) Summarize(_ context.Context, _ []tracker.Activity, _ string) (*memory.TaskSummary, error) {
	return nil, nil
}

func (s *identityTrackingSummarizer) ReconcileNotes(_ context.Context, existing []memory.NoteRef, candidates []string) ([]memory.NoteOp, error) {
	s.reconcileCalled++
	ops := make([]memory.NoteOp, len(candidates))
	for i, c := range candidates {
		ops[i] = memory.NoteOp{Action: "add", Content: c}
	}
	return ops, nil
}

func (s *identityTrackingSummarizer) AttributeThreads(_ context.Context, _ []tracker.Activity, _ []memory.Thread) (*memory.ThreadAttribution, error) {
	return &memory.ThreadAttribution{
		Threads:  []memory.ThreadUpdate{{Subject: "test task", Kind: "work", State: "s", Summary: "s"}},
		Identity: s.identity,
	}, nil
}

// ─── Thread attribution tests ─────────────────────────────────────────────────

// TestCompiler_SuccessfulAttribution verifies that when AttributeThreads returns
// two concurrent threads (entertainment id=0, work id=5), flush calls UpsertThread
// twice and LogSemanticNode twice, with SameTask = (u.ID != 0) for each, and
// TaskNames matching the thread subjects.
func TestCompiler_SuccessfulAttribution(t *testing.T) {
	twoThreads := &memory.ThreadAttribution{
		Threads: []memory.ThreadUpdate{
			{ID: 0, Subject: "Suits", Kind: "entertainment", State: "s1e3", Summary: "watched ep 3", Novel: true},
			{ID: 5, Subject: "ORA project", Kind: "work", State: "writing tests", Summary: "added thread tests"},
		},
	}
	llm := &fixedAttribSummarizer{attr: twoThreads}
	store := &flushRecordingStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "Netflix", Title: "Suits"})
	compiler.ForceFlush(ctx)

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.upsertCalls) != 2 {
		t.Fatalf("expected 2 UpsertThread calls, got %d", len(store.upsertCalls))
	}
	if len(store.semanticCalls) != 2 {
		t.Fatalf("expected 2 LogSemanticNode calls, got %d", len(store.semanticCalls))
	}

	// thread[0]: ID=0 → SameTask should be false
	if store.semanticCalls[0].SameTask {
		t.Errorf("thread id=0: SameTask should be false, got true")
	}
	if store.semanticCalls[0].TaskName != "Suits" {
		t.Errorf("thread id=0: TaskName should be 'Suits', got %q", store.semanticCalls[0].TaskName)
	}

	// thread[1]: ID=5 → SameTask should be true
	if !store.semanticCalls[1].SameTask {
		t.Errorf("thread id=5: SameTask should be true, got false")
	}
	if store.semanticCalls[1].TaskName != "ORA project" {
		t.Errorf("thread id=5: TaskName should be 'ORA project', got %q", store.semanticCalls[1].TaskName)
	}
}

// TestCompiler_IdentityReconciliation_NonEmpty verifies that when
// ThreadAttribution.Identity is non-empty, ExistingNotes and ReconcileNotes are
// both called.
func TestCompiler_IdentityReconciliation_NonEmpty(t *testing.T) {
	llm := &identityTrackingSummarizer{
		identity: []string{"user prefers Go"},
	}
	store := &flushRecordingStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.ForceFlush(ctx)

	store.mu.Lock()
	defer store.mu.Unlock()

	if store.existingCalled == 0 {
		t.Error("ExistingNotes should be called when Identity is non-empty")
	}
	if llm.reconcileCalled == 0 {
		t.Error("ReconcileNotes should be called when Identity is non-empty")
	}
}

// TestCompiler_IdentityReconciliation_Empty verifies that when
// ThreadAttribution.Identity is empty (nil or zero-length), ExistingNotes and
// ReconcileNotes are NOT called.
func TestCompiler_IdentityReconciliation_Empty(t *testing.T) {
	llm := &identityTrackingSummarizer{
		identity: nil, // empty / unset
	}
	store := &flushRecordingStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.ForceFlush(ctx)

	store.mu.Lock()
	defer store.mu.Unlock()

	if store.existingCalled > 0 {
		t.Errorf("ExistingNotes must NOT be called when Identity is empty, got %d call(s)", store.existingCalled)
	}
	if llm.reconcileCalled > 0 {
		t.Errorf("ReconcileNotes must NOT be called when Identity is empty, got %d call(s)", llm.reconcileCalled)
	}
}

// TestCompiler_FallbackOnAttributionFailure verifies that when AttributeThreads
// returns an error, nil, or an empty Threads list, flush writes a "Raw Activity
// Log" TaskSummary via LogSemanticNode and does NOT call UpsertThread.
func TestCompiler_FallbackOnAttributionFailure(t *testing.T) {
	cases := []struct {
		name string
		llm  *fixedAttribSummarizer
	}{
		{
			name: "error from AttributeThreads",
			llm:  &fixedAttribSummarizer{err: fmt.Errorf("api rate limit")},
		},
		{
			name: "nil attr from AttributeThreads",
			llm:  &fixedAttribSummarizer{attr: nil, err: nil},
		},
		{
			name: "empty Threads slice from AttributeThreads",
			llm: &fixedAttribSummarizer{
				attr: &memory.ThreadAttribution{Threads: []memory.ThreadUpdate{}},
				err:  nil,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &flushRecordingStorage{}
			compiler := memory.NewCompiler(tc.llm, store)
			ctx := context.Background()

			compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
			compiler.ForceFlush(ctx)

			store.mu.Lock()
			defer store.mu.Unlock()

			if len(store.upsertCalls) != 0 {
				t.Errorf("UpsertThread must not be called on attribution failure, got %d call(s)", len(store.upsertCalls))
			}
			if len(store.semanticCalls) != 1 {
				t.Fatalf("expected 1 fallback LogSemanticNode call, got %d", len(store.semanticCalls))
			}
			if store.semanticCalls[0].TaskName != "Raw Activity Log" {
				t.Errorf("expected fallback TaskName 'Raw Activity Log', got %q", store.semanticCalls[0].TaskName)
			}
			if store.semanticCalls[0].SameTask {
				t.Error("fallback SameTask must be false")
			}
		})
	}
}

// ─── Concurrency safety ────────────────────────────────────────────────────

// raceSafeSummarizer/raceSafeStorage are mutex-protected mocks so that any
// data race caught by `go test -race` in the tests below can only originate
// from the Compiler itself (buffer/wordCount/lastFlush), not from the mocks.
type raceSafeSummarizer struct {
	mu sync.Mutex
}

func (m *raceSafeSummarizer) Summarize(ctx context.Context, activities []tracker.Activity, currentTask string) (*memory.TaskSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &memory.TaskSummary{SameTask: true, TaskName: "t", Summary: "s"}, nil
}

func (m *raceSafeSummarizer) ReconcileNotes(ctx context.Context, existing []memory.NoteRef, candidates []string) ([]memory.NoteOp, error) {
	return nil, nil
}

func (m *raceSafeSummarizer) AttributeThreads(ctx context.Context, activities []tracker.Activity, existing []memory.Thread) (*memory.ThreadAttribution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &memory.ThreadAttribution{
		Threads: []memory.ThreadUpdate{{Subject: "t", Kind: "work", State: "s", Summary: "s"}},
	}, nil
}

type raceSafeStorage struct {
	mu sync.Mutex
}

func (s *raceSafeStorage) LogSemanticNode(ctx context.Context, summary memory.TaskSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return nil
}

func (s *raceSafeStorage) LogNote(ctx context.Context, content, kind string) (int64, error) {
	return 0, nil
}

func (s *raceSafeStorage) ExistingNotes(ctx context.Context) ([]memory.NoteRef, error) {
	return nil, nil
}

func (s *raceSafeStorage) UpdateNote(ctx context.Context, id int64, content string) error {
	return nil
}

func (s *raceSafeStorage) UpsertThread(ctx context.Context, u memory.ThreadUpdate) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return 0, nil
}

func (s *raceSafeStorage) ThreadsForAttribution(ctx context.Context, limit int) ([]memory.Thread, error) {
	return nil, nil
}

// TestCompiler_ConcurrentAccess drives Ingest, GetCurrentBuffer, and ForceFlush
// from many goroutines simultaneously, mirroring real daemon usage: the ingest
// loop (cmd/daemon.go ~211-217) calls Ingest, an hourly ticker (~84-97) calls
// ForceFlush, and both the /buffer HTTP handler (~231-239) and Agent.Connect
// (internal/agent/connect.go ~56-57) call GetCurrentBuffer. None of these
// serialize access to Compiler's internal buffer/wordCount/lastFlush fields,
// so this must fail under `go test -race`.
func TestCompiler_ConcurrentAccess(t *testing.T) {
	llm := &raceSafeSummarizer{}
	store := &raceSafeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	apps := []string{"VSCode", "Chrome", "Discord", "Terminal"}

	var wg sync.WaitGroup

	// concurrent ingest, simulating the daemon's event-channel consumer loop
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				compiler.Ingest(ctx, tracker.Activity{
					App:   apps[(i+j)%len(apps)],
					Title: fmt.Sprintf("title-%d-%d", i, j),
				})
			}
		}(i)
	}

	// concurrent reads of the live buffer, simulating the /buffer HTTP handler
	// and Agent.Connect
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				buf := compiler.GetCurrentBuffer()
				// touch the returned slice the way callers do (range over it),
				// which is exactly what races against a concurrent append.
				for _, act := range buf {
					_ = act.App
				}
			}
		}()
	}

	// concurrent forced flush, simulating the hourly safety-net ticker
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				compiler.ForceFlush(ctx)
				time.Sleep(time.Millisecond)
			}
		}()
	}

	wg.Wait()
}

// failingStorage returns errors from the durable-write methods so we can assert
// the compiler surfaces store failures (logs them) instead of silently
// swallowing them with `_ =`, which would lose memory with zero visibility.
type failingStorage struct{}

func (failingStorage) LogSemanticNode(context.Context, memory.TaskSummary) error {
	return fmt.Errorf("boom: LogSemanticNode")
}
func (failingStorage) LogNote(context.Context, string, string) (int64, error) {
	return 0, fmt.Errorf("boom: LogNote")
}
func (failingStorage) ExistingNotes(context.Context) ([]memory.NoteRef, error) { return nil, nil }
func (failingStorage) UpdateNote(context.Context, int64, string) error {
	return fmt.Errorf("boom: UpdateNote")
}
func (failingStorage) UpsertThread(context.Context, memory.ThreadUpdate) (int64, error) {
	return 0, fmt.Errorf("boom: UpsertThread")
}
func (failingStorage) ThreadsForAttribution(context.Context, int) ([]memory.Thread, error) {
	return nil, nil
}

// TestCompiler_FlushLogsStoreErrors pins Slice-0 observability: when a durable
// store write fails during flush, the failure must be logged at ERROR level, not
// silently discarded. Without this we are blind to memory loss (the exact "I
// can't see the failures" problem).
func TestCompiler_FlushLogsStoreErrors(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	c := memory.NewCompiler(&mockSummarizer{}, failingStorage{})
	ctx := context.Background()

	// one salient activity, then force the (synchronous) flush → store writes fail
	c.Ingest(ctx, tracker.Activity{App: "terminal", Title: "debugging", ScreenText: "stack trace here"})
	c.ForceFlush(ctx)

	if out := logBuf.String(); !strings.Contains(out, "level=ERROR") {
		t.Errorf("expected an ERROR log when a store write fails during flush, got: %q", out)
	}
}
