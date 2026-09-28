// settings.go holds GET /settings: the desktop window's Settings screen, backed by real values only — no field this handler cannot compute from the running config, the daemon's own state, or the data directory on disk gets a row.
package ipc

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"ora/internal/agent"
	"ora/internal/config"
	"ora/internal/util"
)

// noVersion is reported when the build carries no injected version string (see cmd/root.go's buildIdentity: there is no ldflags version injection in this repo).
const noVersion = "dev"

// keepAudioNotConfigured is reported for keep_audio_days when the config has no such field yet, rather than inventing a retention policy that does not exist.
const keepAudioNotConfigured = -1

// noEmbedModel is reported for embed_model when no local embedder is configured: hybrid search then runs lexical-only, with no embedding model backing it at all.
const noEmbedModel = "none"

// SettingsView is GET /settings: the daemon's configuration and on-disk footprint. Every field is present; a value the daemon cannot back is a documented sentinel (KeepAudioDays -1, EmbedModel "none") rather than an invented number.
type SettingsView struct {
	DataDir         string       `json:"data_dir"`
	StoreBytes      int64        `json:"store_bytes"`
	RecordingsBytes int64        `json:"recordings_bytes"`
	ModelsBytes     int64        `json:"models_bytes"`
	VoiceModel      string       `json:"voice_model"`
	Brain           string       `json:"brain"`
	EmbedModel      string       `json:"embed_model"`
	MeetingsEnabled bool         `json:"meetings_enabled"`
	CaptureEnabled  bool         `json:"capture_enabled"`
	KeepAudioDays   int          `json:"keep_audio_days"`
	DaemonStarted   string       `json:"daemon_started"`
	Version         string       `json:"version"`
	Hotkey          string       `json:"hotkey"`
	FirstRun        FirstRunView `json:"first_run"`
	// ClaudeUsageFromLogin mirrors config.OraConfig.ClaudeUsageFromLoginEnabled: whether GET /brains is allowed to read the Claude row's usage bars from the undocumented Anthropic endpoint. POST /settings with this field set writes it back to the config.
	ClaudeUsageFromLogin bool `json:"claude_usage_from_login"`
}

// FirstRunView is SettingsView's "first_run" field: which of the ways Ora can answer text are already set up on this machine, and — only when none of them are — plain one-line steps to fix that. Every signal is read live off the machine (an env var, a login file, the config) rather than a stored "setup complete" flag, so a first run that was interrupted keeps asking.
type FirstRunView struct {
	GeminiKey  bool     `json:"gemini_key"`
	CodexLogin bool     `json:"codex_login"`
	ClaudeCLI  bool     `json:"claude_cli"`
	LocalModel bool     `json:"local_model"`
	Steps      []string `json:"steps"`
}

// firstRun builds FirstRunView for cfg, reading login files under home (os.UserHomeDir() at the call site; a parameter here so a test can point it at a temporary directory instead of this machine's real one — the same pattern brainList uses).
func firstRun(cfg config.OraConfig, home string) FirstRunView {
	v := FirstRunView{
		GeminiKey:  os.Getenv("GEMINI_API_KEY") != "",
		CodexLogin: util.Exists(agent.CodexAuthPath(home)),
		ClaudeCLI:  util.Exists(agent.ClaudeCredentialsPath(home)),
		LocalModel: cfg.LocalText.Enabled(cfg),
	}
	v.Steps = firstRunSteps(v)
	return v
}

// firstRunSteps names what is missing, one plain sentence per way of answering text, only when none of them works yet — Ora needs just one, not all four.
func firstRunSteps(v FirstRunView) []string {
	if v.GeminiKey || v.CodexLogin || v.ClaudeCLI || v.LocalModel {
		return []string{}
	}
	return []string{
		"Set GEMINI_API_KEY in " + filepath.Join(config.DataDir(), "env") + ".",
		"Or sign in with the Claude CLI: run claude login.",
		"Or sign in with the Codex CLI: run codex login.",
		"Or point ora-config.json at a local model.",
	}
}

// LiveConfig is the daemon's loaded config as the request goroutines see it: every read hands back a copy of the struct taken under the lock, and the one field a request can change is written under that same lock, so POST /settings and the GET /brains and GET /usage handlers reading the same setting are not touching one struct from several goroutines at once. ponytail: one mutex over the whole config, not per-field — these are a handful of requests a minute.
type LiveConfig struct {
	mu   sync.Mutex
	cfg  *config.OraConfig
	save func(config.OraConfig) error
}

// NewLiveConfig wraps the daemon's config for use from request goroutines. Input: a pointer to the loaded config and the function that persists one to disk (config.SaveConfig in production, a stub in tests). Output: the accessor to hand Settings, and to read the config through anywhere else a request goroutine needs it.
func NewLiveConfig(cfg *config.OraConfig, save func(config.OraConfig) error) *LiveConfig {
	return &LiveConfig{cfg: cfg, save: save}
}

// Get returns a copy of the config taken under the lock, so the caller reads a consistent struct rather than one another request may be part-way through writing. Input: none. Output: the config by value.
func (c *LiveConfig) Get() config.OraConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.cfg
}

// ClaudeUsageEnabled reports whether the Claude row's usage bars may be read from the Anthropic usage endpoint. Input: none. Output: the config's ClaudeUsageFromLogin, read under the lock, which is on when the field was never set.
func (c *LiveConfig) ClaudeUsageEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.ClaudeUsageFromLoginEnabled()
}

// SetClaudeUsage writes ClaudeUsageFromLogin and persists the whole config, both under the lock, so no reader sees the struct between the write and the copy that goes to disk. Input: the new value. Output: whatever the save function returned.
func (c *LiveConfig) SetClaudeUsage(on bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.ClaudeUsageFromLogin = &on
	return c.save(*c.cfg)
}

// Settings builds the /settings handler. GET answers SettingsView as JSON, read off the live config. POST {"claude_usage_from_login": bool} writes that one setting, persists it so it survives a restart, and answers with the same view GET would — the same GET-plus-POST-on-one-route shape as /brains. A POST body that does not carry the field changes nothing: the window sends the whole form back on any change, and a missing field means "not mentioned", not "off". Input: the data directory to walk for disk usage, the config accessor shared with the rest of the daemon so a POST's change is visible everywhere and no two request goroutines touch the struct at once, whether the meeting watcher is running, a func reporting whether capture is currently paused (read at request time so a live /pause toggle is reflected immediately), and the time the daemon started. Output: the handler.
func Settings(dataDir string, cfg *LiveConfig, meetingsEnabled bool, capturePaused func() bool, startedAt time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeSettings(w, dataDir, cfg.Get(), meetingsEnabled, capturePaused, startedAt)
		case http.MethodPost:
			var req struct {
				ClaudeUsageFromLogin *bool `json:"claude_usage_from_login"`
			}
			if !DecodeJSON(w, r, &req) {
				return
			}
			if req.ClaudeUsageFromLogin != nil {
				if err := cfg.SetClaudeUsage(*req.ClaudeUsageFromLogin); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			writeSettings(w, dataDir, cfg.Get(), meetingsEnabled, capturePaused, startedAt)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// writeSettings writes SettingsView as JSON, the body both GET and POST /settings answer with.
func writeSettings(w http.ResponseWriter, dataDir string, cfg config.OraConfig, meetingsEnabled bool, capturePaused func() bool, startedAt time.Time) {
	capture := true
	if capturePaused != nil {
		capture = !capturePaused()
	}
	home, _ := os.UserHomeDir()
	util.WriteJSON(w, SettingsView{
		DataDir:              dataDir,
		StoreBytes:           storeBytes(dataDir),
		RecordingsBytes:      dirBytes(filepath.Join(dataDir, "recordings")),
		ModelsBytes:          dirBytes(filepath.Join(dataDir, "models")),
		VoiceModel:           config.VoiceModel(),
		Brain:                describeBrain(cfg.Brain),
		EmbedModel:           embedModelName(cfg.Embed),
		MeetingsEnabled:      meetingsEnabled,
		CaptureEnabled:       capture,
		KeepAudioDays:        keepAudioNotConfigured,
		DaemonStarted:        startedAt.Format(time.RFC3339),
		Version:              noVersion,
		Hotkey:               windowHotkey(),
		FirstRun:             firstRun(cfg, home),
		ClaudeUsageFromLogin: cfg.ClaudeUsageFromLoginEnabled(),
	})
}

// hotkeyGOOS is runtime.GOOS, indirected so a test can exercise the non-Linux branch of windowHotkey on a Linux box.
var hotkeyGOOS = runtime.GOOS

// gsettingsTimeout bounds each gsettings call windowHotkey makes, so a wedged dconf backend cannot stall the /settings response.
const gsettingsTimeout = 2 * time.Second

// gsettingsRunner runs `gsettings <args...>` and returns its trimmed stdout, or an error. A package variable so the test can swap in a fake instead of touching the real desktop.
var gsettingsRunner = runGsettings

// runGsettings is gsettingsRunner's real implementation.
func runGsettings(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gsettingsTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gsettings", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// mediaKeysSchema is the GNOME schema custom keybindings are registered under.
const mediaKeysSchema = "org.gnome.settings-daemon.plugins.media-keys"

// customKeybindingSchemaPrefix, plus a keybinding's own D-Bus path, names the per-keybinding schema its command and binding live under.
const customKeybindingSchemaPrefix = mediaKeysSchema + ".custom-keybinding:"

// windowToggleCommand is the command the ora-window-hotkey wiring registers, which marks a custom keybinding as ours among however many others GNOME holds.
const windowToggleCommand = "ora-window-toggle"

// windowHotkey reads the GNOME accelerator that opens the window, live off gsettings. Output: the binding string (for example "<Control><Alt>space"), or "" when no keybinding runs ora-window-toggle, gsettings is unavailable, or the platform is not Linux — GNOME's custom-keybindings mechanism is what internal/window wires the hotkey through, and there is nothing else to fall back to.
func windowHotkey() string {
	if hotkeyGOOS != "linux" {
		return ""
	}
	list, err := gsettingsRunner("get", mediaKeysSchema, "custom-keybindings")
	if err != nil {
		return ""
	}
	for _, path := range gvariantStrings(list) {
		schema := customKeybindingSchemaPrefix + path
		command, err := gsettingsRunner("get", schema, "command")
		if err != nil || !strings.Contains(command, windowToggleCommand) {
			continue
		}
		binding, err := gsettingsRunner("get", schema, "binding")
		if err != nil {
			return ""
		}
		return gvariantString(binding)
	}
	return ""
}

// gvariantStrings splits a GVariant string-array literal, as gsettings prints one, into its elements. Input: a literal like "['a', 'b']" or the empty array's "@as []" / "[]". Output: the quoted elements, unquoted. ponytail: plain split-and-trim, not a real GVariant parser — every value gsettings hands back here is a D-Bus object path, which never itself contains a comma or a quote, so this holds; a command or binding string with an embedded comma would break it.
func gvariantStrings(literal string) []string {
	literal = strings.TrimSpace(literal)
	literal = strings.TrimPrefix(literal, "@as")
	literal = strings.TrimSpace(literal)
	literal = strings.TrimPrefix(literal, "[")
	literal = strings.TrimSuffix(literal, "]")
	literal = strings.TrimSpace(literal)
	if literal == "" {
		return nil
	}
	parts := strings.Split(literal, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, gvariantString(p))
	}
	return out
}

// gvariantString strips the single quotes gsettings wraps a string value in. Input: a literal like "'ora-window-toggle'". Output: the value alone, or the input unchanged if it carries no quotes to strip.
func gvariantString(literal string) string {
	return strings.Trim(strings.TrimSpace(literal), "'")
}

// storeBytes sums the size of the sqlite database and its WAL/shm side files directly under dataDir. Input: the data directory. Output: the total bytes, 0 for any file that does not exist.
func storeBytes(dataDir string) int64 {
	var total int64
	for _, name := range []string{"db", "db-wal", "db-shm"} {
		total += fileBytes(filepath.Join(dataDir, name))
	}
	return total
}

// fileBytes is the size of the file at path, or 0 if it cannot be stat'd (missing, or any other error).
func fileBytes(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// dirBytes sums the size of every regular file under dir, recursively. Input: a directory path. Output: the total bytes, 0 when dir does not exist or cannot be read.
func dirBytes(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A missing root, or one unreadable entry, contributes nothing rather than failing the whole walk.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// describeBrain names the backend that answers Ora's one-shot text duties. Input: the brain config. Output: "<cli> <model>" for one of the CLI-backed providers (or just the CLI name when no model is pinned), otherwise the Gemini model name that will actually be called (the configured one, or config.TextModel when none is set).
func describeBrain(cfg config.BrainConfig) string {
	switch cfg.Provider {
	case config.BrainClaudeCLI, config.BrainAgyCLI, config.BrainGrokCLI:
		if cfg.Model == "" {
			return cfg.Provider
		}
		return cfg.Provider + " " + cfg.Model
	default:
		if cfg.Model != "" {
			return cfg.Model
		}
		return config.TextModel
	}
}

// embedModelName names the model backing hybrid search's semantic half. Input: the embed config. Output: config.LocalEmbedModel when a local embedder is configured, or noEmbedModel when hybrid search has no embedder at all and runs lexical-only.
func embedModelName(cfg config.EmbedConfig) string {
	if cfg.LocalEnabled() {
		return config.LocalEmbedModel
	}
	return noEmbedModel
}
