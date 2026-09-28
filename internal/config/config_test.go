package config

import (
	"os"
	"path/filepath"

	"testing"
	"time"
)

// DataDir resolves in a fixed order: the ORA_DATA_DIR override verbatim (what the tests and any scripted/portable install use), then $XDG_DATA_HOME/ora, then ~/.local/share/ora.
func TestDataDir_ResolutionOrder(t *testing.T) {
	t.Run("ORA_DATA_DIR override wins", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ORA_DATA_DIR", dir)
		if got := DataDir(); got != dir {
			t.Errorf("DataDir() = %q, want override %q", got, dir)
		}
	})

	t.Run("XDG_DATA_HOME", func(t *testing.T) {
		t.Setenv("ORA_DATA_DIR", "")
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)
		if got, want := DataDir(), filepath.Join(xdg, "ora"); got != want {
			t.Errorf("DataDir() = %q, want %q", got, want)
		}
	})

	t.Run("home fallback", func(t *testing.T) {
		t.Setenv("ORA_DATA_DIR", "")
		t.Setenv("XDG_DATA_HOME", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		if got, want := DataDir(), filepath.Join(home, ".local", "share", "ora"); got != want {
			t.Errorf("DataDir() = %q, want %q", got, want)
		}
	})
}

// TestEmbedConfigDefaults verifies a config file with no "embed" key at all (every install before the local embedder existed) still comes back with the local port and idle timeout filled in, so the daemon never spawns llama-server on port 0 or reaps it instantly. It also checks that a half-written embed block — a binary named with no model path — leaves LocalEnabled false too: a half-written config must not take embeddings down.
func TestEmbedConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "ora-config.json"), []byte(`{"voice":"Iapetus"}`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()
	if cfg.Embed.Port != DefaultEmbedPort {
		t.Errorf("Embed.Port = %d, want %d", cfg.Embed.Port, DefaultEmbedPort)
	}
	if cfg.Embed.IdleTimeout != DefaultEmbedIdleTimeout {
		t.Errorf("Embed.IdleTimeout = %v, want %v", cfg.Embed.IdleTimeout, DefaultEmbedIdleTimeout)
	}
	if cfg.Embed.LocalEnabled() {
		t.Error("Embed.LocalEnabled() should be false with no llama_server/model_path configured")
	}

	dir2 := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir2)
	if err := os.WriteFile(filepath.Join(dir2, "ora-config.json"), []byte(`{"embed":{"llama_server":"/opt/llama-server"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if LoadConfig().Embed.LocalEnabled() {
		t.Error("Embed.LocalEnabled() = true with a binary but no model_path, want false")
	}
}

// TestBackgroundBrainConfig_PinsGeminiOnlyWhenUnset checks that a background duty on the Gemini API gets the cheap high-allowance model, that a model the user pinned is left alone, and that a CLI provider is untouched because its Model field is a CLI alias rather than a Gemini model name.
func TestBackgroundBrainConfig_PinsGeminiOnlyWhenUnset(t *testing.T) {
	SetBackgroundModels(map[string]string{JobMeetingMinutes: "gemini-3.5-flash-lite"})
	t.Cleanup(func() { SetBackgroundModels(nil) })

	cases := []struct {
		name      string
		in        BrainConfig
		wantModel string
	}{
		{"an empty provider is the gemini api and gets the background model", BrainConfig{}, "gemini-3.5-flash-lite"},
		{"the named gemini api provider gets it too", BrainConfig{Provider: BrainGeminiAPI}, "gemini-3.5-flash-lite"},
		{"a model the user pinned wins", BrainConfig{Provider: BrainGeminiAPI, Model: "gemini-3.6-flash"}, "gemini-3.6-flash"},
		{"a claude-cli alias is left alone", BrainConfig{Provider: BrainClaudeCLI, Model: "sonnet"}, "sonnet"},
		{"a claude-cli with no alias stays empty rather than getting a gemini model name", BrainConfig{Provider: BrainClaudeCLI}, ""},
		{"codex is left alone", BrainConfig{Provider: BrainCodex, Model: "gpt-5.5"}, "gpt-5.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BackgroundBrainConfig(tc.in, JobMeetingMinutes).Model; got != tc.wantModel {
				t.Errorf("Model = %q, want %q", got, tc.wantModel)
			}
		})
	}
}

// TestLoadConfig_KeepsClaudeUsageFromLoginDisabled checks an explicit off survives a save and a reload, because an absent field reads as on.
func TestLoadConfig_KeepsClaudeUsageFromLoginDisabled(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())

	cfg := LoadConfig()
	off := false
	cfg.ClaudeUsageFromLogin = &off
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	if reloaded := LoadConfig(); reloaded.ClaudeUsageFromLoginEnabled() {
		t.Error("expected an explicitly disabled Claude usage setting to survive a reload")
	}
}

// The config sits beside the IPC token and the store in a directory that is the user's alone, and SaveConfig used to write it 0644 into a directory it created 0755.
func TestSaveConfig_WritesTheConfigAndItsDirectoryPrivateToTheUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ora")
	t.Setenv("ORA_DATA_DIR", dir)

	if err := SaveConfig(OraConfig{Voice: DefaultVoice}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	info, err := os.Stat(ConfigPath())
	if err != nil {
		t.Fatalf("stat the config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("expected the config to be 0600, got %o", got)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the data dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0700 {
		t.Errorf("expected the data dir to be 0700, got %o", got)
	}
}

// A config written before a field existed leaves it out and json.Unmarshal keeps the default, but one that carries the field with a zero value overwrites it. A zero dwell samples every window the pointer crosses, and a null blocklist turns off the password-manager list that keeps 1Password and KeePassXC out of capture, so both fall back to their defaults.
func TestLoadConfig_ZeroTrackerFieldsFallBackToTheDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "ora-config.json"), []byte(`{"tracker":{"dwell_time_ms":0,"blocklist":null}}`), 0600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	cfg := LoadConfig()

	if cfg.Tracker.DwellTime != DefaultDwellTime {
		t.Errorf("dwell time = %v, want the default %v", cfg.Tracker.DwellTime, DefaultDwellTime)
	}
	if len(cfg.Tracker.Blocklist) != len(DefaultBlocklist) {
		t.Errorf("blocklist has %d entries, want the default %d", len(cfg.Tracker.Blocklist), len(DefaultBlocklist))
	}
}

// TestTrackerDwellTime_IsMilliseconds pins the unit the dwell time is stored in, because the field was typed time.Duration while holding 15000 milliseconds — as a Duration that number reads as 15 microseconds, and it was only ever right because the one call site multiplied by time.Millisecond a second time. A plain int cannot be handed to a Duration parameter by mistake.
func TestTrackerDwellTime_IsMilliseconds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORA_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "ora-config.json"), []byte(`{"tracker":{"dwell_time_ms":2500}}`), 0600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	// The assignment to a plain int is the assertion about the type: a time.Duration would not compile here, and that is what made 15000 read as 15 microseconds everywhere but the one call site.
	var ms int = LoadConfig().Tracker.DwellTime
	if got := ms; got != 2500 {
		t.Errorf("dwell time = %d, want 2500 milliseconds read straight off the file", got)
	}
	if got := time.Duration(DefaultDwellTime) * time.Millisecond; got != 15*time.Second {
		t.Errorf("the default dwell time is %v once the call site converts it, want 15s", got)
	}
}
