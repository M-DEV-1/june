//go:build taskeval && linux

package taskeval

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// juneBin is the daemon binary built once for the whole run, and model is the scripted Gemini every scripted-mode daemon talks to.
var (
	juneBin string
	model   *scriptedModel
)

// live is true when TASKEVAL_LIVE=1: the daemon then talks to the real Gemini API with GEMINI_API_KEY from the environment instead of the scripted model.
var live = os.Getenv("TASKEVAL_LIVE") == "1"

func TestMain(m *testing.M) {
	os.Exit(runAll(m))
}

// runAll builds june into a temp directory, starts the scripted model, runs the tests and cleans up.
func runAll(m *testing.M) int {
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
