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

// fakeSummarizer stands in for the model. By default AttributeThreads returns one thread and no identity facts, and ReconcileNotes echoes every candidate back as an "add"; set attr, ops or opsErr to change that. Mutex-protected so `go test -race` can only blame the compiler.
type fakeSummarizer struct {
	mu             sync.Mutex
	attr           func() (*memory.ThreadAttribution, error)
	ops            []memory.NoteOp
	opsErr         error
	attrCalls      int
	reconcileCalls int
	received       []tracker.Activity
}

func (s *fakeSummarizer) AttributeThreads(_ context.Context, activities []tracker.Activity, _ []memory.Thread) (*memory.ThreadAttribution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrCalls++
	s.received = activities
	if s.attr != nil {
		return s.attr()
	}
	return &memory.ThreadAttribution{
		Threads: []memory.ThreadUpdate{{Subject: "mock task", Kind: "work", State: "mock state", Summary: "mock summary"}},
	}, nil
}

func (s *fakeSummarizer) ReconcileNotes(_ context.Context, _ []memory.NoteRef, candidates []string) ([]memory.NoteOp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconcileCalls++
	if s.opsErr != nil {
		return nil, s.opsErr
	}
	if s.ops != nil {
		return s.ops, nil
	}
	ops := make([]memory.NoteOp, len(candidates))
	for i, c := range candidates {
		ops[i] = memory.NoteOp{Action: "add", Content: c}
	}
	return ops, nil
}

// identityAttribution is the AttributeThreads result used by the note-reconciliation tests: one thread plus the identity facts that drive ReconcileNotes.
func identityAttribution(facts ...string) func() (*memory.ThreadAttribution, error) {
	return func() (*memory.ThreadAttribution, error) {
		return &memory.ThreadAttribution{
			Threads:  []memory.ThreadUpdate{{Subject: "notes task", Kind: "work", State: "did stuff", Summary: "did stuff"}},
			Identity: facts,
		}, nil
	}
}

type noteCall struct{ content, kind string }
type updateCall struct {
	id      int64
	content string
}

// fakeStorage records every durable write. With fail set, each write returns an error instead.
type fakeStorage struct {
	mu             sync.Mutex
	fail           bool
	existing       []memory.NoteRef
	semantic       []memory.TaskSummary
	notes          []noteCall
	updates        []updateCall
	upserts        []memory.ThreadUpdate
	existingCalled int
}

func (s *fakeStorage) LogSemanticNode(_ context.Context, summary memory.TaskSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.semantic = append(s.semantic, summary)
	if s.fail {
		return fmt.Errorf("boom: LogSemanticNode")
	}
	return nil
}

func (s *fakeStorage) LogNote(_ context.Context, content, kind string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, noteCall{content, kind})
	if s.fail {
		return 0, fmt.Errorf("boom: LogNote")
	}
	return int64(len(s.notes)), nil
}

func (s *fakeStorage) ExistingNotes(context.Context) ([]memory.NoteRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.existingCalled++
	return s.existing, nil
}

func (s *fakeStorage) UpdateNote(_ context.Context, id int64, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, updateCall{id, content})
	if s.fail {
		return fmt.Errorf("boom: UpdateNote")
	}
	return nil
}

func (s *fakeStorage) UpsertThread(_ context.Context, u memory.ThreadUpdate) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts = append(s.upserts, u)
	if s.fail {
		return 0, fmt.Errorf("boom: UpsertThread")
	}
	return u.ID, nil
}

func (s *fakeStorage) ThreadsForAttribution(context.Context, int) ([]memory.Thread, error) {
	return nil, nil
}

func TestCompiler_BuffersWithoutFlushing(t *testing.T) {
	llm, store := &fakeSummarizer{}, &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "db.go"})
	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "agent.go"})

	if llm.attrCalls > 0 {
		t.Errorf("expected 0 LLM calls, got %d", llm.attrCalls)
	}
	if compiler.BufferSize() != 3 {
		t.Errorf("expected buffer size 3, got %d", compiler.BufferSize())
	}
}

func TestCompiler_FlushesOnAppChange(t *testing.T) {
	llm, store := &fakeSummarizer{}, &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "Google"})

	if llm.attrCalls != 1 {
		t.Errorf("expected 1 LLM call, got %d", llm.attrCalls)
	}
	if len(store.semantic) != 1 {
		t.Errorf("expected 1 store call, got %d", len(store.semantic))
	}
	if compiler.BufferSize() != 1 {
		t.Errorf("expected buffer size 1 (the new app), got %d", compiler.BufferSize())
	}
}

func TestCompiler_FlushesOnWordCountLimit(t *testing.T) {
	llm, store := &fakeSummarizer{}, &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	// 300 words each × 5 = 1500 words → should trigger flush on the 5th ingest
	screenText := strings.Repeat("word ", 300)
	for i := 0; i < 5; i++ {
		compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go", ScreenText: screenText})
	}

	if llm.attrCalls != 1 {
		t.Errorf("expected 1 flush at word limit, got %d", llm.attrCalls)
	}
	if len(store.semantic) != 1 {
		t.Errorf("expected 1 store call, got %d", len(store.semantic))
	}
}

func TestCompiler_NoFlushBelowWordLimit(t *testing.T) {
	llm := &fakeSummarizer{}
	compiler := memory.NewCompiler(llm, &fakeStorage{})
	ctx := context.Background()

	// 100 words each × 5 = 500 words → under limit, no flush
	screenText := strings.Repeat("word ", 100)
	for i := 0; i < 5; i++ {
		compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go", ScreenText: screenText})
	}

	if llm.attrCalls != 0 {
		t.Errorf("expected no flush under word limit, got %d LLM calls", llm.attrCalls)
	}
}

func TestCompiler_PassesScreenTextToSummarizer(t *testing.T) {
	llm := &fakeSummarizer{}
	compiler := memory.NewCompiler(llm, &fakeStorage{})
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

// attributionFails is a summarizer whose AttributeThreads always errors, sending flush down the fallback path.
func attributionFails() *fakeSummarizer {
	return &fakeSummarizer{attr: func() (*memory.ThreadAttribution, error) {
		return nil, fmt.Errorf("api rate limit reached")
	}}
}

// TestCompiler_FallbackOmitsScreenText_KeepsAppAndTitle verifies the LLM-failure fallback stores only "app | title" lines, never the raw ScreenText — dumping full raw captures into one node is exactly the tens-of-KB junk row shape other code (truncateUTF8, excerptContent) exists to defend against.
func TestCompiler_FallbackOmitsScreenText_KeepsAppAndTitle(t *testing.T) {
	store := &fakeStorage{}
	compiler := memory.NewCompiler(attributionFails(), store)
	ctx := context.Background()

	// Pad screen text to exceed minFlushWords so the flush is not discarded.
	screenText := "func validateToken " + strings.Repeat("word ", 30)
	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "auth.go", ScreenText: screenText})
	compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "Google"})

	if len(store.semantic) != 1 {
		t.Fatalf("expected 1 fallback summary stored, got %d", len(store.semantic))
	}
	got := store.semantic[0].Summary
	if strings.Contains(got, "func validateToken") {
		t.Errorf("expected the fallback summary to omit ScreenText, got: %q", got)
	}
	if !strings.Contains(got, "VSCode | auth.go") {
		t.Errorf("expected the fallback summary to keep the app | title line, got: %q", got)
	}
}

// TestCompiler_FallbackCapsTotalLength verifies the fallback summary is capped at roughly 2000 runes even with many activities in the buffer, instead of growing unbounded with the buffer size.
func TestCompiler_FallbackCapsTotalLength(t *testing.T) {
	store := &fakeStorage{}
	compiler := memory.NewCompiler(attributionFails(), store)
	ctx := context.Background()

	// Enough long titles to exceed 2000 runes if uncapped: ~50 runes/line * 100 lines = ~5000 runes.
	for i := 0; i < 100; i++ {
		compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: fmt.Sprintf("a-fairly-long-file-name-number-%d.go", i)})
	}
	compiler.ForceFlush(ctx)

	if len(store.semantic) != 1 {
		t.Fatalf("expected 1 fallback summary stored, got %d", len(store.semantic))
	}
	// +1 tolerance: truncateRunes appends a trailing "…" marker after cutting to the cap.
	if got := len([]rune(store.semantic[0].Summary)); got > 2001 {
		t.Errorf("expected the fallback summary capped at ~2000 runes, got %d", got)
	}
}

func TestCompiler_AutoExtractsNotes(t *testing.T) {
	llm := &fakeSummarizer{attr: identityAttribution("user prefers terse responses", "user is debugging the React PR")}
	store := &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.ForceFlush(ctx)

	if len(store.semantic) != 1 {
		t.Fatalf("expected 1 LogSemanticNode call, got %d", len(store.semantic))
	}
	if len(store.notes) != 2 {
		t.Fatalf("expected 2 LogNote calls, got %d", len(store.notes))
	}
	if store.notes[0].content != "user prefers terse responses" || store.notes[1].content != "user is debugging the React PR" {
		t.Errorf("unexpected notes: %+v", store.notes)
	}
	for _, c := range store.notes {
		if c.kind != "fact" {
			t.Errorf("expected kind 'fact', got %q", c.kind)
		}
	}
}

func TestCompiler_ReconcileUpdate(t *testing.T) {
	llm := &fakeSummarizer{
		attr: identityAttribution("user prefers terse and concise responses"),
		ops: []memory.NoteOp{
			{Action: "update", ID: 7, Content: "user prefers terse and concise responses"},
			{Action: "skip"},
		},
	}
	store := &fakeStorage{existing: []memory.NoteRef{{ID: 7, Content: "user prefers terse responses"}}}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.ForceFlush(ctx)

	if len(store.notes) != 0 {
		t.Errorf("expected 0 LogNote calls for update/skip ops, got %d", len(store.notes))
	}
	if len(store.updates) != 1 {
		t.Fatalf("expected 1 UpdateNote call, got %d", len(store.updates))
	}
	if store.updates[0].id != 7 || store.updates[0].content != "user prefers terse and concise responses" {
		t.Errorf("unexpected UpdateNote call: %+v", store.updates[0])
	}
}

func TestCompiler_ReconcileAdd(t *testing.T) {
	llm := &fakeSummarizer{
		attr: identityAttribution("user works in Go"),
		ops:  []memory.NoteOp{{Action: "add", Content: "user works in Go"}},
	}
	store := &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.ForceFlush(ctx)

	if len(store.notes) != 1 || store.notes[0].content != "user works in Go" {
		t.Fatalf("expected the added note to be logged, got %+v", store.notes)
	}
	if len(store.updates) != 0 {
		t.Errorf("expected 0 UpdateNote calls for add op, got %d", len(store.updates))
	}
}

// A reconciliation the model could not do must still keep the facts: they get logged as new notes rather than dropped.
func TestCompiler_ReconcileErrorFallback(t *testing.T) {
	llm := &fakeSummarizer{
		attr:   identityAttribution("user prefers terse responses", "user is debugging the React PR"),
		opsErr: fmt.Errorf("llm timeout"),
	}
	store := &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
	compiler.ForceFlush(ctx)

	if len(store.notes) != 2 {
		t.Fatalf("expected 2 LogNote calls on fallback, got %d", len(store.notes))
	}
	if store.notes[0].content != "user prefers terse responses" || store.notes[1].content != "user is debugging the React PR" {
		t.Errorf("unexpected fallback notes: %+v", store.notes)
	}
	if len(store.updates) != 0 {
		t.Errorf("expected 0 UpdateNote calls on fallback, got %d", len(store.updates))
	}
}

func TestIsSalient(t *testing.T) {
	cases := []struct {
		name string
		act  tracker.Activity
		want bool
	}{
		{"empty title and empty screen text", tracker.Activity{App: "Explorer"}, false},
		{"whitespace-only title", tracker.Activity{App: "Explorer", Title: "   "}, false},
		{"new tab lowercase", tracker.Activity{App: "Chrome", Title: "new tab"}, false},
		{"New Tab mixed case", tracker.Activity{App: "Chrome", Title: "New Tab"}, false},
		{"untitled", tracker.Activity{App: "Notepad", Title: "Untitled"}, false},
		{"desktop", tracker.Activity{App: "Explorer", Title: "Desktop"}, false},
		{"trivial title but non-trivial screen text", tracker.Activity{App: "Chrome", Title: "New Tab", ScreenText: "package main\n\nfunc main() {}"}, true},
		{"real title", tracker.Activity{App: "VSCode", Title: "compiler.go"}, true},
		{"real title and screen text", tracker.Activity{App: "VSCode", Title: "auth.go", ScreenText: "func validateToken(tok string) bool"}, true},
		{"empty title but has screen text", tracker.Activity{App: "Terminal", ScreenText: "go build ./..."}, true},
	}

	for _, tc := range cases {
		if got := memory.IsSalient(tc.act); got != tc.want {
			t.Errorf("%s: IsSalient(%+v) = %v, want %v", tc.name, tc.act, got, tc.want)
		}
	}
}

func TestCompiler_NonSalientActivitiesNotBuffered(t *testing.T) {
	llm, store := &fakeSummarizer{}, &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "New Tab"})
	compiler.Ingest(ctx, tracker.Activity{App: "Explorer", Title: "Desktop"})
	compiler.Ingest(ctx, tracker.Activity{App: "Notepad", Title: "Untitled"})

	if compiler.BufferSize() != 0 {
		t.Errorf("expected buffer size 0 for non-salient activities, got %d", compiler.BufferSize())
	}
	if llm.attrCalls != 0 || len(store.semantic) != 0 {
		t.Errorf("non-salient activities reached the model or the store: %d LLM calls, %d store calls", llm.attrCalls, len(store.semantic))
	}
}

// TestCompiler_ThinTitleActivityFlushes checks that a salient activity with only a window title (no screen text) is summarized — social/gaming/meeting sessions produce sparse screen content but are still worth remembering.
func TestCompiler_ThinTitleActivityFlushes(t *testing.T) {
	llm, store := &fakeSummarizer{}, &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "Discord", Title: "General (voice)"})
	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})

	if llm.attrCalls != 1 {
		t.Errorf("expected 1 LLM call for thin-title flush, got %d", llm.attrCalls)
	}
	if len(store.semantic) != 1 {
		t.Errorf("expected 1 store call for thin-title flush, got %d", len(store.semantic))
	}
}

// TestCompiler_SuccessfulAttribution checks that when AttributeThreads returns two concurrent threads (entertainment id=0, work id=5), flush calls UpsertThread twice and LogSemanticNode twice, with SameTask = (u.ID != 0) for each and TaskNames matching the thread subjects.
func TestCompiler_SuccessfulAttribution(t *testing.T) {
	llm := &fakeSummarizer{attr: func() (*memory.ThreadAttribution, error) {
		return &memory.ThreadAttribution{Threads: []memory.ThreadUpdate{
			{ID: 0, Subject: "Suits", Kind: "entertainment", State: "s1e3", Summary: "watched ep 3", Novel: true},
			{ID: 5, Subject: "ORA project", Kind: "work", State: "writing tests", Summary: "added thread tests"},
		}}, nil
	}}
	store := &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "Netflix", Title: "Suits"})
	compiler.ForceFlush(ctx)

	if len(store.upserts) != 2 {
		t.Fatalf("expected 2 UpsertThread calls, got %d", len(store.upserts))
	}
	if len(store.semantic) != 2 {
		t.Fatalf("expected 2 LogSemanticNode calls, got %d", len(store.semantic))
	}
	// An unknown thread (ID 0) starts a new task; a known one (ID 5) continues it.
	if store.semantic[0].SameTask || store.semantic[0].TaskName != "Suits" {
		t.Errorf("thread id=0: got SameTask=%v TaskName=%q, want false/\"Suits\"", store.semantic[0].SameTask, store.semantic[0].TaskName)
	}
	if !store.semantic[1].SameTask || store.semantic[1].TaskName != "ORA project" {
		t.Errorf("thread id=5: got SameTask=%v TaskName=%q, want true/\"ORA project\"", store.semantic[1].SameTask, store.semantic[1].TaskName)
	}
}

// Note reconciliation costs an LLM call and a table scan, so flush only does it when the attribution actually produced identity facts.
func TestCompiler_IdentityReconciliationOnlyWhenThereAreFacts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts []string
		want  bool
	}{
		{"identity facts present", []string{"user prefers Go"}, true},
		{"no identity facts", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			llm := &fakeSummarizer{attr: identityAttribution(tc.facts...)}
			store := &fakeStorage{}
			compiler := memory.NewCompiler(llm, store)
			ctx := context.Background()

			compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
			compiler.ForceFlush(ctx)

			if got := store.existingCalled > 0; got != tc.want {
				t.Errorf("ExistingNotes called = %v, want %v", got, tc.want)
			}
			if got := llm.reconcileCalls > 0; got != tc.want {
				t.Errorf("ReconcileNotes called = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCompiler_FallbackOnAttributionFailure checks that when AttributeThreads returns an error, nil, or an empty Threads list, flush writes a "Raw Activity Log" TaskSummary via LogSemanticNode and does NOT call UpsertThread.
func TestCompiler_FallbackOnAttributionFailure(t *testing.T) {
	cases := []struct {
		name string
		attr func() (*memory.ThreadAttribution, error)
	}{
		{"error from AttributeThreads", func() (*memory.ThreadAttribution, error) {
			return nil, fmt.Errorf("api rate limit")
		}},
		{"nil attr from AttributeThreads", func() (*memory.ThreadAttribution, error) { return nil, nil }},
		{"empty Threads slice from AttributeThreads", func() (*memory.ThreadAttribution, error) {
			return &memory.ThreadAttribution{Threads: []memory.ThreadUpdate{}}, nil
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStorage{}
			compiler := memory.NewCompiler(&fakeSummarizer{attr: tc.attr}, store)
			ctx := context.Background()

			compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
			compiler.ForceFlush(ctx)

			if len(store.upserts) != 0 {
				t.Errorf("UpsertThread must not be called on attribution failure, got %d call(s)", len(store.upserts))
			}
			if len(store.semantic) != 1 {
				t.Fatalf("expected 1 fallback LogSemanticNode call, got %d", len(store.semantic))
			}
			if store.semantic[0].TaskName != "Raw Activity Log" {
				t.Errorf("expected fallback TaskName 'Raw Activity Log', got %q", store.semantic[0].TaskName)
			}
			if store.semantic[0].SameTask {
				t.Error("fallback SameTask must be false")
			}
		})
	}
}

// TestCompiler_ConcurrentAccess drives Ingest, GetCurrentBuffer, and ForceFlush from many goroutines at once, mirroring daemon usage (ingest loop, hourly ticker, /buffer HTTP handler, Agent.Connect) — none of which serialize access to Compiler's buffer/wordCount/lastFlush, so this must pass under `go test -race`.
func TestCompiler_ConcurrentAccess(t *testing.T) {
	compiler := memory.NewCompiler(&fakeSummarizer{}, &fakeStorage{})
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

	// concurrent reads of the live buffer, simulating the /buffer HTTP handler and Agent.Connect
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				buf := compiler.GetCurrentBuffer()
				// touch the returned slice the way callers do (range over it) — exactly what races against a concurrent append.
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

// TestCompiler_FlushLogsStoreErrors checks that a failed durable store write during flush gets logged at ERROR level, not silently dropped.
func TestCompiler_FlushLogsStoreErrors(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	c := memory.NewCompiler(&fakeSummarizer{}, &fakeStorage{fail: true})
	ctx := context.Background()

	// one salient activity, then force the (synchronous) flush → store writes fail
	c.Ingest(ctx, tracker.Activity{App: "terminal", Title: "debugging", ScreenText: "stack trace here"})
	c.ForceFlush(ctx)

	if out := logBuf.String(); !strings.Contains(out, "level=ERROR") {
		t.Errorf("expected an ERROR log when a store write fails during flush, got: %q", out)
	}
}
