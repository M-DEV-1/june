package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type OraConfig struct {
	Tracker TrackerConfig `json:"tracker"`
	// Voice is the Gemini Live prebuilt voice name used for the assistant's spoken output (see AvailableVoices).
	// Defaults to DefaultVoice when unset.
	Voice string `json:"voice"`
	// Autostart is whether the daemon should be launched when the user logs in. Defaults to false — an upgrading user must opt in to a screen-recording daemon starting at login, not get one installed silently — and the daemon reconciles the on-disk autostart entry to match this field on every startup.
	Autostart bool `json:"autostart"`
	// ProactiveAudio turns on the Gemini Live "proactive audio" feature, which lets the model stay silent when what the mic picked up wasn't addressed to it — a room conversation, a video, the user talking to someone else. Defaults to on; set it to false in ora-config.json to have the model answer everything it hears.
	// A pointer, not a plain bool, so a config file written before this field existed (no key at all) is distinguishable from one where the user explicitly turned it off. Use ProactiveAudioEnabled rather than reading it directly.
	ProactiveAudio *bool `json:"proactive_audio,omitempty"`
	// Embed selects which embedding engine backs hybrid search. Zero value means the Gemini API, as before.
	Embed EmbedConfig `json:"embed"`
	// Brain selects which backend answers the one-shot text duties. Zero value means the Gemini API, as before.
	Brain BrainConfig `json:"brain"`
	// Transcribe holds the settings for the whisper.cpp build that transcribes meetings.
	Transcribe TranscribeConfig `json:"transcribe"`
	// Proactive schedules the daily proactive seams: the morning brief and the evening diary close.
	Proactive ProactiveConfig `json:"proactive"`
	// Dream schedules the overnight dreaming loop. Zero value means the defaults; a negative hour disables dreaming.
	Dream DreamConfig `json:"dream"`
}

// DreamConfig sets when the overnight dreaming loop may run, and reserves the knobs the later local-model slice will need. ModelPath and ShutdownCommand are read by nothing yet and Port is bound by nothing yet — reserved now so a config written today survives the slice that uses them.
type DreamConfig struct {
	// Hour is the local hour the dreaming window opens; the window closes at the morning brief hour. Zero means DefaultDreamHour, negative disables dreaming.
	Hour int `json:"hour"`
	// ModelPath is the GGUF the future local dreaming model loads. Unused this slice.
	ModelPath string `json:"model_path"`
	// Port is the loopback port reserved for the future local dreaming model server.
	Port int `json:"port"`
	// ShutdownCommand is what a finished night may run to power the machine down. Unused this slice.
	ShutdownCommand string `json:"shutdown_command"`
	// Brain overrides which backend dreams. An empty provider means the main Brain block — but a dream brain that is not claude-cli is exempt from the Claude window curfew, which is the point of setting one: the other paid CLIs can dream all night without touching the user's Claude usage windows.
	Brain BrainConfig `json:"brain"`
	// Device is the llama.cpp Vulkan device name for the shadow model (e.g. "Vulkan1"); empty means let llama-server choose.
	Device string `json:"device"`
}

// DefaultDreamHour is the local hour the dreaming window opens when the config leaves it zero.
const DefaultDreamHour = 23

// DefaultDreamPort continues the daemon's port run (6942 IPC, 6943 embeddings) with the next one up.
const DefaultDreamPort = 6944

// DreamHour returns the effective local hour the dreaming window opens: zero becomes DefaultDreamHour, a negative value passes through and means dreaming is disabled.
func (d DreamConfig) DreamHour() int {
	if d.Hour == 0 {
		return DefaultDreamHour
	}
	return d.Hour
}

// DreamPort returns the reserved local-model port, defaulting to DefaultDreamPort.
func (d DreamConfig) DreamPort() int {
	if d.Port <= 0 {
		return DefaultDreamPort
	}
	return d.Port
}

// ProactiveConfig sets the local hours after which the two daily proactive seams may run. A zero hour means the default (a config written before this block existed keeps working), and a negative hour disables that seam entirely.
type ProactiveConfig struct {
	// BriefHour is the local hour after which the morning brief waits for the user's first activity.
	BriefHour int `json:"brief_hour"`
	// CloseHour is the local hour after which the evening close writes the day's diary entry.
	CloseHour int `json:"close_hour"`
}

// Default local hours for the proactive seams, used when the config leaves them zero.
const (
	DefaultBriefHour = 9
	DefaultCloseHour = 22
)

// Hours returns the effective brief and close hours: zero fields become the defaults, negative values pass through unchanged and mean the seam is disabled.
func (p ProactiveConfig) Hours() (brief, close int) {
	brief, close = p.BriefHour, p.CloseHour
	if brief == 0 {
		brief = DefaultBriefHour
	}
	if close == 0 {
		close = DefaultCloseHour
	}
	return brief, close
}

// TranscribeConfig holds the settings for the whisper.cpp build that turns a meeting's audio into text.
type TranscribeConfig struct {
	// GPUDevice is which GPU whisper.cpp should decode on, numbered as whisper.cpp numbers the Vulkan devices it finds. Zero, the default, leaves the choice to whisper.cpp, which takes the first device it sees — on a laptop with both integrated and discrete graphics that is usually the slower of the two, so this normally wants setting.
	GPUDevice int `json:"gpu_device"`
}

// BrainConfig chooses which backend answers ORA's one-shot text duties — the meeting minutes and the personal context updater. The zero value is the Gemini API on TextModel, which is what ORA did before this block existed, so a config file written without it behaves exactly as it always has.
// The voice assistant is not covered by this: that is a Gemini Live session, not a one-shot call.
type BrainConfig struct {
	// Provider is BrainGeminiAPI (the default) or BrainClaudeCLI to run `claude -p` under whatever Claude Code login the machine already has. Anything else falls back to the Gemini API.
	Provider string `json:"provider"`
	// Model names the model each provider should use: a Gemini model name (defaulting to TextModel), or for claude-cli a model alias passed as --model ("sonnet", "opus"); empty means the login's default.
	Model string `json:"model"`
	// Binary is the path to the claude CLI to run. Empty means look "claude" up on PATH.
	Binary string `json:"binary"`
	// TimeoutSeconds is the hard limit on a single CLI run, after which the child is killed and the call fails. Defaults to DefaultBrainTimeoutSeconds. The Gemini provider ignores it and is bounded by the caller's context.
	TimeoutSeconds int `json:"timeout_seconds"`
}

// The provider names accepted in BrainConfig.Provider.
const (
	BrainGeminiAPI = "gemini-api"
	BrainClaudeCLI = "claude-cli"
	BrainAgyCLI    = "agy-cli"
	BrainGrokCLI   = "grok-cli"
)

// DefaultBrainTimeoutSeconds caps one CLI run. Measured on this machine: `claude -p` answered a trivial prompt in 3.6 seconds, and a meeting transcript is a far bigger input than that. A CLI with no terminal attached can also sit forever, so the cap is generous but finite: five minutes.
const DefaultBrainTimeoutSeconds = 300

// EmbedConfig points ORA at a local llama.cpp llama-server running EmbeddingGemma instead of the Gemini embeddings API. The daemon owns the server process: it spawns it on the first embed, reaps it after IdleTimeout with no embeds, and kills it on shutdown.
type EmbedConfig struct {
	// LlamaServer is the absolute path to the llama-server binary. Empty (or ModelPath empty) keeps ORA on the Gemini API.
	LlamaServer string `json:"llama_server"`
	// ModelPath is the absolute path to the EmbeddingGemma GGUF the server loads.
	ModelPath string `json:"model_path"`
	// Port is the loopback port llama-server binds. Defaults to DefaultEmbedPort.
	Port int `json:"port"`
	// IdleTimeout is how long the server may sit with no embed request before the daemon kills it to free its memory. Defaults to DefaultEmbedIdleTimeout. Milliseconds on disk, like TrackerConfig.DwellTime.
	IdleTimeout time.Duration `json:"idle_timeout_ms"`
	// SimilarityFloor is the cosine floor a vector hit must clear to enter hybrid search's fusion. Defaults to DefaultLocalSimilarityFloor; set it here to retune retrieval without a rebuild.
	SimilarityFloor float64 `json:"similarity_floor"`
}

// DefaultLocalSimilarityFloor is the cosine floor for EmbeddingGemma, against internal/db's 0.55 default for Gemini. Measured by replaying thirteen real queries from the log against both indexes: the same genuinely-relevant documents that Gemini scored 0.55-0.79 EmbeddingGemma scores 0.43-0.81, so keeping 0.55 dropped every vector candidate on the open-ended questions ("what did i do today") and quietly reduced those searches to lexical-only.
const DefaultLocalSimilarityFloor = 0.40

// Floor is the cosine floor to hand db.Store.SetVectorSimilarityFloor, defaulting to DefaultLocalSimilarityFloor when the config names none.
func (e EmbedConfig) Floor() float64 {
	if e.SimilarityFloor > 0 {
		return e.SimilarityFloor
	}
	return DefaultLocalSimilarityFloor
}

// DefaultEmbedPort continues the daemon's 6942 with the next port up. Bound to 127.0.0.1 only.
const DefaultEmbedPort = 6943

// LocalEmbedModel is the model name sent in each /v1/embeddings request. llama-server serves whatever GGUF it was started with and ignores this field, but OpenAI-compatible request bodies require it.
const LocalEmbedModel = "embeddinggemma-300m"

// LocalEmbedDim is the native output dimensionality of EmbeddingGemma-300M, against the Gemini path's 3072. Vectors of the two sizes cannot share a chromem collection, so switching engines means building a new vector index.
const LocalEmbedDim = 768

// DefaultEmbedIdleTimeout is how long the embedding server may idle before the daemon reaps it. Ten minutes: long enough to cover a conversation, short enough that a machine left alone gets its ~600 MB back. In milliseconds, matching the JSON field.
const DefaultEmbedIdleTimeout = time.Duration(10 * 60 * 1000)

// LocalEnabled reports whether the local embedder should be used. Both paths must be set — a half-written config stays on Gemini rather than taking embeddings down.
func (e EmbedConfig) LocalEnabled() bool {
	return e.LlamaServer != "" && e.ModelPath != ""
}

// BaseURL is the root the local embeddings server is reachable at, for embed.NewLocalEmbedder.
func (e EmbedConfig) BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", e.Port)
}

// ProactiveAudioEnabled reports whether proactive audio should be requested at the next Live API handshake. Unset means on.
func (cfg OraConfig) ProactiveAudioEnabled() bool {
	return cfg.ProactiveAudio == nil || *cfg.ProactiveAudio
}

// DefaultVoice is used when the config has no voice set (fresh installs, or configs written before /voice existed).
const DefaultVoice = "Iapetus"

// AvailableVoices are the Gemini Live API's prebuilt voice names, current as of July 2026: https://ai.google.dev/gemini-api/docs/speech-generation#voices (Live API uses the same TTS voice roster).
// Hardcoded rather than fetched at runtime, so update this list if Google adds more.
var AvailableVoices = []string{
	"Zephyr", "Puck", "Charon", "Kore", "Fenrir", "Leda", "Orus", "Aoede",
	"Callirrhoe", "Autonoe", "Enceladus", "Iapetus", "Umbriel", "Algieba",
	"Despina", "Erinome", "Algenib", "Rasalgethi", "Laomedeia", "Achernar",
	"Alnilam", "Schedar", "Gacrux", "Pulcherrima", "Achird", "Zubenelgenubi",
	"Vindemiatrix", "Sadachbia", "Sadaltager", "Sulafat",
}

// NormalizeVoice case-insensitively matches name against AvailableVoices and returns the canonical spelling.
// ok is false when name isn't a known voice.
func NormalizeVoice(name string) (canonical string, ok bool) {
	for _, v := range AvailableVoices {
		if strings.EqualFold(v, name) {
			return v, true
		}
	}
	return "", false
}

// IsValidVoice reports whether name (case-insensitive) is one of AvailableVoices.
func IsValidVoice(name string) bool {
	_, ok := NormalizeVoice(name)
	return ok
}

type TrackerConfig struct {
	Blocklist []string      `json:"blocklist"`
	DwellTime time.Duration `json:"dwell_time_ms"`
}

// DefaultBlocklist is matched via tracker.MatchesBlocklist, a case-insensitive, ".exe"-stripped substring match, so one entry covers both platforms.
// Windows app names carry a ".exe" suffix ("1Password.exe"); Linux app IDs don't and are often lowercase or reverse-DNS ("org.keepassxc.KeePassXC").
var DefaultBlocklist = []string{
	// Windows
	"1Password.exe",
	"Bitwarden.exe",
	"Taskmgr.exe",
	"LockApp.exe",
	// Linux / cross-platform password managers and secret stores
	"1password",
	"bitwarden",
	"keepassxc",
	"keepass",
	"proton pass",
	"lastpass",
	"dashlane",
	"enpass",
	"gnome-keyring",
	"seahorse",
}

// DataDir returns the directory ORA stores all of its local state in: the sqlite database, the vector index, the IPC token and the config file. Resolution order: the ORA_DATA_DIR environment variable if set (a test/override hook, never itself subject to migration below), otherwise $XDG_DATA_HOME/ora, falling back to ~/.local/share/ora when XDG_DATA_HOME is unset.
// Every one of those paths used to be resolved relative to the process's working directory ("ora-db/..."), which meant a daemon launched by the login autostart entry (cwd = the binary's own directory) and a client launched from a terminal (cwd = wherever the user happened to be) opened entirely different files. This is the fix: one directory, independent of cwd.
// On first resolution, if this directory doesn't exist yet but a legacy "ora-db" directory exists in the current working directory, its contents are moved here so existing installs aren't orphaned by the change.
func DataDir() string {
	if dir := os.Getenv("ORA_DATA_DIR"); dir != "" {
		return dir
	}

	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			slog.Error("failed to determine home directory, falling back to relative ora-db", "error", err)
			return "ora-db"
		}
		dir = filepath.Join(home, ".local", "share")
	}

	target := filepath.Join(dir, "ora")
	migrateLegacyDataDir(target)
	return target
}

// migrateLegacyDataDir moves the contents of the legacy cwd-relative "ora-db" directory (if one exists) into target, a one-time upgrade step.
// The decision is keyed on whether target already holds a database, NOT on whether the target directory exists: InitTelemetry creates that directory for the log file and SaveConfig creates it for the config, so on a real upgrade it usually exists before anything looks for a legacy directory. Keying on existence would skip the move and strand every existing memory in the old location while ORA quietly started a fresh empty database.
// A no-op once target has a database (already migrated, or a fresh install that wrote there directly) or when there's no legacy database to move.
func migrateLegacyDataDir(target string) {
	if _, err := os.Stat(filepath.Join(target, "db")); err == nil {
		return
	}
	const legacy = "ora-db"
	info, err := os.Stat(legacy)
	if err != nil || !info.IsDir() {
		return
	}
	if _, err := os.Stat(filepath.Join(legacy, "db")); err != nil {
		return
	}

	if err := os.MkdirAll(target, 0700); err != nil {
		slog.Error("failed to create data directory for legacy migration", "target", target, "error", err)
		return
	}
	// Entries are moved one at a time rather than renaming the directory itself, because target may already exist and hold files (a log, a config) that must survive. Anything already present in target wins — it is newer than the legacy copy by definition.
	entries, err := os.ReadDir(legacy)
	if err != nil {
		slog.Error("failed to read legacy ora-db directory", "from", legacy, "error", err)
		return
	}
	moved := 0
	for _, e := range entries {
		from := filepath.Join(legacy, e.Name())
		to := filepath.Join(target, e.Name())
		if _, err := os.Stat(to); err == nil {
			continue
		}
		if err := os.Rename(from, to); err != nil {
			// os.Rename fails across filesystems — fall back to a recursive copy, leaving the original in place for that entry.
			if copyErr := copyDir(from, to); copyErr != nil {
				slog.Error("failed to migrate legacy entry", "from", from, "to", to, "error", copyErr)
				continue
			}
			os.RemoveAll(from)
		}
		moved++
	}
	if moved == 0 {
		return
	}
	// Only remove the legacy directory once it is empty, so a partial migration never destroys anything that did not make it across.
	if rest, err := os.ReadDir(legacy); err == nil && len(rest) == 0 {
		os.Remove(legacy)
	}
	slog.Info("migrated legacy ora-db directory to new data directory", "from", legacy, "to", target, "entries", moved)
}

// copyDir recursively copies every file and directory under src into dst. Used only as migrateLegacyDataDir's cross-filesystem fallback when os.Rename can't do the move in place.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	})
}

// ConfigPath is the on-disk location of the persisted app config.
func ConfigPath() string {
	return filepath.Join(DataDir(), "ora-config.json")
}

// get or create
func LoadConfig() OraConfig {
	cfg := OraConfig{
		Tracker: TrackerConfig{
			Blocklist: DefaultBlocklist,
			// 3s is too less to be a dwell time, so 15s sounded better. honestly, it has to be tab switching + dwell, and im not sure what the right number is?
			DwellTime: 15000,
		},
		Voice:     DefaultVoice,
		Autostart: false,
		Embed: EmbedConfig{
			Port:        DefaultEmbedPort,
			IdleTimeout: DefaultEmbedIdleTimeout,
		},
	}

	configPath := ConfigPath()

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		slog.Info("Creating default config file", "path", configPath)
		if err := SaveConfig(cfg); err != nil {
			slog.Error("Failed to write default config file", "error", err)
		}
		return cfg
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		slog.Error("Failed to read config file, using defaults", "error", err)
		return cfg
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Error("Failed to parse config file, using defaults", "error", err)
		return cfg
	}

	// configs written before /voice existed (or with a bad value) fall back to default
	if cfg.Voice == "" || !IsValidVoice(cfg.Voice) {
		cfg.Voice = DefaultVoice
	}

	// json.Unmarshal only overwrites keys the file actually carries, so a config with no "embed" block keeps the defaults set above. These two guards cover the case where the block exists but zeroes a field explicitly, which would otherwise mean binding port 0 or reaping the embedding server on every tick.
	if cfg.Embed.Port <= 0 {
		cfg.Embed.Port = DefaultEmbedPort
	}
	if cfg.Embed.IdleTimeout <= 0 {
		cfg.Embed.IdleTimeout = DefaultEmbedIdleTimeout
	}

	return cfg
}

// SaveConfig persists cfg to disk, creating the data directory if needed.
func SaveConfig(cfg OraConfig) error {
	if err := os.MkdirAll(DataDir(), 0755); err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if err := os.WriteFile(ConfigPath(), data, 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}
	return nil
}

// SetVoice validates name against AvailableVoices, updates cfg in place with the canonical spelling, and persists the change to disk.
// Invalid names are rejected and leave cfg/disk untouched.
func (cfg *OraConfig) SetVoice(name string) error {
	canonical, ok := NormalizeVoice(name)
	if !ok {
		return fmt.Errorf("invalid voice: %q (see AvailableVoices)", name)
	}
	prev := cfg.Voice
	cfg.Voice = canonical
	if err := SaveConfig(*cfg); err != nil {
		cfg.Voice = prev
		return err
	}
	return nil
}
