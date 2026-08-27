//go:build linux

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ora/internal/config"
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

	data, err := os.ReadFile(filepath.Join(dir, "autostart", "ora.desktop"))
	if err != nil {
		t.Fatalf("expected ora.desktop to exist: %v", err)
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
	if _, err := os.Stat(filepath.Join(dir, "autostart", "ora.desktop")); !os.IsNotExist(err) {
		t.Errorf("expected ora.desktop to be removed, stat err = %v", err)
	}

	// disabling again on an already-absent entry is not an error
	if err := setAutostart(false); err != nil {
		t.Errorf("setAutostart(false) on an absent entry returned error: %v", err)
	}
}

// TestApplyAutostart_WritesConfigAndEntry checks the --autostart flag path: it persists the choice to ora-config.json and installs or removes the login entry to match, and rejects anything that isn't on or off.
func TestApplyAutostart_WritesConfigAndEntry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())

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

// TestReconcileAutostart_MakesDiskMatchConfig covers both directions of the startup reconcile: config on with nothing installed installs it, config off with an entry present removes it.
func TestReconcileAutostart_MakesDiskMatchConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	reconcileAutostart(true)
	if !autostartEnabled() {
		t.Error("expected reconcileAutostart(true) to install the autostart entry")
	}

	reconcileAutostart(false)
	if autostartEnabled() {
		t.Error("expected reconcileAutostart(false) to remove the autostart entry")
	}
}
