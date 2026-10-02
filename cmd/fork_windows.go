//go:build windows

package cmd

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// spawnHiddenDaemon starts this same program as the daemon, with no console window and in a process group of its own. Output: the error from starting it, if any.
// Without its own (windowless) console the daemon would share the terminal's, and closing that terminal or pressing Ctrl+C in it would end the daemon too. A windowless console rather than none at all (DETACHED_PROCESS), so a console program the daemon starts inherits it and gets no window of its own either.
func spawnHiddenDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, "--daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP}
	return cmd.Start()
}
