package agent

import (
	"context"
	"fmt"
	"log/slog"

	"ora/internal/config"

	"google.golang.org/genai"
)

// previewPhrase is spoken back for every /voice preview -- short and fixed, so the one-shot TTS call stays cheap and the user hears the same line regardless of which voice they're trying.
// It deliberately claims no identity. Measured against the TTS model on 2026-09-07: "Hi, I'm Ora. This is how I sound." and "Hi, I am Ora. This is how I sound." both came back with no candidates and promptFeedback.blockReason PROHIBITED_CONTENT, while this line alone returned audio -- a synthetic voice introducing itself by name reads to the filter as impersonation. Keep any future wording free of "I am <name>".
const previewPhrase = "This is how I sound."

// PreviewVoice synthesizes previewPhrase with voiceName via a one-shot (non-live) Gemini TTS call and plays it straight through the agent's own speaker.
// It never touches the agent's configured/persisted voice (SetVoice/GetVoice) or the Live session -- purely a "let me hear it first" side effect.
func (a *Agent) PreviewVoice(ctx context.Context, voiceName string) error {
	return SpeakPreview(ctx, a.apiKey, voiceName, a.speaker.Play)
}

// SpeakPreview synthesizes the preview line in one voice and hands the audio to play. It takes no Agent, so the daemon's /voices/preview route can speak a voice with nothing but a key and a speaker, outside any live session.
// Input: a context, the Gemini API key, the voice in any casing, and what to do with each chunk of audio. Output: an error for a name that is not a Gemini voice, a failed call, a failed playback, or a response carrying no audio at all.
//
// Gemini TTS returns 24kHz mono 16-bit PCM (see config.TTSModel), the same format audio.Speaker already expects from the Live API, so the bytes play as-is with no conversion.
func SpeakPreview(ctx context.Context, apiKey, voiceName string, play func([]byte) error) error {
	canonical, ok := config.NormalizeVoice(voiceName)
	if !ok {
		return fmt.Errorf("unknown voice: %q", voiceName)
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
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
			if err := play(part.InlineData.Data); err != nil {
				return fmt.Errorf("failed to play voice preview: %w", err)
			}
			played = true
		}
	}
	if !played {
		return fmt.Errorf("voice preview for %q: %s", canonical, noAudioReason(resp))
	}

	slog.Debug("played voice preview", "voice", canonical)
	return nil
}

// noAudioReason says why a TTS response carried no audio, so a refusal is not reported as an empty answer. Input: the response, which may be nil. Output: the safety block reason when the prompt was refused, and a plain sentence otherwise.
// The two are indistinguishable in the response shape -- both are a response with no usable parts -- and reporting them the same way sent a blocked preview back to the window as "returned no audio", which reads as a bug in Ora rather than a refusal by the model.
func noAudioReason(resp *genai.GenerateContentResponse) string {
	if resp != nil && resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
		return fmt.Sprintf("the model refused the line it was given (%s)", resp.PromptFeedback.BlockReason)
	}
	return "the model returned no audio"
}
