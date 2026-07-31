package config

import "testing"

func TestIsValidVoice(t *testing.T) {
	if !IsValidVoice("Kore") {
		t.Error("expected Kore to be a valid voice")
	}
	if !IsValidVoice("kore") {
		t.Error("expected case-insensitive match to accept lowercase kore")
	}
	if IsValidVoice("NotARealVoice") {
		t.Error("expected garbage voice name to be invalid")
	}
	if IsValidVoice("") {
		t.Error("expected empty string to be invalid")
	}
}

func TestNormalizeVoice(t *testing.T) {
	canonical, ok := NormalizeVoice("iapetus")
	if !ok {
		t.Fatal("expected iapetus to normalize successfully")
	}
	if canonical != "Iapetus" {
		t.Errorf("expected canonical spelling Iapetus, got %q", canonical)
	}

	if _, ok := NormalizeVoice("garbage-voice"); ok {
		t.Error("expected garbage voice to fail normalization")
	}
}

func TestAvailableVoicesContainsDefault(t *testing.T) {
	if !IsValidVoice(DefaultVoice) {
		t.Errorf("DefaultVoice %q must itself be a valid/available voice", DefaultVoice)
	}
}

func TestLoadConfig_DefaultsVoice(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg := LoadConfig()
	if cfg.Voice != DefaultVoice {
		t.Errorf("expected fresh config to default voice to %q, got %q", DefaultVoice, cfg.Voice)
	}
}

func TestSetVoice_RoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg := LoadConfig()
	if err := cfg.SetVoice("kore"); err != nil {
		t.Fatalf("SetVoice returned unexpected error: %v", err)
	}
	if cfg.Voice != "Kore" {
		t.Errorf("expected in-memory config voice to be canonical Kore, got %q", cfg.Voice)
	}

	// reload from disk to prove the change persisted
	reloaded := LoadConfig()
	if reloaded.Voice != "Kore" {
		t.Errorf("expected reloaded config voice to be Kore, got %q", reloaded.Voice)
	}
}

func TestSetVoice_RejectsInvalid(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg := LoadConfig()
	original := cfg.Voice

	if err := cfg.SetVoice("NotAVoice"); err == nil {
		t.Fatal("expected error for invalid voice name")
	}
	if cfg.Voice != original {
		t.Errorf("expected voice to remain %q after rejected set, got %q", original, cfg.Voice)
	}

	reloaded := LoadConfig()
	if reloaded.Voice != original {
		t.Errorf("expected persisted config to remain %q, got %q", original, reloaded.Voice)
	}
}

func TestLoadConfig_RejectsBadPersistedVoice(t *testing.T) {
	t.Chdir(t.TempDir())

	// write a config with an invalid voice directly, simulating a hand-edited or stale ora-config.json
	cfg := LoadConfig()
	cfg.Voice = "TotallyMadeUp"
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	reloaded := LoadConfig()
	if reloaded.Voice != DefaultVoice {
		t.Errorf("expected LoadConfig to fall back to default for invalid persisted voice, got %q", reloaded.Voice)
	}
}
