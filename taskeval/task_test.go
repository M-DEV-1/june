//go:build taskeval && linux

package taskeval

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// maxLiveAsks caps how many real-model asks one test run may make, so a large -count cannot spend an unbounded amount of API quota.
const maxLiveAsks = 40

// liveAsks counts the real-model asks made so far in this run.
var liveAsks int

// taskRun is what one ask produced. Tools is every tool result the daemon handed the model, joined with newlines, and is empty in live mode because no scripted model is there to see it.
type taskRun struct {
	Answer string
	Steps  int
	Wall   time.Duration
	Tools  string
}

// runTask asks the daemon question. In scripted mode it first loads scripts/<task>.json with vars filled in and afterwards collects what the tools returned to the model; in live mode it just asks the real model, within the live ask cap. Each task gets three minutes live and one minute scripted.
func runTask(t *testing.T, d *daemon, task, question string, vars map[string]string) (taskRun, error) {
	t.Helper()
	timeout := time.Minute
	if live {
		if liveAsks >= maxLiveAsks {
			t.Skipf("live ask cap of %d reached", maxLiveAsks)
		}
		liveAsks++
		timeout = 3 * time.Minute
	} else {
		steps, err := readScript(task, vars)
		if err != nil {
			return taskRun{}, err
		}
		model.load(steps)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	got, err := d.ask(ctx, question)
	run := taskRun{Answer: got.Text, Steps: got.Steps, Wall: time.Since(start)}
	if live {
		return run, err
	}

	tools, _, problems := model.finish()
	var outputs []string
	for _, tool := range tools {
		outputs = append(outputs, tool.Output)
	}
	run.Tools = strings.Join(outputs, "\n")
	if err == nil && len(problems) > 0 {
		err = fmt.Errorf("scripted model: %s", strings.Join(problems, "; "))
	}
	return run, err
}
