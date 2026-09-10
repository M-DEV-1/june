// ask.go puts one question to the user on whichever single surface is there to answer it. It is shared by every question Ora asks — the morning brief's stale action item, and the microphone watcher's "in a meeting?" — so all of them obey the same rule about which surface a question appears on.
package proactive

import (
	"log/slog"
	"time"
)

// BannerAsk posts a question as a desktop notification and blocks until it is answered or dismissed. Input: the title, the body, and the buttons as "key=Label" strings. Output: the key of the button pressed, empty for a dismissal, and an error when the notification could not be posted. NotifySendAsk is the production implementation; the tests pass one that records what it was given.
type BannerAsk func(title, body string, actions []string) (string, error)

// Ask puts n to the user and returns the key of the button they pressed, or "" when nobody answered. Input: the notice, which must carry its own Actions; how long to wait for an answer on the window's card; the banner to fall back to when no window is listening, which may be nil on a daemon with none; and apply, what one of these buttons does, which may be nil for a question whose caller registers its own action for a longer life than this one question (the meeting watcher does, so its Start recording button lasts as long as there is a recorder to stop again). Output: the pressed key.
// One surface, never two: with a window up this is a card of Ora's own carrying these buttons, answered through POST /notices/{kind}/{id}/action, and no desktop banner is posted at all — posting both asks the same question twice, which is what the meeting question did until 2026-09-07. With no window it is the banner it has always been.
// The waiter is registered before the notice goes out, so an answer pressed the instant the card is drawn still has somewhere to land, and released however this returns, so an unanswered question leaves nothing in the registry.
// apply is registered against this notice for each of its buttons and, unlike the waiter, is deliberately left registered after this returns: a button has to work when it is pressed, not only while the goroutine that asked is still parked on it. A press arriving after the wait ran out, or on a card drawn on a second surface, runs it through Act instead. Registering it here rather than at each call site is what makes that true of every question Ora asks, including the ones not yet written: the stale-item question was answered by hand for months and its two buttons said "Could not do that" the moment its goroutine gave up.
// ponytail: one closure per unanswered question stays in the registry — a few a day, replaced whenever the same item is asked about again. If that ever matters, drop it when the notice's own Expires has passed.
func Ask(n Notice, wait time.Duration, banner BannerAsk, apply func(key string) error) string {
	keys := make([]string, 0, len(n.Actions))
	labels := make([]string, 0, len(n.Actions))
	for _, a := range n.Actions {
		keys = append(keys, a.Key)
		labels = append(labels, a.Key+"="+a.Label)
	}

	// The card counts down to this and removes itself at zero, so a question is never left on screen with a button whose waiter has gone.
	// Nano precision rather than the RFC3339 seconds Until uses: a second-precision stamp truncates downwards, which for a short wait can name a moment that has already passed.
	n.Expires = time.Now().Add(wait).Format(time.RFC3339Nano)

	if apply != nil {
		for _, k := range keys {
			setNoticeActionFor(n.Kind, n.ID, k, func() error { return apply(k) })
		}
	}
	// A key that came back from either surface is applied here, so the answer takes the same way whichever surface gave it.
	answer := func(chosen string) string {
		if chosen != "" && apply != nil {
			if err := apply(chosen); err != nil {
				slog.Warn("could not apply the answer to a question", "kind", n.Kind, "id", n.ID, "action", chosen, "error", err)
			}
		}
		return chosen
	}

	answered, release := awaitAnswer(noticeKey(n), keys)
	if sendNotice(n) {
		defer release()
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case chosen := <-answered:
			return answer(chosen)
		case <-timer.C:
			return ""
		}
	}

	// No window took it, so no card can be pressed and the waiter would only sit in the registry until the banner came back.
	release()
	if banner == nil {
		return ""
	}
	chosen, err := banner(n.Title, n.Body, labels)
	if err != nil {
		slog.Debug("could not ask the user a question on the desktop", "kind", n.Kind, "id", n.ID, "error", err)
		return ""
	}
	return answer(chosen)
}
