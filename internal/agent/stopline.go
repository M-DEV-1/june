package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"ora/internal/act"
)

// actionPattern matches the words and short phrases a control's own label or window title uses when pressing it, or typing into what it names, commits to something that cannot be undone from the screen: sending, submitting, posting, publishing, paying, buying, confirming or placing an order, checking out, deleting, removing, unsubscribing, signing out, or transferring. It is checked against the label a control carries as observe_screen showed it, not against free speech, so the phrases are the exact words a button or a page title actually uses ("Confirm Order"), not the looser way a person says the same thing ("confirm the order") — consentPattern below is the loosened form of the same list, for reading that back out of what the user said. \b anchors only the left edge of each word, not the right, so "Sender" and "posting" still trip it — deliberately: catching a compound like that costs nothing but an extra question, where missing one risks an unwanted send.
var actionPattern = regexp.MustCompile(`(?i)\b(?:send|submit|post|publish|pay|buy|confirm(?:\s+(?:your|the))?\s+order|place(?:\s+(?:your|the))?\s+order|checkout|delete|remove|unsubscribe|sign out|transfer)`)

// exitPattern matches a label that only leaves a screen without committing to anything: Cancel, Back, Close, Dismiss, "No thanks", "Not now". A control with one of these as its whole label is never irreversible, whatever the window it sits in is called, since stopping before Cancel on a "Delete Account" page would ask the user to say "yes, delete it" in order to not delete anything.
var exitPattern = regexp.MustCompile(`(?i)^\s*(?:cancel|back|go back|close|dismiss|no thanks|not now)\s*$`)

// secretPattern matches the words that mark a field as holding a secret rather than an ordinary value: a password, a card number, a CVV, a one-time code, a PIN, or a bank account number. Checked against the field's own label and the window title, the only two places this session can read a field's context from — it has no way to read the plain text sitting next to a field on the screen.
var secretPattern = regexp.MustCompile(`(?i)\b(?:password|card|cvv|cvc|otp|pin|account\s*number)\b`)

// affirmPattern matches the words that make a question an explicit go-ahead rather than merely describing what the user wants done: "delete the old draft" names a task, "yes, delete it" authorizes this exact step.
var affirmPattern = regexp.MustCompile(`(?i)\b(?:yes|yeah|yep|go ahead|confirm(?:ed)?|do it|please do)\b`)

// irreversible reports whether item — the control a click is about to press, or the field type_text is about to fill — commits to something that cannot be undone from the screen. Input: the target's role and label as observe_screen showed it, the window it sits in (checked too, since a page titled "Checkout" names the commitment even when its own button just says "Continue"), and whether this is a type_text call rather than a click — secretField only matters when something is about to be typed into a field; clicking one to focus it changes nothing. Output: true when the target's name, role or window names one of the irreversible actions, or (typed only) the target is a password, card, CVV, OTP, PIN or account-number field.
func irreversible(item act.Item, window string, typed bool) bool {
	if exitPattern.MatchString(item.Label) {
		return false
	}
	if actionPattern.MatchString(item.Label + " " + item.Role + " " + window) {
		return true
	}
	return typed && secretField(item, window)
}

// secretField reports whether item is a field that holds a secret rather than an ordinary value: an AT-SPI password box (whose label the accessibility walk already blanks, so its role is the only signal left), or a field whose label or window names a card, CVV, OTP, PIN or account number. type_text refuses one of these outright — never with a "say go and I will", since a secret already typed cannot be untyped the way an unsent click can be left unclicked.
func secretField(item act.Item, window string) bool {
	return item.Role == "password text" || secretPattern.MatchString(item.Label+" "+window)
}

// consented reports whether question carries the user's own explicit go-ahead for the one irreversible step about to run, rather than only describing what they want done. Input: the ask's own question text (see WithQuestion) and the action word matchedVerb found on the control. Output: true when the text pairs an affirming word (yes, go ahead, confirm, do it) with that same action word — "yes, send it" consents to a send and to nothing else, so a question that said "go ahead and delete the spam" does not unlock a Buy button later in the same turn; a bare "send it" or a bare "yes" never consents, since consent has to affirm and name the action.
func consented(question, verb string) bool {
	return affirmPattern.MatchString(question) && verbPattern(verb).MatchString(question)
}

// verbPattern turns the action word matchedVerb found on a control into the loosened form a person says it in: each word may take a suffix ("confirming", "deleting"), the words of a phrase may have "your" or "the" between them ("place the order"), and "sign out" may be written as one word. Input: the verb, possibly still carrying its "your" or "the". Output: a pattern that matches nothing when the verb is empty.
func verbPattern(verb string) *regexp.Regexp {
	var words []string
	for _, w := range strings.Fields(strings.ToLower(verb)) {
		if w != "your" && w != "the" {
			words = append(words, regexp.QuoteMeta(w))
		}
	}
	if len(words) == 0 {
		return regexp.MustCompile(`$^`)
	}
	return regexp.MustCompile(`(?i)\b` + strings.Join(words, `\w*\s*(?:your\s+|the\s+)?`))
}

// matchedVerb returns the action word or phrase actionPattern matched in item/window, lower-cased, for building the question a stop asks. Output: "" when nothing in the action list matched — the secret-field stop asks a different, fixed question instead of naming a verb.
func matchedVerb(item act.Item, window string) string {
	var words []string
	for _, w := range strings.Fields(strings.ToLower(actionPattern.FindString(item.Label + " " + item.Role + " " + window))) {
		if w != "your" && w != "the" {
			words = append(words, w)
		}
	}
	return strings.Join(words, " ")
}

// consentPrompt is the one-line follow-up a stop ends with, telling the user exactly what to say to unlock the step it just refused. Input: the verb matchedVerb returned. Output: "Say ... and I will." — without "it" for the phrases where "yes, sign out it" or "yes, checkout it" would not be English.
func consentPrompt(verb string) string {
	switch verb {
	case "sign out", "unsubscribe", "confirm order", "place order", "checkout":
		return fmt.Sprintf("Say %q and I will.", "yes, "+verb)
	default:
		return fmt.Sprintf("Say %q and I will.", "yes, "+verb+" it")
	}
}

// blindConsent reports whether the user's own words let this session type or press a key it cannot say the target of. A control it could name is checked by consented, which needs the question to affirm and name that control's action; a focus this session cannot identify has no action to name, so an affirming word on its own ("yes", "go ahead", "do it") is the whole of the consent available here. Input: the ask's own question text. Output: true when it carries one of those words.
func blindConsent(question string) bool {
	return affirmPattern.MatchString(question)
}

// goKey is the unexported context key WithGo stores under.
type goKey struct{}

// WithGo marks ctx as carrying the user's explicit go-ahead for the one irreversible step about to run, set from the ask request's own "go": true field (see internal/ipc's Ask) for that turn only, never persisted past it. This is a second, structured way to grant consent alongside consented() reading an explicit phrase out of the question text itself — internal/ipc's request shape predates that word list and is outside this package's own reach, so both paths still count.
func WithGo(ctx context.Context) context.Context {
	return context.WithValue(ctx, goKey{}, true)
}

// goAllowed reports whether ctx carries the user's go-ahead from WithGo.
func goAllowed(ctx context.Context) bool {
	v, _ := ctx.Value(goKey{}).(bool)
	return v
}

// currentWindow names the window the last observe_screen answer was of, in the same "app · title" form frontWindowChanged uses, for a stop message to say where the control or field sits. Output: "" when nothing has been observed yet.
func (a *Agent) currentWindow(ctx context.Context) string {
	last := a.lastScreen(ctx)
	if last.title != "" {
		return last.app + " · " + last.title
	}
	return last.app
}
