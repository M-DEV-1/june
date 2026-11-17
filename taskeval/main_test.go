//go:build taskeval && linux

package taskeval

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// juneBin is the daemon binary built once for the whole run, and model is the scripted Gemini every scripted-mode daemon talks to.
var (
	juneBin string
	model   *scriptedModel
)

// live is true when TASKEVAL_LIVE=1: the daemon then talks to the real Gemini API with GEMINI_API_KEY from the environment instead of the scripted model.
var live = os.Getenv("TASKEVAL_LIVE") == "1"

// minLiveTimeout is the least go test -timeout a live run accepts. A live task may take three minutes, and go test's default of ten minutes killed an earlier live run part-way through.
const minLiveTimeout = 30 * time.Minute

// TestMain runs the task suites. Scripted run: go test -tags taskeval ./taskeval/... -count 1. Live run with the real Gemini and up to 40 asks: TASKEVAL_LIVE=1 GEMINI_API_KEY=... go test -tags taskeval ./taskeval/... -count 1 -timeout 30m. TASKEVAL_HELDOUT_SEEDS=101,102 adds held-out seeds.
func TestMain(m *testing.M) {
	os.Exit(runAll(m))
}

// runAll builds june into a temp directory, starts the scripted model, runs the tests and cleans up.
func runAll(m *testing.M) int {
	flag.Parse()
	if limit, ok := flag.Lookup("test.timeout").Value.(flag.Getter).Get().(time.Duration); ok && live && limit != 0 && limit < minLiveTimeout {
		fmt.Fprintf(os.Stderr, "taskeval: a live run needs -timeout %s or more, and go test is set to %s\n", minLiveTimeout, limit)
		return 2
	}
	dir, err := os.MkdirTemp("", "taskeval-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "taskeval:", err)
		return 1
	}
	defer os.RemoveAll(dir)

	juneBin = filepath.Join(dir, "june")
	build := exec.Command("go", "build", "-o", juneBin, "june")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "taskeval: go build june:", err)
		return 1
	}

	model = newScriptedModel()
	defer model.close()
	return m.Run()
}
