//go:build windows

package cmd

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// autostartRunKey is the per-user Run key, whose values Explorer launches once at login.
// HKCU rather than HKLM, so installing the entry needs no elevation.
const autostartRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// autostartValueName is the name of ORA's value under the Run key.
const autostartValueName = "ORA"

// autostartEnabled reports whether ORA's value is present under the per-user Run key.
func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(autostartValueName)
	return err == nil
}

// setAutostart writes ORA's value under the per-user Run key when on is true, and deletes it when false.
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

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// The Run key offers no working-directory setting the way an XDG .desktop entry's Path= does, and ORA resolves ora-db relative to the working directory, so the entry passes --workdir with the binary's own directory instead.
	return k.SetStringValue(autostartValueName, `"`+exe+`" --daemon --workdir "`+filepath.Dir(exe)+`"`)
}
