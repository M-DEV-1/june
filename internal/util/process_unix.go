//go:build !windows

package util

import (
	"os/exec"
	"syscall"
)

// OwnProcessGroup makes cmd start in a process group of its own, so KillProcessGroup reaches every process it starts as well as cmd itself.
func OwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// StartProcessGroup starts cmd. The process group OwnProcessGroup gave it is all KillProcessGroup needs here, so there is nothing to let go of afterwards. Output: a release func that does nothing, and the start error.
func StartProcessGroup(cmd *exec.Cmd) (release func(), err error) {
	return func() {}, cmd.Start()
}

// KillProcessGroup kills a started cmd and everything in its process group. Input: a cmd set up with OwnProcessGroup and already started. Output: the kill error, if any.
func KillProcessGroup(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// Detach makes cmd start in a session of its own, so it outlives the daemon and is not stopped with the daemon's terminal or process group.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// HideConsole does nothing outside Windows, where a started program never opens a console window of its own.
func HideConsole(cmd *exec.Cmd) {}

// KillWithDaemon does nothing outside Windows: on Linux ChildProcAttr's parent-death signal already stops the child when the daemon dies.
func KillWithDaemon(cmd *exec.Cmd) {}
