//go:build !windows

package cmd

import (
	"os"
	"os/exec"
)

func spawnHiddenDaemon() error {
	cmd := exec.Command(os.Args[0], "--daemon")
	return cmd.Start()
}
