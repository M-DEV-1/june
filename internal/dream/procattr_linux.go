//go:build linux

package dream

import "syscall"

// childProcAttr asks the kernel to SIGTERM the shadow's llama-server if the daemon dies. Without this a daemon killed with SIGKILL (which runs no shutdown path) would leave the shadow's ~2 GB llama-server orphaned. Linux-only: Pdeathsig does not exist elsewhere, and ShadowLifecycle's Stop covers the ordinary shutdown path on every platform.
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
