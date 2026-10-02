package util

import (
	"os/exec"
	"strconv"
	"syscall"
)

const (
	createNoWindow  = 0x08000000
	detachedProcess = 0x00000008
)

// ChildProcAttr starts a spawned server with no console window. Windows has no parent-death signal, so the child is stopped by its owner's shutdown path alone.
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
