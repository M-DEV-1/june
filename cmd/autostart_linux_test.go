//go:build linux

package cmd

import (
	"testing"

	"june/internal/config"
)

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
