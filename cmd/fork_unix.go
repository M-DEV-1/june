//go:build !windows

package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"june/internal/util"
)

// spawnHiddenDaemon starts this same program as the daemon. --open has the daemon show the window itself once the window is listening, which the client's own few tries could miss on a cold start.
func spawnHiddenDaemon() error {
	p, err := startDetachedDaemon("--daemon", "--open")
	if err != nil {
		return err
	}
	return p.Release()
}

// spawnReplacement starts the daemon that takes over once this one has exited. Input: the pid of the daemon being replaced, which the replacement waits on, and any further flags for it. Output: the started process, or the error from starting it.
func spawnReplacement(oldPid int, extra ...string) (*os.Process, error) {
	args := append([]string{"--daemon", "--wait-pid", strconv.Itoa(oldPid)}, extra...)
	// A replacement started inside a systemd unit is in the unit's cgroup, and when this process, the unit's main one, exits, systemd stops the unit and kills everything left in it, setsid or not. The XDG autostart generator makes June such a unit at a KDE Plasma sign-in, and before systemd 250 it writes it with nothing that keeps the unit up past its main process, so June ended for good at its first restart. A scope of its own is outside the unit.
	if systemdMainProcess() {
		p, err := startInOwnScope(args)
		if err == nil {
			return p, nil
		}
		slog.Warn("could not start the replacement daemon outside this systemd unit, so it is started inside it, where it may end with the unit", "error", err)
	}
	return startDetachedDaemon(args...)
}

// scopeStartWait bounds how long startInOwnScope waits for systemd-run to make the scope and become June. Making a scope is one call to the user's service manager, well under a second.
const scopeStartWait = 5 * time.Second

// startInOwnScope starts this program with args in a systemd scope of its own (systemd-run --user --scope), detached as startDetachedDaemon does. Output: the started process, which is June by the time this returns, or why it could not be started there: no systemd-run, no user service manager to make the scope, or one that did not answer in time.
// systemd-run makes the scope and then becomes June in the same process, or exits with an error when it cannot make it, so which of the two happened is told by the program that process is running.
func startInOwnScope(args []string) (*os.Process, error) {
	run, err := exec.LookPath("systemd-run")
	if err != nil {
		return nil, err
	}
	runExe, err := filepath.EvalSymlinks(run)
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(run, append([]string{"--user", "--scope", "--collect", "--quiet", "--", exe}, args...)...)
	cmd.Env = util.StartupEnviron()
	util.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exeLink := filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "exe")
	deadline := time.Now().Add(scopeStartWait)
	for {
		if target, err := os.Readlink(exeLink); err == nil && target != runExe {
			return cmd.Process, nil
		}
		if !processRunning(cmd.Process.Pid) {
			return nil, fmt.Errorf("systemd-run could not make a scope for June: %v", cmd.Wait())
		}
		if !time.Now().Before(deadline) {
			cmd.Process.Kill()
			cmd.Wait()
			return nil, fmt.Errorf("systemd-run did not start June within %s", scopeStartWait)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startDetachedDaemon starts this program with args in a session of its own, with the environment this process was started with (see util.StartupEnviron). Output: the started process, which the caller releases or keeps, or the error from starting it.
// A session of its own because a daemon left in the terminal's session is sent SIGHUP when that terminal closes, which install.sh's closing `june` would otherwise make the end of June.
func startDetachedDaemon(args ...string) (*os.Process, error) {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = util.StartupEnviron()
	util.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// waitProcessExit waits for the process pid to exit. Input: the pid and the longest to wait. Output: true once it has exited, false when it is still running at the end of the wait.
// The process being replaced was this one's parent, and a parent that has exited stays a zombie until its own parent reaps it, which signal 0 still finds; /proc says which it is.
func waitProcessExit(pid int, bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for {
		if !processRunning(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// processRunning reports whether pid names a live process, a zombie counting as exited.
func processRunning(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	// The state is the first field after the command name, which is in parentheses and may itself hold spaces or parentheses.
	s := string(stat)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	return len(fields) == 0 || fields[0] != "Z"
}

// systemdRestartEnv is the variable a systemd unit sets to "1" to say it restarts June on a failing exit (Restart=on-failure), so a restart is the unit's to make (see supervisedBySystemd).
const systemdRestartEnv = "JUNE_RESTART_BY_SYSTEMD"

// supervisedBySystemd reports whether a systemd unit that restarts June on a failing exit runs this process as its main process, so June exits for it rather than starting its own replacement.
// The unit has to say so with systemdRestartEnv, because being systemd's main process is not enough: the XDG autostart generator, which KDE Plasma's systemd startup runs every sign-in entry through, makes June the main process of a unit with Restart=no, and exiting for systemd there ended June for good at its first restart. June starts its own replacement there instead, in a scope of its own (see spawnReplacement). No unit June ships sets the variable; it is there for one a user writes.
// The main-process check stays, so a June started from a shell that inherited the variable does not exit for a systemd that is not watching it.
func supervisedBySystemd() bool {
	return os.Getenv(systemdRestartEnv) == "1" && systemdMainProcess()
}

// systemdMainProcess reports whether systemd started this process as a unit's main process. INVOCATION_ID alone is not enough to tell: a terminal that is itself a systemd service, as gnome-terminal-server is, hands it to every shell and so to a daemon started from one. SYSTEMD_EXEC_PID (systemd 248 and later) names the process systemd started; before that, the parent being systemd itself is what tells.
func systemdMainProcess() bool {
	if pid := os.Getenv("SYSTEMD_EXEC_PID"); pid != "" {
		return pid == strconv.Itoa(os.Getpid())
	}
	if os.Getenv("INVOCATION_ID") == "" {
		return false
	}
	comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(os.Getppid()), "comm"))
	return err == nil && strings.TrimSpace(string(comm)) == "systemd"
}

// stopDaemonHint tells the user how to end a running daemon.
const stopDaemonHint = "quit June from its tray icon (or run `pkill june`)"

// portInUse reports whether a failed bind found the port held by another socket, which for June's port is almost always another daemon.
func portInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }

// portRefused reports whether a failed bind was the system refusing the port itself rather than another socket holding it.
func portRefused(err error) bool { return errors.Is(err, syscall.EACCES) }

// portRefusedHint is what to do about a refused port. Input: the port. Output: the advice, as a sentence.
func portRefusedHint(string) string {
	return "Ports below 1024 need root; JUNE_PORT picks another, but the desktop window only talks to 6942."
}
