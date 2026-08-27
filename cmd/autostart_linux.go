//go:build linux

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
)

// autostartDesktopPath returns the path of ORA's XDG autostart entry: $XDG_CONFIG_HOME/autostart/ora.desktop, falling back to ~/.config when XDG_CONFIG_HOME is unset.
// A .desktop file here is honoured by GNOME, KDE and XFCE alike, which a systemd user unit is not.
// Returns "" when neither XDG_CONFIG_HOME nor a home directory can be determined.
func autostartDesktopPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", "ora.desktop")
}

// autostartEnabled reports whether the autostart .desktop entry exists on disk.
func autostartEnabled() bool {
	path := autostartDesktopPath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// setAutostart writes the autostart .desktop entry when on is true, and removes it when false.
// Removing an entry that isn't there is not an error.
// Exec= uses the absolute path from os.Executable and Path= pins the working directory to the binary's own directory, because ORA resolves its ora-db config and database relative to the working directory and a session manager launches autostart entries from an arbitrary one.
func setAutostart(on bool) error {
	path := autostartDesktopPath()
	if path == "" {
		return fmt.Errorf("cannot determine XDG config directory")
	}

	if !on {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	entry := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Ora
Comment=Ora context runtime
Exec=%q --daemon
Path=%s
Terminal=false
X-GNOME-Autostart-enabled=true
`, exe, filepath.Dir(exe))

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(entry), 0644)
}
