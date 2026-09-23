// routed.go answers an unattended duty through the same router an ask goes through, so a duty is never stranded on one backend that happens to be down.
// What it replaces: WithCodexFallback, which every duty had and which hands on for one reason (a spent Gemini allowance) to one place (Codex). When Codex itself is spent, as it was for the whole of September 2026, that hand-over catches nothing: on 2026-09-15 every meeting-minutes run died on an Antigravity login that had stopped working, with Grok and Claude both signed in on the same machine and neither ever asked.
package brain

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"ora/internal/agent"
)

// Routed answers a prompt on the first provider agent.RouteDuty offers that can answer it, handing on when one is spent or logged out and marking that provider so the next duty skips it. Input: how to build the backend for one config provider name, which the daemon closes over its API key and per-job model. Output: a Brain that answers through whichever provider did.
// The duty route rather than the ask route: a duty needs a prompt answered, not a question asked, so it can also use the providers that have a one-shot backend here and no asker in internal/agent.
func Routed(build func(provider string) Brain) Brain { return RoutedFor("", build) }

// RoutedFor is Routed with one provider asked ahead of the router's own order: the one the config names for this duty. background_brains exists so a duty can be pinned to a plan the user is already paying for, and the daemon treats naming one as the more deliberate act, so it is asked first whatever the global preference is. The rest of the order still stands behind it, so a pinned provider that is spent or signed out hands the duty on rather than losing it. Input: the config provider name to prefer, "" for none, and the backend builder. Output: the Brain.
func RoutedFor(preferred string, build func(provider string) Brain) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		order := dutyOrder(preferred)
		var err error
		for _, id := range order {
			provider := agent.ConfigProvider(id)
			if provider == "" {
				continue
			}
			var text string
			text, err = build(provider)(ctx, prompt)
			if err == nil {
				return text, nil
			}
			// A refusal made on this machine — the duty's own daily band is full, or this caller has no backend for the provider — hands on without opening the breaker, because the provider itself refused nothing and the asks that share the breaker can still use it.
			var band *ErrDailyQuota
			local := errors.As(err, &band) || errors.Is(err, ErrNoBackend)
			// A failure every provider would repeat — a rejected prompt, a broken parse — costs one more answer for the same result on each one left, so it stops here.
			if !local && !agent.ProviderSpent(err) {
				return "", err
			}
			slog.Warn("duty: provider cannot answer, handing on", "provider", provider, "error", err)
			if !local {
				agent.ProviderFailed(id, agent.BreakerWindow)
			}
		}
		if err == nil {
			return "", ErrNoBackend
		}
		return "", err
	}
}

// dutyOrder is the providers to ask, with preferred first when the router offers it at all. A preferred provider the router has dropped — not installed, or its breaker open — is not forced back in: it could not answer this duty either.
func dutyOrder(preferred string) []string {
	order := agent.RouteDuty(agent.Need{})
	if preferred == "" {
		return order
	}
	id := agent.RouterID(preferred)
	if id == "" || !slices.Contains(order, id) {
		return order
	}
	return append([]string{id}, slices.DeleteFunc(slices.Clone(order), func(o string) bool { return o == id })...)
}
