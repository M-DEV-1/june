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

// autostartEnabled reports whether June's value is present under the per-user Run key.
func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(autostartValueName)
	return err == nil
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

// setAutostart writes June's value under the per-user Run key when on is true, and deletes it when false.
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
		return nil
	}

	cmd, err := autostartCommand()
	if err != nil {
		return err
	}
	return k.SetStringValue(autostartValueName, cmd)
}
