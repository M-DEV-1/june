package config

// global def
const (
	// VoiceModel is the bidirectional Live API
	VoiceModel = "gemini-2.5-flash-native-audio-preview-12-2025"

	// for background tasks like memory compiler
	TextModel = "gemini-3.1-flash-lite"

	// for semantic memory embeddings (HNSW)
	EmbedModel = "gemini-embedding-2"

	// TTSModel is the one-shot (non-live) Gemini text-to-speech model used for
	// /voice preview. Distinct from VoiceModel: this is a single generateContent
	// call, not a bidirectional Live session, so a voice can be previewed
	// without touching the active conversation. Returns 24kHz mono 16-bit PCM
	// (see https://ai.google.dev/gemini-api/docs/speech-generation), matching
	// audio.Speaker's expected format exactly -- no resampling needed.
	TTSModel = "gemini-2.5-flash-preview-tts"
)
