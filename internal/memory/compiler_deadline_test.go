package memory

// In-package so the test can name summarizerCallTimeout; everything else about the compiler is tested from memory_test.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"june/internal/tracker"
)

// neverAnswering is a summarizer that records the deadline it was called with and then refuses to answer. It stands in for the hung HTTP connection the genai client has no timeout of its own against.
type neverAnswering struct {
	mu          sync.Mutex
	hadDeadline bool
	deadline    time.Time
}

func (n *neverAnswering) AttributeThreads(ctx context.Context, _ []tracker.Activity, _ []Thread) (*ThreadAttribution, error) {
	n.mu.Lock()
	n.deadline, n.hadDeadline = ctx.Deadline()
	n.mu.Unlock()
	return nil, errors.New("no answer")
}

func (n *neverAnswering) ReconcileNotes(context.Context, []NoteRef, []string) ([]NoteOp, error) {
	return nil, nil
}

// discardStorage accepts every write and remembers nothing.
type discardStorage struct{}

func (discardStorage) LogSemanticNode(context.Context, TaskSummary) error           { return nil }
func (discardStorage) LogNote(context.Context, string, string) (int64, error)       { return 1, nil }
func (discardStorage) ExistingNotes(context.Context) ([]NoteRef, error)             { return nil, nil }
func (discardStorage) UpdateNote(context.Context, int64, string) error              { return nil }
func (discardStorage) UpsertThread(context.Context, ThreadUpdate) (int64, error)    { return 1, nil }
func (discardStorage) ThreadsForAttribution(context.Context, int) ([]Thread, error) { return nil, nil }
func (discardStorage) LinkEpisodesToThread(context.Context, int64, time.Time, time.Time) error {
	return nil
}

// The attribution call runs inline on the daemon's episode-drain goroutine, so it must carry a deadline of its own even when the caller hands it a context that has none. Without one a stalled connection blocks the drain, fills the event channel, and stops screen capture until the daemon is restarted.
func TestProcessFlush_BoundsTheAttributionCallWithItsOwnDeadline(t *testing.T) {
	llm := &neverAnswering{}
	c := NewCompiler(llm, discardStorage{})

	c.Ingest(context.Background(), tracker.Activity{App: "VSCode", Title: "main.go"})
	c.ForceFlush(context.Background())

	llm.mu.Lock()
	defer llm.mu.Unlock()
	if !llm.hadDeadline {
		t.Fatal("the attribution call was made with no deadline, so a connection that never answers would block the drain forever")
	}
	if left := time.Until(llm.deadline); left <= 0 || left > summarizerCallTimeout {
		t.Errorf("deadline is %s away, want a positive span no longer than summarizerCallTimeout (%s)", left, summarizerCallTimeout)
	}
}
