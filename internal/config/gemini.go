package config

import (
	"strings"
	"sync"
)

// global def
const (
	// VoiceModel is the bidirectional Live API model, on 3.1 Flash Live since 2026-09-02 as a trial; the previous model was "gemini-2.5-flash-native-audio-preview-12-2025" and switching back is this one string. The 3.x models close with "You exceeded your current quota" when Google Search grounding is sent beside the function tools, so liveToolsFor drops it for them. Anything that only one generation supports (proactive audio, the thinking budget versus thinking level) is gated on VoiceModelIsLive3 rather than on this string.
	VoiceModel = "gemini-3.1-flash-live-preview" // the speech front; the brain is the branch delegate. Measured 2026-09-03 with the same spoken question and Ora's real handshake: 3.1 first audio at 1.9 s, 2.5 at 5.4 to 7.9 s, and 2.5 with proactive audio at 8.1 s. 2.5 ("gemini-2.5-flash-native-audio-preview-12-2025") keeps affective dialog and proactive audio, which 3.1 lacks; switch back here if tone matters more than the four seconds.

	// for background tasks like memory compiler.
	// gemini-3.5-flash-lite supersedes 3.1 at the same cost-effective positioning (2026-07-21), verified current against ai.google.dev.
	// TextModel: gemini-3.5-flash. The lite model answered 503 UNAVAILABLE for the whole evening of 2026-09-04 while 3.5-flash answered in 6 s; probed again then, 3.6-flash answered in 12 s, 3.7 and 3.8 were also 503, 3-flash-preview took 30 s.
	TextModel = "gemini-3.5-flash"
	// TextFallbackModel answers an ask when TextModel returns 503 UNAVAILABLE.
	TextFallbackModel = "gemini-3.6-flash"

	// SubtaskModel answers the branch tool's background search. It is deliberately not TextModel: the free tier gives 3.5-flash 20 requests a day and the lite model several hundred, and a background search that spent the same budget as the answers themselves left nothing for the answers — on 2026-09-06 every branch call came back "You exceeded your current quota" (HTTP 429) while flash sat at its 20-call ceiling and lite had used 51 of its own allowance. A search that reads back memory is also a smaller job than the answer it feeds.
	SubtaskModel = "gemini-3.5-flash-lite"

	// TTSModel is the one-shot (non-live) TTS model used for /voice preview -- a single generateContent call, not a Live session, so previewing doesn't touch the active conversation.
	// Returns 24kHz mono 16-bit PCM (https://ai.google.dev/gemini-api/docs/speech-generation), matching audio.Speaker's format exactly -- no resampling needed.
	// gemini-3.1-flash-tts-preview supersedes 2.5 with more language coverage and audio tags for steering delivery, verified against ai.google.dev 2026-07-24.
	TTSModel = "gemini-3.1-flash-tts-preview"
)

// The background job names accepted as keys in OraConfig.BackgroundModels, one per unattended duty that reaches a metered model. They are strings rather than an enum so a user can pin a model per job in the config file by name.
const (
	// JobWorkingState is the five-minute working-state derive in internal/memory.DeriveState.
	JobWorkingState = "working_state"
	// JobEpisodeSummary is the per-episode summary the memory compiler writes as the user works.
	JobEpisodeSummary = "episode_summary"
	// JobEpisodicCompaction is the twelve-hourly roll-up of fine-grained summaries into daily digests.
	JobEpisodicCompaction = "episodic_compaction"
	// JobNoteConsolidation is the six-hourly merge of near-duplicate notes.
	JobNoteConsolidation = "note_consolidation"
	// JobPersonalContext is the personal-context updater that keeps the stable facts about the user current.
	JobPersonalContext = "personal_context"
	// JobMeetingMinutes is the one-shot write-up of a finished meeting's transcript.
	JobMeetingMinutes = "meeting_minutes"
	// JobScreenSight is the vision call that reads what is on the screen during a capture.
	JobScreenSight = "screen_sight"
	// JobDream is the overnight dreaming stages.
	JobDream = "dream"
)

// DefaultBackgroundModel is the model every unattended job runs on unless the config pins another. It is the lite model on purpose, and the reason is a per-day request count rather than a price: the 429 bodies in ora.log name a free-tier limit of 500 requests a day for gemini-3.5-flash-lite against 20 a day for gemini-3.5-flash, so one unattended job left on the latter can spend the user's whole day before they ask anything.
// TextModel stays on gemini-3.5-flash for the asks the user actually makes and waits on, where the lite model's 503s during the evening of 2026-09-04 would be felt.
const DefaultBackgroundModel = "gemini-3.5-flash-lite"

// backgroundModels is the per-job model map last loaded from the config, read by BackgroundModel. A job absent from it runs on DefaultBackgroundModel.
var backgroundModels map[string]string

// backgroundModelsMu guards backgroundModels, which the daemon writes once at startup and several background goroutines then read.
var backgroundModelsMu sync.RWMutex

// SetBackgroundModels installs the per-job model map that BackgroundModel reads, normally from OraConfig.BackgroundModels at daemon startup. Input: job name to model name; a nil or empty map puts every job back on DefaultBackgroundModel.
func SetBackgroundModels(models map[string]string) {
	backgroundModelsMu.Lock()
	defer backgroundModelsMu.Unlock()
	backgroundModels = models
}

// BackgroundModel returns the model the named background job should call. Input: one of the Job* names. Output: the model pinned for that job in the config, or DefaultBackgroundModel when the config names none — including for a job name it does not recognise, so a typo in the config file degrades to the safe cheap model rather than to an empty model name that would fail every call.
func BackgroundModel(job string) string {
	backgroundModelsMu.RLock()
	defer backgroundModelsMu.RUnlock()
	if m := strings.TrimSpace(backgroundModels[job]); m != "" {
		return m
	}
	return DefaultBackgroundModel
}

// BackgroundBrainConfig returns cfg with its Model filled in from BackgroundModel(job) when the config names no model of its own and the provider is the Gemini API. Input: the brain block a background duty is about to run on, and the Job* name it runs as. Output: the same block with the background model pinned, or unchanged.
// A CLI provider is left alone on purpose: its Model field carries a CLI alias such as "sonnet", and writing a Gemini model name into it would be passed to the CLI as --model and fail the call.
func BackgroundBrainConfig(cfg BrainConfig, job string) BrainConfig {
	if cfg.Model != "" {
		return cfg
	}
	if cfg.Provider != "" && cfg.Provider != BrainGeminiAPI {
		return cfg
	}
	cfg.Model = BackgroundModel(job)
	return cfg
}
