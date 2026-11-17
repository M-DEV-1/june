//go:build taskeval && linux

package taskeval

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// devSeeds are the seeds anyone may tune against. Held-out seeds come only from TASKEVAL_HELDOUT_SEEDS, so they are never written down in the repo.
var devSeeds = []int64{1, 2, 3, 4, 5}

// seedRun is one seed to run and which split it belongs to.
type seedRun struct {
	Seed  int64
	Split string
}

// seeds returns the dev seeds followed by the comma-separated seeds in TASKEVAL_HELDOUT_SEEDS. A held-out seed that is also a dev seed is a mistake and stops the run.
func seeds(t *testing.T) []seedRun {
	t.Helper()
	var out []seedRun
	dev := map[int64]bool{}
	for _, s := range devSeeds {
		out, dev[s] = append(out, seedRun{s, "dev"}), true
	}
	for _, field := range strings.Split(os.Getenv("TASKEVAL_HELDOUT_SEEDS"), ",") {
		if field = strings.TrimSpace(field); field == "" {
			continue
		}
		s, err := strconv.ParseInt(field, 10, 64)
		if err != nil || dev[s] {
			t.Fatalf("TASKEVAL_HELDOUT_SEEDS has %q, which is not a number or is a dev seed", field)
		}
		out = append(out, seedRun{s, "heldout"})
	}
	return out
}
