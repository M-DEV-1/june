package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// DataDir resolves in a fixed order: the JUNE_DATA_DIR override verbatim, then the platform data home: %LOCALAPPDATA%\june on Windows, $XDG_DATA_HOME/june or ~/.local/share/june elsewhere.
func TestDataDir_ResolutionOrder(t *testing.T) {
	t.Run("JUNE_DATA_DIR override wins", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("JUNE_DATA_DIR", dir)
		if got := DataDir(); got != dir {
			t.Errorf("DataDir() = %q, want override %q", got, dir)
		}
	})

	if runtime.GOOS == "windows" {
		t.Run("LOCALAPPDATA", func(t *testing.T) {
			t.Setenv("JUNE_DATA_DIR", "")
			local := t.TempDir()
			t.Setenv("LOCALAPPDATA", local)
			if got, want := DataDir(), filepath.Join(local, "june"); got != want {
				t.Errorf("DataDir() = %q, want %q", got, want)
			}
		})
		return
	}

	t.Run("XDG_DATA_HOME", func(t *testing.T) {
		t.Setenv("JUNE_DATA_DIR", "")
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)
		if got, want := DataDir(), filepath.Join(xdg, "june"); got != want {
			t.Errorf("DataDir() = %q, want %q", got, want)
		}
	})

	t.Run("home fallback", func(t *testing.T) {
		t.Setenv("JUNE_DATA_DIR", "")
		t.Setenv("XDG_DATA_HOME", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		if got, want := DataDir(), filepath.Join(home, ".local", "share", "june"); got != want {
			t.Errorf("DataDir() = %q, want %q", got, want)
		}
	})
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
		{"a model the user pinned wins", BrainConfig{Provider: BrainGeminiAPI, Model: "gemini-3.6-flash"}, "gemini-3.6-flash"},
		{"a claude-cli with no alias stays empty rather than getting a gemini model name", BrainConfig{Provider: BrainClaudeCLI}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BackgroundBrainConfig(tc.in, JobMeetingMinutes).Model; got != tc.wantModel {
				t.Errorf("Model = %q, want %q", got, tc.wantModel)
			}
		})
	}
}

// The config sits beside the IPC token and the store in a directory that is the user's alone, and SaveConfig used to write it 0644 into a directory it created 0755.
func TestSaveConfig_WritesTheConfigAndItsDirectoryPrivateToTheUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "june")
	t.Setenv("JUNE_DATA_DIR", dir)

	if err := SaveConfig(JuneConfig{Voice: DefaultVoice}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX permission bits; the profile directory's ACL keeps the file private")
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
	t.Setenv("JUNE_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "june-config.json"), []byte(`{"tracker":{"dwell_time_ms":0,"blocklist":null}}`), 0600); err != nil {
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
