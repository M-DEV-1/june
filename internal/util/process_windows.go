package util

import (
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	createNoWindow  = 0x08000000
	detachedProcess = 0x00000008
)

// ChildProcAttr starts a spawned server with no console window. Windows has no parent-death signal, so pair it with KillWithDaemon after Start.
func ChildProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNoWindow}
}

// OwnProcessGroup makes cmd start in a new process group with no console window, so KillProcessGroup can find the tree it starts.
func OwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow}
}

// KillProcessGroup kills a started cmd and every process it started, through taskkill /T. Input: a started cmd. Output: taskkill's error, if any.
func KillProcessGroup(cmd *exec.Cmd) error {
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	return kill.Run()
}

// Detach makes cmd start with no console and in a process group of its own, so it outlives the daemon.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP}
}

// HideConsole makes cmd start with no console window. The daemon has no console, so every console program it starts would otherwise open a window of its own.
func HideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}

var (
	daemonJobOnce sync.Once
	daemonJob     windows.Handle
	daemonJobErr  error
)

// KillWithDaemon puts a started child into a Job Object that Windows closes when the daemon exits, however it exits, and closing the job kills every process in it. This stands in for Linux's parent-death signal, so a crashed daemon does not leave llama-server holding the GPU. Input: a started cmd. Output: none; a failure is logged and the child runs on unguarded.
func KillWithDaemon(cmd *exec.Cmd) {
	daemonJobOnce.Do(func() { daemonJob, daemonJobErr = newKillOnCloseJob() })
	err := daemonJobErr
	if err == nil {
		var h windows.Handle
		if h, err = windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)); err == nil {
			err = windows.AssignProcessToJobObject(daemonJob, h)
			windows.CloseHandle(h)
		}
	}
	if err != nil {
		slog.Warn("could not tie a child process to the daemon's lifetime; it may outlive a crash", "pid", cmd.Process.Pid, "error", err)
	}
}

// newKillOnCloseJob creates the Job Object KillWithDaemon assigns children to. Its handle is never closed by hand: the process exiting closes it, which is what kills the children.
func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}
