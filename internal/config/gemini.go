package config

// global def
const (
	// VoiceModel is the bidirectional Live API
	VoiceModel = "gemini-2.5-flash-native-audio-preview-12-2025"

	// for background tasks like memory compiler
	TextModel = "gemini-3.1-flash-lite"

	// for semantic memory embeddings (HNSW)
	EmbedModel = "gemini-embedding-2"
)
