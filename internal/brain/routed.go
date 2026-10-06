// routed.go answers an unattended duty through the same router an ask goes through, so a duty is never stranded on one backend that happens to be down.
// What it replaces: WithCodexFallback, which every duty had and which hands on for one reason (a spent Gemini allowance) to one place (Codex). When Codex itself is spent, as it was for the whole of September 2026, that hand-over catches nothing: on 2026-09-15 every meeting-minutes run died on an Antigravity login that had stopped working, with Grok and Claude both signed in on the same machine and neither ever asked.
package brain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"june/internal/agent"
)

// maxDutyAnswers is how many providers one duty may reach before it gives up, two: the one it was meant for and one more. A duty carries a day of screen text or a meeting, and handing it down the whole router's order spent up to four paid plans on one summary; the breaker each refusal opens already sends the next duty past the providers that failed, so a duty that stops here loses one run, not the job.
const maxDutyAnswers = 2

// dutyFallbackAllowed is agent.DutyFallbackAllowed, the user's "allow_fallback" setting read afresh for every duty. A variable so tests pin it rather than read the config of whatever machine runs them.
var dutyFallbackAllowed = agent.DutyFallbackAllowed

// Routed answers a prompt on the first provider agent.RouteDuty offers that can answer it, handing on when one is spent or logged out and marking that provider so the next duty skips it. Input: how to build the backend for one config provider name, which the daemon closes over its API key and per-job model. Output: a Brain that answers through whichever provider did.
// The duty route rather than the ask route: a duty needs a prompt answered, not a question asked, so it can also use the providers that have a one-shot backend here and no asker in internal/agent.
func Routed(build func(provider string) Brain) Brain { return RoutedFor("", build) }

// RoutedFor is Routed with one provider asked ahead of the router's own order: the one the config names for this duty. background_brains exists so a duty can be pinned to a plan the user is already paying for, and the daemon treats naming one as the more deliberate act, so it is asked first whatever the global preference is. The rest of the order still stands behind it, so a pinned provider that is spent or signed out hands the duty on rather than losing it — unless the user has turned fallback off, and then only the brains they chose are asked (see dutyOrder). Input: the config provider name to prefer, "" for none, and the backend builder. Output: the Brain.
func RoutedFor(preferred string, build func(provider string) Brain) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		fallback := dutyFallbackAllowed()
		order := dutyOrder(preferred, fallback)
		var err error
		reached := 0
		for _, id := range order {
			provider := agent.ConfigProvider(id)
			if provider == "" {
				continue
			}
			if reached >= maxDutyAnswers {
				slog.Warn("duty: asked as many providers as one duty may, leaving it for the next run", "asked", reached)
				break
			}
			var text string
			text, err = build(provider)(ctx, prompt)
			if err == nil {
				return text, nil
			}
			// A refusal made on this machine — the duty's own daily band is full, or this caller has no backend for the provider — hands on without opening the breaker, because the provider itself refused nothing and the asks that share the breaker can still use it. So does a run that ended with nothing said (agent.ErrNoAnswer): it says nothing about the provider's next prompt, though it did reach the provider and counts against maxDutyAnswers.
			var band *ErrDailyQuota
			refusedHere := errors.As(err, &band) || errors.Is(err, ErrNoBackend)
			if !refusedHere {
				reached++
			}
			local := refusedHere || errors.Is(err, agent.ErrNoAnswer)
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
			if !fallback {
				return "", fmt.Errorf("%w: the brain background work uses cannot answer right now, and June may not use another one for it", ErrNoBackend)
			}
			return "", ErrNoBackend
		}
		return "", err
	}
}

// dutyOrder is the providers to ask, with preferred first when the router offers it at all. A preferred provider the router has dropped — not installed, or its breaker open — is not forced back in: it could not answer this duty either.
// Grok is asked only when it is the chosen brain, the duty's own or the one picked in Settings: the window offers no way to pick it, so a duty that reached it went to a service the user never chose just because its command line was on PATH. With fallback off the order is cut to the chosen brains alone, and with none chosen to the router's first, so background work never spends a plan the user did not pick. Input: the duty's config provider ("" for none) and whether fallback is allowed. Output: the router ids to ask, in order.
func dutyOrder(preferred string, fallback bool) []string {
	order := agent.RouteDuty(agent.Need{})
	pinned := agent.RouterID(preferred)
	picked := agent.PreferredProvider()
	chosen := func(id string) bool { return id != "" && (id == pinned || id == picked) }
	unchosenGrok := func(id string) bool { return id == agent.ProviderGrok && !chosen(id) }
	order = slices.DeleteFunc(order, unchosenGrok)
	if !fallback {
		if pinned == "" && picked == "" {
			// The router's first is read with breakers ignored and asked only while its own breaker is shut: RouteDuty has already dropped a provider whose breaker is open, so one 429 on Gemini would make its first Codex for the next hour, and minutes and screen notes would go to a plan nobody picked.
			own := slices.DeleteFunc(agent.RouteDutyIgnoringBreakers(agent.Need{}), unchosenGrok)
			if len(own) == 0 || !slices.Contains(order, own[0]) {
				return nil
			}
			return own[:1]
		}
		order = slices.DeleteFunc(order, func(id string) bool { return !chosen(id) })
	}
	if pinned == "" || !slices.Contains(order, pinned) {
		return order
	}
	return append([]string{pinned}, slices.DeleteFunc(slices.Clone(order), func(o string) bool { return o == pinned })...)
}
