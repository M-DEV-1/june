//go:build !linux

package embed

import "syscall"

// childProcAttr has no parent-death signal to ask for outside Linux, so the child is cleaned up by Engine.Close alone.
func childProcAttr() *syscall.SysProcAttr {
	return nil
}
