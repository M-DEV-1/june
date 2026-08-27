package config

import "testing"

// TestLoadConfig_AutostartDefaultsOn checks a fresh config (no file on disk) has autostart enabled, and that a config file written before the field existed also loads as enabled rather than as the zero value.
func TestLoadConfig_AutostartDefaultsOn(t *testing.T) {
	t.Chdir(t.TempDir())

	if cfg := LoadConfig(); !cfg.Autostart {
		t.Error("expected a fresh config to default autostart to true")
	}
}

func TestSaveLoadConfig_AutostartRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())

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
