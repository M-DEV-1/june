package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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

	// A voice sounds the same every time it says the same line, so it is synthesised once and kept. This is not an optimisation: the TTS model's free tier allows ten requests a day for the whole project (measured 2026-09-07 against gemini-3.1-flash-tts, quotaValue 10), and a picker with thirty voices in it would spend the day's allowance in a minute of listening. Cached, the whole roster costs thirty calls once and nothing afterwards.
	if pcm, err := os.ReadFile(previewPath(canonical)); err == nil && len(pcm) > 0 {
		return play(pcm)
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
		if quotaSpent(err) {
			return fmt.Errorf("no voice previews left today: the free tier allows ten a day for the whole project, and they are kept once played so each voice costs one call ever")
		}
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
			savePreview(canonical, part.InlineData.Data)
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

// previewDir is where the synthesised previews are kept, one file per voice, under the data directory beside everything else Ora stores.
func previewDir() string { return filepath.Join(config.DataDir(), "voice-previews") }

// previewPath is the file one voice's preview audio is kept in. Input: the canonical voice name. Output: the path. The name is a bare word from config.AvailableVoices, so it is used as the filename directly.
func previewPath(voice string) string {
	return filepath.Join(previewDir(), strings.ToLower(voice)+".pcm")
}

// savePreview writes one voice's audio so the next preview of it costs no request. Input: the canonical voice name and the raw PCM. Output: none — a failure to write is logged and nothing else, since a preview that played is a success whether or not it could be kept.
func savePreview(voice string, pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	if err := os.MkdirAll(previewDir(), 0o700); err != nil {
		slog.Debug("could not make the voice preview directory", "error", err)
		return
	}
	if err := os.WriteFile(previewPath(voice), pcm, 0o600); err != nil {
		slog.Debug("could not keep the voice preview", "voice", voice, "error", err)
	}
}

// quotaSpent reports whether err is the TTS model's daily allowance being spent rather than any other failure, so the window can say "no previews left today" instead of "could not play that voice".
func quotaSpent(err error) bool {
	if err == nil {
		return false
	}
	var apiErr genai.APIError
	if errors.As(err, &apiErr) && apiErr.Code == 429 {
		return true
	}
	return strings.Contains(err.Error(), "RESOURCE_EXHAUSTED") || strings.Contains(err.Error(), "exceeded your current quota")
}
