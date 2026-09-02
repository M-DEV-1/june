// Package brain is ORA's one-shot text seam: one prompt in, one answer out, with a choice of backend behind it.
// It exists so the duties that only need a prompt answered — the meeting minutes and the personal context updater — can run on the Gemini API or on the Claude Code login the machine already has, without either of them knowing which. The voice assistant is not one of these duties: that is a bidirectional Gemini Live session and stays on genai.
package brain

import (
	"context"
	"fmt"
	"log/slog"

	"ora/internal/config"

	"google.golang.org/genai"
)

// Brain answers one prompt in one call. It is the exact shape of the recorder's minutes seam, so any of the constructors below can be assigned straight to it.
type Brain func(ctx context.Context, prompt string) (string, error)

// FromConfig returns the backend cfg names, given the Gemini API key for the default path.
// An empty or unrecognised provider is the Gemini API, so a config file written before this block existed — or one with a typo in it — keeps working exactly as it did.
func FromConfig(cfg config.BrainConfig, apiKey string) Brain {
	timeout := cfg.TimeoutSeconds
	if timeout <= 0 {
		timeout = config.DefaultBrainTimeoutSeconds
	}
	switch cfg.Provider {
	case config.BrainClaudeCLI:
		// cfg.Model rides through to `--model`, so the writing duties can be pinned to a cheaper tier than the login's default.
		return ClaudeCLI(or(cfg.Binary, "claude"), cfg.Model, timeout)
	case config.BrainAgyCLI:
		return AgyCLI(or(cfg.Binary, "agy"), timeout)
	case config.BrainGrokCLI:
		return GrokCLI(or(cfg.Binary, "grok"), timeout)
	case "", config.BrainGeminiAPI:
	default:
		slog.Warn("unknown brain provider in config, using the Gemini API", "provider", cfg.Provider)
	}
	return GeminiAPI(apiKey, or(cfg.Model, config.TextModel))
}

// GeminiAPI answers with one non-streaming GenerateContent call, the same shape memory.GeminiSummarizer uses for its background summaries. This is what ORA has always done and stays the default.
func GeminiAPI(apiKey, model string) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		if apiKey == "" {
			return "", fmt.Errorf("no GEMINI_API_KEY, cannot generate text (whatever was to be summarised is still on disk)")
		}
		client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: apiKey, Backend: genai.BackendGeminiAPI})
		if err != nil {
			return "", fmt.Errorf("gemini client: %w", err)
		}
		resp, err := client.Models.GenerateContent(ctx, model, genai.Text(prompt), nil)
		if err != nil {
			return "", fmt.Errorf("generate: %w", err)
		}
		text := resp.Text()
		if text == "" {
			return "", fmt.Errorf("gemini returned no text")
		}
		return text, nil
	}
}

// or returns s, or fallback when s is empty.
func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
