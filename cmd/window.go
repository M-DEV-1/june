package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// The daemon owns the desktop window's lifetime, so a user starts and stops one thing rather than two. The login entry launches the daemon (see autostart.go), the daemon launches the window, and quitting the daemon takes the window with it.

// windowHealthy is how long the window must stay up before a later crash is treated as a fresh problem rather than one more failure in a run of them.
const windowHealthy = 30 * time.Second

// windowStopGrace is how long a window is given to close itself after being asked, before it is killed.
const windowStopGrace = 3 * time.Second

// errNoWindow says the desktop window binary could not be found, which is normal on a machine running only the daemon.
var errNoWindow = errors.New("no desktop window binary found")

// windowBinary finds the desktop window to run. Input: none; it reads ORA_WINDOW and the daemon's own location. Output: the path to run, or errNoWindow when there is none. It looks at ORA_WINDOW first so a developer can point at any build, then beside the daemon binary as an installed copy would sit, then at the two paths a checkout builds into.
func windowBinary() (string, error) {
	var candidates []string
	if set := os.Getenv("ORA_WINDOW"); set != "" {
		candidates = append(candidates, set)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "ora-window"),
			filepath.Join(dir, "app", "src-tauri", "target", "release", "ora"),
			filepath.Join(dir, "app", "src-tauri", "target", "debug", "ora"),
		)
	}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		// The daemon binary is named "ora" too, so a candidate resolving back to this very process would fork the daemon endlessly.
		if same, err := sameFile(path); err == nil && same {
			continue
		}
		return path, nil
	}
	return "", errNoWindow
}

// sameFile reports whether path is this running program. Input: a candidate path. Output: true when it is the daemon's own binary, so it is never launched as the window.
func sameFile(path string) (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	a, err := os.Stat(exe)
	if err != nil {
		return false, err
	}
	b, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return os.SameFile(a, b), nil
}

// startWindow launches the window binary with the daemon's own environment. Input: the context that ends the window's life and the path to run. Output: the started command, or an error when it could not be started.
func startWindow(ctx context.Context, path string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, path)
	cmd.Dir = filepath.Dir(path)
	// The window is told which process is running it, so quitting from its tray can stop the daemon too and "Quit" means quitting Ora rather than leaving a headless daemon with no way back.
	cmd.Env = append(os.Environ(), fmt.Sprintf("ORA_SUPERVISOR_PID=%d", os.Getpid()))
	// The window is asked to close rather than killed outright, so it can hide its windows and release the microphone before it goes.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = windowStopGrace
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// superviseWindow keeps the desktop window running for as long as the daemon does, restarting it when it exits on its own. Input: the context that ends when the daemon stops, the binary to run, and the function that starts it (a parameter so a test can stand in for a real process). Output: none; it returns when the context ends or the restart policy gives up.
func superviseWindow(ctx context.Context, path string, start func(context.Context, string) (*exec.Cmd, error)) {
	failures := 0
	for {
		began := time.Now()
		cmd, err := start(ctx, path)
		if err != nil {
			slog.Error("could not start the desktop window", "path", path, "error", err)
			return
		}
		slog.Info("desktop window started", "path", path, "pid", cmd.Process.Pid)

		waitErr := cmd.Wait()
		if ctx.Err() != nil {
			slog.Info("desktop window stopped with the daemon")
			return
		}
		ranFor := time.Since(began)
		// A window that stayed up and then died is a fresh problem, not one more in a run of them, so the count starts again.
		if ranFor >= windowHealthy {
			failures = 0
		}
		failures++

		wait, giveUp := shouldRestartWindow(failures, ranFor, waitErr == nil)
		if giveUp {
			slog.Info("the desktop window closed itself, leaving it stopped", "path", path, "ran_for", ranFor.Round(time.Millisecond))
			return
		}
		slog.Warn("the desktop window exited, starting it again", "ran_for", ranFor.Round(time.Millisecond), "wait", wait, "failures", failures, "error", waitErr)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// windowRetryMin is the pause before the first restart after a crash, short enough that a one-off crash is invisible to the user.
const windowRetryMin = time.Second

// windowRetryMax is the longest pause between restart attempts. A window that cannot start yet may start later, after a rebuild or once a library it needs arrives, so trying for ever at one attempt a minute costs nothing and is better than a window that is silently gone until the daemon is restarted.
const windowRetryMax = time.Minute

// shouldRestartWindow decides what to do after the desktop window has exited on its own.
// Input: failures is how many times in a row it has exited without first staying up for windowHealthy; ranFor is how long it lasted this time; cleanExit is true when it finished with no error, which is what quitting from the tray does.
// Output: wait is how long to pause before starting it again; giveUp is true to stop trying altogether.
func shouldRestartWindow(failures int, ranFor time.Duration, cleanExit bool) (wait time.Duration, giveUp bool) {
	// Quitting from the tray is the user closing the window on purpose, and starting it again would make that menu item do nothing.
	if cleanExit {
		return 0, true
	}
	// A crash is never given up on, because the tray icon and the ring overlay go with the window and the only sign of their absence is a log line nobody reads. The pause doubles so a window failing at once does not spin, and stops at a minute so a window that recovers is picked up soon after.
	wait = windowRetryMin << (failures - 1)
	if wait > windowRetryMax || wait <= 0 {
		wait = windowRetryMax
	}
	return wait, false
}

// runWindow starts supervising the desktop window unless there is none to run or the user has turned it off. Input: the context that ends when the daemon stops, and whether the user wants a window at all. Output: none; it runs on its own goroutine and logs why it did nothing when it does nothing.
func runWindow(ctx context.Context, want bool) {
	if !want {
		slog.Info("the desktop window is turned off in the config, running the daemon alone")
		return
	}
	path, err := windowBinary()
	if err != nil {
		slog.Info("no desktop window to run, running the daemon alone", "hint", "build it in app/ or point ORA_WINDOW at it")
		return
	}
	go superviseWindow(ctx, path, startWindow)
}
