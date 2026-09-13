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
	links          []linkCall
	existingCalled int
}

// linkCall is one LinkEpisodesToThread the compiler made, so a test can check the buffer's evidence was joined to the thread it was attributed to.
type linkCall struct {
	threadID     int64
	since, until time.Time
}

func (s *fakeStorage) LinkEpisodesToThread(_ context.Context, threadID int64, since, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links = append(s.links, linkCall{threadID, since, until})
	return nil
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

// An application switch flushes only above the compiler's floor. Alt-tabbing between two windows produces a switch every few seconds, and each flush is one metered attribution call, so a switch that comes moments after the last flush or with almost nothing buffered leaves the buffer alone — the word limit and the hourly tick still flush it.
func TestCompiler_AppChangeBelowTheFloorDoesNotFlush(t *testing.T) {
	llm, store := &fakeSummarizer{}, &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	// Twenty switches back and forth, all within the minimum interval of the compiler's construction.
	for i := 0; i < 10; i++ {
		compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
		compiler.Ingest(ctx, tracker.Activity{App: "Chrome", Title: "Google"})
	}

	if llm.attrCalls != 0 {
		t.Errorf("expected no attribution call for app switches inside the floor, got %d", llm.attrCalls)
	}
	if len(store.semantic) != 0 {
		t.Errorf("expected nothing written for app switches inside the floor, got %d", len(store.semantic))
	}
	if compiler.BufferSize() != 20 {
		t.Errorf("expected all 20 activities still buffered, got %d", compiler.BufferSize())
	}
}

// The compiler flushes once the buffer's word count crosses its limit, and must not flush a moment early.
func TestCompiler_FlushesOnWordCountLimit(t *testing.T) {
	cases := []struct {
		name          string
		wordsPerEntry int
		wantFlush     bool
	}{
		{"500 words total is under the limit: no flush", 100, false},
		{"1500 words total crosses the limit: flushes", 300, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			llm, store := &fakeSummarizer{}, &fakeStorage{}
			compiler := memory.NewCompiler(llm, store)
			ctx := context.Background()

			screenText := strings.Repeat("word ", c.wordsPerEntry)
			for i := 0; i < 5; i++ {
				compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go", ScreenText: screenText})
			}

			wantCalls := 0
			if c.wantFlush {
				wantCalls = 1
			}
			if llm.attrCalls != wantCalls {
				t.Errorf("LLM calls = %d, want %d", llm.attrCalls, wantCalls)
			}
			if len(store.semantic) != wantCalls {
				t.Errorf("store calls = %d, want %d", len(store.semantic), wantCalls)
			}
		})
	}
}

func TestCompiler_PassesScreenTextToSummarizer(t *testing.T) {
	llm := &fakeSummarizer{}
	compiler := memory.NewCompiler(llm, &fakeStorage{})
	ctx := context.Background()

	// Pad screen text to exceed minFlushWords so the flush is not discarded.
	screenText := "func validateToken " + strings.Repeat("word ", 30)
	compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "auth.go", ScreenText: screenText})
	compiler.ForceFlush(ctx)

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
	compiler.ForceFlush(ctx)

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

// Reconciling identity facts against existing notes can add a new note, update one that already says almost the same thing (and skip a duplicate), or — when the model call itself fails — fall back to logging every fact as new rather than dropping it.
func TestCompiler_Reconcile(t *testing.T) {
	cases := []struct {
		name        string
		facts       []string
		ops         []memory.NoteOp
		opsErr      error
		existing    []memory.NoteRef
		wantNotes   []noteCall
		wantUpdates []updateCall
	}{
		{
			name:      "add",
			facts:     []string{"user works in Go"},
			ops:       []memory.NoteOp{{Action: "add", Content: "user works in Go"}},
			wantNotes: []noteCall{{content: "user works in Go", kind: "fact"}},
		},
		{
			name:        "update an existing note and skip a duplicate",
			facts:       []string{"user prefers terse and concise responses"},
			ops:         []memory.NoteOp{{Action: "update", ID: 7, Content: "user prefers terse and concise responses"}, {Action: "skip"}},
			existing:    []memory.NoteRef{{ID: 7, Content: "user prefers terse responses"}},
			wantUpdates: []updateCall{{id: 7, content: "user prefers terse and concise responses"}},
		},
		{
			// A reconciliation the model could not do must still keep the facts: they get logged as new notes rather than dropped.
			name:   "a failed reconciliation call falls back to logging every fact",
			facts:  []string{"user prefers terse responses", "user is debugging the React PR"},
			opsErr: fmt.Errorf("llm timeout"),
			wantNotes: []noteCall{
				{content: "user prefers terse responses", kind: "fact"},
				{content: "user is debugging the React PR", kind: "fact"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			llm := &fakeSummarizer{attr: identityAttribution(c.facts...), ops: c.ops, opsErr: c.opsErr}
			store := &fakeStorage{existing: c.existing}
			compiler := memory.NewCompiler(llm, store)
			ctx := context.Background()

			compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go"})
			compiler.ForceFlush(ctx)

			if len(store.notes) != len(c.wantNotes) {
				t.Fatalf("LogNote calls = %+v, want %+v", store.notes, c.wantNotes)
			}
			for i, n := range c.wantNotes {
				if store.notes[i] != n {
					t.Errorf("note %d = %+v, want %+v", i, store.notes[i], n)
				}
			}
			if len(store.updates) != len(c.wantUpdates) {
				t.Fatalf("UpdateNote calls = %+v, want %+v", store.updates, c.wantUpdates)
			}
			for i, u := range c.wantUpdates {
				if store.updates[i] != u {
					t.Errorf("update %d = %+v, want %+v", i, store.updates[i], u)
				}
			}
		})
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

// Attributing a buffer to a thread is the one moment the connection between a thread and the screens behind it is known. The compiler used to compute it and throw it away on every flush, leaving threads that could say "reviewed the code, eleven findings" with no path to the findings.
func TestCompiler_FlushLinksTheBuffersEpisodesToEachThread(t *testing.T) {
	store := &fakeStorage{}
	llm := &fakeSummarizer{attr: func() (*memory.ThreadAttribution, error) {
		return &memory.ThreadAttribution{Threads: []memory.ThreadUpdate{
			{Subject: "code review of ora", Kind: "work", Summary: "eleven findings"},
			{Subject: "reading the diff", Kind: "work", Summary: "walked the changes"},
		}}, nil
	}}
	c := memory.NewCompiler(llm, store)
	c.Ingest(context.Background(), tracker.Activity{App: "Code", Title: "search.go", ScreenText: strings.Repeat("reviewing the findings ", 30)})

	before := time.Now()
	c.ForceFlush(context.Background())

	if len(store.links) != 2 {
		t.Fatalf("got %d links, want one per attributed thread", len(store.links))
	}
	for _, l := range store.links {
		if l.until.Before(before) {
			t.Errorf("link window ends before the flush began: %+v", l)
		}
		if l.since.After(l.until) {
			t.Errorf("link window runs backwards: %+v", l)
		}
	}
}

// The link window has to close when the buffer is snapshotted, not when the linking happens — processFlush runs an attribution LLM call in between, and at a two-second capture poll a thirty-second call would attach fifteen unrelated screens to the thread.
func TestCompiler_LinkWindowClosesBeforeTheAttributionCall(t *testing.T) {
	store := &fakeStorage{}
	llm := &fakeSummarizer{attr: func() (*memory.ThreadAttribution, error) {
		time.Sleep(60 * time.Millisecond) // stand-in for the attribution call
		return &memory.ThreadAttribution{Threads: []memory.ThreadUpdate{{Subject: "code review", Kind: "work"}}}, nil
	}}
	c := memory.NewCompiler(llm, store)
	c.Ingest(context.Background(), tracker.Activity{App: "Code", Title: "search.go", ScreenText: strings.Repeat("reviewing ", 40)})

	c.ForceFlush(context.Background())

	if len(store.links) != 1 {
		t.Fatalf("got %d links, want 1", len(store.links))
	}
	if slept := store.links[0].until.Add(50 * time.Millisecond); slept.After(time.Now()) {
		t.Errorf("window end %v looks like it was read after the attribution call, not at snapshot", store.links[0].until)
	}
}

// The compiler writes summaries from screen text that names the user in the third person — a calendar entry "Meeting with Zemna Braxen" became "participated in a scheduled meeting with Zemna Braxen" on 2026-09-01, and Ora then told the user about their meetings with Zemna. The prompt has to say who the user is.
func TestAttributePrompt_NamesTheUser(t *testing.T) {
	prompt := memory.AttributePrompt(nil, nil, "The user is Zemna Braxen — goes by Zemna.")
	if !strings.Contains(prompt, "Zemna Braxen") {
		t.Errorf("prompt does not carry the identity line:\n%s", prompt)
	}
	if !strings.Contains(prompt, "third party") {
		t.Errorf("prompt does not tell the model the user is never a third party:\n%s", prompt)
	}
	if strings.Contains(memory.AttributePrompt(nil, nil, ""), "third party") {
		t.Error("with no identity known, the prompt should not carry an empty identity rule")
	}
}

// The shutdown flush empties the buffer before the model call starts, so a caller whose deadline expires first would walk away from activity that is nowhere else. ForceFlush must instead record the drained buffer as a raw-activity node when the deadline hits.
func TestCompiler_ForceFlush_WritesTheFallbackWhenTheDeadlineHits(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	llm := &fakeSummarizer{attr: func() (*memory.ThreadAttribution, error) {
		<-release
		return nil, fmt.Errorf("the model finally answered, long after the caller gave up")
	}}
	store := &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)

	compiler.Ingest(context.Background(), tracker.Activity{App: "VSCode", Title: "main.go"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	compiler.ForceFlush(ctx)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.semantic) != 1 {
		t.Fatalf("expected the drained buffer written as 1 fallback node when the deadline hit, got %d writes", len(store.semantic))
	}
	if store.semantic[0].TaskName != "Raw Activity Log" {
		t.Errorf("task name = %q, want %q", store.semantic[0].TaskName, "Raw Activity Log")
	}
	if !strings.Contains(store.semantic[0].Summary, "VSCode | main.go") {
		t.Errorf("fallback summary = %q, want it to carry the drained activity's app and title", store.semantic[0].Summary)
	}
}
