package util

import "syscall"

// ChildProcAttr asks the kernel to SIGTERM a spawned server if the daemon dies. Without it a daemon killed with SIGKILL, which runs no shutdown path, leaves its llama-server children orphaned and holding their GPU memory.
func ChildProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
