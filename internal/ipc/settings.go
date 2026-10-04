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

	"june/internal/agent"
	"june/internal/config"
	"june/internal/util"
)

// keepAudioNotConfigured is reported for keep_audio_days when the config has no such field yet, rather than inventing a retention policy that does not exist.
const keepAudioNotConfigured = -1

// noEmbedModel is reported for embed_model when no local embedder is configured: hybrid search then runs lexical-only, with no embedding model backing it at all.
const noEmbedModel = "none"

// noVoiceKey is reported for voice_model when there is no Gemini API key: the Live model is the only thing a voice session can talk through, and without a key it cannot be dialled at all, so naming it said June could talk when it could not.
const noVoiceKey = "off — no Gemini API key"

// noBrainSignedIn is reported for brain when no brain is picked and the router has nothing to answer on: no Gemini key and no subscription signed in.
const noBrainSignedIn = "nothing — no brain is signed in"

// SettingsView is GET /settings: the daemon's configuration and on-disk footprint. Every field is present; a value the daemon cannot back is a documented sentinel (KeepAudioDays -1, EmbedModel "none", VoiceModel noVoiceKey, Brain noBrainSignedIn) rather than an invented number.
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
	// ClaudeUsageFromLogin mirrors config.JuneConfig.ClaudeUsageFromLoginEnabled: whether GET /brains is allowed to read the Claude row's usage bars from the undocumented Anthropic endpoint. POST /settings with this field set writes it back to the config.
	ClaudeUsageFromLogin bool `json:"claude_usage_from_login"`
	// UpdateCheck mirrors config.JuneConfig.UpdateCheckEnabled: whether June looks for a new release once a day. The updater reads the file before each check, so a POST holds from the next one.
	UpdateCheck bool `json:"update_check"`
	// AllowFallback mirrors config.BrainConfig.FallbackAllowed: whether background work may go to another signed-in brain when the one it was meant for fails.
	AllowFallback bool `json:"allow_fallback"`
	// MeetingsOffer is "ask" when June offers to record a call it notices and "off" when it says nothing (config.MeetingsConfig.Offer). The meeting watcher reads the file again each time something takes the microphone, so "off" holds at once, and so does "ask" while meetings_enabled is true. meetings_enabled false means no watcher was started with this daemon, so "ask" then holds from the next start.
	MeetingsOffer string `json:"meetings_offer"`
}

// The two words meetings_offer takes.
const (
	meetingsOfferAsk = "ask"
	meetingsOfferOff = "off"
)

// FirstRunView is SettingsView's "first_run" field: which of the ways June can answer text are already set up on this machine, and — only when none of them are — plain one-line steps to fix that. Every signal is read live off the machine (an env var, a login file, the config) rather than a stored "setup complete" flag, so a first run that was interrupted keeps asking.
type FirstRunView struct {
	GeminiKey  bool     `json:"gemini_key"`
	CodexLogin bool     `json:"codex_login"`
	ClaudeCLI  bool     `json:"claude_cli"`
	LocalModel bool     `json:"local_model"`
	Steps      []string `json:"steps"`
}

// firstRun builds FirstRunView for cfg, reading login files under home (os.UserHomeDir() at the call site; a parameter here so a test can point it at a temporary directory instead of this machine's real one — the same pattern brainList uses).
func firstRun(cfg config.JuneConfig, home string) FirstRunView {
	v := FirstRunView{
		GeminiKey:  os.Getenv("GEMINI_API_KEY") != "",
		CodexLogin: util.Exists(agent.CodexAuthPath(home)),
		ClaudeCLI:  loggedIn("claude", home, onPath),
		LocalModel: cfg.LocalText.Enabled(cfg),
	}
	v.Steps = firstRunSteps(v)
	return v
}

// firstRunSteps names what is missing, one plain sentence per way of answering text, only when none of them works yet — June needs just one, not all four. Each names a place in the window or an app the person already has, never a file or a command.
func firstRunSteps(v FirstRunView) []string {
	if v.GeminiKey || v.CodexLogin || v.ClaudeCLI || v.LocalModel {
		return []string{}
	}
	return []string{
		"Add a free Gemini key in Settings → Brain.",
		"Or sign in to Claude Code on this computer.",
		"Or sign in to the Codex app with your ChatGPT account.",
		"Or set up On-device summaries in Settings → Local features.",
	}
}

// LiveConfig is the daemon's loaded config as the request goroutines see it: every read hands back a copy of the struct taken under the lock, and the one field a request can change is written under that same lock, so POST /settings and the GET /brains and GET /usage handlers reading the same setting are not touching one struct from several goroutines at once. ponytail: one mutex over the whole config, not per-field — these are a handful of requests a minute.
type LiveConfig struct {
	mu   sync.Mutex
	cfg  *config.JuneConfig
	read func() (config.JuneConfig, error)
	save func(config.JuneConfig) error
}

// NewLiveConfig wraps the daemon's config for use from request goroutines. Input: a pointer to the loaded config, the function that reads the file as it is now (config.ReadConfig in production; nil in a test, which makes the daemon's own copy the base every change is made to), and the function that persists one to disk (config.SaveConfig in production, a stub in tests). The two are handed in together so a change is always read from the same file it is written to. Output: the accessor to hand Settings, and to read the config through anywhere else a request goroutine needs it.
func NewLiveConfig(cfg *config.JuneConfig, read func() (config.JuneConfig, error), save func(config.JuneConfig) error) *LiveConfig {
	return &LiveConfig{cfg: cfg, read: read, save: save}
}

// Get returns a copy of the config taken under the lock, so the caller reads a consistent struct rather than one another request may be part-way through writing. Input: none. Output: the config by value.
func (c *LiveConfig) Get() config.JuneConfig {
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

// Settings builds the /settings handler. GET answers SettingsView as JSON, read off the live config. POST {"claude_usage_from_login": bool, "update_check": bool, "allow_fallback": bool, "meetings_offer": "ask"|"off"} writes the fields it carries, persists them in one save so they survive a restart, and answers with the same view GET would — the same GET-plus-POST-on-one-route shape as /brains. A field the body does not carry changes nothing: the window sends the whole form back on any change, and a missing field means "not mentioned", not "off". A meetings_offer other than the two words is 400 and changes nothing. Input: the data directory to walk for disk usage, the config accessor shared with the rest of the daemon so a POST's change is visible everywhere and no two request goroutines touch the struct at once, whether the meeting watcher is running, a func reporting whether capture is currently paused (read at request time so a live /pause toggle is reflected immediately), the time the daemon started, and the usage lookup GET /brains reads (nil for none), so the brain line counts a login its provider refused as signed out exactly as the picker does. Output: the handler.
func Settings(dataDir string, cfg *LiveConfig, meetingsEnabled bool, capturePaused func() bool, startedAt time.Time, limitsFor BrainLimits) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeSettings(r.Context(), w, dataDir, cfg.Get(), meetingsEnabled, capturePaused, startedAt, limitsFor)
		case http.MethodPost:
			var req struct {
				ClaudeUsageFromLogin *bool   `json:"claude_usage_from_login"`
				UpdateCheck          *bool   `json:"update_check"`
				AllowFallback        *bool   `json:"allow_fallback"`
				MeetingsOffer        *string `json:"meetings_offer"`
			}
			if !DecodeJSON(w, r, &req) {
				return
			}
			var offer *bool
			if req.MeetingsOffer != nil {
				switch *req.MeetingsOffer {
				case meetingsOfferAsk:
					offer = new(bool)
					*offer = true
				case meetingsOfferOff:
					offer = new(bool)
				default:
					http.Error(w, `meetings_offer must be "ask" or "off"`, http.StatusBadRequest)
					return
				}
			}
			if req.ClaudeUsageFromLogin != nil || req.UpdateCheck != nil || req.AllowFallback != nil || offer != nil {
				// One Update for all of them, so a form sent back whole is one save rather than one per field. The pointers are the request's own and never written again, so the file's copy and the daemon's sharing them is safe.
				err := cfg.Update(func(c *config.JuneConfig) {
					if req.ClaudeUsageFromLogin != nil {
						c.ClaudeUsageFromLogin = req.ClaudeUsageFromLogin
					}
					if req.UpdateCheck != nil {
						c.UpdateCheck = req.UpdateCheck
					}
					if req.AllowFallback != nil {
						c.Brain.AllowFallback = req.AllowFallback
					}
					if offer != nil {
						c.Meetings.Offer = offer
					}
				})
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			writeSettings(r.Context(), w, dataDir, cfg.Get(), meetingsEnabled, capturePaused, startedAt, limitsFor)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// writeSettings writes SettingsView as JSON, the body both GET and POST /settings answer with.
func writeSettings(ctx context.Context, w http.ResponseWriter, dataDir string, cfg config.JuneConfig, meetingsEnabled bool, capturePaused func() bool, startedAt time.Time, limitsFor BrainLimits) {
	capture := true
	if capturePaused != nil {
		capture = !capturePaused()
	}
	home, _ := os.UserHomeDir()
	voiceModel := config.VoiceModel()
	if os.Getenv("GEMINI_API_KEY") == "" {
		voiceModel = noVoiceKey
	}
	// Worked out only while no brain is picked: each brain checked can reach its provider (Codex's login check, Claude's usage endpoint), and with a brain picked the answer is never shown.
	auto := ""
	if cfg.Brain.Provider == "" {
		auto = automaticBrainID(func(id string) bool { return brainSignedIn(ctx, id, home, onPath, limitsFor) })
	}
	util.WriteJSON(w, SettingsView{
		DataDir:              dataDir,
		StoreBytes:           storeBytes(dataDir),
		RecordingsBytes:      dirBytes(filepath.Join(dataDir, "recordings")),
		ModelsBytes:          dirBytes(filepath.Join(dataDir, "models")),
		VoiceModel:           voiceModel,
		Brain:                describeBrain(cfg, auto),
		EmbedModel:           embedModelName(cfg.Embed),
		MeetingsEnabled:      meetingsEnabled,
		CaptureEnabled:       capture,
		KeepAudioDays:        keepAudioNotConfigured,
		DaemonStarted:        startedAt.Format(time.RFC3339),
		Version:              config.Version,
		Hotkey:               windowHotkey(),
		FirstRun:             firstRun(cfg, home),
		ClaudeUsageFromLogin: cfg.ClaudeUsageFromLoginEnabled(),
		UpdateCheck:          cfg.UpdateCheckEnabled(),
		AllowFallback:        cfg.Brain.FallbackAllowed(),
		MeetingsOffer:        meetingsOffer(cfg.Meetings),
	})
}

// meetingsOffer is meetings_offer's word for the config's offer switch.
func meetingsOffer(m config.MeetingsConfig) string {
	if m.OfferEnabled() {
		return meetingsOfferAsk
	}
	return meetingsOfferOff
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

// windowToggleCommand is the command the june-window-hotkey wiring registers, which marks a custom keybinding as ours among however many others GNOME holds.
const windowToggleCommand = "june-window-toggle"

// windowHotkey reads the GNOME accelerator that opens the window, live off gsettings. Output: the binding string (for example "<Control><Alt>space"), or "" when no keybinding runs june-window-toggle, gsettings is unavailable, or the platform is neither Linux nor Windows (Windows always answers the chord the window registers) — GNOME's custom-keybindings mechanism is what internal/window wires the hotkey through, and there is nothing else to fall back to.
func windowHotkey() string {
	// The Windows window registers this chord itself (app/src-tauri/src/lib.rs), so there is nothing to read.
	if hotkeyGOOS == "windows" {
		return "<Control><Alt>space"
	}
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

// gvariantString strips the single quotes gsettings wraps a string value in. Input: a literal like "'june-window-toggle'". Output: the value alone, or the input unchanged if it carries no quotes to strip.
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

// describeBrain names the backend that answers June's one-shot text duties. Input: the config, and the id of the brain the router answers on while none is picked (automaticBrainID, the same one GET /brains marks default; not read when one is). Output: the picked brain as describeProvider names it; with none picked, the brain the router lands on, marked "(automatic)", or noBrainSignedIn when it has nothing to land on. It used to name config.TextModel whenever no brain was picked, so a machine with no Gemini key read "gemini-3.5-flash" while every ask went to a subscription.
func describeBrain(cfg config.JuneConfig, auto string) string {
	if cfg.Brain.Provider != "" {
		return describeProvider(cfg.Brain)
	}
	if auto == "" {
		return noBrainSignedIn
	}
	// Gemini is called on the config's own model; every other brain is named with the model last picked for it, as its /brains row is.
	answering := config.BrainConfig{Provider: config.BrainGeminiAPI, Model: cfg.Brain.Model}
	if provider, ok := providerForBrainID(auto); ok && auto != "gemini" {
		answering = config.BrainConfig{Provider: provider, Model: cfg.BrainModels[auto]}
	}
	return describeProvider(answering) + " (automatic)"
}

// describeProvider names one configured backend. Input: a brain config with its provider set. Output: "<provider> <model>" for every provider but the Gemini API (or just the provider name when no model is pinned), and for the Gemini API the model name that will actually be called (the configured one, or config.TextModel when none is set).
func describeProvider(cfg config.BrainConfig) string {
	switch cfg.Provider {
	case config.BrainClaudeCLI, config.BrainAgyCLI, config.BrainGrokCLI, config.BrainCodex, config.BrainOllama:
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
