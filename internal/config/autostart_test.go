package config

import (
	"os"
	"path/filepath"
	"testing"
)

// An upgrading user must not silently get a screen-recording daemon installed at login, so autostart stays off both for a fresh install (no config file at all) and for a config file written before the field existed (no "autostart" key).
func TestLoadConfig_AutostartDefaultsOff(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	if cfg := LoadConfig(); cfg.Autostart {
		t.Error("expected a fresh config to default autostart to false")
	}

	if err := os.MkdirAll(filepath.Dir(ConfigPath()), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(ConfigPath(), []byte(`{"tracker":{"dwell_time_ms":15000},"voice":"Iapetus"}`), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if cfg := LoadConfig(); cfg.Autostart {
		t.Error("expected a config file with no autostart key to load as autostart=false")
	}
}

func TestSaveLoadConfig_AutostartRoundTrip(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())

	cfg := LoadConfig()
	cfg.Autostart = false
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig returned unexpected error: %v", err)
	}
	if reloaded := LoadConfig(); reloaded.Autostart {
		t.Error("expected autostart=false to survive SaveConfig/LoadConfig")
	}

	cfg.Autostart = true
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig returned unexpected error: %v", err)
	}
	if reloaded := LoadConfig(); !reloaded.Autostart {
		t.Error("expected autostart=true to survive SaveConfig/LoadConfig")
	}
}
