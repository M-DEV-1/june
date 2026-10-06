//go:build !windows

package ipc

import "errors"

// probeHotkey has nothing to register a shortcut against off Windows: GNOME's shortcuts are checked by reading its keybindings instead (see gnomeBindings). Output: always the error.
func probeHotkey(Accel) (bool, error) {
	return false, errors.New("this system has no shortcut test")
}
