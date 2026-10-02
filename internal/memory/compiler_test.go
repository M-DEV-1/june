package memory_test

import (
	"context"
	"fmt"
	"june/internal/memory"
	"june/internal/tracker"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSummarizer stands in for the model. By default AttributeThreads returns one thread and no identity facts, and ReconcileNotes echoes every candidate back as an "add"; set attr, ops or opsErr to change that. Mutex-protected so `go test -race` can only blame the compiler.
type fakeSummarizer struct {
	mu        sync.Mutex
	attr      func() (*memory.ThreadAttribution, error)
	ops       []memory.NoteOp
	opsErr    error
	attrCalls int
}

func (s *fakeSummarizer) AttributeThreads(_ context.Context, activities []tracker.Activity, _ []memory.Thread) (*memory.ThreadAttribution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrCalls++
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
	mu       sync.Mutex
	fail     bool
	existing []memory.NoteRef
	semantic []memory.TaskSummary
	notes    []noteCall
	updates  []updateCall
	upserts  []memory.ThreadUpdate
	links    []linkCall
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

// The compiler flushes once the buffer's word count crosses its limit, and not a moment early. An application switch flushes only above the compiler's floor: alt-tabbing produces a switch every few seconds and each flush is one metered attribution call, so switches moments after the last flush leave the buffer alone.
func TestCompiler_FlushTriggers(t *testing.T) {
	cases := []struct {
		name      string
		ingest    []tracker.Activity
		wantFlush bool
	}{
		{"twenty app switches inside the floor do not flush", func() []tracker.Activity {
			var acts []tracker.Activity
			for i := 0; i < 10; i++ {
				acts = append(acts, tracker.Activity{App: "VSCode", Title: "main.go"}, tracker.Activity{App: "Chrome", Title: "Google"})
			}
			return acts
		}(), false},
		{"500 words total is under the limit", repeatActivity(5, 100), false},
		{"1500 words total crosses the limit", repeatActivity(5, 300), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			llm, store := &fakeSummarizer{}, &fakeStorage{}
			compiler := memory.NewCompiler(llm, store)
			for _, a := range c.ingest {
				compiler.Ingest(context.Background(), a)
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

// repeatActivity is n editor activities each carrying words words of screen text.
func repeatActivity(n, words int) []tracker.Activity {
	acts := make([]tracker.Activity, n)
	for i := range acts {
		acts[i] = tracker.Activity{App: "VSCode", Title: "main.go", ScreenText: strings.Repeat("word ", words)}
	}
	return acts
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

// TestCompiler_SuccessfulAttribution checks that when AttributeThreads returns two concurrent threads (entertainment id=0, work id=5), flush calls UpsertThread twice and LogSemanticNode twice, with SameTask = (u.ID != 0) for each and TaskNames matching the thread subjects.
// Each thread is also linked to the buffer's episodes, the one moment that connection is known. The link window closes when the buffer is snapshotted, not after the attribution call: at a two-second capture poll a thirty-second call would otherwise attach fifteen unrelated screens to the thread.
func TestCompiler_SuccessfulAttribution(t *testing.T) {
	llm := &fakeSummarizer{attr: func() (*memory.ThreadAttribution, error) {
		time.Sleep(60 * time.Millisecond) // stand-in for the attribution call
		return &memory.ThreadAttribution{Threads: []memory.ThreadUpdate{
			{ID: 0, Subject: "Suits", Kind: "entertainment", State: "s1e3", Summary: "watched ep 3", Novel: true},
			{ID: 5, Subject: "June project", Kind: "work", State: "writing tests", Summary: "added thread tests"},
		}}, nil
	}}
	store := &fakeStorage{}
	compiler := memory.NewCompiler(llm, store)
	ctx := context.Background()

	compiler.Ingest(ctx, tracker.Activity{App: "Netflix", Title: "Suits"})
	before := time.Now()
	compiler.ForceFlush(ctx)

	if len(store.links) != 2 {
		t.Fatalf("got %d links, want one per attributed thread", len(store.links))
	}
	for _, l := range store.links {
		if l.until.Before(before) || l.until.After(before.Add(50*time.Millisecond)) || l.since.After(l.until) {
			t.Errorf("link window %v..%v, want it closed at the snapshot taken at %v, before the attribution call", l.since, l.until, before)
		}
	}
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
	if !store.semantic[1].SameTask || store.semantic[1].TaskName != "June project" {
		t.Errorf("thread id=5: got SameTask=%v TaskName=%q, want true/\"June project\"", store.semantic[1].SameTask, store.semantic[1].TaskName)
	}
}

// TestCompiler_FallbackOnAttributionFailure checks that when AttributeThreads returns an error or an empty Threads list, flush writes a "Raw Activity Log" TaskSummary via LogSemanticNode and does NOT call UpsertThread. The fallback keeps "app | title" lines and never the raw screen text, which is the tens-of-KB junk row other code defends against.
func TestCompiler_FallbackOnAttributionFailure(t *testing.T) {
	cases := []struct {
		name string
		attr func() (*memory.ThreadAttribution, error)
	}{
		{"error from AttributeThreads", func() (*memory.ThreadAttribution, error) {
			return nil, fmt.Errorf("api rate limit")
		}},
		{"empty Threads slice from AttributeThreads", func() (*memory.ThreadAttribution, error) {
			return &memory.ThreadAttribution{Threads: []memory.ThreadUpdate{}}, nil
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStorage{}
			compiler := memory.NewCompiler(&fakeSummarizer{attr: tc.attr}, store)
			ctx := context.Background()

			compiler.Ingest(ctx, tracker.Activity{App: "VSCode", Title: "main.go", ScreenText: "func validateToken " + strings.Repeat("word ", 30)})
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
			if got := store.semantic[0].Summary; strings.Contains(got, "func validateToken") || !strings.Contains(got, "VSCode | main.go") {
				t.Errorf("fallback summary = %q, want the app | title line and no screen text", got)
			}
		})
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
