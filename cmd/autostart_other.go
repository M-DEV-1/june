//go:build !windows && !linux

package cmd

import "fmt"

// autostartEnabled always reports false: there is no login-entry mechanism wired up on platforms other than Linux and Windows.
func autostartEnabled() bool { return false }

// setAutostart reports that start-on-login is unsupported here.
// Turning it off is a no-op success, since there is nothing installed to remove.
func setAutostart(on bool) error {
	if !on {
		return nil
	}
	return fmt.Errorf("start on login is not supported on this platform")
}

// autostartCurrent always reports true: there is no entry to fall out of date.
func autostartCurrent() bool { return true }

// autostartSystemChoice reports no choice: there is no startup list to read here.
func autostartSystemChoice() (on, ok bool) { return false, false }
