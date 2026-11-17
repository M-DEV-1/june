//go:build taskeval && linux

package taskeval

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"june/taskeval/fixture"
)

// budgets are the memory ceilings in budgets.json, in kB of peak resident memory summed over the daemon and its children.
type budgets struct {
	Idle60sKB int64 `json:"idle_60s_kb"`
	Asks10KB  int64 `json:"asks_10_kb"`
}

func readBudgets(t *testing.T) budgets {
	t.Helper()
	raw, err := os.ReadFile("budgets.json")
	if err != nil {
		t.Fatal(err)
	}
	var b budgets
	if err := json.Unmarshal(raw, &b); err != nil || b.Idle60sKB <= 0 || b.Asks10KB <= 0 {
		t.Fatalf("budgets.json must give positive idle_60s_kb and asks_10_kb: %v", err)
	}
	return b
}

// TestRAM measures the daemon's peak memory after a minute idle and after ten scripted asks, fails above the budgets in budgets.json, and appends one line per measurement to out/results.jsonl. It runs on seed 1's world, always in scripted mode.
func TestRAM(t *testing.T) {
	if live {
		t.Skip("memory is measured against the scripted model only")
	}
	budget := readBudgets(t)
	world := fixture.Build(1)

	finish := func(t *testing.T, d *daemon, task string, limit int64, steps int, wall time.Duration) {
		t.Helper()
		rss, err := d.peakRSSKB()
		if err != nil {
			t.Fatal(err)
		}
		r := result{Suite: "ram", Task: task, Seed: world.Seed, Split: "dev", Pass: rss <= limit, Steps: steps, WallMS: wall.Milliseconds(), PeakRSSKB: rss}
		if !r.Pass {
			r.Note = "peak memory is above the budget"
			t.Errorf("peak %d kB is above the budget of %d kB", rss, limit)
		}
		t.Logf("peak %d kB, budget %d kB", rss, limit)
		record(t, r)
	}

	t.Run("idle_60s", func(t *testing.T) {
		d := startDaemon(t, world)
		start := time.Now()
		time.Sleep(60 * time.Second)
		finish(t, d, "idle_60s", budget.Idle60sKB, 0, time.Since(start))
	})

	t.Run("asks_10", func(t *testing.T) {
		d := startDaemon(t, world)
		steps := 0
		start := time.Now()
		for i := 0; i < 10; i++ {
			task := memoryTasks[i%len(memoryTasks)]
			run, err := runTask(t, d, task.name, task.question(world), task.vars(world))
			if err != nil {
				t.Fatalf("ask %d (%s): %v", i+1, task.name, err)
			}
			steps += run.Steps
		}
		finish(t, d, "asks_10", budget.Asks10KB, steps, time.Since(start))
	})
}
