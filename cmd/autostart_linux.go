//go:build linux

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// autostartDesktopPath returns the path of June's XDG autostart entry: $XDG_CONFIG_HOME/autostart/june.desktop, falling back to ~/.config when XDG_CONFIG_HOME is unset.
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
	return filepath.Join(dir, "autostart", "june.desktop")
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

// autostartCurrent reports whether an installed entry needs no rewrite. It always does not on Linux; see reconcileAutostart.
func autostartCurrent() bool { return true }

// setAutostart writes the autostart .desktop entry when on is true, and removes it when false.
// Removing an entry that isn't there is not an error.
// Exec= uses the absolute path from os.Executable and Path= pins the working directory to the binary's own directory, because June loads .env relative to the working directory and a session manager launches autostart entries from an arbitrary one. (The database, vector index, config and IPC token no longer depend on cwd — see config.DataDir.)
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
	entry := desktopEntry(exe, filepath.Dir(exe))

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(entry), 0644)
}

// desktopEntry renders the .desktop file body that launches exe as the daemon with its working directory pinned to dir, escaping both per the Desktop Entry Specification's string quoting rules (see desktopEntryQuoteExec, desktopEntryEscapeString) rather than Go's strconv.Quote (%q), which uses Go escape syntax a Desktop Entry parser doesn't understand. Without this, an install path containing a space or backslash produces an entry that silently fails to launch.
func desktopEntry(exe, dir string) string {
	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=June
Comment=June context runtime
Exec=%s --daemon
Path=%s
Terminal=false
X-GNOME-Autostart-enabled=true
`, desktopEntryQuoteExec(exe), desktopEntryEscapeString(dir))
}

// desktopEntryQuoteExec quotes s as one whitespace-tokenized argument in a Desktop Entry Exec= value, per the spec's Exec key quoting rules: wrap in double quotes and backslash-escape the characters that are otherwise special inside a quoted argument (", `, $, \) so a compliant parser reads the whole quoted string back as a single literal argument.
func desktopEntryQuoteExec(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '`', '$', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// desktopEntryEscapeString escapes s for use as a plain Desktop Entry string value (e.g. Path=), per the spec's general string-type escaping: a literal backslash must be written as \\, otherwise a parser reads the following character as one of the spec's \s \n \t \r escape sequences instead of a literal character.
func desktopEntryEscapeString(s string) string {
	return strings.ReplaceAll(s, `\`, `\\`)
}
