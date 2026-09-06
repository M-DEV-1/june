package dream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/db"
	"ora/internal/db/dbtest"
)

// insertSummaryNode writes one 'summary' node with a controlled created_at, JSON-shaped the way the compiler writes them, for the replay stage to read back through SummaryTimeline.
func insertSummaryNode(t *testing.T, store *db.Store, taskName, summary string, ts time.Time) {
	t.Helper()
	content := fmt.Sprintf(`{"task_name": %q, "summary": %q}`, taskName, summary)
	if _, err := store.DB().Exec(`INSERT INTO nodes (type, content, created_at) VALUES ('summary', ?, ?)`,
		content, ts.UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatalf("insert summary node: %v", err)
	}
}

// insertActiveThread writes one status='active' thread row for the replay stage's per-item subject list.
func insertActiveThread(t *testing.T, store *db.Store, subject, state string) {
	t.Helper()
	if _, err := store.DB().Exec(`INSERT INTO threads (subject, kind, state, status) VALUES (?, 'project', ?, 'active')`, subject, state); err != nil {
		t.Fatalf("insert active thread: %v", err)
	}
}

// fakeShadowSeq answers each call with the next reply from a fixed queue, in order — the replay stage's one-call-per-item shape needs a distinct canned reply per item, unlike fakeBrain's one-reply-per-kind dispatch.
type fakeShadowSeq struct {
	mu      sync.Mutex
	replies []string
	calls   int
}

func (f *fakeShadowSeq) fn(ctx context.Context, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i >= len(f.replies) {
		return "", fmt.Errorf("fakeShadowSeq: no reply queued for call %d", i)
	}
	return f.replies[i], nil
}

// stepClock is a fake clock that hands out the next time in a fixed sequence on each call, holding the last entry once the sequence runs out — how the budget-cutoff test fast-forwards past the stage's time budget between two checks.
type stepClock struct {
	mu    sync.Mutex
	times []time.Time
	i     int
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.times[c.i]
	if c.i < len(c.times)-1 {
		c.i++
	}
	return t
}

// With no shadow brain configured, the replay stage skips outright: no calls, no artifact, and — deliberately — no stage token, so a shadow configured later still gets its first night.
func TestReplayStage_NoShadowSkipsCleanly(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(dayFormat)
	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	if err := store.StartDreamRun(ctx, night); err != nil {
		t.Fatal(err)
	}

	rep, err := r.replayStage(ctx, night)
	if err != nil {
		t.Fatalf("replayStage: %v", err)
	}
	if !rep.skipped {
		t.Errorf("report = %+v, want skipped", rep)
	}
	run, _, _ := store.DreamRun(ctx, night)
	if run.StagesDone != "" {
		t.Errorf("stages_done = %q, want no token committed on a skip", run.StagesDone)
	}
}

// The accumulator: salience 0 drops an item's contribution entirely, facts merge case-insensitively across items with an occurrence count, people accumulate the same way, a hallucinated thread that names no active subject falls into the "none" pile alongside an explicit "none", and the artifact renders the piles with "none" last regardless of score.
func TestReplayStage_AccumulatesPilesGroupingDedupAndDropsZeroSalience(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.StartDreamRun(ctx, night); err != nil {
		t.Fatal(err)
	}
	insertActiveThread(t, store, "Project Nimbus", "mid sprint")

	base := at(10, 0)
	insertSummaryNode(t, store, "Raw Activity Log", "app|title noise, must never be read", base.Add(-1*time.Minute))
	insertSummaryNode(t, store, "Ora dreaming loop", "shipped the API", base)
	insertSummaryNode(t, store, "Ora dreaming loop", "shipped the API again", base.Add(time.Minute))
	insertSummaryNode(t, store, "Reading", "read chapter 3", base.Add(2*time.Minute))
	insertSummaryNode(t, store, "Ora dreaming loop", "a nothing moment", base.Add(3*time.Minute))
	insertSummaryNode(t, store, "Elsewhere", "wandered off topic", base.Add(4*time.Minute))

	shadow := &fakeShadowSeq{replies: []string{
		`{"salience":2,"facts":["Shipped the API"],"people":["Alice"],"thread":"Project Nimbus"}`,
		`{"salience":1,"facts":["shipped the api"],"people":["Alice","Bob"],"thread":"Project Nimbus"}`,
		`{"salience":3,"facts":["Read chapter 3"],"people":[],"thread":"none"}`,
		`{"salience":0,"facts":["Should not appear"],"people":["Ghost"],"thread":"Project Nimbus"}`,
		`{"salience":1,"facts":["Wandered off topic"],"people":[],"thread":"Some Hallucinated Subject"}`,
	}}
	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	r.activeShadow = shadow.fn
	r.DataDir = t.TempDir()

	rep, err := r.replayStage(ctx, night)
	if err != nil {
		t.Fatalf("replayStage: %v", err)
	}
	if rep.items != 5 {
		t.Errorf("items = %d, want 5 (the Raw Activity Log entry excluded)", rep.items)
	}
	if rep.calls != 5 || rep.failures != 0 {
		t.Errorf("calls=%d failures=%d, want 5 calls and no failures", rep.calls, rep.failures)
	}
	if rep.piles != 2 {
		t.Errorf("piles = %d, want 2 (Project Nimbus and none)", rep.piles)
	}

	raw, err := os.ReadFile(filepath.Join(r.DataDir, "dreams", night+"-replay.md"))
	if err != nil {
		t.Fatalf("reading the artifact: %v", err)
	}
	artifact := string(raw)
	for _, want := range []string{
		"Items read: 5",
		"Calls made: 5",
		"Parse failures: 0",
		"Budget: not hit",
		"## Project Nimbus (score 3)",
		"State: mid sprint",
		"Shipped the API (x2)",
		"Alice (x2)",
		"Bob\n",
		"## none (score 4)",
		"Read chapter 3 (x1)",
		"Wandered off topic (x1)",
	} {
		if !strings.Contains(artifact, want) {
			t.Errorf("artifact lacks %q:\n%s", want, artifact)
		}
	}
	if strings.Contains(artifact, "Should not appear") || strings.Contains(artifact, "Ghost") {
		t.Error("the salience-0 item's facts and people must not appear anywhere")
	}
	if strings.Index(artifact, "## Project Nimbus") > strings.Index(artifact, "## none") {
		t.Error("the none pile must render last")
	}
}

// A call error and an unparsable reply both count as a failure and skip the item; neither fails the stage, and the token still commits.
func TestReplayStage_ParseFailureCountedNotFatal(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.StartDreamRun(ctx, night); err != nil {
		t.Fatal(err)
	}
	base := at(10, 0)
	insertSummaryNode(t, store, "Work", "first moment", base)
	insertSummaryNode(t, store, "Work", "second moment", base.Add(time.Minute))

	shadow := &fakeShadowSeq{replies: []string{
		"I would rather not answer in JSON today.",
		`{"salience":1,"facts":["ok"],"people":[],"thread":"none"}`,
	}}
	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	r.activeShadow = shadow.fn

	rep, err := r.replayStage(ctx, night)
	if err != nil {
		t.Fatalf("replayStage: %v", err)
	}
	if rep.items != 2 || rep.calls != 2 {
		t.Errorf("items=%d calls=%d, want both items read and called", rep.items, rep.calls)
	}
	if rep.failures != 1 {
		t.Errorf("failures = %d, want 1", rep.failures)
	}
	if rep.piles != 1 {
		t.Errorf("piles = %d, want 1 (only the item that parsed)", rep.piles)
	}
	run, _, _ := store.DreamRun(ctx, night)
	if !strings.Contains(run.StagesDone, "replay") {
		t.Errorf("stages_done = %q, want the replay token despite the parse failure", run.StagesDone)
	}
}

// Running out of the stage's time budget mid-night still writes the partial piles built so far and commits the token, so the next wake does not redo the work.
func TestReplayStage_BudgetCutoffWritesPartialArtifactAndToken(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.StartDreamRun(ctx, night); err != nil {
		t.Fatal(err)
	}
	base := at(10, 0)
	insertSummaryNode(t, store, "Work", "first moment", base)
	insertSummaryNode(t, store, "Work", "second moment", base.Add(time.Minute))

	shadow := &fakeShadowSeq{replies: []string{
		`{"salience":2,"facts":["Did the first thing"],"people":[],"thread":"none"}`,
	}}
	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	r.activeShadow = shadow.fn
	r.DataDir = t.TempDir()
	wall := at(23, 30)
	clock := &stepClock{times: []time.Time{wall, wall, wall.Add(time.Hour)}}
	r.now = clock.now

	rep, err := r.replayStage(ctx, night)
	if err != nil {
		t.Fatalf("replayStage: %v", err)
	}
	if rep.items != 1 || rep.calls != 1 {
		t.Errorf("items=%d calls=%d, want exactly the one item read before the budget tripped", rep.items, rep.calls)
	}
	if !rep.partial || rep.partialReason != "budget" {
		t.Errorf("partial=%v reason=%q, want a budget-caused partial", rep.partial, rep.partialReason)
	}
	run, _, _ := store.DreamRun(ctx, night)
	if !strings.Contains(run.StagesDone, "replay") {
		t.Errorf("stages_done = %q, want the replay token committed on a partial night", run.StagesDone)
	}
	raw, err := os.ReadFile(filepath.Join(r.DataDir, "dreams", night+"-replay.md"))
	if err != nil {
		t.Fatalf("reading the partial artifact: %v", err)
	}
	if artifact := string(raw); !strings.Contains(artifact, "Items read: 1") || !strings.Contains(artifact, "Budget: hit (budget)") {
		t.Errorf("partial artifact does not report the cutoff:\n%s", artifact)
	}
}

// End to end: with a shadow brain configured, one full Tick runs every stage in order, and the replay stage's token lands in stages_done after compact and before procedures.
func TestTick_ReplayRunsLastAfterCompact(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(dayFormat)
	if err := store.SetDiaryEntry(ctx, night, "day", "A quiet day."); err != nil {
		t.Fatal(err)
	}
	insertSummaryNode(t, store, "Work", "a moment worth noting", at(10, 0))

	primary := &fakeBrain{verdicts: "[]", extract: "[]", und: "An understanding."}
	r := newRunner(store, primary, yesProbes(), at(23, 30))
	r.Shadow = func(ctx context.Context, prompt string) (string, error) {
		return `{"salience":1,"facts":["noted"],"people":[],"thread":"none"}`, nil
	}
	r.DataDir = t.TempDir()

	r.Tick(ctx)

	run, ok, _ := store.DreamRun(ctx, night)
	if !ok || !run.Finished {
		t.Fatalf("run not finished: %+v ok=%v", run, ok)
	}
	if run.StagesDone != "hyp und compact replay procedures prune" {
		t.Errorf("stages_done = %q, want replay after compact and the pruning stage last", run.StagesDone)
	}
	entry, _ := store.DiaryEntry(ctx, night, "dream")
	if !strings.Contains(entry, "I replayed 1 items into 1 piles.") {
		t.Errorf("dream report does not mention the replay stage: %q", entry)
	}
}
