package dream

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/db/dbtest"
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
	// Each of these, when set, is returned instead of doing the call, which is how a test makes the store unreachable at one exact point.
	protectedConversationsErr error
	pruneConversationsErr     error
	protectedRunsErr          error
	pruneRunsErr              error
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
	if err := c.record("ProtectedConversations", c.protectedConversationsErr); err != nil {
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
	if err := c.record("ProtectedActRuns", c.protectedRunsErr); err != nil {
		return 0, 0, err
	}
	return c.Store.ProtectedActRuns(ctx, failedGrace)
}

func (c *countingStore) PruneActRuns(ctx context.Context, keep int, failedGrace time.Duration) (int64, error) {
	if err := c.record("PruneActRuns", c.pruneRunsErr); err != nil {
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

// A store that cannot be reached stops the stage where it failed: the pass after it never runs, and the error comes back so the caller commits no token and the next wake tries the whole stage again. An unreachable store must never read as a store with nothing to prune.
func TestPruneStageStopsWhenTheStoreCannotBeReached(t *testing.T) {
	ctx := context.Background()
	unreachable := errors.New("database is locked")

	cases := []struct {
		name string
		fail func(*countingStore)
		want []string
	}{
		{
			name: "the conversation pass cannot be reached",
			fail: func(c *countingStore) { c.pruneConversationsErr = unreachable },
			want: []string{"ProtectedConversations", "PruneEmptyConversations"},
		},
		{
			name: "the act run protection count cannot be read",
			fail: func(c *countingStore) { c.protectedRunsErr = unreachable },
			want: []string{"ProtectedConversations", "PruneEmptyConversations", "ProtectedActRuns"},
		},
		{
			name: "the act run pass cannot be reached",
			fail: func(c *countingStore) { c.pruneRunsErr = unreachable },
			want: []string{"ProtectedConversations", "PruneEmptyConversations", "ProtectedActRuns", "PruneActRuns"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &countingStore{Store: dbtest.Open(t)}
			c.fail(store)
			r, _ := pruneRunner(store)

			rep, err := r.pruneStage(ctx)
			if !errors.Is(err, unreachable) {
				t.Fatalf("pruneStage returned error %v, want the store's own error back", err)
			}
			if got := store.recorded(); !slices.Equal(got, c.want) {
				t.Errorf("after the failure the stage called the store as %v, want it to stop at %v", got, c.want)
			}
			if rep != (pruneReport{}) {
				t.Errorf("a failed stage reported %+v, want nothing counted", rep)
			}
		})
	}
}

// The report carries both halves of what the night should log: what each pass removed, and what the policy held back. Three conversations and five act runs go in, one of each is removable, and every other number names the rule that saved a row.
func TestPruneStageReportsWhatItRemovedAndKept(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)

	spokenIn, err := store.CreateConversation(ctx, "what did vexil ask about", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, spokenIn, "you", "what did vexil ask about", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	oldEmpty, err := store.CreateConversation(ctx, "", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	taskedEmpty, err := store.CreateConversation(ctx, "", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddUserTask(ctx, "book the flight", taskedEmpty); err != nil {
		t.Fatalf("AddUserTask: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	for _, id := range []int64{spokenIn, oldEmpty, taskedEmpty} {
		if _, err := store.DB().Exec(`UPDATE conversations SET created_at = ?, updated_at = ? WHERE id = ?`, old, old, id); err != nil {
			t.Fatalf("backdate conversation %d: %v", id, err)
		}
	}

	addPrunableRun(t, store, "open the pricing page", "ok", 365*24*time.Hour)
	if _, err := store.LogNote(ctx, "How I did open the pricing page: looked at the screen.", "procedure"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	addPrunableRun(t, store, "click the broken button", "error", time.Hour)
	addPrunableRun(t, store, "an old failure nobody looked at", "error", 400*24*time.Hour)
	addPrunableRun(t, store, "ordinary one", "ok", time.Hour)
	addPrunableRun(t, store, "ordinary two", "ok", time.Hour)

	r, _ := pruneRunner(store)
	// A cap of one ordinary run, so exactly one ordinary run and the stale failure are over it.
	r.retention = func() (int, time.Duration) { return 1, testFailedGrace }

	rep, err := r.pruneStage(ctx)
	if err != nil {
		t.Fatalf("pruneStage: %v", err)
	}
	want := pruneReport{
		conversationsRemoved:   1,
		conversationsProtected: 2,
		runsRemoved:            2,
		runsKeptForNotes:       1,
		runsKeptFailedYoung:    1,
	}
	if rep != want {
		t.Errorf("report = %+v, want %+v", rep, want)
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

// A config edit takes effect on the very next night, with no restart: r.retention is configRetention, which calls config.LoadConfig fresh every time it is called rather than reading a value cached at Runner construction. This test proves that end to end on one live Runner — write the config once, read retention, edit the config file on disk, read retention again on the same Runner — rather than trusting the wiring by inspection.
func TestRetention_SeesAConfigEditWithoutRestart(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	store := dbtest.Open(t)
	r := New(store, (&fakeBrain{}).fn, yesProbes(), 23, 9)

	if err := config.SaveConfig(config.OraConfig{ActRunKeep: 500, ActRunFailedKeepDays: 10}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	keep, grace := r.retention()
	if keep != 500 || grace != 10*24*time.Hour {
		t.Fatalf("retention() = (%d, %v) right after writing the config, want (500, 240h)", keep, grace)
	}

	// The same Runner, no restart: only the file on disk changes.
	if err := config.SaveConfig(config.OraConfig{ActRunKeep: 7, ActRunFailedKeepDays: 3}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	keep, grace = r.retention()
	if keep != 7 || grace != 3*24*time.Hour {
		t.Errorf("retention() = (%d, %v) after editing the config on disk, want (7, 72h): a config change is not reaching the pruning stage without a restart", keep, grace)
	}
}
