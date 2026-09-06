// usage.go keeps what each provider says about the user's own allowance — Codex's five-hour and weekly windows, Claude's subscription windows, Gemini's daily request ceiling — so the brain picker can draw a bar per brain instead of only saying whether the machine is signed in. Nothing here calls a provider on its own: the readings come off responses Ora already makes, or out of the counter internal/brain/quota.go already keeps.
package brain

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"ora/internal/agent"
	"ora/internal/util"
)

// UsageLimit is one allowance window a provider reports for the user's account: the window ("5h", "daily", "weekly", "monthly"), how much of it is spent as a fraction from 0 to 1, when it resets, and the field or header the reading came from.
// The type itself is declared in internal/agent, which is where the Codex headers and the Claude endpoint are read, because that package cannot import this one: this package already imports it (see codex.go). This alias is how the rest of the daemon names it.
type UsageLimit = agent.UsageLimit

// UsageSnapshot is the newest reading for one provider: its windows, and the moment they were read, so the window can say how old a bar is.
type UsageSnapshot struct {
	Limits []UsageLimit `json:"limits"`
	At     time.Time    `json:"at"`
	// Note is a sentence about the reading itself rather than the allowance — today only that a ceiling is a default rather than an observed one — which GET /brains carries into the row's limits_note. Empty for a reading that needs no caveat.
	Note string `json:"note,omitempty"`
}

// UsageStore keeps the newest reading per provider in memory and writes it to brain_usage.json beside brain_quota.json, so a restart still has the last reading to draw until the next response refreshes it.
type UsageStore struct {
	mu    sync.Mutex
	path  string
	snaps map[string]UsageSnapshot
}

// NewUsageStore returns the store backed by brain_usage.json under dataDir, loading whatever the last run left there. A missing or unreadable file is an empty store, not an error: a fresh install has nothing to draw yet, which is the same as a corrupt file.
func NewUsageStore(dataDir string) *UsageStore {
	s := &UsageStore{path: filepath.Join(dataDir, "brain_usage.json"), snaps: map[string]UsageSnapshot{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, &s.snaps); err != nil {
		slog.Warn("brain: usage file unreadable, starting with no readings", "path", s.path, "error", err)
		s.snaps = map[string]UsageSnapshot{}
	}
	return s
}

// Record stores one provider's newest windows and writes the file. Input: the provider id ("codex", "claude") and its windows. Output: none. A call with no windows is ignored, because a response that carried no rate-limit headers says nothing about the allowance and would otherwise blank out a real bar.
func (s *UsageStore) Record(provider string, limits []UsageLimit) {
	if len(limits) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snaps[provider] = UsageSnapshot{Limits: limits, At: time.Now()}
	data, err := json.Marshal(s.snaps)
	if err != nil {
		slog.Warn("brain: could not encode the usage readings", "error", err)
		return
	}
	if err := util.WriteFileAtomic(s.path, data, 0o600); err != nil {
		slog.Warn("brain: could not write the usage file", "path", s.path, "error", err)
	}
}

// Get returns one provider's newest reading. Input: the provider id. Output: the reading and true, or the zero snapshot and false when nothing has ever been recorded for it.
func (s *UsageStore) Get(provider string) (UsageSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snaps[provider]
	return snap, ok
}

// GeminiDaily is the Gemini brain's own allowance window, which no Gemini response reports: how much of a model's free-tier daily request ceiling today's calls have already taken, from the counter internal/brain/quota.go keeps. Input: that counter, the model name, the configured ceilings, and the clock to read today and the next midnight from. Output: a one-window snapshot, and false only for a name that is not a Gemini model at all, since there is then no number to measure against.
// A Gemini model with no measured entry is drawn against QuotaOptions.DefaultQuotaLimit and the snapshot's Note says so, because a bar that quietly disappears reads as "no usage" rather than "nobody knows this model's ceiling".
func GeminiDaily(state *QuotaState, model string, opts QuotaOptions, now time.Time) (UsageSnapshot, bool) {
	limit, known := opts.For(model)
	if limit.Limit <= 0 {
		return UsageSnapshot{}, false
	}
	note := ""
	if !known {
		note = "nobody has measured " + model + "'s free-tier ceiling, so this bar is drawn against the default of " + strconv.Itoa(limit.Limit) + " requests a day"
	}
	state.mu.Lock()
	used := state.load()[now.Format("2006-01-02")][model]
	state.mu.Unlock()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	return UsageSnapshot{
		At:   now,
		Note: note,
		Limits: []UsageLimit{{
			Window:       "daily",
			UsedFraction: float64(used) / float64(limit.Limit),
			ResetsAt:     midnight,
			Source:       "brain_quota.json " + model,
		}},
	}, true
}

// GrokNote is the sentence GET /brains and GET /usage carry as the Grok row's limits_note, so the picker's empty bar reads as a checked fact rather than a gap nobody looked at.
func GrokNote() string {
	return "grok exposes no usage data: its CLI, config, logs, and session files carry no quota, usage, or rate-limit reading, and it has no command that reports one"
}
