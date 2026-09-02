//go:build linux

package embed

import "syscall"

// childProcAttr asks the kernel to SIGTERM the embedding server if the daemon dies. Without this a daemon killed with SIGKILL (which runs no shutdown path) would leave a 600 MB llama-server orphaned. Linux-only: Pdeathsig does not exist elsewhere, and the shutdown path in Engine.Close covers the ordinary case on every platform.
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
