package config

// global def
const (
	// VoiceModel is the bidirectional Live API
	VoiceModel = "gemini-2.5-flash-native-audio-preview-12-2025"

	// for background tasks like memory compiler.
	// gemini-3.5-flash-lite supersedes 3.1 at the same cost-effective positioning (2026-07-21), verified current against ai.google.dev.
	TextModel = "gemini-3.5-flash-lite"

	// for semantic memory embeddings (hybrid FTS5+vector search).
	// ora uses the public Gemini Developer API (see connect.go/compiler.go), where this model is "gemini-embedding-2-preview" -- "gemini-embedding-2" (no -preview) is the Vertex AI identifier and fails on ora's API surface. Verified against ai.google.dev 2026-07-24.
	// Pick output_dimensionality (1536/3072) for retrieval quality, not to hit a RAM target -- RAM is soft, never traded against quality.
	EmbedModel = "gemini-embedding-2-preview"

	// TTSModel is the one-shot (non-live) TTS model used for /voice preview -- a single generateContent call, not a Live session, so previewing doesn't touch the active conversation.
	// Returns 24kHz mono 16-bit PCM (https://ai.google.dev/gemini-api/docs/speech-generation), matching audio.Speaker's format exactly -- no resampling needed.
	// gemini-3.1-flash-tts-preview supersedes 2.5 with more language coverage and audio tags for steering delivery, verified against ai.google.dev 2026-07-24.
	TTSModel = "gemini-3.1-flash-tts-preview"
)
