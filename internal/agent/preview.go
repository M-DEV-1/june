package agent

import (
	"context"
	"fmt"
	"log/slog"

	"ora/internal/config"

	"google.golang.org/genai"
)

// previewPhrase is spoken back for every /voice preview -- short and fixed, so the one-shot TTS call stays cheap and the user hears the same line regardless of which voice they're trying.
const previewPhrase = "Hi, I'm Ora. This is how I sound."

// PreviewVoice synthesizes previewPhrase with voiceName via a one-shot (non-live) Gemini TTS call and plays it straight through the speaker.
// It never touches the agent's configured/persisted voice (SetVoice/GetVoice) or the Live session -- purely a "let me hear it first" side effect.
//
// Gemini TTS returns 24kHz mono 16-bit PCM (see config.TTSModel), the same format audio.Speaker already expects from the Live API, so the bytes play as-is with no conversion.
func (a *Agent) PreviewVoice(ctx context.Context, voiceName string) error {
	canonical, ok := config.NormalizeVoice(voiceName)
	if !ok {
		return fmt.Errorf("unknown voice: %q", voiceName)
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  a.apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize genai client: %w", err)
	}

	resp, err := client.Models.GenerateContent(ctx, config.TTSModel, genai.Text(previewPhrase), &genai.GenerateContentConfig{
		ResponseModalities: []string{string(genai.ModalityAudio)},
		SpeechConfig: &genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
					VoiceName: canonical,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("voice preview request failed: %w", err)
	}

	var played bool
	for _, cand := range resp.Candidates {
		if cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			if part.InlineData == nil || len(part.InlineData.Data) == 0 {
				continue
			}
			if err := a.speaker.Play(part.InlineData.Data); err != nil {
				return fmt.Errorf("failed to play voice preview: %w", err)
			}
			played = true
		}
	}
	if !played {
		return fmt.Errorf("voice preview returned no audio for %q", canonical)
	}

	slog.Debug("played voice preview", "voice", canonical)
	return nil
}
