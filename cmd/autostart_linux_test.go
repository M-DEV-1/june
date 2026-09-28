//go:build linux

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"june/internal/config"
)

// TestSetAutostart_WritesAndRemovesDesktopEntry drives the whole enable/disable cycle against a throwaway XDG_CONFIG_HOME so the real ~/.config/autostart is never touched.
func TestSetAutostart_WritesAndRemovesDesktopEntry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if autostartEnabled() {
		t.Fatal("expected autostart to be disabled in a fresh XDG_CONFIG_HOME")
	}

	if err := setAutostart(true); err != nil {
		t.Fatalf("setAutostart(true) returned unexpected error: %v", err)
	}
	if !autostartEnabled() {
		t.Error("expected autostart to report enabled after setAutostart(true)")
	}

	data, err := os.ReadFile(filepath.Join(dir, "autostart", "june.desktop"))
	if err != nil {
		t.Fatalf("expected june.desktop to exist: %v", err)
	}
	entry := string(data)

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable failed: %v", err)
	}
	for _, want := range []string{
		"[Desktop Entry]",
		"Type=Application",
		"Terminal=false",
		"X-GNOME-Autostart-enabled=true",
		"Exec=\"" + exe + "\" --daemon",
		"Path=" + filepath.Dir(exe),
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("expected desktop entry to contain %q, got:\n%s", want, entry)
		}
	}

	if err := setAutostart(false); err != nil {
		t.Fatalf("setAutostart(false) returned unexpected error: %v", err)
	}
	if autostartEnabled() {
		t.Error("expected autostart to report disabled after setAutostart(false)")
	}
	if _, err := os.Stat(filepath.Join(dir, "autostart", "june.desktop")); !os.IsNotExist(err) {
		t.Errorf("expected june.desktop to be removed, stat err = %v", err)
	}

	// disabling again on an already-absent entry is not an error
	if err := setAutostart(false); err != nil {
		t.Errorf("setAutostart(false) on an absent entry returned error: %v", err)
	}
}

// TestDesktopEntry_EscapesSpecialCharacters verifies a path containing a space and a backslash is escaped per the Desktop Entry Specification's quoting rules (FINDING 11): Exec= is Go's strconv.Quote today, which is not Desktop Entry quoting, and Path= is emitted raw with no escaping at all — an install path with either character produces an entry a spec-compliant parser reads wrong or silently fails to launch.
func TestDesktopEntry_EscapesSpecialCharacters(t *testing.T) {
	exe := `/home/user/My Apps/back\slash/june`
	dir := filepath.Dir(exe)

	entry := desktopEntry(exe, dir)

	wantExec := `Exec="/home/user/My Apps/back\\slash/june" --daemon`
	if !strings.Contains(entry, wantExec) {
		t.Errorf("expected entry to contain %q, got:\n%s", wantExec, entry)
	}
	wantPath := `Path=/home/user/My Apps/back\\slash`
	if !strings.Contains(entry, wantPath) {
		t.Errorf("expected entry to contain %q, got:\n%s", wantPath, entry)
	}
}

// TestApplyAutostart_WritesConfigAndEntry checks the --autostart flag path: it persists the choice to june-config.json and installs or removes the login entry to match, and rejects anything that isn't on or off.
func TestApplyAutostart_WritesConfigAndEntry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("JUNE_DATA_DIR", t.TempDir())

	if err := applyAutostart("off"); err != nil {
		t.Fatalf("applyAutostart(off) returned unexpected error: %v", err)
	}
	if config.LoadConfig().Autostart {
		t.Error("expected --autostart=off to persist autostart=false to the config")
	}
	if autostartEnabled() {
		t.Error("expected --autostart=off to leave no login entry installed")
	}

	if err := applyAutostart("ON"); err != nil {
		t.Fatalf("applyAutostart(ON) returned unexpected error: %v", err)
	}
	if !config.LoadConfig().Autostart {
		t.Error("expected --autostart=ON to persist autostart=true to the config")
	}
	if !autostartEnabled() {
		t.Error("expected --autostart=ON to install the login entry")
	}

	if err := applyAutostart("maybe"); err == nil {
		t.Error("expected applyAutostart to reject a value that is neither on nor off")
	}
}
