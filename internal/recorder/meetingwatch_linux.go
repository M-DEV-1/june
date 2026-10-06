package recorder

import (
	"context"
	"log/slog"
	"os/exec"
)

// readMicUsers asks the audio server which applications are in a call, and logs any that hold the microphone without playing anything back so the rule can be checked against the log. It returns nothing when pw-dump is missing or fails, which is also what a machine with no PipeWire reports, so the watcher simply never fires there.
func readMicUsers(ctx context.Context) []micHolder {
	out, err := exec.CommandContext(ctx, "pw-dump").Output()
	if err != nil {
		return nil
	}
	calls, captureOnly := micUsers(out)
	if len(captureOnly) > 0 {
		slog.Debug("holding the microphone without playing anything back, not a call", "processes", captureOnly)
	}
	// streamName's word is both what the window tracker records the process as and what a person calls it, so it serves as both.
	users := make([]micHolder, len(calls))
	for i, c := range calls {
		users[i] = micHolder{proc: c, name: c}
	}
	return users
}
