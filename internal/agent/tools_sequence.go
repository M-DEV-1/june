package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"june/internal/act"
)

const (
	// maxSequenceSteps bounds one click_sequence. A sequence is for UI that does not survive a model round trip — a menu that closes on the next repaint, a hover card, a toast — and those are two or three taps, never a whole task. A longer list is the model trying to run the task blind, which is exactly what it must not do.
	maxSequenceSteps = 6
	// defaultSequenceGapMs is how long the pointer waits between taps in a sequence, and maxSequenceGapMs is the most one step may ask for. The default is long enough for a menu to paint and short enough to beat one that closes on its own.
	defaultSequenceGapMs = 250
	maxSequenceGapMs     = 2000
)

// sequenceStep is one tap in a click_sequence, already resolved to a point on the screen.
type sequenceStep struct {
	x, y  int
	label string
}

// clickSequence fires several taps back to back with no model round trip between them, for UI that does not survive one: a menu that closes when focus moves, a hover card, a toast, an overlay a first click opens and a second click has to hit before it repaints. Every step is consent-checked against what is known of the screen before any tap fires, so a burst cannot slip an irreversible click past the checks a single click would face. Input: the click arguments, whose own n or x/y is the first tap and whose "then" list carries the rest, plus an optional "gap_ms". Output: one line naming every tap that landed, or the step that stopped it and why.
func (a *Agent) clickSequence(ctx context.Context, args map[string]any) string {
	rest, ok := args["then"].([]any)
	if !ok || len(rest) == 0 {
		return toolError("then is the list of further taps, each {\"n\": number} or {\"x\": .., \"y\": ..}; leave it out to click once")
	}
	raw := append([]any{map[string]any{}}, rest...)
	// The first tap is the call's own n or x/y, so the burst reads as one click that carries the taps which must follow it before the screen can be looked at again.
	raw[0] = args
	if len(raw) > maxSequenceSteps {
		return toolError(fmt.Sprintf("a click carries at most %d taps in one burst; a longer run is a task, not a burst, so take it one action at a time", maxSequenceSteps))
	}

	window := a.frontWindow(ctx)
	steps := make([]sequenceStep, 0, len(raw))
	for i, r := range raw {
		step, ok := r.(map[string]any)
		if !ok {
			return toolError(fmt.Sprintf("step %d is not an object; each step is {\"n\": number} or {\"x\": .., \"y\": ..}", i+1))
		}
		resolved, errText := a.resolveStep(ctx, step)
		if errText != "" {
			return fmt.Sprintf("Stopped before any tap fired: step %d, %s", i+1, strings.TrimPrefix(errText, "error: "))
		}
		if stop := a.guardPoint(ctx, resolved.x, resolved.y, window); stop != "" {
			return fmt.Sprintf("Stopped before any tap fired: step %d, %s", i+1, stop)
		}
		steps = append(steps, resolved)
	}

	dev, errText := a.inputDevice(ctx)
	if errText != "" {
		return errText
	}
	gap := sequenceGap(args)
	before := a.beforePress(ctx)
	var landed []string
	for i, s := range steps {
		a.tapAt(s.x, s.y, s.label)
		if err := dev.ClickAt(float64(s.x), float64(s.y)); err != nil {
			return toolError(fmt.Sprintf("the sequence stopped at step %d of %d, %d,%d: %v; %s", i+1, len(steps), s.x, s.y, err, landedLine(landed)))
		}
		landed = append(landed, s.describe())
		if i < len(steps)-1 {
			select {
			case <-ctx.Done():
				return toolError(fmt.Sprintf("the sequence was cut short after step %d of %d; %s", i+1, len(steps), landedLine(landed)))
			case <-time.After(gap):
			}
		}
	}

	// Checked once, after the last tap: a press check between steps would wait for the screen to settle, which is the round trip the sequence exists to skip.
	if missed := a.pressCheck(ctx, before, steps[len(steps)-1].x, steps[len(steps)-1].y); missed != "" {
		return missed
	}
	// The pointer has moved somewhere this session cannot name, exactly as a single click_at leaves it, so typing refuses until a fresh observe_screen says where the keyboard is.
	a.focusLost(ctx)
	return fmt.Sprintf("tapped %s in one burst; call observe_screen to see where it left things", strings.Join(landed, ", then "))
}

// describe names one fired tap for the result line.
func (s sequenceStep) describe() string {
	if s.label == "" {
		return fmt.Sprintf("%d,%d", s.x, s.y)
	}
	return fmt.Sprintf("%d,%d (%s)", s.x, s.y, s.label)
}

// landedLine says what a stopped sequence did manage to tap, so the model knows what state the screen is in rather than assuming nothing happened.
func landedLine(landed []string) string {
	if len(landed) == 0 {
		return "nothing was tapped"
	}
	return "already tapped: " + strings.Join(landed, ", then ")
}

// sequenceGap reads the optional gap_ms argument, clamped to [0, maxSequenceGapMs]. Input: the tool arguments. Output: the wait between taps, defaultSequenceGapMs when the argument is absent or unusable.
func sequenceGap(args map[string]any) time.Duration {
	ms, ok := args["gap_ms"].(float64)
	if !ok || ms < 0 {
		ms = defaultSequenceGapMs
	}
	if ms > maxSequenceGapMs {
		ms = maxSequenceGapMs
	}
	return time.Duration(ms) * time.Millisecond
}

// resolveStep turns one step of a sequence into a point on the screen. Input: the step object, either {"n": number} naming an element from the last observe_screen list or {"x": .., "y": ..} in the last look's picture coordinates. Output: the point and its label, or a tool error when neither form is present or the number names nothing listed.
func (a *Agent) resolveStep(ctx context.Context, step map[string]any) (sequenceStep, string) {
	if _, ok := step["n"]; ok {
		it, errText := a.seenItem(ctx, step)
		if errText != "" {
			return sequenceStep{}, errText
		}
		it, errText = a.stillThere(ctx, it)
		if errText != "" {
			return sequenceStep{}, errText
		}
		x, y, w, h, frErr := a.freshRect(ctx, it)
		if frErr != "" {
			x, y, w, h = it.X, it.Y, it.W, it.H
		}
		return sequenceStep{x: x + w/2, y: y + h/2, label: it.Label}, ""
	}
	x, y, errText := a.picturePoint(ctx, step)
	if errText != "" {
		return sequenceStep{}, errText
	}
	label := ""
	if it, ok := itemAt(a.seen(ctx), x, y); ok {
		label = it.Label
	}
	return sequenceStep{x: x, y: y, label: label}, ""
}

// guardPoint runs the checks a single click at a point would face: a password or other secret field, and an irreversible action the user has not said go to. Input: the screen point and the front window's "app · title". Output: "" when the tap may fire, or the sentence explaining why it may not.
func (a *Agent) guardPoint(ctx context.Context, x, y int, window string) string {
	it, ok := itemAt(a.seen(ctx), x, y)
	if !ok {
		if verb := matchedVerb(act.Item{}, window); verb != "" && !consented(questionFrom(ctx), verb) && !goAllowed(ctx) {
			return fmt.Sprintf("%d,%d carries no label, so the window's own title in %q is all I have to go on, and it names a %s. %s", x, y, window, verb, consentPrompt(verb))
		}
		return ""
	}
	if secretField(it, window) {
		return fmt.Sprintf("%d,%d lands on %s %q in %q, which holds a password or another secret. Click it yourself if you want it focused.", x, y, it.Role, it.Label, window)
	}
	if irreversible(it, window, false) && !consented(questionFrom(ctx), matchedVerb(it, window)) && !goAllowed(ctx) {
		return fmt.Sprintf("%d,%d lands on [%d] %s %q in %q. %s", x, y, it.N, it.Role, it.Label, window, consentPrompt(matchedVerb(it, window)))
	}
	return ""
}

// click is the one tool for putting a pointer on something, in the order a locator should try: an element number from the last observe_screen list when the accessibility walk saw the thing, a bare point from the last look when it did not, and a "then" list of further taps when the UI will not survive a round trip between them.
// button picks which pointer button presses, "left" by default and "right" for the context menu; an unknown name is refused rather than clicked with the left button, since a click nobody asked for is what the model would otherwise get back.
func (a *Agent) click(ctx context.Context, args map[string]any) string {
	switch button, _ := args["button"].(string); strings.ToLower(strings.TrimSpace(button)) {
	case "", "left":
	case "right":
		return a.rightClick(ctx, args)
	default:
		return toolError(fmt.Sprintf("button is left or right, not %q", button))
	}
	if _, ok := args["then"]; ok {
		return a.clickSequence(ctx, args)
	}
	if _, ok := args["n"]; ok {
		return a.clickNumbered(ctx, args)
	}
	return a.clickPoint(ctx, args)
}

// clickNumbered presses the element the last observe_screen listed under n, through its own accessibility action where it has one and with the pointer where it does not. Input: the tool arguments, which must carry n. Output: what was clicked and what the window became, or the reason it was refused.
func (a *Agent) clickNumbered(ctx context.Context, args map[string]any) string {
	it, errText := a.seenItem(ctx, args)
	if errText != "" {
		return errText
	}
	if errText := a.frontWindowChanged(ctx); errText != "" {
		return errText
	}
	it, errText = a.stillThere(ctx, it)
	if errText != "" {
		return errText
	}
	window := a.currentWindow(ctx)
	if irreversible(it, window, false) && !consented(questionFrom(ctx), matchedVerb(it, window)) && !goAllowed(ctx) {
		return a.stopBeforeClick(ctx, it, window)
	}
	// The tap is shown at the element's live centre before the click fires, not only on the pointer fallback below: most clicks go through doAction, and those clicks deserve the same pointer indicator.
	fx, fy, fw, fh, frErr := a.freshRect(ctx, it)
	if frErr != "" {
		fx, fy, fw, fh = it.X, it.Y, it.W, it.H
	}
	cx, cy := fx+fw/2, fy+fh/2
	a.tapAt(cx, cy, it.Label)
	action, err := a.doAction(ctx, it.Ref)
	if err != nil {
		// An element with no accessibility action to fire is clicked where it sits instead: the real pointer moves to the centre of its rectangle, which is in the same picture pixels click_at uses. The tap already flew there above, so this branch does not tap again.
		if it.W <= 0 || it.H <= 0 {
			return toolError(fmt.Sprintf("could not click [%d] %s %q: %v", it.N, it.Role, it.Label, err))
		}
		dev, errText := a.inputDevice(ctx)
		if errText != "" {
			return toolError(fmt.Sprintf("could not click [%d] %s %q: %v; %s", it.N, it.Role, it.Label, err, errText))
		}
		before := a.beforePress(ctx)
		if perr := dev.ClickAt(float64(cx), float64(cy)); perr != nil {
			return toolError(fmt.Sprintf("could not click [%d] %s %q: %v; pointer: %v", it.N, it.Role, it.Label, err, perr))
		}
		if missed := a.pressCheck(ctx, before, cx, cy); missed != "" {
			return missed
		}
		action = "pointer"
	}
	a.rememberClick(ctx, it)
	a.rememberTarget(ScreenTarget{Label: it.Label, Role: it.Role, Window: window})
	clicked := fmt.Sprintf("clicked [%d] %s %q via %s", it.N, it.Role, it.Label, action)
	// Read the front window's title fresh, the same call observe_screen opens with, so the result says what the click actually did rather than what the stale pre-click list said. A click can resume, play or navigate to something other than what was asked, and the title is where that shows up first.
	_, title, _, err := a.observe(ctx)
	if err != nil || title == "" {
		return clicked + "; call observe_screen to see the result"
	}
	return fmt.Sprintf("%s; the window is now %q; check it matches what was asked, then call observe_screen if you need the list", clicked, title)
}

// clickPoint clicks a bare point on the screen with the real pointer, for anything the accessibility walk never listed: a canvas, a video, a custom widget. Input: the tool arguments, which must carry x and y in the last look's picture coordinates. Output: what was clicked and where it landed, or the reason it was refused.
func (a *Agent) clickPoint(ctx context.Context, args map[string]any) string {
	x, y, errText := a.picturePoint(ctx, args)
	if errText != "" {
		return errText
	}
	window := a.frontWindow(ctx)
	// A point does have a name when the last observe_screen listed something covering it, and then the click is checked exactly as a numbered click on that item would be. Only a point no listed rectangle covers falls back to the window's own title.
	lands := ""
	if it, ok := itemAt(a.seen(ctx), x, y); ok {
		lands = fmt.Sprintf("; the point lands on [%d] %s %q", it.N, it.Role, it.Label)
		if secretField(it, window) {
			return fmt.Sprintf("Stopped before clicking %d,%d, the point lands on %s %q in %q, which holds a password or another secret. Click it yourself if you want it focused.", x, y, it.Role, it.Label, window)
		}
		if irreversible(it, window, false) && !consented(questionFrom(ctx), matchedVerb(it, window)) && !goAllowed(ctx) {
			return fmt.Sprintf("Stopped before clicking %d,%d, the point lands on [%d] %s %q in %q. %s", x, y, it.N, it.Role, it.Label, window, consentPrompt(matchedVerb(it, window)))
		}
	} else if verb := matchedVerb(act.Item{}, window); verb != "" && !consented(questionFrom(ctx), verb) && !goAllowed(ctx) {
		return fmt.Sprintf("Stopped before clicking %d,%d in %q. A point on the screen carries no label, so the window's own title is all I have to go on, and it names a %s. %s", x, y, window, verb, consentPrompt(verb))
	}
	dev, errText := a.inputDevice(ctx)
	if errText != "" {
		return errText
	}
	// The tap indicator is drawn at this exact point, and beforePress photographs this exact point, so the indicator must be on screen before the camera starts or the two race: whichever shot catches the ring reads as a change that the press did not make. clickNumbered has always had them this way round.
	a.tapAt(x, y, strings.TrimPrefix(lands, "; the point lands on "))
	before := a.beforePress(ctx)
	if err := dev.ClickAt(float64(x), float64(y)); err != nil {
		return toolError(fmt.Sprintf("could not click %d,%d: %v", x, y, err))
	}
	if missed := a.pressCheck(ctx, before, x, y); missed != "" {
		// The click went out whatever the pixels say, so the keyboard has moved with the pointer and the session no longer knows where it is. Saying nothing here left type_text typing into a field the click had already left.
		a.focusLost(ctx)
		return missed
	}
	// The pointer has moved the keyboard somewhere this session cannot name, whatever was under the point, so type_text and a focused key press refuse until a fresh observe_screen or a numbered click says where the keyboard is again.
	a.focusLost(ctx)
	return fmt.Sprintf("clicked %d,%d on the screen%s; look or call observe_screen to see what it did", x, y, lands)
}

// rightClick presses the right pointer button on whatever the call named, which is what opens a context menu. There is no accessibility path for it: a node's showContextMenu action is Chromium's alone and the walk deliberately never fires it (see pickAction in internal/tracker), so this always drives the real pointer — at the centre of a numbered element's rectangle, or at a bare point read off the last look. Input: the click arguments, carrying n from the latest observe_screen list or x and y in the last look's picture coordinates. Output: the line saying where it clicked, or the reason it was refused, in which case nothing was pressed.
func (a *Agent) rightClick(ctx context.Context, args map[string]any) string {
	if _, ok := args["then"]; ok {
		return toolError("a right-click is one press; leave then out and call click again for the menu item")
	}
	step, errText := a.resolveStep(ctx, args)
	if errText != "" {
		return errText
	}
	// The same stop line a left click at that point would face. Opening a menu commits to nothing on its own, but the point is resolved against the same listing, and a control this session may not press is not one it may open a menu on either.
	window := a.frontWindow(ctx)
	if stop := a.guardPoint(ctx, step.x, step.y, window); stop != "" {
		return "Stopped before right-clicking: " + stop
	}
	dev, errText := a.inputDevice(ctx)
	if errText != "" {
		return errText
	}
	a.tapAt(step.x, step.y, step.label)
	if err := dev.RightClickAt(float64(step.x), float64(step.y)); err != nil {
		return toolError(fmt.Sprintf("could not right-click %s: %v", step.describe(), err))
	}
	// The pointer has moved the keyboard somewhere this session cannot name, exactly as a left click at a point does, so type_text and a focused key press refuse until a fresh observe_screen or a numbered click says where it is.
	a.focusLost(ctx)
	return fmt.Sprintf("right-clicked %s; call observe_screen to see the menu it opened", step.describe())
}
