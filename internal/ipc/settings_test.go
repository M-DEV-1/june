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
	"sync"
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

// TestSettings_PostEmptyBodyLeavesClaudeUsageUnchanged checks a POST that names no setting changes none. The window sends the whole form back on any Settings change, so a body that happens to carry no claude_usage_from_login field must not read as "turn it off".
func TestSettings_PostEmptyBodyLeavesClaudeUsageUnchanged(t *testing.T) {
	withFakeGsettings(t, noCustomKeybindings)
	withEmptyFirstRun(t)
	on := true
	cfg := &config.JuneConfig{ClaudeUsageFromLogin: &on}
	saves := 0
	save := func(config.JuneConfig) error { saves++; return nil }
	srv := httptest.NewServer(Settings(t.TempDir(), NewLiveConfig(cfg, save), false, nil, time.Now()))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /settings: %v", err)
	}
	defer resp.Body.Close()
	var got SettingsView
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.ClaudeUsageFromLogin {
		t.Errorf("claude_usage_from_login = false after a POST that did not mention it, want true")
	}
	if !cfg.ClaudeUsageFromLoginEnabled() {
		t.Errorf("the config itself was turned off by a POST that did not mention the setting")
	}
	if saves != 0 {
		t.Errorf("save was called %d times for a POST that changed nothing, want 0", saves)
	}
}

// TestSettings_ConcurrentPostAndClaudeUsageRead is the -race check on the config the request goroutines share: POST /settings writes ClaudeUsageFromLogin while GET /brains reads it through the same accessor (brainLimitsFrom in cmd/daemon.go), each on its own goroutine, so both sides have to go through the lock.
func TestSettings_ConcurrentPostAndClaudeUsageRead(t *testing.T) {
	withFakeGsettings(t, noCustomKeybindings)
	withEmptyFirstRun(t)
	live := NewLiveConfig(&config.JuneConfig{}, noopSave)
	srv := httptest.NewServer(Settings(t.TempDir(), live, false, nil, time.Now()))
	defer srv.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"claude_usage_from_login":%t}`, i%2 == 0)
			resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
			if err != nil {
				t.Errorf("POST /settings: %v", err)
				return
			}
			resp.Body.Close()
		}(i)
		go func() {
			defer wg.Done()
			// What brainLimitsFrom reads on every GET /brains and GET /usage, plus the whole-struct copy GET /settings takes.
			_ = live.ClaudeUsageEnabled()
			_ = live.Get()
		}()
	}
	wg.Wait()
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
		return "'/usr/bin/june-window-toggle'", nil
	case strings.HasSuffix(schema, "custom1/") && key == "binding":
		return "'<Control><Alt>space'", nil
	}
	return "", fmt.Errorf("unexpected schema/key %s/%s", schema, key)
}

// TestSettings_Hotkey covers GET /settings' Hotkey field over every gsettings scenario it must
// resolve without failing the whole settings read: the real binding picked out from among other
// keybindings, an empty custom-keybindings list, gsettings itself unavailable, and — on top of any
// of those — never looked up at all off Linux.
func TestSettings_Hotkey(t *testing.T) {
	cases := []struct {
		name      string
		gsettings func(args ...string) (string, error)
		goos      string
		want      string
	}{
		{"found among others", fakeGsettingsWithHotkey, "linux", "<Control><Alt>space"},
		{"none configured", noCustomKeybindings, "linux", ""},
		{"gsettings unavailable", func(args ...string) (string, error) {
			return "", fmt.Errorf("gsettings: command not found")
		}, "linux", ""},
		{"never looked up off Linux", fakeGsettingsWithHotkey, "darwin", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFakeGsettings(t, tc.gsettings)
			prevGOOS := hotkeyGOOS
			hotkeyGOOS = tc.goos
			t.Cleanup(func() { hotkeyGOOS = prevGOOS })

			srv := httptest.NewServer(Settings(t.TempDir(), NewLiveConfig(&config.JuneConfig{}, noopSave), false, nil, time.Now()))
			defer srv.Close()

			var got SettingsView
			getJSON(t, srv, "/", &got)
			if got.Hotkey != tc.want {
				t.Errorf("Hotkey = %q, want %q", got.Hotkey, tc.want)
			}
		})
	}
}

// TestFirstRun covers firstRun() over every combination of what is set up: nothing at all (every
// field false, and a step listed for each), and each of the four ways on its own (June needs only
// one of them to answer text, not all of them, so whichever one is set up the steps list is empty).
func TestFirstRun(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, home string) config.JuneConfig
		check func(t *testing.T, got FirstRunView)
	}{
		{name: "nothing set up", setup: func(t *testing.T, home string) config.JuneConfig {
			t.Setenv("GEMINI_API_KEY", "")
			return config.JuneConfig{}
		}, check: func(t *testing.T, got FirstRunView) {
			if got.GeminiKey || got.CodexLogin || got.ClaudeCLI || got.LocalModel {
				t.Errorf("firstRun() = %+v, want every field false", got)
			}
			if len(got.Steps) == 0 {
				t.Error("firstRun() with nothing set up returned no steps")
			}
		}},
		{name: "gemini key", setup: func(t *testing.T, home string) config.JuneConfig {
			t.Setenv("GEMINI_API_KEY", "sk-test")
			return config.JuneConfig{}
		}},
		{name: "codex login", setup: func(t *testing.T, home string) config.JuneConfig {
			t.Setenv("GEMINI_API_KEY", "")
			mustWriteFile(t, filepath.Join(home, ".codex", "auth.json"), `{"auth_mode":"chatgpt"}`)
			return config.JuneConfig{}
		}},
		{name: "claude cli", setup: func(t *testing.T, home string) config.JuneConfig {
			t.Setenv("GEMINI_API_KEY", "")
			mustWriteFile(t, filepath.Join(home, ".claude", ".credentials.json"), `{}`)
			return config.JuneConfig{}
		}},
		{name: "local model", setup: func(t *testing.T, home string) config.JuneConfig {
			t.Setenv("GEMINI_API_KEY", "")
			return config.JuneConfig{LocalText: config.LocalTextConfig{ModelPath: "/models/local.gguf"}}
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			cfg := c.setup(t, home)
			got := firstRun(cfg, home)

			if c.check != nil {
				c.check(t, got)
				return
			}
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
