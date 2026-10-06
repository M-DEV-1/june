//go:build !windows

package ipc

// letOpenedComeForward has nothing to do outside Windows, where the desktop decides for itself whether a newly opened window comes forward.
func letOpenedComeForward() {}
