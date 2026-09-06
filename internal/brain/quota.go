// quota.go caps how many requests a metered Gemini model may take in one local day, so an unattended job cannot spend a free-tier allowance the user needed for something they were waiting on. It sits beside tally.Wrap (which counts every call for the weekly self-log) as a second, narrower decorator that can actually refuse a call before it happens.
package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ora/internal/config"
	"ora/internal/util"

	"google.golang.org/genai"
)

// ErrDailyQuota is returned when a model's configured daily request ceiling has already been reached for today, so the call is refused before it ever reaches Gemini. Unwrap returns a genai.APIError{Code: 429}, the exact shape a real quota-exhausted response from Gemini itself would take, so agent.GeminiCannotAnswer and the codex/claude fallback chain that already checks for it treat this refusal the same way as a real one.
type ErrDailyQuota struct {
	Model string
	Limit int
}

func (e *ErrDailyQuota) Error() string {
	return fmt.Sprintf("%s: daily request quota of %d reached", e.Model, e.Limit)
}

func (e *ErrDailyQuota) Unwrap() error {
	return genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: e.Error()}
}

// QuotaLimit is one model's daily request ceiling, and how much of it stays off-limits to a background caller so an interactive ask made later in the day always has requests left to spend.
type QuotaLimit struct {
	Limit    int
	Reserved int
}

// DefaultQuotaLimit is the ceiling any Gemini model with no measured entry of its own is metered against. It is the flash number rather than the lite one on purpose: an unmeasured model could be either, and metering a 500-a-day model at 20 costs the user some background work, while leaving a 20-a-day model uncapped costs them the whole day's interactive allowance. GeminiDaily says in its note when a bar is drawn against this guess.
var DefaultQuotaLimit = QuotaLimit{Limit: 20, Reserved: 8}

// QuotaOptions maps a Gemini model name to its measured daily ceiling. Use For rather than indexing it: a Gemini model with no entry is metered against DefaultQuotaLimit, and only a name from another provider's namespace is left unmetered.
// These belong in config.BrainConfig once another agent adds fields for them (for example DailyRequestLimit and ReservedForAsks per model, or a map keyed by model name); until then the daemon fills this from DefaultQuotaOptions.
type QuotaOptions map[string]QuotaLimit

// DefaultQuotaOptions is the ceiling observed in ora.log's 429 bodies and recorded in internal/config/gemini.go's comments: Google's free tier allows 20 requests a day for gemini-3.5-flash and 500 for gemini-3.5-flash-lite. 8 and 100 requests respectively stay reserved for interactive asks.
func DefaultQuotaOptions() QuotaOptions {
	return QuotaOptions{
		"gemini-3.5-flash":      {Limit: 20, Reserved: 8},
		"gemini-3.5-flash-lite": {Limit: 500, Reserved: 100},
	}
}

// For is the daily ceiling to meter a model against. Input: the model name FromConfig would call. Output: the entry named for that model and known=true; failing that, DefaultQuotaLimit and known=false for any other gemini- model, so a model nobody has measured is still capped; and the zero QuotaLimit for a name that is not a Gemini model at all, which WithDailyQuota reads as "do not meter".
func (o QuotaOptions) For(model string) (limit QuotaLimit, known bool) {
	if l, ok := o[model]; ok {
		return l, true
	}
	if strings.HasPrefix(model, "gemini-") {
		return DefaultQuotaLimit, false
	}
	return QuotaLimit{}, false
}

// QuotaState is the on-disk counter WithDailyQuota reads and increments, shared by every wrapped brain in the process (and, across a restart, by the same file on disk) so a limit is enforced against the true total rather than per-wrapper.
type QuotaState struct {
	mu   sync.Mutex
	path string
}

// dayCounts is today's-and-other-days' request counts, model name to count, keyed by local calendar day. Old days are never pruned: the file holds one int per model per day ever seen, which for a handful of models over a year is a few thousand bytes.
type dayCounts map[string]map[string]int

// NewQuotaState returns a counter backed by a small JSON file under dataDir, created on first use.
func NewQuotaState(dataDir string) *QuotaState {
	return &QuotaState{path: filepath.Join(dataDir, "brain_quota.json")}
}

// load reads the counter file, treating a missing or unreadable file as an empty count rather than an error: a fresh install or a corrupt file should meter from zero, not refuse to run.
func (s *QuotaState) load() dayCounts {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return dayCounts{}
	}
	var counts dayCounts
	if err := json.Unmarshal(data, &counts); err != nil {
		slog.Warn("brain: quota file unreadable, metering from zero", "path", s.path, "error", err)
		return dayCounts{}
	}
	return counts
}

// save writes the counter file. A failure is logged, not returned: losing today's count file is a smaller problem than refusing every brain call over it.
func (s *QuotaState) save(counts dayCounts) {
	data, err := json.Marshal(counts)
	if err != nil {
		slog.Warn("brain: could not encode quota counts", "error", err)
		return
	}
	if err := util.WriteFileAtomic(s.path, data, 0o600); err != nil {
		slog.Warn("brain: could not write quota file", "path", s.path, "error", err)
	}
}

// take reports whether model already had cap or more requests today and, if not, records one more. It holds the state's lock for the whole read-check-write so two goroutines racing on the same model never both slip through on the last request. Input: the model name, today's cap for the caller asking (already reservation-adjusted). Output: true when the request is refused.
func (s *QuotaState) take(model string, cap int) (refused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	counts := s.load()
	if counts[today] == nil {
		counts[today] = map[string]int{}
	}
	if counts[today][model] >= cap {
		return true
	}
	counts[today][model]++
	s.save(counts)
	return false
}

// refund hands back one request take reserved for model today, never going below zero. Input: the model name. Output: none.
func (s *QuotaState) refund(model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	counts := s.load()
	if counts[today][model] <= 0 {
		return
	}
	counts[today][model]--
	s.save(counts)
}

// reachedTheProvider reports whether err leaves the request counted. Input: the error primary returned, or nil. Output: true when Google could have billed the call — an answer, an answer with no text in it, or a refusal that came back over the wire — and false for a failure that happened on this machine: no API key, a client that could not be built, a dead network, a cancelled or expired context.
// take reserves its slot before the call so two goroutines cannot both take the last one; this is what decides whether that reservation is kept.
func reachedTheProvider(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, ErrLocalFailure) || errors.Is(err, ErrNoBackend) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A genai.APIError is Google's own answer, so it counts even though it is an error; a *url.Error or any other net.Error is this machine failing to reach it.
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return true
	}
	var netErr net.Error
	return !errors.As(err, &netErr)
}

// WithDailyQuota returns a Brain that refuses with ErrDailyQuota once model's shared daily count reaches its cap. Otherwise it reserves one request, calls primary, and hands the reservation back when the call never reached the provider (see reachedTheProvider), so the count mirrors what Google could bill rather than counting an offline laptop's every retry. Input: the shared counter, the model this brain calls, whether this wrapping is the interactive-ask band (true) or a background job (false, capped at Limit minus Reserved so nightly jobs cannot spend requests an ask would need), the configured limits, and the brain to guard. Output: a Brain identical to primary for a model opts sets no ceiling for.
func WithDailyQuota(state *QuotaState, model string, forAsks bool, opts QuotaOptions, primary Brain) Brain {
	limit, _ := opts.For(model)
	if limit.Limit <= 0 {
		return primary
	}
	dayCap := limit.Limit
	if !forAsks {
		dayCap -= limit.Reserved
		if dayCap < 0 {
			dayCap = 0
		}
	}
	return func(ctx context.Context, prompt string) (string, error) {
		if state.take(model, dayCap) {
			slog.Warn("brain: daily request quota reached, refusing before the call", "model", model, "cap", dayCap, "for_asks", forAsks)
			return "", &ErrDailyQuota{Model: model, Limit: dayCap}
		}
		answer, err := primary(ctx, prompt)
		if !reachedTheProvider(err) {
			state.refund(model)
		}
		return answer, err
	}
}

// Metered is the daemon's one-line call site: it builds cfg's brain with FromConfig and, only when cfg actually resolves to the Gemini API (see GeminiModelFor), wraps it in the same daily quota every other Metered call sharing state and opts is gated by. A CLI-provider cfg is returned unmetered, since the ceiling only exists to protect Gemini's free tier. Input: the same arguments FromConfig takes, plus the shared counter, whether this call site is the interactive-ask band, and the configured limits. Output: a Brain ready to hand to tally.Wrap.
func Metered(cfg config.BrainConfig, apiKey string, state *QuotaState, forAsks bool, opts QuotaOptions) Brain {
	primary := FromConfig(cfg, apiKey)
	if model, ok := GeminiModelFor(cfg); ok {
		return WithDailyQuota(state, model, forAsks, opts, primary)
	}
	return primary
}
