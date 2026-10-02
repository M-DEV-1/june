package dream

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"june/internal/db"
	"june/internal/db/dbtest"
)

const (
	// testActRunKeep and testFailedGrace are the retention numbers the dream tests hand the pruning stage, standing in for the config file so no test reads the machine's real one.
	testActRunKeep  = 2000
	testFailedGrace = 30 * 24 * time.Hour
)

// countingStore wraps the real store so a test can watch what the pruning stage asks it for, in order, and make any one of those calls fail. Every other method the dreaming loop needs is the embedded store's own, so a whole night runs for real around the two passes being watched.
type countingStore struct {
	*db.Store
	mu    sync.Mutex
	calls []string
	// pruneConversationsErr, when set, is returned instead of doing the call, which is how a test makes the store unreachable.
	pruneConversationsErr error
}

// record notes one call by name and reports the error the test wants it to fail with, if any.
func (c *countingStore) record(name string, err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name)
	return err
}

// recorded returns the names of the store calls the pruning stage made, in order.
func (c *countingStore) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *countingStore) ProtectedConversations(ctx context.Context, olderThan time.Duration) (int64, error) {
	if err := c.record("ProtectedConversations", nil); err != nil {
		return 0, err
	}
	return c.Store.ProtectedConversations(ctx, olderThan)
}

func (c *countingStore) PruneEmptyConversations(ctx context.Context, olderThan time.Duration) (int64, error) {
	if err := c.record("PruneEmptyConversations", c.pruneConversationsErr); err != nil {
		return 0, err
	}
	return c.Store.PruneEmptyConversations(ctx, olderThan)
}

func (c *countingStore) ProtectedActRuns(ctx context.Context, failedGrace time.Duration) (int64, int64, error) {
	if err := c.record("ProtectedActRuns", nil); err != nil {
		return 0, 0, err
	}
	return c.Store.ProtectedActRuns(ctx, failedGrace)
}

func (c *countingStore) PruneActRuns(ctx context.Context, keep int, failedGrace time.Duration) (int64, error) {
	if err := c.record("PruneActRuns", nil); err != nil {
		return 0, err
	}
	return c.Store.PruneActRuns(ctx, keep, failedGrace)
}

func (c *countingStore) CommitPruneStage(ctx context.Context, night string) error {
	if err := c.record("CommitPruneStage", nil); err != nil {
		return err
	}
	return c.Store.CommitPruneStage(ctx, night)
}

// pruneRunner builds a Runner on the given store with the clock pinned to 23:30 and the retention numbers fixed, so a pruning test never reads the machine's config file.
func pruneRunner(store Store) (*Runner, *fakeBrain) {
	b := &fakeBrain{verdicts: "[]", extract: "[]", und: "An understanding."}
	r := New(store, b.fn, yesProbes(), 23, 9)
	r.now = func() time.Time { return at(23, 30) }
	r.watchEvery = time.Hour
	r.retention = func() (int, time.Duration) { return testActRunKeep, testFailedGrace }
	return r, b
}

// addPrunableRun writes one act run straight into the store with a chosen outcome and age, which is the only way to make a run old enough for the cap to reach.
func addPrunableRun(t *testing.T, store *db.Store, question, outcome string, age time.Duration) {
	t.Helper()
	if _, err := store.DB().Exec(
		`INSERT INTO act_runs (started_at, question, model, outcome, answer, error, duration_ms, steps_json) VALUES (?, ?, 'sonnet', ?, '', '', 10, '[]')`,
		time.Now().Add(-age).UTC().Format("2006-01-02 15:04:05"), question, outcome); err != nil {
		t.Fatalf("insert act run %q: %v", question, err)
	}
}

// The stage runs once a night: a finished night carries the 'prune' token, and a night that already carries it never touches the store's retention passes again on a later wake.
func TestPruneStageRunsOnceANight(t *testing.T) {
	ctx := context.Background()
	night := at(23, 30).Format(time.DateOnly)

	t.Run("a night that has not pruned yet does, and commits the token", func(t *testing.T) {
		store := &countingStore{Store: dbtest.Open(t)}
		if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
			t.Fatal(err)
		}
		// One conversation nothing was ever said in, two days old, and two ordinary act runs under a cap of one: the night should come out with both gone.
		empty, err := store.CreateConversation(ctx, "", "claude")
		if err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02 15:04:05")
		if _, err := store.DB().Exec(`UPDATE conversations SET created_at = ?, updated_at = ? WHERE id = ?`, old, old, empty); err != nil {
			t.Fatal(err)
		}
		addPrunableRun(t, store.Store, "ordinary one", "ok", 48*time.Hour)
		addPrunableRun(t, store.Store, "ordinary two", "ok", time.Hour)

		r, _ := pruneRunner(store)
		r.retention = func() (int, time.Duration) { return 1, testFailedGrace }
		r.Tick(ctx)

		if got := store.recorded(); !slices.Contains(got, "PruneEmptyConversations") || !slices.Contains(got, "PruneActRuns") {
			t.Errorf("the night called the store as %v, want both retention passes in it", got)
		}
		var conversations, runs int
		if err := store.DB().QueryRow(`SELECT (SELECT COUNT(*) FROM conversations), (SELECT COUNT(*) FROM act_runs)`).Scan(&conversations, &runs); err != nil {
			t.Fatal(err)
		}
		if conversations != 0 || runs != 1 {
			t.Errorf("after the night there are %d conversations and %d act runs, want 0 and 1", conversations, runs)
		}
		run, _, _ := store.DreamRun(ctx, night)
		if !slices.Contains(strings.Fields(run.StagesDone), "prune") {
			t.Errorf("stages_done = %q, want the prune token committed", run.StagesDone)
		}
	})

	t.Run("a night that already pruned does not prune again", func(t *testing.T) {
		store := &countingStore{Store: dbtest.Open(t)}
		if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
			t.Fatal(err)
		}
		if err := store.StartDreamRun(ctx, night); err != nil {
			t.Fatal(err)
		}
		if err := store.Store.CommitPruneStage(ctx, night); err != nil {
			t.Fatal(err)
		}
		r, _ := pruneRunner(store)
		r.Tick(ctx)

		if got := store.recorded(); len(got) != 0 {
			t.Errorf("a night whose prune token was already committed called the store as %v, want nothing", got)
		}
	})
}

// A stage that cannot reach the store leaves the token uncommitted, so the next wake tries again rather than the night being remembered as pruned.
func TestPruneStageLeavesTheTokenOffWhenItFails(t *testing.T) {
	ctx := context.Background()
	night := at(23, 30).Format(time.DateOnly)
	store := &countingStore{Store: dbtest.Open(t)}
	store.pruneConversationsErr = errors.New("database is locked")
	if err := store.SetDiaryEntry(ctx, night, "day", "A day."); err != nil {
		t.Fatal(err)
	}

	r, _ := pruneRunner(store)
	r.Tick(ctx)

	run, _, _ := store.DreamRun(ctx, night)
	if slices.Contains(strings.Fields(run.StagesDone), "prune") {
		t.Errorf("stages_done = %q after a failed pruning stage, want no prune token", run.StagesDone)
	}
	if !run.Finished {
		t.Error("a failed pruning stage cost the night its morning report, which it must not")
	}
}
