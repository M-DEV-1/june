// usage.go keeps what each provider says about the user's own allowance — Codex's five-hour and weekly windows, Claude's subscription windows, Gemini's daily request ceiling — so the brain picker can draw a bar per brain instead of only saying whether the machine is signed in. Nothing here calls a provider on its own: the readings come off responses June already makes, or out of the counter internal/brain/quota.go already keeps.
package brain

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"june/internal/agent"
	"june/internal/util"
)

// UsageLimit is one allowance window a provider reports for the user's account: the window ("5h", "daily", "weekly", "monthly"), how much of it is spent as a fraction from 0 to 1, when it resets, and the field or header the reading came from.
// The type itself is declared in internal/agent, which is where the Codex headers and the Claude endpoint are read, because that package cannot import this one: this package already imports it (see codex.go). This alias is how the rest of the daemon names it.
type UsageLimit = agent.UsageLimit

// UsageSnapshot is the newest reading for one provider: its windows, and the moment they were read, so the window can say how old a bar is.
type UsageSnapshot struct {
	Limits []UsageLimit `json:"limits"`
	At     time.Time    `json:"at"`
	// SignedOut says the provider refused the credential when this reading was attempted, which is the one thing a usage fetch can tell about a login that a file on disk cannot: every one of these CLIs holds a refresh token, so an expired access token is ordinary and only the provider can say the login is actually dead. Note carries what to do about it.
	SignedOut bool `json:"signed_out,omitempty"`
	// SignedOutAt is when SignedOut was set, so a login renewed after it can be told from the one that was refused (see agent.NoteClaudeRefused). Zero when SignedOut is false, and for a mark written before this field existed.
	SignedOutAt time.Time `json:"signed_out_at,omitzero"`
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

// Record stores one provider's newest windows and writes the file. Input: the provider id ("codex", "claude") and its windows. Output: none.
// A reading that arrives is proof the credential works, so it clears any signed-out mark an earlier refusal left. A call with no windows does only that and keeps the last windows, because a response that carried no rate-limit headers says nothing about the allowance and would otherwise blank out a real bar.
func (s *UsageStore) Record(provider string, limits []UsageLimit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snaps[provider]
	if len(limits) > 0 {
		snap = UsageSnapshot{Limits: limits, At: time.Now()}
	} else if snap.SignedOut {
		snap.SignedOut, snap.SignedOutAt, snap.Note = false, time.Time{}, ""
	} else {
		return
	}
	s.snaps[provider] = snap
	s.save()
}

// RecordSignedOut marks a provider's login as refused, keeping whatever windows were last read, and the time they were read, so the picker can still say what the allowance was when it last worked. Input: the provider id and the sentence saying what to do about it. Output: none.
func (s *UsageStore) RecordSignedOut(provider, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snaps[provider]
	snap.SignedOut, snap.SignedOutAt, snap.Note = true, time.Now(), note
	s.snaps[provider] = snap
	s.save()
}

// save writes every reading to the file. The caller holds s.mu. A failure is logged, not returned: the readings in memory are still right, and the next write tries again.
func (s *UsageStore) save() {
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
	// The provider's day, not the machine's: the counter writes under QuotaDay and Google's free tier rolls over on Pacific midnight, so a local date reads a day nothing was ever written under. On IST that is every morning from midnight until about half past twelve, which drew an empty allowance over a spent one.
	state.mu.Lock()
	used := state.load()[quotaDayAt(now)][model]
	state.mu.Unlock()
	midnight := quotaResetMidnight(now)
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
