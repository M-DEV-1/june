package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfig_AutostartDefaultsOff checks a fresh config (no file on disk) has autostart disabled — upgrading users must not silently get a screen-recording daemon installed at login (FINDING 6).
func TestLoadConfig_AutostartDefaultsOff(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())

	if cfg := LoadConfig(); cfg.Autostart {
		t.Error("expected a fresh config to default autostart to false")
	}
}

// TestLoadConfig_MissingAutostartKey_LoadsFalse checks a config file written before the autostart field existed (no "autostart" key at all) loads as false rather than defaulting to true.
func TestLoadConfig_MissingAutostartKey_LoadsFalse(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())

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
