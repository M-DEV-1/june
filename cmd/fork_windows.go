//go:build windows

package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"june/internal/util"

	"golang.org/x/sys/windows"
)

// spawnHiddenDaemon starts this same program as the daemon, with no console window and in a process group of its own. Output: the error from starting it, if any.
// Without its own (windowless) console the daemon would share the terminal's, and closing that terminal or pressing Ctrl+C in it would end the daemon too. A windowless console rather than none at all (DETACHED_PROCESS), so a console program the daemon starts inherits it and gets no window of its own either.
// --open has the daemon show the window itself once the window is listening, which the client's own few tries could miss on a cold start.
func spawnHiddenDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	p, err := startDetachedDaemon(exe, "--daemon", "--open")
	if err != nil {
		return err
	}
	return p.Release()
}

// spawnReplacement starts the daemon that takes over once this one has exited. Input: the pid of the daemon being replaced, which the replacement waits on, and any further flags for it. Output: the started process, or why no program of the pair would start.
// junew.exe is tried first when it sits beside this program, so a daemon a terminal's june.exe happened to start is replaced by the windowless build the login entry runs. The other program of the pair is tried next, so a junew.exe that antivirus holds or an update is replacing calls off nothing that june.exe can do instead.
func spawnReplacement(oldPid int, extra ...string) (*os.Process, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var candidates []string
	for _, path := range []string{filepath.Join(filepath.Dir(exe), "junew.exe"), exe, buildTwin(exe)} {
		if path != "" && util.Exists(path) && !slices.ContainsFunc(candidates, func(c string) bool { return strings.EqualFold(c, path) }) {
			candidates = append(candidates, path)
		}
	}
	var errs []error
	for _, path := range candidates {
		p, err := startDetachedDaemon(path, append([]string{"--daemon", "--wait-pid", strconv.Itoa(oldPid)}, extra...)...)
		if err == nil {
			return p, nil
		}
		slog.Warn("could not start this program as the replacement daemon", "path", path, "error", err)
		errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(path), err))
	}
	if len(errs) == 0 {
		return nil, errors.New("no June program found beside " + exe)
	}
	return nil, errors.Join(errs...)
}

// startDetachedDaemon starts exe with args as a daemon that outlives its starter. Input: the program and its arguments. Output: the started process, which the caller releases or keeps, or the error from starting it.
// It is started with the environment this process was started with (see util.StartupEnviron), breaking away from any job this process runs in: a terminal or an installer that put June in a kill-on-close job would otherwise take the new daemon down with it the moment it exited. A job that forbids breaking away refuses the start outright, so the start is tried again inside it rather than not at all.
func startDetachedDaemon(exe string, args ...string) (*os.Process, error) {
	start := func(flags uint32) (*os.Process, error) {
		cmd := exec.Command(exe, args...)
		cmd.Env = util.StartupEnviron()
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | flags}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Process, nil
	}
	p, err := start(windows.CREATE_BREAKAWAY_FROM_JOB)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		p, err = start(0)
	}
	return p, err
}

// waitProcessExit waits for the process pid to exit. Input: the pid and the longest to wait. Output: true once it has exited, or when there is no such process; false when it is still running at the end of the wait.
func waitProcessExit(pid int, bound time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER is no such process. Anything else is a process this user cannot open, which June's own daemon never is, so it is not one worth waiting for.
		return true
	}
	defer windows.CloseHandle(h)
	ms := uint32(max(bound, 0) / time.Millisecond)
	event, err := windows.WaitForSingleObject(h, ms)
	return err == nil && event == windows.WAIT_OBJECT_0
}

// supervisedBySystemd is always false on Windows, where nothing restarts the daemon but its own replacement.
func supervisedBySystemd() bool { return false }

// stopDaemonHint tells the user how to end a running daemon. The package runs it as junew.exe from the login entry and as june.exe from a terminal, so both names are given. taskkill without /F closes the tray's window, which quits June the same way the tray's Quit does; /F is only for a daemon whose tray never came up.
const stopDaemonHint = "quit June from its tray icon (or run `taskkill /IM june.exe /IM junew.exe`)"

// portInUse reports whether a failed bind found the port held by another socket (WSAEADDRINUSE), which for June's port is almost always another daemon.
func portInUse(err error) bool { return errors.Is(err, windows.WSAEADDRINUSE) }

// portRefused reports whether a failed bind was Windows refusing the port itself (WSAEACCES). A port inside a range Hyper-V, WSL or Docker reserved fails this way with nothing listening on it; a port another program holds, even with SO_EXCLUSIVEADDRUSE, gives WSAEADDRINUSE instead.
func portRefused(err error) bool { return errors.Is(err, windows.WSAEACCES) }

// portRefusedHint is what to do about a refused port. Input: the port. Output: the advice, as sentences.
// An administered exclusion still lets a program bind the port explicitly; what it stops is the reserved ranges being handed out over it again, and winnat holds those ranges, which is why it is stopped around the change.
func portRefusedHint(port string) string {
	return "Windows reserves port ranges for Hyper-V, WSL and Docker, and `netsh int ipv4 show excludedportrange protocol=tcp` lists them. To keep the port for June, run from an administrator prompt: `net stop winnat`, then `netsh int ipv4 add excludedportrange protocol=tcp startport=" + port + " numberofports=1`, then `net start winnat`. JUNE_PORT starts the daemon on another port, but the desktop window only talks to 6942."
}
