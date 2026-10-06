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
	"june/internal/config"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// Need is what a question requires of whichever provider answers it. The zero value asks for nothing in particular and every provider qualifies.
type Need struct {
	// Web is set for a question that cannot be answered from memory and needs something looked up. Only a provider with a web search of its own qualifies — Gemini's grounding is not counted, because its free-tier allowance is separate from the model's and runs out on its own (measured 2026-09-07: both Gemini models answered plain and refused every grounded call).
	Web bool
}

// BreakerWindow is how long a provider is skipped after it fails in a way that will repeat. An hour: a spent daily allowance will not come back sooner, and a rate limit usually clears well within it, so this trades one wasted call an hour for never wasting one on a provider that is certainly going to refuse.
const BreakerWindow = time.Hour

// card is what the router knows about one provider.
type card struct {
	// id is the provider name a caller routes by, and the same id GET /brains publishes.
	id string
	// web says whether this provider can look something up on the web by itself. Claude's own WebSearch is the only one wired today (see claudeWebTool).
	web bool
	// rank orders the providers a question could go to, lowest first.
	rank int
	// asks is whether an ask can be routed here, which needs a tool-calling asker in askRouted's switch. False means the provider answers duties only: internal/brain can build it a one-shot backend, but a question routed to it would fall through that switch to Gemini and be answered, and billed, under this provider's name.
	asks bool
	// ready reports whether this machine can run the provider at all — a CLI that is installed and logged into. It is a func because the answer can change while the daemon runs.
	ready func() bool
}

// cards is every provider an ask can be routed to, in rank order.
var cards = []card{
	{id: ProviderGemini, web: false, rank: 0, asks: true, ready: geminiKeySet},
	{id: ProviderCodex, web: false, rank: 1, asks: true, ready: codexLoggedIn},
	{id: ProviderAgy, web: false, rank: 2, asks: true, ready: agyReady},
	{id: ProviderGrok, web: false, rank: 3, asks: false, ready: grokReady},
	{id: ProviderClaude, web: true, rank: 4, asks: true, ready: claudeLoggedIn},
}

// geminiKeySet reports whether the Gemini API can be called at all. Without a key every ask was routed to Gemini first and failed at client construction, which is not a failure askInOrder hands on from, so a desk signed into Claude or Codex but with no Gemini key could not answer a single question.
func geminiKeySet() bool {
	return strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != ""
}

// ProviderGrok is the Grok command line. It answers duties through internal/brain and has no asker here, so no ask is ever routed to it.
const ProviderGrok = "grok"

// grokReady reports whether the Grok command line is on this machine. Like agyReady this only says the CLI is installed; a run under a login that no longer works fails and opens the breaker.
func grokReady() bool {
	_, err := exec.LookPath("grok")
	return err == nil
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

// RouterID is providerCard for callers outside this package: internal/brain needs it to put a duty's own configured provider at the front of the order.
func RouterID(provider string) string { return providerCard(provider) }

// PreferredProvider is the router id of the brain the user picked, "" when none is picked. internal/brain reads it to keep a duty on the brains the user chose (see brain.dutyOrder).
func PreferredProvider() string {
	routerState.mu.Lock()
	defer routerState.mu.Unlock()
	return routerState.preferred
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
	case config.BrainGrokCLI:
		return ProviderGrok
	}
	return ""
}

// ConfigProvider is providerCard's inverse: the config spelling of a router provider id, which is what internal/brain builds a backend from. Input: a router id. Output: the config.Brain* name, or "" when the router has no such id.
func ConfigProvider(id string) string {
	switch id {
	case ProviderGemini:
		return config.BrainGeminiAPI
	case ProviderCodex:
		return config.BrainCodex
	case ProviderAgy:
		return config.BrainAgyCLI
	case ProviderClaude:
		return config.BrainClaudeCLI
	case ProviderGrok:
		return config.BrainGrokCLI
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

// ProviderFailed records that a provider failed in a way that will repeat, so the router skips it until the window passes. Input: the provider id and how long to skip it — BreakerWindow for a spent allowance or a rate limit. Output: none.
func ProviderFailed(id string, window time.Duration) {
	routerState.mu.Lock()
	defer routerState.mu.Unlock()
	if routerState.openUntil == nil {
		routerState.openUntil = make(map[string]time.Time, len(cards))
	}
	routerState.openUntil[id] = time.Now().Add(window)
}

// closeBreaker closes a provider's breaker before its window has passed, for a failure that has since been put right. Input: the provider id. Output: none.
func closeBreaker(id string) {
	routerState.mu.Lock()
	defer routerState.mu.Unlock()
	delete(routerState.openUntil, id)
}

// reviveRenewedLogins closes the breaker on a provider whose refused login has been renewed since, so a user who signs in again is not kept waiting out BreakerWindow. Today that is Claude, whose login file says when it was written. It runs before the router's lock is taken, because it reads that file and writes the usage store. Input: none. Output: none.
func reviveRenewedLogins() {
	if home, err := os.UserHomeDir(); err == nil {
		reviveClaude(home)
	}
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
func Route(need Need) []string { return route(need, true, true) }

// RouteDuty is Route for an unattended duty — meeting minutes, the memory jobs — which needs a prompt answered rather than a question asked. It offers the same providers in the same order plus the ones that have a one-shot backend in internal/brain but no asker here. Input: what the duty requires. Output: the provider ids to try in order.
func RouteDuty(need Need) []string { return route(need, false, true) }

// RouteDutyIgnoringBreakers is RouteDuty as it would be with no breaker open. internal/brain reads its first entry as background work's own brain when the user has picked none and turned fallback off: RouteDuty's first moves to the next plan down for an hour after one 429, and that plan is one the user never chose (see brain.dutyOrder). Input: what the duty requires. Output: the provider ids this machine can run, in the router's order.
func RouteDutyIgnoringBreakers(need Need) []string { return route(need, false, false) }

// route is the body of all three. onlyAsks drops the providers no ask can be routed to, and skipOpen drops the ones whose breaker is open.
func route(need Need, onlyAsks, skipOpen bool) []string {
	reviveRenewedLogins()
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
		if onlyAsks && !c.asks {
			continue
		}
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
		if until, open := routerState.openUntil[c.id]; skipOpen && open && now.Before(until) {
			continue
		}
		out = append(out, c.id)
	}
	return out
}

// ErrLoggedOut is what a command-line backend's ask returns when that CLI's own login is no longer valid. It is the second kind of failure another provider can fix, alongside a spent allowance: the credential has expired and no retry on this brain will work, while the next brain's login is untouched. On 2026-09-15 an expired Antigravity token failed three asks in a row with Claude signed in and never asked, because a 401 was neither a 429 nor a Gemini exhaustion and askInOrder stopped on it.
var ErrLoggedOut = errors.New("the login is no longer valid")

// loggedOut reports whether a CLI's own failure message says its login has expired rather than something about this particular question.
//
// Input: the reason the CLI gave, which is free text and differs per CLI. Output: true only when it is an authentication failure.
//
// A true here costs a brain for an hour, so the markers have to be ones that cannot appear in an ordinary refusal. The one message measured so far is agy 1.2.3 on 2026-09-15: "Eligibility check failed: UNAUTHENTICATED (code 401): Request had invalid authentication credentials. Expected OAuth 2 access token, login cookie or other valid authentication credential."
func loggedOut(reason string) bool {
	// Both markers come from the one message measured. UNAUTHENTICATED is a gRPC status code rather than prose, so it does not appear in a model's own refusal; the phrase is the backend's wording for the same thing. A bare "401" is not matched: it appears inside URLs and inside text a model wrote.
	lower := strings.ToLower(reason)
	return strings.Contains(reason, "UNAUTHENTICATED") || strings.Contains(lower, "invalid authentication credentials")
}

// ProviderSpent reports whether err is the kind of failure another provider can fix rather than repeat: an expired login on a command-line backend, Gemini overloaded or out of its daily allowance, or an HTTP 429 from one of the command-line backends whose subscription allowance is spent. Input: the error from one provider's ask, possibly wrapped. Output: true only for those, since any other failure — a rejected prompt, a broken tool call — would fail the same way everywhere and asking on is three more costs for the same answer.
func ProviderSpent(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrLoggedOut) {
		return true
	}
	if geminiCannotAnswer(err) {
		return true
	}
	var httpErr codexHTTPError
	return errors.As(err, &httpErr) && httpErr.Code == http.StatusTooManyRequests
}

// ErrNoAnswer is what a command-line asker returns when its run ended cleanly with nothing said: agy does this when the model reached for one of its own tools June does not grant, and asking it once more in the same session had not helped either. It is a third kind of failure another provider can fix, but unlike the other two it says nothing about the provider's next question, so it hands this one on without opening the provider's breaker.
var ErrNoAnswer = errors.New("no answer")

// ErrCouldNotRun is what a command-line backend returns when its program would not start, so no model was asked anything: agy under Windows' RedirectionGuard died in a quarter of a second on every ask and duty, and the ask stopped there with Claude signed in behind it (2026-10-06). Another provider's program is not affected, so the question is handed on. The breaker is not opened, because what stops a program starting is this machine's state rather than the provider's, and the next start can work (an expired login agy reports as it starts is ErrLoggedOut instead; see AgyStderrError); and since nothing reached a model, a duty does not count it as one of its answers.
var ErrCouldNotRun = errors.New("the brain's program could not start")

// ProviderUsable reports whether a provider is worth asking right now: this machine can run it and no breaker is open on it. Input: the provider id. Output: true when the router would offer it. For a caller that asks one provider directly rather than through Route, such as the job's Claude fallback, so it does not spend a call on a login the router already knows is dead.
func ProviderUsable(id string) bool {
	reviveRenewedLogins()
	routerState.mu.Lock()
	defer routerState.mu.Unlock()
	if until, open := routerState.openUntil[id]; open && time.Now().Before(until) {
		return false
	}
	if ready, pinned := routerState.readyOverride[id]; pinned {
		return ready
	}
	for _, c := range cards {
		if c.id == id {
			return c.ready == nil || c.ready()
		}
	}
	return false
}

// ErrNoProvider is what askInOrder returns when the router offered nobody to ask. It is its own error because it means something different from a failure: every provider is either unusable on this machine or known to be refusing, and the caller should say so rather than report the last provider's error. Exported so the window can say that in its own sentence: a machine with no Gemini key and no CLI signed in now gets this, where it used to get Gemini's missing-key error.
var ErrNoProvider = errors.New("no provider can answer this right now: every one is either unavailable on this machine or out of allowance")

// askInOrder tries each provider in turn until one answers. Input: the provider ids the router returned, best first, and how to ask one. Output: the first answer, or the last failure.
// It hands on only when the failure is one another provider can fix (a spent allowance, an expired login, ErrNoAnswer or ErrCouldNotRun), and only while no action has run: a read like observe_screen can be repeated on another provider and change nothing, where a click or a keystroke would happen twice. A provider that reports a spent allowance is marked so the next question skips it rather than paying the same failure again.
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
		if errors.Is(err, ErrNoAnswer) || errors.Is(err, ErrCouldNotRun) {
			slog.Warn("ask: provider gave no answer, handing the question on", "provider", id, "error", err)
			continue
		}
		if !ProviderSpent(err) {
			return tr, err
		}
		slog.Warn("ask: provider out of allowance, handing the question on", "provider", id, "error", err)
		ProviderFailed(id, BreakerWindow)
	}
	if err == nil {
		return tr, ErrNoProvider
	}
	return tr, err
}
