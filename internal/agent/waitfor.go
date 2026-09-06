package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ora/internal/act"
)

// How wait_for watches for a change. Polling four hundred milliseconds apart is fast enough that a page settling in half a second costs half a round rather than a whole one, and slow enough that a five-second wait is a dozen accessibility walks rather than hundreds.
const (
	waitPollEvery      = 400 * time.Millisecond
	waitDefaultTimeout = 5 * time.Second
	// waitMaxTimeout bounds what a caller may ask for, so a model that writes 600000 cannot hold a job's whole wall budget in one call.
	waitMaxTimeout = 30 * time.Second
)

// waitTimeout reads the timeout a wait_for call asked for. Input: the tool's arguments, where timeout_ms arrives as a JSON number. Output: the timeout, defaulted when it is missing and capped at waitMaxTimeout.
func waitTimeout(args map[string]any) time.Duration {
	ms, ok := args["timeout_ms"].(float64)
	if !ok || ms <= 0 {
		return waitDefaultTimeout
	}
	if d := time.Duration(ms) * time.Millisecond; d < waitMaxTimeout {
		return d
	}
	return waitMaxTimeout
}

// focusedText is the text of the field the last click put the keyboard in, read off a fresh walk of the window so it carries what has just been typed rather than what the field held before. Input: the items of the fresh walk. Output: that field's label, which for an entry is its own contents, or "" when nothing has been clicked or the field is no longer there.
func (a *Agent) focusedText(ctx context.Context, items []act.Item) string {
	clicked, _ := a.focus(ctx)
	if clicked.Ref == "" {
		return ""
	}
	for _, it := range items {
		if it.Ref == clicked.Ref {
			return it.Label
		}
	}
	return clicked.Label
}

// listCheck reports whether a check is matched against the numbered listing rather than against the window itself. Input: the check's kind. Output: true for item_present and item_absent, false for the other two.
func listCheck(kind string) bool {
	return kind == act.ItemPresent || kind == act.ItemAbsent
}

// CheckHolds reads the screen once and says whether a check is already satisfied. Input: a context and the check a job wrote down for the action it is about to take. Output: true when that one reading matches it, false when it does not, when the check names nothing to look for, or when the screen cannot be read at all.
// It is the reading a job takes before it acts, to tell a check that was true either way from one the action made true, so it does none of what wait_for does around the same match: no polling, since nothing has acted yet and there is nothing to wait for, and no second walk to ask whether another window came to the front, since nothing has happened that could have brought one.
func (a *Agent) CheckHolds(ctx context.Context, check act.Check) bool {
	if check.Value == "" {
		return false
	}
	_, title, nodes, err := a.observe(ctx)
	if err != nil {
		return false
	}
	items := act.Filter(nodes)
	held, _ := act.Match(check, title, items, a.focusedText(ctx, items))
	return held
}

// waitFor polls the window in front until the expected change shows up or the timeout runs out. Input: the check the caller wrote down before it acted and how long to wait. Output: act.WaitPassPrefix and what was found when the change came, act.WaitFailPrefix with how long it waited and what was there when it did not, or an "error:" line when the check itself says nothing that can be looked for.
// It reads the screen without storing what it read: the numbered list a click resolves against is the one the last observe_screen produced, and renumbering it behind the model's back is how a click lands on the wrong row.
func (a *Agent) waitFor(ctx context.Context, check act.Check, timeout time.Duration) string {
	if check.Value == "" {
		return toolError("wait_for needs the text to look for, in value")
	}
	switch check.Kind {
	case act.TitleContains, act.ItemPresent, act.ItemAbsent, act.FieldHolds:
	default:
		return toolError(fmt.Sprintf("wait_for has no check called %q; it has %s, %s, %s and %s", check.Kind, act.TitleContains, act.ItemPresent, act.ItemAbsent, act.FieldHolds))
	}

	deadline := time.Now().Add(timeout)
	why := "the screen could not be read at all"
	for {
		_, title, nodes, err := a.observe(ctx)
		if err == nil {
			items := act.Filter(nodes)
			var ok bool
			ok, why = act.Match(check, title, items, a.focusedText(ctx, items))
			// A list check is only about the window the step acted in. Another window that took focus since carries its own items, and it satisfies item_present by coincidence and item_absent by never having held the thing at all, so while the front window is not the one the list came from neither may pass. A title check is about whatever is in front by definition and is left alone.
			if ok && listCheck(check.Kind) {
				if changed := a.frontWindowChanged(ctx); changed != "" {
					ok, why = false, strings.TrimPrefix(changed, "error: ")
				}
			}
			if ok {
				return act.WaitPassPrefix + why
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Sprintf("%s%s: %s", act.WaitFailPrefix, timeout.Round(time.Millisecond), why)
		}
		select {
		case <-time.After(waitPollEvery):
		case <-ctx.Done():
			return fmt.Sprintf("%s%s: %s", act.WaitFailPrefix, time.Since(deadline.Add(-timeout)).Round(time.Millisecond), why)
		}
	}
}
