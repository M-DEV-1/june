//go:build !linux

package dream

import "syscall"

// childProcAttr has no parent-death signal to ask for outside Linux, so the child is cleaned up by ShadowLifecycle's Stop alone.
func childProcAttr() *syscall.SysProcAttr {
	return nil
}
