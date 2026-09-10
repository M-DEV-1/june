package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"ora/internal/config"
	"ora/internal/obs"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"
)

// StateInterval is the shortest time allowed between two working-state derives. The loop that drives it ticks every five minutes, and before this gate existed every tick that found any new summary at all made an API call: 288 calls a day at the ceiling, and on 2026-09-04 that spent the whole 500-request free-tier day on gemini-3.5-flash-lite by noon. Ten minutes halves the ceiling to 144 and still tracks a working session closely enough, because the state paragraph describes what the user is doing rather than each thing they touched.
const StateInterval = 10 * time.Minute

// StateSummaryThreshold is how many new summary and digest nodes must land under an unchanged set of apps and window titles before the state is worth deriving again. Chosen from this store's own numbers: on 2026-09-04 it wrote 295 summary and digest nodes across about sixteen active hours, roughly three per ten-minute window, with the busiest hour writing 68 (about eleven per window). At five, a quiet stretch where the user stays in one window and a few summaries trickle in does not re-derive, while a genuinely busy stretch still does.
const StateSummaryThreshold = 5

// StateGate decides whether the working-state derive should run now, so that an unattended five-minute loop cannot spend a metered daily quota on material that has not changed. It is safe for concurrent use.
// The rule is that a derive needs both a passed time floor and changed material: at least StateInterval since the last derive, and either a different set of apps and window titles or at least StateSummaryThreshold new summaries since then.
type StateGate struct {
	mu sync.Mutex
	// lastDerive is when the last derive ran, zero before the first one.
	lastDerive time.Time
	// lastSignature is the set of apps and window titles the last derive saw, as EpisodeSignature rendered it.
	lastSignature string
	// lastSummaries is the running summary-and-digest count as of the last derive, which the next count is compared against.
	lastSummaries int
}

// ShouldDerive reports whether a working-state derive is worth making now. Input: the current time, the signature of the tracked episodes since the last derive (see EpisodeSignature), and the running count of summary and digest nodes. Output: true when the derive should run.
// The first call always returns true, because nothing has been derived yet and the cache is empty.
func (g *StateGate) ShouldDerive(now time.Time, signature string, summaries int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.lastDerive.IsZero() {
		return true
	}
	if now.Sub(g.lastDerive) < StateInterval {
		return false
	}
	if signature != g.lastSignature {
		return true
	}
	return summaries-g.lastSummaries >= StateSummaryThreshold
}

// Derived records that a derive just ran, so the next ShouldDerive compares against this moment, this signature and this summary count. Input: when the derive ran, the signature it ran on, and the summary count it ran on.
// The caller records only a derive that actually produced a state, so a failed call does not start the floor running and lose the next real change.
func (g *StateGate) Derived(at time.Time, signature string, summaries int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastDerive, g.lastSignature, g.lastSummaries = at, signature, summaries
}

// EpisodeSignature renders a set of tracked windows as one comparable string, so two derives can be asked whether the user is in the same place as before. Input: one "app|title" string per tracked episode, in any order and with repeats. Output: the distinct entries sorted and joined, which is equal for two calls exactly when they name the same set of windows.
func EpisodeSignature(windows []string) string {
	seen := make(map[string]struct{}, len(windows))
	distinct := make([]string, 0, len(windows))
	for _, w := range windows {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		distinct = append(distinct, w)
	}
	sort.Strings(distinct)
	return strings.Join(distinct, "\n")
}

// TextBackend answers one prompt in one call. It is the same shape as internal/brain.Brain, repeated here rather than imported because internal/brain imports internal/agent, which imports this package.
type TextBackend func(ctx context.Context, prompt string) (string, error)

// QuotaOrOverload reports whether err is a Gemini failure no Gemini model can fix — 429 because the day's free-tier request allowance is spent, or 503 because every model is overloaded — and so is worth handing to another provider. Input: the error from a call, possibly wrapped. Output: true only for those two, since any other failure would fail the same way anywhere.
// It repeats the rule internal/agent applies to the user's own asks rather than importing it, because internal/agent imports this package.
func QuotaOrOverload(err error) bool {
	if err == nil {
		return false
	}
	var byValue genai.APIError
	var byPointer *genai.APIError
	if errors.As(err, &byValue) {
		return byValue.Code == 429 || byValue.Code == 503
	}
	if errors.As(err, &byPointer) {
		return byPointer.Code == 429 || byPointer.Code == 503
	}
	return false
}

// SetBackgroundFallback gives the summarizer a brain to hand a prompt to when Gemini answers 429 or 503 — in the daemon, Codex under the user's ChatGPT login. Input: the fallback brain; nil turns the fallback off and a quota failure is returned as it was.
// The rule that a fallback never happens after a tool has run holds by construction here: these are one-shot prompt-in answer-out calls with no tools in the seam.
func (g *GeminiSummarizer) SetBackgroundFallback(backend TextBackend) {
	g.backendMu.Lock()
	defer g.backendMu.Unlock()
	g.backgroundFallback = backend
}

// backgroundFallbackFn reads the installed fallback brain under the lock, for the same reason stateBackendFn does.
func (g *GeminiSummarizer) backgroundFallbackFn() TextBackend {
	g.backendMu.RLock()
	defer g.backendMu.RUnlock()
	return g.backgroundFallback
}

// fallbackText hands prompt to the configured fallback when err is a quota or overload failure and a fallback exists. Input: the failing call's error and the prompt it sent. Output: the fallback's answer, or ok false when there is no fallback to try or the failure is not one another provider could fix.
func (g *GeminiSummarizer) fallbackText(ctx context.Context, err error, prompt string) (string, bool) {
	fallback := g.backgroundFallbackFn()
	if fallback == nil || !QuotaOrOverload(err) {
		return "", false
	}
	slog.Warn("background summarizer handing over to the fallback brain", "error", err)
	text, fbErr := fallback(ctx, prompt)
	if fbErr != nil {
		slog.Warn("background fallback brain failed too", "error", fbErr)
		return "", false
	}
	return strings.TrimSpace(text), true
}

// stateInstruction is everything the model is told before the material itself. The voice rules matter as much as the length: without them the model writes a comma-chain of every thing the user touched, named after the apps it happened in, which reads as a log rather than as a person's own account of their day.
const stateInstruction = `You are the working-memory module for an OS companion.
Write a SHORT present-tense paragraph (<= 120 words) addressed to the user as "you", in their own words. Name the thing, not the file or app it lives in. Say what they decided and what they are still deciding.
One thing leads; two topics a sentence at most; never a comma-chain of parallel items; no tool or pipeline words.
Plain text, no bullets or headings.`

// DeriveState synthesizes a short present-tense working-state summary (<= ~120 words, addressed to the user as "you", in their own words for their own work) from recent episodic summaries and stable notes.
// The result is stored as a single-row cache (working_state) and injected into GetImplicitContext in place of the raw summary dump.
// Returns "", nil immediately when both inputs are empty — no API call made.
func (g *GeminiSummarizer) DeriveState(ctx context.Context, recentSummaries []string, notes []string) (string, error) {
	if len(recentSummaries) == 0 && len(notes) == 0 {
		return "", nil
	}

	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "GeminiSummarizer.DeriveState")
	defer span.End()

	// The local backend below and the genai client above both answer over HTTP with no timeout of their own.
	ctx, cancel := context.WithTimeout(ctx, summarizerCallTimeout)
	defer cancel()

	var parts []string
	if len(notes) > 0 {
		parts = append(parts, "Stable facts about the user:\n"+strings.Join(notes, "\n"))
	}
	if len(recentSummaries) > 0 {
		numbered := make([]string, len(recentSummaries))
		for i, s := range recentSummaries {
			numbered[i] = fmt.Sprintf("%d. %s", i+1, s)
		}
		parts = append(parts, "Recent activity summaries (newest first):\n"+strings.Join(numbered, "\n"))
	}

	// Every pending summary goes into this one prompt, so a batch of new material costs one call rather than one call each.
	prompt := fmt.Sprintf("%s\n\n%s", stateInstruction, strings.Join(parts, "\n\n"))

	_, genSpan := tracer.Start(ctx, "Generate.DeriveState")
	text, err := g.text(ctx, config.JobWorkingState, prompt, false)
	if err != nil {
		genSpan.RecordError(err)
		genSpan.End()
		if fallback, ok := g.fallbackText(ctx, err, prompt); ok {
			return fallback, nil
		}
		return "", fmt.Errorf("derive state: %w", err)
	}
	genSpan.End()
	return strings.TrimSpace(text), nil
}
