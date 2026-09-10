// router.go decides which provider answers a question, and in what order to try the rest when one fails.
//
// What it replaces: a fixed chain of ifs — Gemini, then Codex, then Claude — that handed over whenever the one before it errored. That chain routed on liveness alone, which is how a question needing the web reached Claude on 2026-09-07 only after two providers failed, and how before that it reached a provider with no web search at all and got "I don't actually have web access wired up right now" back. A provider being alive says nothing about whether it can answer this particular question.
//
// The order is settled by three filters, in this order:
//  1. Capability. A question that needs the web is offered only to providers that have a web search. Everything else is offered to all of them.
//  2. Health. A provider this machine cannot run at all is dropped, and so is one that failed recently in a way that will repeat — a spent daily allowance, a rate limit — until its breaker closes.
//  3. Rank. What is left is ordered by the user's own preference: Gemini first because it is cheapest, Claude last because it is also their daily coding workhorse.
package agent

import (
	"errors"
	"log/slog"
	"net/http"
	"ora/internal/config"
	"os/exec"
	"slices"
	"sync"
	"time"
)

// Need is what a question requires of whichever provider answers it. The zero value asks for nothing in particular and every provider qualifies.
type Need struct {
	// Web is set for a question that cannot be answered from memory and needs something looked up. Only a provider with a web search of its own qualifies — Gemini's grounding is not counted, because its free-tier allowance is separate from the model's and runs out on its own (measured 2026-09-07: both Gemini models answered plain and refused every grounded call).
	Web bool
}

// breakerWindow is how long a provider is skipped after it fails in a way that will repeat. An hour: a spent daily allowance will not come back sooner, and a rate limit usually clears well within it, so this trades one wasted call an hour for never wasting one on a provider that is certainly going to refuse.
const breakerWindow = time.Hour

// card is what the router knows about one provider.
type card struct {
	// id is the provider name a caller routes by, and the same id GET /brains publishes.
	id string
	// web says whether this provider can look something up on the web by itself. Claude's own WebSearch is the only one wired today (see claudeWebTool).
	web bool
	// rank orders the providers a question could go to, lowest first.
	rank int
	// ready reports whether this machine can run the provider at all — a CLI that is installed and logged into. It is a func because the answer can change while the daemon runs.
	ready func() bool
}

// cards is every provider an ask can be routed to, in rank order.
var cards = []card{
	{id: ProviderGemini, web: false, rank: 0, ready: func() bool { return true }},
	{id: ProviderCodex, web: false, rank: 1, ready: codexLoggedIn},
	{id: ProviderAgy, web: false, rank: 2, ready: agyReady},
	{id: ProviderClaude, web: true, rank: 3, ready: claudeLoggedIn},
}

// agyReady reports whether the Antigravity command line is on this machine. There is no credential file at a stable path to check the way claudeLoggedIn does, so this only says the CLI is installed; a run that turns out not to be logged in fails and opens the provider's breaker like any other failure.
func agyReady() bool {
	_, err := exec.LookPath("agy")
	return err == nil
}

// routerState holds what the router has learned while the daemon has been running: which providers are open, and any readiness a test or the daemon has pinned.
var routerState struct {
	mu sync.Mutex
	// openUntil is when each provider's breaker closes; a provider absent from it has no breaker open.
	openUntil map[string]time.Time
	// readyOverride pins a provider's readiness, set by the tests so they do not depend on which CLIs this machine happens to have.
	readyOverride map[string]bool
	// preferred is the provider the user picked as their brain, which goes first whatever its rank; "" when none is picked or the pick names no card.
	preferred string
}

// SetPreferredProvider records the brain the user picked, by its config provider name (config.BrainClaudeCLI and the rest), so Route offers it first. Input: the provider as the config spells it. Output: none; a name no card answers to clears the preference rather than setting one nothing can honour.
func SetPreferredProvider(provider string) {
	routerState.mu.Lock()
	defer routerState.mu.Unlock()
	routerState.preferred = providerCard(provider)
}

// providerCard maps a config provider name to the router's own id for it. Input: the config spelling. Output: the card id, or "" when the router has no card for that provider.
func providerCard(provider string) string {
	switch provider {
	case config.BrainGeminiAPI:
		return ProviderGemini
	case config.BrainCodex:
		return ProviderCodex
	case config.BrainAgyCLI:
		return ProviderAgy
	case config.BrainClaudeCLI:
		return ProviderClaude
	}
	return ""
}

// SetProviderReady pins whether a provider is usable, overriding its own check. Input: the provider id and whether it is usable. Output: none. Meant for tests and for a daemon that already knows the answer.
func SetProviderReady(id string, ready bool) {
	routerState.mu.Lock()
	defer routerState.mu.Unlock()
	if routerState.readyOverride == nil {
		routerState.readyOverride = make(map[string]bool, len(cards))
	}
	routerState.readyOverride[id] = ready
}

// ProviderFailed records that a provider failed in a way that will repeat, so the router skips it until the window passes. Input: the provider id and how long to skip it — breakerWindow for a spent allowance or a rate limit. Output: none.
func ProviderFailed(id string, window time.Duration) {
	routerState.mu.Lock()
	defer routerState.mu.Unlock()
	if routerState.openUntil == nil {
		routerState.openUntil = make(map[string]time.Time, len(cards))
	}
	routerState.openUntil[id] = time.Now().Add(window)
}

// ResetRouter forgets every breaker and every pinned readiness. Meant for tests, so one does not leak into the next.
func ResetRouter() {
	routerState.mu.Lock()
	defer routerState.mu.Unlock()
	routerState.openUntil = nil
	routerState.readyOverride = nil
	routerState.preferred = ""
}

// Route returns the providers that can answer a question needing need, best first. Input: what the question requires. Output: the provider ids to try in order, empty when none of them can serve it — which is a real answer, not an error: it means every provider is either unusable on this machine or known to be refusing.
func Route(need Need) []string {
	routerState.mu.Lock()
	defer routerState.mu.Unlock()

	now := time.Now()
	out := make([]string, 0, len(cards))
	// The picked provider is offered first and the rest keep their rank behind it.
	ordered := slices.Clone(cards)
	slices.SortStableFunc(ordered, func(a, b card) int {
		switch {
		case a.id == routerState.preferred && b.id != routerState.preferred:
			return -1
		case b.id == routerState.preferred && a.id != routerState.preferred:
			return 1
		}
		return 0
	})
	for _, c := range ordered {
		if need.Web && !c.web {
			continue
		}
		if ready, pinned := routerState.readyOverride[c.id]; pinned {
			if !ready {
				continue
			}
		} else if c.ready != nil && !c.ready() {
			continue
		}
		if until, open := routerState.openUntil[c.id]; open && now.Before(until) {
			continue
		}
		out = append(out, c.id)
	}
	return out
}

// providerSpent reports whether err is the kind of failure another provider can fix rather than repeat: Gemini overloaded or out of its daily allowance, or an HTTP 429 from one of the command-line backends whose subscription allowance is spent. Input: the error from one provider's ask, possibly wrapped. Output: true only for those, since any other failure — a rejected prompt, a broken tool call — would fail the same way everywhere and asking on is three more costs for the same answer.
func providerSpent(err error) bool {
	if err == nil {
		return false
	}
	if geminiCannotAnswer(err) {
		return true
	}
	var httpErr codexHTTPError
	return errors.As(err, &httpErr) && httpErr.Code == http.StatusTooManyRequests
}

// errNoProvider is what askInOrder returns when the router offered nobody to ask. It is its own error because it means something different from a failure: every provider is either unusable on this machine or known to be refusing, and the caller should say so rather than report the last provider's error.
var errNoProvider = errors.New("no provider can answer this right now: every one is either unavailable on this machine or out of allowance")

// askInOrder tries each provider in turn until one answers. Input: the provider ids the router returned, best first, and how to ask one. Output: the first answer, or the last failure.
// It hands on only when the failure is one another provider can fix, and only while no action has run: a read like observe_screen can be repeated on another provider and change nothing, where a click or a keystroke would happen twice. A provider that reports a spent allowance is marked so the next question skips it rather than paying the same failure again.
func askInOrder(order []string, ask func(id string) (TurnTrace, error)) (TurnTrace, error) {
	var tr TurnTrace
	var err error
	for i, id := range order {
		if i > 0 && actionHops(tr.ToolHops) > 0 {
			break
		}
		tr, err = ask(id)
		if err == nil {
			return tr, nil
		}
		if !providerSpent(err) {
			return tr, err
		}
		slog.Warn("ask: provider out of allowance, handing the question on", "provider", id, "error", err)
		ProviderFailed(id, breakerWindow)
	}
	if err == nil {
		return tr, errNoProvider
	}
	return tr, err
}
