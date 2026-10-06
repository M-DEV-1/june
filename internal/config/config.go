package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"june/internal/util"
)

// Version is June's release number, the one source of truth for every place that reports it: GET /settings, GET /setup, the updater and the telemetry tags. A var rather than a const so a release build sets it from the tag with -ldflags "-X june/internal/config.Version=0.2.0"; a constant written by hand had already drifted from tauri.conf.json. "dev" is a build nobody stamped, which the updater never replaces.
var Version = "dev"

type JuneConfig struct {
	Tracker TrackerConfig `json:"tracker"`
	// SetupDone is set when the user finishes the window's first-run setup. Until then the tracker stays paused, so nothing on the screen is observed before the user has been told what June keeps.
	// Never omitted, so every file this version writes answers it one way or the other: a file without the key was written by a June from before setup existed, and that is the one case readConfig decides it for (see storeExists).
	SetupDone bool `json:"setup_done"`
	// UpdateCheck turns the daily check for a new June release on or off. nil means on. SignPath's terms ask for a way to turn off anything that contacts a server the user did not choose, and GitHub's release API is one.
	UpdateCheck *bool `json:"update_check,omitempty"`
	// Voice is the Gemini Live prebuilt voice name used for the assistant's spoken output (see AvailableVoices).
	// Defaults to DefaultVoice when unset.
	Voice string `json:"voice"`
	// LiveModel is the bidirectional Live API model the voice session dials, one of config.LiveVoiceModels. Empty, or a name June does not know, runs on DefaultVoiceModel. It is a choice rather than a constant because the two models trade tone against latency and neither answer is right for everyone: see LiveVoiceModels in gemini.go for the measured numbers.
	LiveModel string `json:"live_model,omitempty"`
	// Autostart is whether the daemon should be launched when the user logs in. Defaults to false — an upgrading user must opt in to a screen-recording daemon starting at login, not get one installed silently — and the daemon reconciles the on-disk autostart entry to match this field on every startup.
	Autostart bool `json:"autostart"`
	// Window is whether the daemon also runs the desktop window, so the login entry starts one thing and gets both. Defaults to true; set it to false in june-config.json to run the daemon headless, and see cmd/window.go for where the window binary is looked for.
	Window bool `json:"window"`
	// ProactiveAudio turns on the Gemini Live "proactive audio" feature, which lets the model stay silent when what the mic picked up wasn't addressed to it — a room conversation, a video, the user talking to someone else. Defaults to on; set it to false in june-config.json to have the model answer everything it hears.
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
	// Meetings sets what June does when it notices a call. Zero value means it offers to record and never records on its own.
	Meetings MeetingsConfig `json:"meetings"`
	// BrainModels remembers the model last picked for each brain from POST /brains (see internal/ipc/brains.go), keyed by brain id ("claude", "codex", "gemini", "grok", "ollama"). It lives here rather than inside BrainConfig because BrainConfig is compared with == in tests, which a map field would break.
	BrainModels map[string]string `json:"brain_models,omitempty"`
	// BackgroundModels pins a Gemini model per unattended job, keyed by the Job* names in gemini.go ("working_state", "meeting_minutes", ...). A job with no entry runs on DefaultBackgroundModel. It exists so the model each background duty spends the free tier's per-day request allowance on is named in the config rather than buried in code; the daemon installs it with SetBackgroundModels at startup.
	BackgroundModels map[string]string `json:"background_models,omitempty"`
	// BackgroundBrains names which provider answers each unattended job, keyed by the same Job* names as BackgroundModels. A job with no entry stays on the Gemini API, which is what every job did before this existed, so a config file written without this block behaves exactly as it did. It is what lets the memory duties run on a CLI login — the user's own Antigravity or Claude plan — and spend no metered request at all.
	BackgroundBrains map[string]BrainConfig `json:"background_brains,omitempty"`
	// LocalText points the working-state derive at a local llama-server instead of the metered API. Zero value means there is no local text model and the job stays on Gemini.
	LocalText LocalTextConfig `json:"local_text"`
	// ActRunKeep caps how many ordinary act runs the store keeps — the screen-tool traces behind db.PruneActRuns, which grow without bound otherwise. Zero means DefaultActRunKeep; a negative number keeps every run. Runs the nightly notes were written from, and recent failures, are kept whatever this says. Read it through ActRunsKept rather than directly.
	ActRunKeep int `json:"act_run_keep"`
	// ActRunFailedKeepDays is how many days a failed act run is kept regardless of ActRunKeep — the grace db.Store.PruneActRuns measures a failure's age against. Zero means DefaultActRunFailedKeepDays; a negative number turns the grace off, so a failed run is capped by count like any other. Read it through FailedActRunsKeptDays rather than directly.
	ActRunFailedKeepDays int `json:"act_run_failed_keep_days"`
	// DailyTokenBudget caps how many tokens a provider (see the Provider* names in internal/agent/ask.go — "codex", "gemini", ...) may spend in one local calendar day, keyed by provider name. A provider named with 0 or absent from the map has no budget: the default is off, since a user who never set one should never see a warning. Read it through DailyTokenBudgetFor rather than directly.
	DailyTokenBudget map[string]int `json:"daily_token_budget,omitempty"`
	// ClaudeUsageFromLogin is whether GET /brains reads the Claude row's usage bars from the undocumented https://api.anthropic.com/api/oauth/usage endpoint, using the access token Claude Code's own login already wrote to ~/.claude/.credentials.json. Defaults to on; a pointer, like ProactiveAudio, so a config written before this field existed is distinguishable from one where the user explicitly turned it off. Read it through ClaudeUsageFromLoginEnabled rather than directly.
	ClaudeUsageFromLogin *bool `json:"claude_usage_from_login,omitempty"`
	// ExaMonthlyRequests is the request ceiling of the user's own Exa plan, so GET /usage can show calls-this-month against it as a fraction. Exa's own usage endpoint needs a team-management key June does not hold (see internal/agent/websearch.go), so this is the only ceiling there is, and it is never guessed: 0 means unset, and the usage view then shows the call count with no bar.
	ExaMonthlyRequests int `json:"exa_monthly_requests,omitempty"`
	// Hotkey is the shortcut that shows and hides June's hover, as canonical text such as "Ctrl+Shift+Space" (see ipc.ParseHotkey). "" means DefaultHotkey. It is the shortcut the user asked for, not necessarily the one in effect: one another program already holds leaves June with none, and GET /settings reports what is actually in effect.
	Hotkey string `json:"hotkey,omitempty"`
}

// DefaultHotkey is the shortcut June asks for when the config names none. There is deliberately no second choice behind it: a shortcut June quietly picked for itself is one the user does not know to press, so when this one is taken June has none until the user chooses their own.
const DefaultHotkey = "Ctrl+Alt+Space"

// HotkeyWanted is the shortcut June should try to hold: the configured one, or DefaultHotkey when none is set.
func (cfg JuneConfig) HotkeyWanted() string {
	if cfg.Hotkey == "" {
		return DefaultHotkey
	}
	return cfg.Hotkey
}

// ClaudeUsageFromLoginEnabled reports whether the Claude usage endpoint should be read. Unset means on.
func (cfg JuneConfig) ClaudeUsageFromLoginEnabled() bool {
	return cfg.ClaudeUsageFromLogin == nil || *cfg.ClaudeUsageFromLogin
}

// DefaultActRunKeep is how many ordinary act runs are kept when the config names no number. Two thousand: the nightly procedures stage reads the newest 200 runs, so this leaves ten times its window, and at the few dozen screen asks a day the machine actually logs it covers a couple of months of them for a few megabytes.
const DefaultActRunKeep = 2000

// ActRunsKept returns the act run cap to hand db.Store.PruneActRuns: the number the user set, DefaultActRunKeep when they set none, and the negative number unchanged when they turned the cap off.
func (cfg JuneConfig) ActRunsKept() int {
	if cfg.ActRunKeep == 0 {
		return DefaultActRunKeep
	}
	return cfg.ActRunKeep
}

// DefaultActRunFailedKeepDays is how many days a failed act run is kept when the config names no number. Thirty: a failure is never written up as a nightly "How I did X" note, so nothing else protects it, and the point of keeping one is being able to look back at what went wrong on the screen — worth a month, not worth forever.
const DefaultActRunFailedKeepDays = 30

// FailedActRunsKeptDays returns the failed-run grace in days to hand db.Store.PruneActRuns: the number the user set, DefaultActRunFailedKeepDays when they set none, and the negative number unchanged when they turned the grace off.
func (cfg JuneConfig) FailedActRunsKeptDays() int {
	if cfg.ActRunFailedKeepDays == 0 {
		return DefaultActRunFailedKeepDays
	}
	return cfg.ActRunFailedKeepDays
}

// DailyTokenBudgetFor returns the daily token budget set for provider, or 0 when none was set — reading a nil map the zero value the same way an empty one does, so a config file written before this field existed behaves exactly like one that set no budgets at all. Zero always means off; there is no default to fall back to, unlike ActRunsKept.
func (cfg JuneConfig) DailyTokenBudgetFor(provider string) int {
	return cfg.DailyTokenBudget[provider]
}

// MeetingsConfig sets how June reacts to another application taking the microphone, which is how it notices a call is happening.
type MeetingsConfig struct {
	// Offer is whether June asks "in a meeting?" when another application has held the microphone long enough to be a call. Defaults to on, since noticing a call and then saying nothing about it is no use.
	// A pointer, not a plain bool, so a config file written before this field existed is distinguishable from one where the user explicitly turned the prompt off. Use OfferEnabled rather than reading it directly.
	Offer *bool `json:"offer,omitempty"`
	// AutoRecord starts recording on the same signal without asking first. Off by default: the microphone says a call is likely, not that it is certain, and a recording nobody asked for is the wrong way to be wrong about that.
	AutoRecord bool `json:"auto_record"`
}

// OfferEnabled reports whether the prompt should be shown, treating an absent setting as on.
func (m MeetingsConfig) OfferEnabled() bool { return m.Offer == nil || *m.Offer }

// DreamConfig sets when the overnight dreaming loop may run, and reserves the knobs the later local-model slice will need.
type DreamConfig struct {
	// Hour is the local hour the dreaming window opens; the window closes at the morning brief hour. Zero means DefaultDreamHour, negative disables dreaming.
	Hour int `json:"hour"`
	// ModelPath is the GGUF the future local dreaming model loads. Unused this slice.
	ModelPath string `json:"model_path"`
	// Port is the loopback port reserved for the future local dreaming model server.
	Port int `json:"port"`
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
	// GPUDevice is which GPU whisper.cpp should decode on, numbered as its backend numbers the devices it finds. Zero, the default, leaves the choice to whisper.cpp, which takes the first device it sees. On Linux that backend is Vulkan, which counts the integrated graphics too, usually as the slower device 0, so this normally wants setting there. On Windows it is CUDA, which counts only the NVIDIA cards, so zero is normally right and a number carried over from a Vulkan machine names no GPU at all.
	GPUDevice int `json:"gpu_device"`
	// ClusterThreshold is the cosine distance at which the diarizer stops treating two stretches of the call's audio as the same voice. Smaller splits one person into several; larger merges two people into one. Zero means the built-in default.
	// It wants setting per machine rather than guessing: the right value depends on the embedding model, on how the other people's microphones colour their voices, and on how much everyone talks over each other. Sherpa's own default of 0.5 gave 35 clusters for a six-person standup on this laptop, which is the symptom of it being set too low.
	ClusterThreshold float64 `json:"cluster_threshold"`
	// SpeakerCountFromScreen tells the diarizer how many people are in the call, counted from the names the meeting app shows inside its own window, instead of letting it estimate from ClusterThreshold. Off by default: the count is read out of one flattened run of accessibility text, where three names in a row can parse as one, and undercounting fuses several people into a single voice in a way nothing downstream can undo. The recorder logs the count it would have used on every meeting, so turn this on once that log shows the right number for a real group call.
	SpeakerCountFromScreen bool `json:"speaker_count_from_screen"`
	// Model is the whisper.cpp model file in the whispercpp directory, such as "ggml-small.bin". Empty means ggml-medium.bin. A machine with no GPU whisper can use gets the small model from first-run setup, because medium on the CPU takes several seconds per dictation.
	Model string `json:"model,omitempty"`
}

// BrainConfig chooses which backend answers June's one-shot text duties — the meeting minutes and the personal context updater. The zero value is the Gemini API on TextModel, which is what June did before this block existed, so a config file written without it behaves exactly as it always has.
// The voice assistant is not covered by this: that is a Gemini Live session, not a one-shot call.
type BrainConfig struct {
	// AllowFallback lets a background duty (minutes, briefs, summaries) go to another signed-in brain when the one it was meant for fails. nil means allowed, which is how June always behaved; the window and the privacy notice let the user turn it off so nothing goes to a service they did not pick.
	AllowFallback *bool `json:"allow_fallback,omitempty"`
	// Provider is BrainGeminiAPI (the default) or BrainClaudeCLI to run `claude -p` under whatever Claude Code login the machine already has. Anything else falls back to the Gemini API.
	Provider string `json:"provider"`
	// Model names the model each provider should use: a Gemini model name (defaulting to TextModel), or for claude-cli a model alias passed as --model ("sonnet", "opus"); empty means the login's default.
	Model string `json:"model"`
	// Binary is the path to the claude CLI to run. Empty means look "claude" up on PATH.
	Binary string `json:"binary"`
	// TimeoutSeconds is the hard limit on a single call, after which the CLI child is killed or the HTTP request is dropped, and the call fails. Defaults to DefaultBrainTimeoutSeconds. The Gemini provider passes it to the SDK's own HTTPOptions.Timeout, so every provider is bounded by it.
	TimeoutSeconds int `json:"timeout_seconds"`
}

// Timeout returns the hard limit on one brain call, defaulting to DefaultBrainTimeoutSeconds. internal/brain applies the same default when it builds the backend; this is for callers that need to size their own deadline around a call.
func (b BrainConfig) Timeout() time.Duration {
	if b.TimeoutSeconds <= 0 {
		return DefaultBrainTimeoutSeconds * time.Second
	}
	return time.Duration(b.TimeoutSeconds) * time.Second
}

// The provider names accepted in BrainConfig.Provider.
const (
	BrainGeminiAPI = "gemini-api"
	BrainClaudeCLI = "claude-cli"
	BrainAgyCLI    = "agy-cli"
	BrainGrokCLI   = "grok-cli"
	// BrainCodex answers through the Codex Responses backend under the user's ChatGPT login, the way internal/agent.CodexBrain calls it; internal/brain.FromConfig only reaches that backend when its caller supplies an asker, and falls back to the Gemini API otherwise.
	BrainCodex = "codex-direct"
	// BrainOllama names the Ollama brain in a persisted config; internal/brain has no chat backend for it yet, so internal/brain.FromConfig falls back to the Gemini API whenever this is configured.
	BrainOllama = "ollama-cli"
)

// DefaultBrainTimeoutSeconds caps one CLI run. Measured on this machine: `claude -p` answered a trivial prompt in 3.6 seconds, and a meeting transcript is a far bigger input than that. A CLI with no terminal attached can also sit forever, so the cap is generous but finite: five minutes.
const DefaultBrainTimeoutSeconds = 300

// EmbedConfig points June at a local llama.cpp llama-server running EmbeddingGemma instead of the Gemini embeddings API. The daemon owns the server process: it spawns it on the first embed, reaps it after IdleTimeout with no embeds, and kills it on shutdown.
type EmbedConfig struct {
	// LlamaServer is the absolute path to the llama-server binary. Empty (or ModelPath empty) keeps June on the Gemini API.
	LlamaServer string `json:"llama_server"`
	// ModelPath is the absolute path to the EmbeddingGemma GGUF the server loads.
	ModelPath string `json:"model_path"`
	// Port is the loopback port llama-server binds. Defaults to DefaultEmbedPort.
	Port int `json:"port"`
	// IdleTimeout is how long the server may sit with no embed request before the daemon kills it to free its memory. Defaults to DefaultEmbedIdleTimeout. Milliseconds on disk, like TrackerConfig.DwellTime.
	IdleTimeout time.Duration `json:"idle_timeout_ms"`
	// Device is the llama.cpp device name to run the server on, such as "Vulkan1", as listed by `llama-server --list-devices`. Empty leaves the choice to llama-server, which on a machine with two GPUs may spread the model across both.
	Device string `json:"device"`
	// SimilarityFloor is the cosine floor a vector hit must clear to enter hybrid search's fusion. Defaults to DefaultLocalSimilarityFloor, WindowsLocalSimilarityFloor on Windows; set it here to retune retrieval without a rebuild.
	SimilarityFloor float64 `json:"similarity_floor"`
	// ActRunSimilarityFloor is the cosine a past screen run's question must reach against a new one before that run is shown to the model as reference. Defaults to db.DefaultActRunSimilarity, which was measured on written-for-the-purpose desktop questions rather than on anything in this store, and wants revisiting once there are real questions asked days apart to measure. Zero or less is ignored. Read it through ActRunFloor rather than directly.
	ActRunSimilarityFloor float64 `json:"act_run_similarity_floor"`
}

// DefaultLocalSimilarityFloor is the cosine floor for EmbeddingGemma, against internal/db's 0.55 default for Gemini. Measured by replaying thirteen real queries from the log against both indexes: the same genuinely-relevant documents that Gemini scored 0.55-0.79 EmbeddingGemma scores 0.43-0.81, so keeping 0.55 dropped every vector candidate on the open-ended questions ("what did i do today") and quietly reduced those searches to lexical-only.
// That store is a Linux one, whose captures are long pages; a Windows store has its own floor (see WindowsLocalSimilarityFloor).
const DefaultLocalSimilarityFloor = 0.40

// WindowsLocalSimilarityFloor is DefaultLocalSimilarityFloor for a Windows store. Measured on 2026-10-03 against twenty Windows captures (Calculator, Settings, File Explorer, Notepad, a browser, a terminal) through the running server: a question put in other words than the window's own scored its window 0.29-0.30 at the least ("calculation history" against Calculator 0.295, "sitcom I was streaming" against the episode playing 0.292, "how much RAM is installed" against Settings' About page 0.303), while the best match for ten questions about things never on the screen (a dentist, a flight, a budget email, a pull request) was 0.10-0.26. At 0.40 every one of those right answers was dropped and the search was keyword-only; 0.28 keeps them and still turns the absent topics away. The band below the best hit (internal/db's vectorSimilarityBand) is what trims the tail once a strong match is in.
// Windows only, because the Linux store was not measured again: what its off-topic questions score between 0.28 and 0.40 is not known, so it keeps the floor it was measured at.
const WindowsLocalSimilarityFloor = 0.28

// Floor is the cosine floor to hand db.Store.SetVectorSimilarityFloor, defaulting to WindowsLocalSimilarityFloor on Windows and DefaultLocalSimilarityFloor elsewhere when the config names none.
func (e EmbedConfig) Floor() float64 {
	if e.SimilarityFloor > 0 {
		return e.SimilarityFloor
	}
	if runtime.GOOS == "windows" {
		return WindowsLocalSimilarityFloor
	}
	return DefaultLocalSimilarityFloor
}

// ActRunFloor is the cosine floor to hand db.Store.SetActRunSimilarityFloor, or zero when the config names none — which the store reads as "keep your own default", so this package does not restate a number that belongs to internal/db.
func (e EmbedConfig) ActRunFloor() float64 {
	if e.ActRunSimilarityFloor > 0 {
		return e.ActRunSimilarityFloor
	}
	return 0
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

// LocalTextConfig points June's unattended text jobs at a local llama.cpp llama-server running an instruction-tuned model, so they spend no metered per-day request allowance at all. The daemon owns the server process the same way it owns the embedding one: spawned on first use, reaped after IdleTimeout, killed on shutdown.
type LocalTextConfig struct {
	// LlamaServer is the absolute path to the llama-server binary. Empty falls back to EmbedConfig.LlamaServer, and then to "llama-server" on PATH.
	LlamaServer string `json:"llama_server"`
	// ModelPath is the absolute path to the instruction-tuned GGUF the server loads. Empty falls back to DreamConfig.ModelPath, which is where this machine's Gemma already is; with neither set the local path is off.
	ModelPath string `json:"model_path"`
	// Port is the loopback port llama-server binds. Defaults to DefaultLocalTextPort.
	Port int `json:"port"`
	// IdleTimeout is how long the server may sit unused before the daemon kills it to give its GPU memory back. Defaults to DefaultLocalTextIdleTimeout. Milliseconds on disk, like the other timeouts here.
	IdleTimeout time.Duration `json:"idle_timeout_ms"`
	// Device is the llama.cpp device name to run on, such as "Vulkan1". Empty falls back to DreamConfig.Device and then to llama-server's own choice.
	Device string `json:"device"`
	// TimeoutSeconds is the hard limit on one local generation. Defaults to DefaultLocalTextTimeoutSeconds.
	TimeoutSeconds int `json:"timeout_seconds"`
}

// DefaultLocalTextPort continues the port run (6942 IPC, 6943 embeddings, 6944 the dream shadow) with the next one up, so the working-state server and a night's dream shadow can hold their own models at once.
const DefaultLocalTextPort = 6945

// DefaultLocalTextIdleTimeout is how long the local text server may idle before the daemon reaps it, in milliseconds to match the JSON field. Fifteen minutes: longer than the ten-minute working-state floor, so a working session keeps one loaded model rather than paying the reload on every derive, and short enough that a machine left alone gets the memory back.
const DefaultLocalTextIdleTimeout = time.Duration(15 * 60 * 1000)

// DefaultLocalTextTimeoutSeconds bounds one local generation. Three minutes: a quantised model on this machine writes a 120-word paragraph in seconds, and the cap is only there so a wedged server cannot hang the job forever.
const DefaultLocalTextTimeoutSeconds = 180

// Enabled reports whether the local text path should be used, resolving ModelPath against the dream block so a machine that already names a local GGUF there needs no new configuration. Input: the whole config, for those fallbacks. Output: true when a model file is named somewhere.
func (l LocalTextConfig) Enabled(cfg JuneConfig) bool {
	return l.ResolvedModelPath(cfg) != ""
}

// ResolvedModelPath returns the GGUF to load: this block's own, else the dream block's, else empty.
func (l LocalTextConfig) ResolvedModelPath(cfg JuneConfig) string {
	if l.ModelPath != "" {
		return l.ModelPath
	}
	return cfg.Dream.ModelPath
}

// ResolvedBinary returns the llama-server binary to run: this block's own, else the embed block's, else "llama-server" from PATH.
func (l LocalTextConfig) ResolvedBinary(cfg JuneConfig) string {
	if l.LlamaServer != "" {
		return l.LlamaServer
	}
	if cfg.Embed.LlamaServer != "" {
		return cfg.Embed.LlamaServer
	}
	return "llama-server"
}

// ResolvedDevice returns the llama.cpp device to pin the server to: this block's own, else the dream block's, else empty for llama-server's own choice.
func (l LocalTextConfig) ResolvedDevice(cfg JuneConfig) string {
	if l.Device != "" {
		return l.Device
	}
	return cfg.Dream.Device
}

// LocalTextPort returns the loopback port for the local text server, defaulting to DefaultLocalTextPort.
func (l LocalTextConfig) LocalTextPort() int {
	if l.Port <= 0 {
		return DefaultLocalTextPort
	}
	return l.Port
}

// Timeout returns the hard limit in seconds on one local generation, defaulting to DefaultLocalTextTimeoutSeconds.
func (l LocalTextConfig) Timeout() int {
	if l.TimeoutSeconds <= 0 {
		return DefaultLocalTextTimeoutSeconds
	}
	return l.TimeoutSeconds
}

// Idle returns how long the local text server may sit unused before it is reaped, defaulting to DefaultLocalTextIdleTimeout.
func (l LocalTextConfig) Idle() time.Duration {
	if l.IdleTimeout <= 0 {
		return DefaultLocalTextIdleTimeout
	}
	return l.IdleTimeout
}

// BaseURL is the root the local text server is reachable at, for internal/brain.LlamaServer.
func (l LocalTextConfig) BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", l.LocalTextPort())
}

// ProactiveAudioEnabled reports whether proactive audio should be requested at the next Live API handshake. Unset means on.
func (cfg JuneConfig) ProactiveAudioEnabled() bool {
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

// voiceTraits is Google's own one-word description of how each prebuilt voice sounds, from https://ai.google.dev/gemini-api/docs/speech-generation#voices (checked 2026-09-07). The picker shows it beside the name because thirty star names say nothing about how any of them sounds.
var voiceTraits = map[string]string{
	"Zephyr": "Bright", "Puck": "Upbeat", "Charon": "Informative", "Kore": "Firm",
	"Fenrir": "Excitable", "Leda": "Youthful", "Orus": "Firm", "Aoede": "Breezy",
	"Callirrhoe": "Easy-going", "Autonoe": "Bright", "Enceladus": "Breathy", "Iapetus": "Clear",
	"Umbriel": "Easy-going", "Algieba": "Smooth", "Despina": "Smooth", "Erinome": "Clear",
	"Algenib": "Gravelly", "Rasalgethi": "Informative", "Laomedeia": "Upbeat", "Achernar": "Soft",
	"Alnilam": "Firm", "Schedar": "Even", "Gacrux": "Mature", "Pulcherrima": "Forward",
	"Achird": "Friendly", "Zubenelgenubi": "Casual", "Vindemiatrix": "Gentle", "Sadachbia": "Lively",
	"Sadaltager": "Knowledgeable", "Sulafat": "Warm",
}

// VoiceTrait is how a voice sounds, in one word. Input: a voice name in any casing. Output: the trait, or "" for a name that is not one of AvailableVoices.
func VoiceTrait(name string) string {
	canonical, ok := NormalizeVoice(name)
	if !ok {
		return ""
	}
	return voiceTraits[canonical]
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

// DefaultDwellTime is how long a window must hold focus before the tracker records it, in milliseconds. Used both as the built-in default and as the fallback for a config file that carries an explicit zero.
// It is a plain int rather than a time.Duration because that is what the number on disk means: as a Duration, 15000 reads as 15 microseconds, and the value was only ever right because the one call site multiplied by time.Millisecond a second time.
const DefaultDwellTime = 15000

type TrackerConfig struct {
	Blocklist []string `json:"blocklist"`
	// DwellTime is milliseconds, as the JSON tag says. cmd/daemon.go converts it once, where it is handed to the tracker.
	DwellTime int `json:"dwell_time_ms"`
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

// DataDir returns the directory June stores all of its local state in: the sqlite database, the vector index, the IPC token and the config file. Resolution order: the JUNE_DATA_DIR environment variable if set (a test/override hook, never itself subject to migration below), otherwise $XDG_DATA_HOME/june, falling back to ~/.local/share/june when XDG_DATA_HOME is unset.
// Every one of those paths used to be resolved relative to the process's working directory ("june-db/..."), which meant a daemon launched by the login autostart entry (cwd = the binary's own directory) and a client launched from a terminal (cwd = wherever the user happened to be) opened entirely different files. This is the fix: one directory, independent of cwd.
// On first resolution, if this directory doesn't exist yet but a legacy "june-db" directory exists in the current working directory, its contents are moved here so existing installs aren't orphaned by the change.
func DataDir() string {
	if dir := os.Getenv("JUNE_DATA_DIR"); dir != "" {
		return dir
	}

	dir := util.DataHome()
	if dir == "" {
		slog.Error("failed to determine home directory, falling back to relative june-db")
		return "june-db"
	}
	return filepath.Join(dir, "june")
}

// ConfigPath is the on-disk location of the persisted app config.
func ConfigPath() string {
	return filepath.Join(DataDir(), "june-config.json")
}

// ErrUnreadable is what ReadConfig wraps when june-config.json is there but is not a config it can read: a hand edit with a stray comma, or a file a power cut left half written. Callers tell it apart from a read that failed, which says nothing about the file, with errors.Is. SaveConfig wraps it too, when such a file could not be kept aside and so was not written over.
var ErrUnreadable = errors.New("june-config.json is not a config June can read")

// get or create
// A file with no setup_done yet has it decided by readConfig and written straight back, so the decision is made once, against the store as it stood then. The daemon loads its config before it opens the store for exactly this reason: decided after, the store a new install's first daemon had just created would count as one from an earlier June.
// A file that will not parse is never changed here. LoadConfig runs on every ask, dictation and voice start of a running daemon, and setting such a file aside there undid settings the daemon still held: a hand edit saved with one stray comma, or read half-way through Notepad's save, became the defaults on disk, and the updater and the meeting watcher, which read the file, went back to checking and offering. So the defaults run under whatever of the file decoded, with setup_done kept true when a store shows June has run here before, and the file is left for whoever is editing it. A save replaces it, keeping it aside (see SaveConfig), and the daemon does that once as it starts (see ReplaceUnreadable). One that could not be read at all is treated the same way, so a passing lock on the file does not send someone who has used June for months back to first-run setup with watching paused.
func LoadConfig() JuneConfig {
	configPath := ConfigPath()
	// Once per process: an install whose data folder is outside the profile and was made before June protected such folders is protected the next time June starts, not only once a setting is next saved (see util.MkdirPrivate).
	protectOnce.Do(func() {
		if _, err := os.Stat(DataDir()); err == nil {
			util.MkdirPrivate(DataDir())
		}
	})

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		cfg := defaultConfig()
		// No file at all is a new install, or one whose config was deleted by hand, and the store tells them apart the same way it does for a file from an earlier June.
		cfg.SetupDone = storeExists()
		slog.Info("Creating default config file", "path", configPath, "setup_done", cfg.SetupDone)
		if err := SaveConfig(cfg); err != nil {
			slog.Error("Failed to write default config file", "error", err)
		}
		return cfg
	}

	cfg, decided, err := readConfig()
	if errors.Is(err, ErrUnreadable) {
		// Once until the file reads again, since a file left broken would otherwise be logged on every ask.
		if !unreadableLogged.Swap(true) {
			slog.Warn("june-config.json could not be read; running on the defaults under what of it did, and leaving it as it is until a setting is saved", "error", err)
		}
		return recoveredWithoutFile(cfg)
	}
	if err != nil {
		cfg.SetupDone = storeExists()
		slog.Error("Failed to load config file, using defaults", "error", err, "setup_done", cfg.SetupDone)
		return cfg
	}
	unreadableLogged.Store(false)
	if decided {
		slog.Info("this config predates first-run setup; recording whether setup counts as done", "setup_done", cfg.SetupDone)
		if err := SaveConfig(cfg); err != nil {
			slog.Warn("could not record setup_done; it is decided again on the next load", "error", err)
		}
	}
	return cfg
}

// protectOnce keeps LoadConfig's protection of the data folder to the first load in a process.
var protectOnce sync.Once

// unreadableLogged is set once LoadConfig has logged that the file will not parse, and cleared when it next reads.
var unreadableLogged atomic.Bool

// recoveredWithoutFile is what a broken file is read as: the defaults under whatever of it decoded (a value of the wrong type spoils only its own field), with setup_done kept true when a store shows June has run here before. Before, the defaults ran with setup_done false, and the daemon paused watching and opened first-run setup for someone who had used June for months.
func recoveredWithoutFile(cfg JuneConfig) JuneConfig {
	cfg.SetupDone = cfg.SetupDone || storeExists()
	repair(&cfg)
	return cfg
}

// ReplaceUnreadable writes cfg in place of a june-config.json that will not parse, keeping the broken file beside it (see SaveConfig), and leaves a file that reads, is missing or cannot be opened as it is. The daemon calls it once as it starts, with the config it runs on, which LoadConfig made of that same file: every other reader of the file, the updater and the meeting watcher among them, then sees what the daemon runs on, and the user is told at this start rather than after the next save. Output: whether the file was replaced.
func ReplaceUnreadable(cfg JuneConfig) bool {
	saveMu.Lock()
	defer saveMu.Unlock()
	if _, _, err := readConfig(); !errors.Is(err, ErrUnreadable) {
		return false
	}
	if err := saveLocked(cfg); err != nil {
		slog.Error("could not write a new june-config.json in place of the unreadable one", "error", err)
		return false
	}
	return true
}

// setAsideNote is the file that says a june-config.json was set aside and the user has not been told yet. It holds the name the broken file was kept under.
func setAsideNote() string {
	return filepath.Join(DataDir(), "june-config.set-aside")
}

// keepAside copies data, the bytes of an unreadable june-config.json, to june-config.json.unreadable-<date>-<time> beside it, and leaves a note for the next daemon to tell the user about (see TakeSetAside). A copy rather than a rename, so there is never a moment with no config file, in which another load would write the defaults in its place. Output: the copy's path, or the error from writing it.
func keepAside(data []byte) (string, error) {
	base := ConfigPath() + ".unreadable-" + time.Now().Format("20060102-150405")
	// A name already taken may be an earlier broken file kept the same second.
	aside := base
	for i := 2; util.Exists(aside); i++ {
		aside = fmt.Sprintf("%s-%d", base, i)
	}
	if err := util.WriteFilePrivate(aside, data); err != nil {
		return "", err
	}
	if err := os.WriteFile(setAsideNote(), []byte(filepath.Base(aside)), 0o600); err != nil {
		slog.Warn("could not leave a note that june-config.json was set aside; the user will not be told", "error", err)
	}
	return aside, nil
}

// TakeSetAside reports a june-config.json that was set aside since the user was last told, and forgets it, so it is told once. The daemon calls it as it starts. Output: the name the broken file was kept under, in the data directory, or "" when there is nothing to tell.
func TakeSetAside() string {
	note := setAsideNote()
	data, err := os.ReadFile(note)
	if err != nil {
		return ""
	}
	os.Remove(note)
	return strings.TrimSpace(string(data))
}

// storeExists reports whether June's store is already in the data directory. It is what decides setup_done for an install that has never recorded it: an earlier June that has run has a store, and its tracking must not stop because the version that brought setup arrived, while a new install has none until its first daemon opens one.
func storeExists() bool {
	info, err := os.Stat(filepath.Join(DataDir(), "db"))
	return err == nil && !info.IsDir()
}

// defaultConfig is the config a machine with no june-config.json runs on, and the base a file's own keys are read over.
func defaultConfig() JuneConfig {
	return JuneConfig{
		Tracker: TrackerConfig{
			Blocklist: slices.Clone(DefaultBlocklist),
			// 3s is too less to be a dwell time, so 15s sounded better. honestly, it has to be tab switching + dwell, and im not sure what the right number is?
			DwellTime: DefaultDwellTime,
		},
		Voice:     DefaultVoice,
		Autostart: false,
		Window:    true,
		Embed: EmbedConfig{
			Port:        DefaultEmbedPort,
			IdleTimeout: DefaultEmbedIdleTimeout,
		},
	}
}

// ReadConfig reads june-config.json as it is on disk right now, with the same defaults and repairs LoadConfig applies, but it never creates the file and never hides a failure. A running daemon reads through it before it saves one setting, so the save changes that setting alone: saving the copy it loaded at startup put back every field changed since — `june --autostart on`, or a hand edit — and the next start then reconciled the login entry against the stale value. Output: the config, and an error when the file is missing, unreadable or not valid JSON, in which case the config is the defaults, or as much of the file as decoded.
func ReadConfig() (JuneConfig, error) {
	cfg, _, err := readConfig()
	return cfg, err
}

// readConfig is ReadConfig, also saying whether the file lacked setup_done so that this read decided it (see storeExists). Every caller that saves what it read then writes the decision down, the installer's `june --autostart` among them, which runs before the new daemon ever starts; read as plain false instead, an upgrade's first save would have turned an existing install's tracking off.
func readConfig() (cfg JuneConfig, decided bool, err error) {
	cfg = defaultConfig()
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		return cfg, false, fmt.Errorf("failed to read config file: %w", err)
	}
	data = bytes.TrimPrefix(data, utf8BOM)
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, false, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(data, &keys) == nil {
		if _, ok := keys["setup_done"]; !ok {
			cfg.SetupDone = storeExists()
			decided = true
		}
	}
	repair(&cfg)
	return cfg, decided, nil
}

// repair puts back the defaults a file may have zeroed or spoiled, for every config read from disk, a recovered one included.
func repair(cfg *JuneConfig) {
	// configs written before /voice existed (or with a bad value) fall back to default
	if _, ok := NormalizeVoice(cfg.Voice); cfg.Voice == "" || !ok {
		cfg.Voice = DefaultVoice
	}

	// json.Unmarshal only overwrites keys the file actually carries, so a config with no "embed" block keeps the defaults defaultConfig set. These two guards cover the case where the block exists but zeroes a field explicitly, which would otherwise mean binding port 0 or reaping the embedding server on every tick.
	if cfg.Embed.Port <= 0 {
		cfg.Embed.Port = DefaultEmbedPort
	}
	if cfg.Embed.IdleTimeout <= 0 {
		cfg.Embed.IdleTimeout = DefaultEmbedIdleTimeout
	}
	// The tracker block needs the same two guards for the same reason: a file carrying "dwell_time_ms": 0 records every window the pointer crosses, and one carrying "blocklist": null silently turns off the password-manager blocklist that keeps 1Password and KeePassXC out of capture.
	if cfg.Tracker.DwellTime <= 0 {
		cfg.Tracker.DwellTime = DefaultDwellTime
	}
	if cfg.Tracker.Blocklist == nil {
		cfg.Tracker.Blocklist = slices.Clone(DefaultBlocklist)
	}
}

// utf8BOM is the byte order mark a file may start with. Windows PowerShell 5.1's Set-Content and Out-File -Encoding utf8 write one, and so does Notepad's "UTF-8 with BOM", and encoding/json refuses it, so a file edited that way read as broken although its JSON was fine.
var utf8BOM = []byte("\xef\xbb\xbf")

// SaveConfig persists cfg to disk, creating the data directory if needed. A june-config.json already there that will not parse is first kept beside the new one, as june-config.json.unreadable-<date>-<time>, and the next daemon to start tells the user once (see TakeSetAside): written over, a hand edit with one stray comma was gone for good, and the installer's `june --autostart` did exactly that. When it cannot be kept, nothing is written and the error wraps ErrUnreadable.
func SaveConfig(cfg JuneConfig) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	return saveLocked(cfg)
}

// saveMu keeps this process to one save at a time, so the look for a broken file and the write that replaces it are never split by another save: one that ran between them would have its good file kept aside as broken, then written over.
var saveMu sync.Mutex

// saveLocked is SaveConfig for a caller that holds saveMu.
func saveLocked(cfg JuneConfig) error {
	// The user's alone (0700, and on Windows a DACL of its own when it is outside the profile): the same directory holds the store, the IPC token and the log, so it must not depend on which of the three writers happened to create it first.
	if err := util.MkdirPrivate(DataDir()); err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if old, err := os.ReadFile(ConfigPath()); err == nil && json.Unmarshal(bytes.TrimPrefix(old, utf8BOM), new(JuneConfig)) != nil {
		aside, err := keepAside(old)
		if err != nil {
			return fmt.Errorf("%w, and it could not be kept aside, so it was not written over: %v", ErrUnreadable, err)
		}
		slog.Warn("june-config.json could not be read; kept it aside and wrote a new one", "kept_as", aside, "setup_done", cfg.SetupDone)
	}
	if err := util.WriteFilePrivate(ConfigPath(), data); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}
	return nil
}

// SetVoice validates name against AvailableVoices, updates cfg in place with the canonical spelling, and persists the change to disk.
// Invalid names are rejected and leave cfg/disk untouched.
func (cfg *JuneConfig) SetVoice(name string) error {
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

// UpdateCheckEnabled reports whether the daily release check runs: on unless the config turned it off.
func (c JuneConfig) UpdateCheckEnabled() bool { return c.UpdateCheck == nil || *c.UpdateCheck }

// FallbackAllowed reports whether a background duty may go to another signed-in brain when its own fails: allowed unless the config turned it off.
func (b BrainConfig) FallbackAllowed() bool { return b.AllowFallback == nil || *b.AllowFallback }
