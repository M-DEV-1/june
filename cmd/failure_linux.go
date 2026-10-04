//go:build linux

package cmd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// failureNoticeWait bounds the desktop notification showFailure posts. notify-send returns as soon as the notification server has it; the bound is for a session bus that never answers.
const failureNoticeWait = 5 * time.Second

// stderrUnseen reports whether nothing will show what this process writes to stderr: true when it has no controlling terminal. A click on June in the app grid or the dock, and the sign-in entry, start it without one, with stderr going to the session's journal or ~/.xsession-errors, where nobody looks; june.desktop has Terminal=false. A terminal shows stderr, and so does a script run from one, whose pipe or file its user reads.
func stderrUnseen() bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return true
	}
	tty.Close()
	return false
}

// showFailure puts msg in front of the user, for a June started from the desktop whose failure would otherwise reach only june.log and the journal: as a desktop notification through notify-send, kept on screen until dismissed, or in a zenity dialog when there is no notify-send or it could not post one. With neither installed the log is all there is, and it says so.
func showFailure(msg string) {
	if path, err := exec.LookPath("notify-send"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), failureNoticeWait)
		defer cancel()
		err := exec.CommandContext(ctx, path, "--app-name=June", "--urgency=critical", "--icon=dialog-warning", "June", msg).Run()
		if err == nil {
			return
		}
		slog.Warn("could not post the failure as a desktop notification", "error", err)
	}
	if path, err := exec.LookPath("zenity"); err == nil {
		// It waits for the dialog to be dismissed, which is all this process has left to do. zenity exits 1 when the dialog is closed rather than answered, which is still a dialog the user saw.
		err := exec.Command(path, "--warning", "--title=June", "--no-markup", "--width=420", "--text="+msg).Run()
		var exit *exec.ExitError
		if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
			slog.Warn("could not show the failure in a dialog", "error", err)
		}
		return
	}
	slog.Warn("neither notify-send nor zenity is installed, so this failure is only in the log")
}
