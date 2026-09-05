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
// asker is optional and only ever read for config.BrainCodex: it is how a caller that already holds an *agent.Agent (the daemon, wrapping it as agent.CodexBrain) lets this provider answer through the user's ChatGPT login instead of falling back to Gemini; a caller with no asker to give, such as the meeting-minutes recorder, simply omits it.
func FromConfig(cfg config.BrainConfig, apiKey string, asker ...CodexAsker) Brain {
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
	case config.BrainCodex:
		if len(asker) > 0 && asker[0] != nil {
			return FromAsker(asker[0])
		}
		slog.Warn("codex brain configured but no asker was supplied, using the Gemini API", "provider", cfg.Provider)
	case config.BrainOllama:
		slog.Warn("ollama brain configured but has no backend in this package yet, using the Gemini API", "provider", cfg.Provider)
	case "", config.BrainGeminiAPI:
	default:
		slog.Warn("unknown brain provider in config, using the Gemini API", "provider", cfg.Provider)
	}
	return GeminiAPI(apiKey, geminiModel(cfg))
}

// geminiModel is the model name the Gemini API is actually called with for cfg. Input: the brain block. Output: cfg.Model when cfg is a Gemini config, and config.TextModel otherwise — because a model name means something only to the provider it was written for, and every path here that is not a Gemini config is a fallback from some other provider.
// The 2026-09-05 config named provider "codex-direct" with model "gpt-5.5". FromConfig's Codex case falls back to Gemini when its caller supplies no asker, which the meeting summariser does not, and the model name rode along into the SDK: every hourly retry of that day's stuck recording failed with "models/gpt-5.5 is not found for API version v1beta".
func geminiModel(cfg config.BrainConfig) string {
	if cfg.Provider != "" && cfg.Provider != config.BrainGeminiAPI {
		return config.TextModel
	}
	return or(cfg.Model, config.TextModel)
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

// GeminiModelFor mirrors FromConfig's own provider switch to say whether cfg — called the way the daemon calls FromConfig, with no CodexAsker — ends up on the Gemini API, and if so, which model. WithDailyQuota needs this so it never meters a Claude CLI, agy, or Grok call under a Gemini model's daily count: without an asker, FromConfig's BrainCodex and BrainOllama cases fall back to Gemini too, same as an empty or unrecognised provider, so only the three real CLI providers return false here. Input: the BrainConfig FromConfig would build a brain from. Output: the model name FromConfig would call, and whether that model is on the Gemini API.
// It reads the model through geminiModel for the same reason FromConfig does, so the two never disagree about which model a fallback config is metered and called under.
func GeminiModelFor(cfg config.BrainConfig) (model string, ok bool) {
	switch cfg.Provider {
	case config.BrainClaudeCLI, config.BrainAgyCLI, config.BrainGrokCLI:
		return "", false
	default:
		return geminiModel(cfg), true
	}
}

// or returns s, or fallback when s is empty.
func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
