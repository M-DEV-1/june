//go:build windows

package cmd

import (
	"os"
	"os/exec"
	"syscall"
)

func spawnHiddenDaemon() error {
	exePath, err := os.Executable()
	if err != nil {
		exePath = os.Args[0] // find current pwd if os.Exec fails but os.Exec mostly wont
	}
	cmd := exec.Command(exePath, "--daemon")
	// CREATE_NO_WINDOW flag
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}
