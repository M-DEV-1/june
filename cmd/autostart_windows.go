//go:build windows

package cmd

import (
	"errors"
	"os"
	"path/filepath"

	"june/internal/util"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// autostartRunKey is the per-user Run key, whose values Explorer launches once at login.
// HKCU rather than HKLM, so installing the entry needs no elevation.
const autostartRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// autostartValueName is the name of June's value under the Run key.
const autostartValueName = "June"

// startupApprovedKey is where Explorer keeps the switch Task Manager's Startup apps tab and Settings > Apps > Startup flip for each Run value. A Run value stays in place when the user turns it off there; only this switch says it no longer runs.
const startupApprovedKey = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`

// autostartEnabled reports whether June starts at sign-in: its value is present under the per-user Run key, and Windows' startup switch for it is not off.
func autostartEnabled() bool {
	if !runValuePresent() {
		return false
	}
	set, on := startupApproval()
	return !set || on
}

// autostartSystemChoice reports what the user chose for June in Windows' own startup list. Output: on, and true when June's Run value is there and the list holds a switch for it; false when there is no such choice, as there never is once June has written or removed its own entry, since setAutostart clears the switch.
func autostartSystemChoice() (on, ok bool) {
	if !runValuePresent() {
		return false, false
	}
	set, on := startupApproval()
	return on, set
}

// runValuePresent reports whether June's value is under the per-user Run key.
func runValuePresent() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(autostartValueName)
	return err == nil
}

// startupApproval reads Windows' startup switch for June. Output: whether there is one, and whether it is on. The value is binary and its first byte says it: even (2, or 6) is on, odd (3, or 7) is off; the rest is when it was last turned off.
func startupApproval() (set, on bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, startupApprovedKey, registry.QUERY_VALUE)
	if err != nil {
		return false, false
	}
	defer k.Close()
	b, _, err := k.GetBinaryValue(autostartValueName)
	if err != nil || len(b) == 0 {
		return false, false
	}
	return true, b[0]&1 == 0
}

// clearStartupApproval removes Windows' startup switch for June, so the Run value June has just written or removed is the whole truth: a switch left off would keep June from starting at sign-in however often it turned itself on. With no switch Explorer runs the value, and its startup list shows it as enabled.
func clearStartupApproval() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, startupApprovedKey, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(autostartValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// autostartCurrent reports whether June's Run value is exactly the command setAutostart would write now. It is not after the folder holding june.exe is moved, or when junew.exe has appeared beside it, and reconcileAutostart then rewrites it.
func autostartCurrent() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	got, _, err := k.GetStringValue(autostartValueName)
	want, werr := autostartCommand()
	return err == nil && werr == nil && got == want
}

// autostartCommand is the Run value that starts the binary running now as the daemon. Output: the command line, or the error from finding this executable.
func autostartCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// june.exe is a console program, so Explorer would open a console window for it at login and closing that window would end the daemon. The package ships the same program built without a console as junew.exe beside it (see packaging/release-windows.ps1), and the login entry runs that one when it is there.
	if gui := filepath.Join(filepath.Dir(exe), "junew.exe"); util.Exists(gui) {
		exe = gui
	}
	// The Run key offers no working-directory setting the way an XDG .desktop entry's Path= does, and June loads .env relative to the working directory, so the entry passes --workdir with the binary's own directory instead.
	// EscapeArg quotes a path with spaces and doubles a trailing backslash, so a root directory like C:\ does not swallow the closing quote.
	return windows.EscapeArg(exe) + " --daemon --workdir " + windows.EscapeArg(filepath.Dir(exe)), nil
}

// setAutostart writes June's value under the per-user Run key when on is true, and deletes it when false, clearing Windows' startup switch for it either way (see clearStartupApproval).
// Deleting a value that isn't there is not an error.
func setAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, autostartRunKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	if !on {
		if err := k.DeleteValue(autostartValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return clearStartupApproval()
	}

	cmd, err := autostartCommand()
	if err != nil {
		return err
	}
	if err := k.SetStringValue(autostartValueName, cmd); err != nil {
		return err
	}
	return clearStartupApproval()
}
