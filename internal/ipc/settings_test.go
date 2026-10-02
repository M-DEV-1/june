package ipc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"june/internal/config"
)

// withEmptyFirstRun points HOME at a fresh, empty temp directory and clears GEMINI_API_KEY, so a test that does not care about first-run detection gets the same "nothing set up" answer regardless of what is actually on this machine.
func withEmptyFirstRun(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GEMINI_API_KEY", "")
}

// noFirstRunSteps is the FirstRunView a config with no brain, no login files and no local model produces: nothing is set up, so every step is listed.
// A function rather than a package variable because one of the steps now names a real path under the data directory, and a variable is evaluated at init — before a test has pointed HOME at its own temp directory, so it named the developer's own home.
func noFirstRunSteps() FirstRunView { return FirstRunView{Steps: firstRunSteps(FirstRunView{})} }

// noopSave is the persist function for a test that never posts a change and so never needs one to actually write anything.
func noopSave(config.JuneConfig) error { return nil }

// withFakeGsettings replaces the gsettings runner for the rest of the test, restoring the real one when it ends. Every test that does not care about the hotkey uses the "nothing configured" fake, so it never depends on what this machine's own desktop actually has bound.
func withFakeGsettings(t *testing.T, fake func(args ...string) (string, error)) {
	t.Helper()
	prev := gsettingsRunner
	gsettingsRunner = fake
	t.Cleanup(func() { gsettingsRunner = prev })
}

// noCustomKeybindings is the gsettings runner fake for a desktop with no custom keybindings registered at all.
func noCustomKeybindings(args ...string) (string, error) {
	return "@as []", nil
}

func TestSettings_RealValuesFromDiskAndConfig(t *testing.T) {
	withFakeGsettings(t, noCustomKeybindings)
	withEmptyFirstRun(t)
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "db"), make([]byte, 100), 0600); err != nil {
		t.Fatalf("seed db file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "db-wal"), make([]byte, 20), 0600); err != nil {
		t.Fatalf("seed db-wal file: %v", err)
	}
	// no db-shm: exercises the missing-file-is-zero path within a sum.
	if err := os.MkdirAll(filepath.Join(dataDir, "recordings", "2026-09-04"), 0755); err != nil {
		t.Fatalf("seed recordings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "recordings", "2026-09-04", "audio.wav"), make([]byte, 500), 0600); err != nil {
		t.Fatalf("seed recording file: %v", err)
	}
	// no models dir at all: exercises the missing-directory-is-zero path.

	startedAt := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)

	cases := []struct {
		name            string
		cfg             config.JuneConfig
		meetingsEnabled bool
		paused          bool
		want            SettingsView
	}{
		{
			name:            "gemini brain, local embedder, meetings and capture on",
			cfg:             config.JuneConfig{Brain: config.BrainConfig{}, Embed: config.EmbedConfig{LlamaServer: "/usr/bin/llama-server", ModelPath: "/models/embed.gguf"}},
			meetingsEnabled: true,
			paused:          false,
			want: SettingsView{
				DataDir: dataDir, StoreBytes: 120, RecordingsBytes: 500, ModelsBytes: 0,
				VoiceModel: config.VoiceModel(), Brain: config.TextModel, EmbedModel: config.LocalEmbedModel,
				MeetingsEnabled: true, CaptureEnabled: true, KeepAudioDays: -1,
				DaemonStarted: "2026-09-04T08:00:00Z", Version: "dev", FirstRun: noFirstRunSteps(), ClaudeUsageFromLogin: true,
			},
		},
		{
			name:            "claude-cli brain with a pinned model, no local embedder, meetings and capture off",
			cfg:             config.JuneConfig{Brain: config.BrainConfig{Provider: config.BrainClaudeCLI, Model: "sonnet"}},
			meetingsEnabled: false,
			paused:          true,
			want: SettingsView{
				DataDir: dataDir, StoreBytes: 120, RecordingsBytes: 500, ModelsBytes: 0,
				VoiceModel: config.VoiceModel(), Brain: "claude-cli sonnet", EmbedModel: "none",
				MeetingsEnabled: false, CaptureEnabled: false, KeepAudioDays: -1,
				DaemonStarted: "2026-09-04T08:00:00Z", Version: "dev", FirstRun: noFirstRunSteps(), ClaudeUsageFromLogin: true,
			},
		},
		{
			name:            "claude-cli brain with no pinned model falls back to the CLI name alone",
			cfg:             config.JuneConfig{Brain: config.BrainConfig{Provider: config.BrainClaudeCLI}},
			meetingsEnabled: true,
			paused:          false,
			want: SettingsView{
				DataDir: dataDir, StoreBytes: 120, RecordingsBytes: 500, ModelsBytes: 0,
				VoiceModel: config.VoiceModel(), Brain: config.BrainClaudeCLI, EmbedModel: "none",
				MeetingsEnabled: true, CaptureEnabled: true, KeepAudioDays: -1,
				DaemonStarted: "2026-09-04T08:00:00Z", Version: "dev", FirstRun: noFirstRunSteps(), ClaudeUsageFromLogin: true,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paused := c.paused
			cfg := c.cfg
			srv := httptest.NewServer(Settings(dataDir, NewLiveConfig(&cfg, noopSave), c.meetingsEnabled, func() bool { return paused }, startedAt))
			defer srv.Close()

			var got SettingsView
			getJSON(t, srv, "/", &got)
			// The hotkey is whatever the platform answers (Windows always reports the chord its window registers), and TestSettings_Hotkey covers it.
			c.want.Hotkey = windowHotkey()
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Settings() = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestSettings_PostClaudeUsageFromLoginPersists checks POST {"claude_usage_from_login": false} comes back false on the same response, is written to the on-disk config, and is read back correctly by a fresh LoadConfig — the point being that turning off the undocumented Claude usage fetch survives a daemon restart, the same round trip TestBrainsPostPersists checks for the brain picker.
func TestSettings_PostClaudeUsageFromLoginPersists(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	withFakeGsettings(t, noCustomKeybindings)
	cfg := &config.JuneConfig{}
	srv := httptest.NewServer(Settings(t.TempDir(), NewLiveConfig(cfg, config.SaveConfig), false, nil, time.Now()))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"claude_usage_from_login":false}`))
	if err != nil {
		t.Fatalf("POST /settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /settings = %d, want 200", resp.StatusCode)
	}
	var got SettingsView
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ClaudeUsageFromLogin {
		t.Errorf("claude_usage_from_login in the response = true, want false")
	}

	reloaded := config.LoadConfig()
	if reloaded.ClaudeUsageFromLoginEnabled() {
		t.Errorf("claude_usage_from_login on disk is still enabled after turning it off")
	}
}
