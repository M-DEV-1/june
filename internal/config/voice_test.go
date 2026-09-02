package config

import "testing"

func TestNormalizeVoice(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"Iapetus", "Iapetus", true},
		{"iapetus", "Iapetus", true}, // matching is case-insensitive
		{"garbage-voice", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeVoice(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("NormalizeVoice(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// TestLoadConfig_DefaultsProactiveAudioOn covers both a fresh config and one written before the field existed: proactive audio is on unless the user turns it off, so an upgrading install gets it without editing anything. The field is a *bool precisely so "absent" and "explicitly false" stay distinguishable — a plain bool would read a pre-existing config's missing key as "off".
func TestLoadConfig_DefaultsProactiveAudioOn(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())

	if cfg := LoadConfig(); !cfg.ProactiveAudioEnabled() {
		t.Error("expected a fresh config to have proactive audio enabled")
	}
}

func TestLoadConfig_KeepsProactiveAudioDisabled(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())

	cfg := LoadConfig()
	off := false
	cfg.ProactiveAudio = &off
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	if reloaded := LoadConfig(); reloaded.ProactiveAudioEnabled() {
		t.Error("expected an explicitly disabled proactive audio setting to survive a reload")
	}
}

func TestSetVoice_RoundTrip(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())

	cfg := LoadConfig()
	if err := cfg.SetVoice("kore"); err != nil {
		t.Fatalf("SetVoice returned unexpected error: %v", err)
	}
	if cfg.Voice != "Kore" {
		t.Errorf("expected in-memory config voice to be canonical Kore, got %q", cfg.Voice)
	}

	// reload from disk to prove the change persisted
	if reloaded := LoadConfig(); reloaded.Voice != "Kore" {
		t.Errorf("expected reloaded config voice to be Kore, got %q", reloaded.Voice)
	}
}

func TestSetVoice_RejectsInvalid(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())

	cfg := LoadConfig()
	original := cfg.Voice

	if err := cfg.SetVoice("NotAVoice"); err == nil {
		t.Fatal("expected error for invalid voice name")
	}
	if cfg.Voice != original {
		t.Errorf("expected voice to remain %q after rejected set, got %q", original, cfg.Voice)
	}
	if reloaded := LoadConfig(); reloaded.Voice != original {
		t.Errorf("expected persisted config to remain %q, got %q", original, reloaded.Voice)
	}
}

// A hand-edited or stale ora-config.json naming a voice the API doesn't have must not reach the Live API.
func TestLoadConfig_RejectsBadPersistedVoice(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())

	cfg := LoadConfig()
	cfg.Voice = "TotallyMadeUp"
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	if reloaded := LoadConfig(); reloaded.Voice != DefaultVoice {
		t.Errorf("expected LoadConfig to fall back to default for invalid persisted voice, got %q", reloaded.Voice)
	}
}
