package config

import "strings"

// global def
const (
	// VoiceModel is the bidirectional Live API model, on 3.1 Flash Live since 2026-09-02 as a trial; the previous model was "gemini-2.5-flash-native-audio-preview-12-2025" and switching back is this one string. The 3.x models close with "You exceeded your current quota" when Google Search grounding is sent beside the function tools, so liveToolsFor drops it for them. Anything that only one generation supports (proactive audio, the thinking budget versus thinking level) is gated on VoiceModelIsLive3 rather than on this string.
	VoiceModel = "gemini-3.1-flash-live-preview"

	// for background tasks like memory compiler.
	// gemini-3.5-flash-lite supersedes 3.1 at the same cost-effective positioning (2026-07-21), verified current against ai.google.dev.
	TextModel = "gemini-3.5-flash-lite"

	// TTSModel is the one-shot (non-live) TTS model used for /voice preview -- a single generateContent call, not a Live session, so previewing doesn't touch the active conversation.
	// Returns 24kHz mono 16-bit PCM (https://ai.google.dev/gemini-api/docs/speech-generation), matching audio.Speaker's format exactly -- no resampling needed.
	// gemini-3.1-flash-tts-preview supersedes 2.5 with more language coverage and audio tags for steering delivery, verified against ai.google.dev 2026-07-24.
	TTSModel = "gemini-3.1-flash-tts-preview"
)

// VoiceModelIsLive3 reports whether the Live model is a Gemini 3 generation one. The 3.x Live models take a thinking level instead of a thinking budget and, as of 2026-09-02, do not support proactive audio or affective dialog, so a config carrying those fields must not be sent to them.
func VoiceModelIsLive3() bool {
	return strings.HasPrefix(VoiceModel, "gemini-3")
}
