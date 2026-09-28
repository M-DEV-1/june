// Package brain is ORA's one-shot text seam: one prompt in, one answer out, with a choice of backend behind it.
// It exists so the duties that only need a prompt answered — the meeting minutes and the personal context updater — can run on the Gemini API or on the Claude Code login the machine already has, without either of them knowing which. The voice assistant is not one of these duties: that is a bidirectional Gemini Live session and stays on genai.
package brain

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"ora/internal/config"

	"google.golang.org/genai"
)

// ErrNoBackend is what every call to a brain built for a provider this package has no code for comes back with. It exists because the alternative — quietly answering on the Gemini API instead — spends the metered free tier the user picked another brain to avoid and sends the prompt to a provider they did not choose. RoutedFor treats it as a reason to hand the prompt on to the next provider without marking this one as refusing, so a caller that cannot build Codex still gets its duty answered.
var ErrNoBackend = errors.New("this brain has no backend on this machine")

// ErrLocalFailure marks a failure that happened on this machine before a request could have reached the provider, so the daily quota counter knows to hand back the slot it reserved. It is only ever wrapped into another error and matched with errors.Is.
var ErrLocalFailure = errors.New("the call never reached the provider")

// failing returns a Brain that answers every prompt with err and calls nothing. Input: the error to return. Output: that Brain.
func failing(err error) Brain {
	return func(context.Context, string) (string, error) { return "", err }
}

// NoBackendNote says why this package cannot answer for a provider, in the voice GET /brains uses for a row's limits_note. Input: a config.Brain* provider string. Output: the reason, or "" when that provider does have a backend here.
func NoBackendNote(provider string) string {
	if provider == config.BrainOllama {
		return "Ora has no Ollama backend yet, so this brain cannot answer its duties"
	}
	return ""
}

// Brain answers one prompt in one call. It is the exact shape of the recorder's minutes seam, so any of the constructors below can be assigned straight to it.
type Brain func(ctx context.Context, prompt string) (string, error)

// FromConfig returns the backend cfg names, given the Gemini API key for the default path.
// An empty or unrecognised provider is the Gemini API, so a config file written before this block existed — or one with a typo in it — keeps working exactly as it did.
// asker is optional and only ever read for config.BrainCodex: it is how a caller that already holds an *agent.Agent (the daemon, wrapping it as agent.CodexBrain) lets this provider answer through the user's ChatGPT login; a caller with no asker to give, such as the meeting-minutes recorder, omits it and gets a brain that fails with ErrNoBackend.
func FromConfig(cfg config.BrainConfig, apiKey string, asker ...CodexAsker) Brain {
	timeout := cfg.TimeoutSeconds
	if timeout <= 0 {
		timeout = config.DefaultBrainTimeoutSeconds
	}
	switch cfg.Provider {
	case config.BrainClaudeCLI:
		// cfg.Model rides through to `--model`, so the writing duties can be pinned to a cheaper tier than the login's default.
		return ClaudeCLI(cmp.Or(cfg.Binary, "claude"), cfg.Model, timeout)
	case config.BrainAgyCLI:
		// cfg.Model rides through to --model, the same way it does for claude.
		return AgyCLI(cmp.Or(cfg.Binary, "agy"), timeout, cfg.Model)
	case config.BrainGrokCLI:
		return GrokCLI(cmp.Or(cfg.Binary, "grok"), timeout, cfg.Model)
	case config.BrainCodex:
		if len(asker) > 0 && asker[0] != nil {
			return FromAsker(asker[0])
		}
		return failing(fmt.Errorf("%w: the codex brain answers through a ChatGPT asker and this caller supplied none", ErrNoBackend))
	case config.BrainOllama:
		return failing(fmt.Errorf("%w: %s", ErrNoBackend, NoBackendNote(config.BrainOllama)))
	case "", config.BrainGeminiAPI:
	default:
		slog.Warn("unknown brain provider in config, using the Gemini API", "provider", cfg.Provider)
	}
	return GeminiAPI(apiKey, geminiModel(cfg), timeout)
}

// geminiModel is the model name the Gemini API is actually called with for cfg. Input: the brain block. Output: cfg.Model when cfg is a Gemini config, and config.TextModel otherwise — because a model name means something only to the provider it was written for, and every path here that is not a Gemini config is a fallback from some other provider.
// The 2026-09-05 config named provider "codex-direct" with model "gpt-5.5". FromConfig's Codex case then fell back to Gemini when its caller supplied no asker, which the meeting summariser did not, and the model name rode along into the SDK: every hourly retry of that day's stuck recording failed with "models/gpt-5.5 is not found for API version v1beta".
func geminiModel(cfg config.BrainConfig) string {
	if cfg.Provider != "" && cfg.Provider != config.BrainGeminiAPI {
		return config.TextModel
	}
	return cmp.Or(cfg.Model, config.TextModel)
}

// GeminiAPI answers with one non-streaming GenerateContent call, the same shape memory.GeminiSummarizer uses for its background summaries. This is what ORA has always done and stays the default.
// Input: the API key, the model name, and a hard timeout in seconds — passed to the SDK as HTTPOptions.Timeout, which is what it puts on the request's own context. Output: the answer text.
// The timeout is not optional comfort: the caller's context is the daemon's root context, which never ends, and the SDK's own http.Client has no timeout, so a connection that dies silently (a suspend, a Wi-Fi drop mid-TLS) used to block the proactive scheduler's single goroutine until the daemon was restarted.
func GeminiAPI(apiKey, model string, timeoutSeconds int) Brain {
	if timeoutSeconds <= 0 {
		timeoutSeconds = config.DefaultBrainTimeoutSeconds
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	return func(ctx context.Context, prompt string) (string, error) {
		if apiKey == "" {
			return "", fmt.Errorf("%w: no GEMINI_API_KEY, cannot generate text (whatever was to be summarised is still on disk)", ErrLocalFailure)
		}
		client, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey:      apiKey,
			Backend:     genai.BackendGeminiAPI,
			HTTPOptions: genai.HTTPOptions{Timeout: &timeout},
		})
		if err != nil {
			return "", fmt.Errorf("%w: gemini client: %v", ErrLocalFailure, err)
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

// GeminiModelFor mirrors FromConfig's own provider switch to say whether cfg — called the way the daemon calls FromConfig, with no CodexAsker — ends up on the Gemini API, and if so, which model. WithDailyQuota needs this so it never meters a call under a Gemini model's daily count that was never sent to Gemini: the three CLI providers answer elsewhere, and Codex without an asker and Ollama now fail with ErrNoBackend rather than falling back, so all five return false here. Input: the BrainConfig FromConfig would build a brain from. Output: the model name FromConfig would call, and whether that model is on the Gemini API.
// It reads the model through geminiModel for the same reason FromConfig does, so the two never disagree about which model a fallback config is metered and called under.
func GeminiModelFor(cfg config.BrainConfig) (model string, ok bool) {
	switch cfg.Provider {
	case config.BrainClaudeCLI, config.BrainAgyCLI, config.BrainGrokCLI, config.BrainCodex, config.BrainOllama:
		return "", false
	default:
		return geminiModel(cfg), true
	}
}
