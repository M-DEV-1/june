//go:build taskeval && linux

package taskeval

import (
	"context"
	"fmt"
	"testing"
	"time"

	"june/taskeval/fixture"
)

// TestMemoryReachesTheModelWithoutATool asks each memory question against a scripted model that answers at once and never calls a tool, and checks that the first request already carries the seeded row. This is why a live ask can answer correctly with zero tool steps: the daemon puts the best-matching rows into the first turn before the model is called.
func TestMemoryReachesTheModelWithoutATool(t *testing.T) {
	if live {
		t.Skip("scripted mode only")
	}
	for _, seed := range devSeeds {
		world := fixture.Build(seed)
		for _, task := range memoryTasks {
			t.Run(fmt.Sprintf("seed=%d/%s", seed, task.name), func(t *testing.T) {
				d := startDaemon(t, world)
				model.load([]scriptStep{{Say: "ok"}})
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				got, err := d.ask(ctx, task.question(world))
				if err != nil {
					t.Fatal(err)
				}
				if got.Steps != 0 {
					t.Errorf("ask made %d tool calls, the script has none", got.Steps)
				}
				if err := task.toolsHave(world, model.firstPrompt()); err != nil {
					t.Errorf("first request to the model: %v", err)
				}
				model.finish()
			})
		}
	}
}
