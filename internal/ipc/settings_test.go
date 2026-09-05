package ipc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ora/internal/config"
)

// withEmptyFirstRun points HOME at a fresh, empty temp directory and clears GEMINI_API_KEY, so a test that does not care about first-run detection gets the same "nothing set up" answer regardless of what is actually on this machine.
func withEmptyFirstRun(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GEMINI_API_KEY", "")
}

// noFirstRunSteps is the FirstRunView a config with no brain, no login files and no local model produces: nothing is set up, so every step is listed.
var noFirstRunSteps = FirstRunView{Steps: firstRunSteps(FirstRunView{})}

// noopSave is the persist function for a test that never posts a change and so never needs one to actually write anything.
func noopSave(config.OraConfig) error { return nil }

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
		cfg             config.OraConfig
		meetingsEnabled bool
		paused          bool
		want            SettingsView
	}{
		{
			name:            "gemini brain, local embedder, meetings and capture on",
			cfg:             config.OraConfig{Brain: config.BrainConfig{}, Embed: config.EmbedConfig{LlamaServer: "/usr/bin/llama-server", ModelPath: "/models/embed.gguf"}},
			meetingsEnabled: true,
			paused:          false,
			want: SettingsView{
				DataDir: dataDir, StoreBytes: 120, RecordingsBytes: 500, ModelsBytes: 0,
				VoiceModel: config.VoiceModel, Brain: config.TextModel, EmbedModel: config.LocalEmbedModel,
				MeetingsEnabled: true, CaptureEnabled: true, KeepAudioDays: -1,
				DaemonStarted: "2026-09-04T08:00:00Z", Version: "dev", FirstRun: noFirstRunSteps, ClaudeUsageFromLogin: true,
			},
		},
		{
			name:            "claude-cli brain with a pinned model, no local embedder, meetings and capture off",
			cfg:             config.OraConfig{Brain: config.BrainConfig{Provider: config.BrainClaudeCLI, Model: "sonnet"}},
			meetingsEnabled: false,
			paused:          true,
			want: SettingsView{
				DataDir: dataDir, StoreBytes: 120, RecordingsBytes: 500, ModelsBytes: 0,
				VoiceModel: config.VoiceModel, Brain: "claude-cli sonnet", EmbedModel: "none",
				MeetingsEnabled: false, CaptureEnabled: false, KeepAudioDays: -1,
				DaemonStarted: "2026-09-04T08:00:00Z", Version: "dev", FirstRun: noFirstRunSteps, ClaudeUsageFromLogin: true,
			},
		},
		{
			name:            "claude-cli brain with no pinned model falls back to the CLI name alone",
			cfg:             config.OraConfig{Brain: config.BrainConfig{Provider: config.BrainClaudeCLI}},
			meetingsEnabled: true,
			paused:          false,
			want: SettingsView{
				DataDir: dataDir, StoreBytes: 120, RecordingsBytes: 500, ModelsBytes: 0,
				VoiceModel: config.VoiceModel, Brain: config.BrainClaudeCLI, EmbedModel: "none",
				MeetingsEnabled: true, CaptureEnabled: true, KeepAudioDays: -1,
				DaemonStarted: "2026-09-04T08:00:00Z", Version: "dev", FirstRun: noFirstRunSteps, ClaudeUsageFromLogin: true,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paused := c.paused
			cfg := c.cfg
			srv := httptest.NewServer(Settings(dataDir, &cfg, noopSave, c.meetingsEnabled, func() bool { return paused }, startedAt))
			defer srv.Close()

			var got SettingsView
			getJSON(t, srv, "/", &got)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Settings() = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestSettings_MissingDataDirGivesZeroSizes(t *testing.T) {
	withFakeGsettings(t, noCustomKeybindings)
	dataDir := filepath.Join(t.TempDir(), "does-not-exist")
	srv := httptest.NewServer(Settings(dataDir, &config.OraConfig{}, noopSave, false, nil, time.Now()))
	defer srv.Close()

	var got SettingsView
	getJSON(t, srv, "/", &got)
	if got.StoreBytes != 0 || got.RecordingsBytes != 0 || got.ModelsBytes != 0 {
		t.Errorf("sizes over a missing data dir = %d/%d/%d, want all zero", got.StoreBytes, got.RecordingsBytes, got.ModelsBytes)
	}
	// capturePaused nil (no tracker wired) defaults to capture enabled, the same "nothing wired yet" default the rest of the daemon uses.
	if !got.CaptureEnabled {
		t.Errorf("CaptureEnabled with a nil capturePaused = false, want true")
	}
}

// TestSettings_ClaudeUsageFromLoginReflectsLiveConfig checks GET reads the flag off the pointer it was given, not a snapshot taken when the route was built — the same reason POST /brains is handed a pointer.
func TestSettings_ClaudeUsageFromLoginReflectsLiveConfig(t *testing.T) {
	withFakeGsettings(t, noCustomKeybindings)
	off := false
	cfg := &config.OraConfig{ClaudeUsageFromLogin: &off}
	srv := httptest.NewServer(Settings(t.TempDir(), cfg, noopSave, false, nil, time.Now()))
	defer srv.Close()

	var got SettingsView
	getJSON(t, srv, "/", &got)
	if got.ClaudeUsageFromLogin {
		t.Errorf("claude_usage_from_login = true, want false: the config had it explicitly turned off")
	}
}

// TestSettings_PostClaudeUsageFromLoginPersists checks POST {"claude_usage_from_login": false} comes back false on the same response, is written to the on-disk config, and is read back correctly by a fresh LoadConfig — the point being that turning off the undocumented Claude usage fetch survives a daemon restart, the same round trip TestBrainsPostPersists checks for the brain picker.
func TestSettings_PostClaudeUsageFromLoginPersists(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	withFakeGsettings(t, noCustomKeybindings)
	cfg := &config.OraConfig{}
	srv := httptest.NewServer(Settings(t.TempDir(), cfg, config.SaveConfig, false, nil, time.Now()))
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

// fakeGsettingsWithHotkey answers the exact three-call sequence windowHotkey makes when the machine has two custom keybindings, the second of which is ours, and returns the binding gsettings would actually print for it.
func fakeGsettingsWithHotkey(args ...string) (string, error) {
	if len(args) != 3 {
		return "", fmt.Errorf("unexpected args %v", args)
	}
	schema, key := args[1], args[2]
	switch {
	case schema == "org.gnome.settings-daemon.plugins.media-keys" && key == "custom-keybindings":
		return "['/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/custom0/', '/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/custom1/']", nil
	case strings.HasSuffix(schema, "custom0/") && key == "command":
		return "'firefox'", nil
	case strings.HasSuffix(schema, "custom0/") && key == "binding":
		return "'<Super>f'", nil
	case strings.HasSuffix(schema, "custom1/") && key == "command":
		return "'/usr/bin/ora-window-toggle'", nil
	case strings.HasSuffix(schema, "custom1/") && key == "binding":
		return "'<Control><Alt>space'", nil
	}
	return "", fmt.Errorf("unexpected schema/key %s/%s", schema, key)
}

// TestSettings_HotkeyFoundAmongOthers checks that the custom keybinding whose command names ora-window-toggle is picked out from among others, and its binding is unquoted.
func TestSettings_HotkeyFoundAmongOthers(t *testing.T) {
	withFakeGsettings(t, fakeGsettingsWithHotkey)
	srv := httptest.NewServer(Settings(t.TempDir(), &config.OraConfig{}, noopSave, false, nil, time.Now()))
	defer srv.Close()

	var got SettingsView
	getJSON(t, srv, "/", &got)
	if got.Hotkey != "<Control><Alt>space" {
		t.Errorf("Hotkey = %q, want the binding on the keybinding running ora-window-toggle", got.Hotkey)
	}
}

// TestSettings_HotkeyNoneConfigured checks that an empty custom-keybindings list reads as no hotkey rather than an error.
func TestSettings_HotkeyNoneConfigured(t *testing.T) {
	withFakeGsettings(t, noCustomKeybindings)
	srv := httptest.NewServer(Settings(t.TempDir(), &config.OraConfig{}, noopSave, false, nil, time.Now()))
	defer srv.Close()

	var got SettingsView
	getJSON(t, srv, "/", &got)
	if got.Hotkey != "" {
		t.Errorf("Hotkey = %q, want empty when nothing is bound", got.Hotkey)
	}
}

// TestSettings_HotkeyGsettingsUnavailable checks that a machine with no gsettings (or one that refuses the call) reads as no hotkey rather than failing the whole settings read.
func TestSettings_HotkeyGsettingsUnavailable(t *testing.T) {
	withFakeGsettings(t, func(args ...string) (string, error) {
		return "", fmt.Errorf("gsettings: command not found")
	})
	srv := httptest.NewServer(Settings(t.TempDir(), &config.OraConfig{}, noopSave, false, nil, time.Now()))
	defer srv.Close()

	var got SettingsView
	getJSON(t, srv, "/", &got)
	if got.Hotkey != "" {
		t.Errorf("Hotkey = %q, want empty when gsettings fails", got.Hotkey)
	}
}

// TestSettings_HotkeyNonLinux checks that the hotkey is never looked up off Linux, whatever the fake runner would have said.
func TestSettings_HotkeyNonLinux(t *testing.T) {
	withFakeGsettings(t, fakeGsettingsWithHotkey)
	prevGOOS := hotkeyGOOS
	hotkeyGOOS = "darwin"
	t.Cleanup(func() { hotkeyGOOS = prevGOOS })

	srv := httptest.NewServer(Settings(t.TempDir(), &config.OraConfig{}, noopSave, false, nil, time.Now()))
	defer srv.Close()

	var got SettingsView
	getJSON(t, srv, "/", &got)
	if got.Hotkey != "" {
		t.Errorf("Hotkey = %q, want empty off Linux", got.Hotkey)
	}
}

// TestFirstRun_NothingSetUpListsEveryStep checks that a machine with no login files, no GEMINI_API_KEY and no local model configured reports all four ways as false and lists a step for each.
func TestFirstRun_NothingSetUpListsEveryStep(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	home := t.TempDir()

	got := firstRun(config.OraConfig{}, home)

	if got.GeminiKey || got.CodexLogin || got.ClaudeCLI || got.LocalModel {
		t.Errorf("firstRun() = %+v, want every field false", got)
	}
	if len(got.Steps) == 0 {
		t.Error("firstRun() with nothing set up returned no steps")
	}
}

// TestFirstRun_AnyOneWayIsEnoughToClearTheSteps checks that Ora needs only one of the four ways to answer text, not all of them: whichever one is set up, the steps list is empty.
func TestFirstRun_AnyOneWayIsEnoughToClearTheSteps(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, home string) config.OraConfig
	}{
		{"gemini key", func(t *testing.T, home string) config.OraConfig {
			t.Setenv("GEMINI_API_KEY", "sk-test")
			return config.OraConfig{}
		}},
		{"codex login", func(t *testing.T, home string) config.OraConfig {
			t.Setenv("GEMINI_API_KEY", "")
			mustWriteFile(t, filepath.Join(home, ".codex", "auth.json"), `{"auth_mode":"chatgpt"}`)
			return config.OraConfig{}
		}},
		{"claude cli", func(t *testing.T, home string) config.OraConfig {
			t.Setenv("GEMINI_API_KEY", "")
			mustWriteFile(t, filepath.Join(home, ".claude", ".credentials.json"), `{}`)
			return config.OraConfig{}
		}},
		{"local model", func(t *testing.T, home string) config.OraConfig {
			t.Setenv("GEMINI_API_KEY", "")
			return config.OraConfig{LocalText: config.LocalTextConfig{ModelPath: "/models/local.gguf"}}
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			cfg := c.setup(t, home)

			got := firstRun(cfg, home)
			if len(got.Steps) != 0 {
				t.Errorf("firstRun() with %s set up = %+v, want no steps", c.name, got)
			}
		})
	}
}

// mustWriteFile writes data to path, creating parent directories as needed, failing the test on any error.
func mustWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
