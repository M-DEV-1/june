package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ora/internal/act"
)

// irreversible is the code-level backstop for the user's stop-line rule ("never click or type into anything that sends, pays, deletes or submits unless I said go"): a description in the system prompt is something a model can talk itself out of, this is not.
func TestIrreversible_TableCases(t *testing.T) {
	cases := []struct {
		name  string
		label string
		role  string
		win   string
		typed bool
		want  bool
	}{
		{"send button", "Send", "push button", "Mail", false, true},
		{"post to slack", "Post to Slack", "push button", "Slack", false, true},
		{"pay now", "Pay now", "push button", "Checkout", false, true},
		{"buy it now", "Buy it now", "push button", "Amazon", false, true},
		{"confirm order", "Confirm Order", "push button", "Checkout", false, true},
		{"place order", "Place Order", "push button", "Checkout", false, true},
		{"checkout button", "Checkout", "push button", "Cart", false, true},
		{"delete", "Delete", "push button", "Files", false, true},
		{"remove item", "Remove item", "link", "Cart", false, true},
		{"submit", "Submit", "push button", "Form", false, true},
		{"place your order, Amazon's wording", "Place your order", "push button", "Review your order", false, true},
		{"confirm your order", "Confirm your order", "push button", "Review", false, true},
		{"cancel inside a deleting window", "Cancel", "push button", "Delete Account", false, false},
		{"back inside a checkout window", "Back", "push button", "Checkout", false, false},
		{"close inside a send window", "Close", "push button", "Send message", false, false},
		{"unsubscribe", "Unsubscribe", "link", "Newsletter", false, true},
		{"sign out", "Sign out", "push button", "Settings", false, true},
		{"transfer funds", "Transfer", "push button", "Bank", false, true},
		// A compound built on a trigger word still counts: the left edge is anchored, the right is not.
		{"sender compound", "Sender ID", "push button", "Settings", false, true},
		{"posting compound", "posting rules", "link", "Forum", false, true},
		// The window can name the commitment even when the control itself does not.
		{"window names checkout", "Continue", "push button", "Checkout", false, true},
		// Ordinary controls never trip it.
		{"reload", "Reload", "push button", "Brave", false, false},
		{"merge", "Merge", "push button", "GitHub", false, false},
		{"address bar", "Address and search bar", "entry", "Brave", false, false},
		// Sign IN is not sign out, and never appears in the word list.
		{"sign in", "Sign in", "push button", "Settings", false, false},
		// A password field only matters when something is about to be typed into it: clicking to focus it changes nothing.
		{"password field clicked", "", "password text", "Settings", false, false},
		{"password field typed", "", "password text", "Settings", true, true},
		{"card number field typed", "Card number", "entry", "Billing", true, true},
		{"cvv field typed", "CVV", "entry", "Billing", true, true},
		{"otp field typed", "OTP", "entry", "Bank", true, true},
		{"pin field typed", "PIN", "entry", "Bank", true, true},
		{"account number field typed", "Account number", "entry", "Bank", true, true},
		// The same field clicked, not typed into, does not trip the secret half of the rule.
		{"card number field clicked", "Card number", "entry", "Billing", false, false},
		{"ordinary field typed", "Message", "entry", "Slack", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			item := act.Item{Role: c.role, Label: c.label}
			if got := irreversible(item, c.win, c.typed); got != c.want {
				t.Errorf("irreversible(%+v, %q, %v) = %v, want %v", item, c.win, c.typed, got, c.want)
			}
		})
	}
}

// consented reads the same word list back out of what the user actually said, loosened for a sentence rather than a button label.
func TestConsented_TableCases(t *testing.T) {
	cases := []struct {
		question string
		verb     string
		want     bool
	}{
		{`yes, send it`, "send", true},
		{`go ahead and delete`, "delete", true},
		{`confirm the order`, "confirm order", true},
		{`please do place the order`, "place order", true},
		{`yes, sign out`, "sign out", true},
		{`click send`, "send", false},                                                      // names the action but never affirms it
		{`yes`, "send", false},                                                             // affirms but names nothing
		{`yes, that looks right`, "send", false},                                           // affirms something unrelated
		{`go ahead and delete the spam email, then check the shopping cart`, "buy", false}, // consent for one action must not unlock another
		{`yes, send it`, "delete", false},
		{``, "send", false},
	}
	for _, c := range cases {
		if got := consented(c.question, c.verb); got != c.want {
			t.Errorf("consented(%q, %q) = %v, want %v", c.question, c.verb, got, c.want)
		}
	}
}

// guardedClickAgent wires an Agent whose only observed element trips the stop-line rule, so click's refusal path can be exercised through executeTool the way the model would actually call it.
func guardedClickAgent(t *testing.T) (*Agent, *fakeActions, *[]string) {
	t.Helper()
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "FAKE_API_KEY")
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		return "mail", "Compose", []act.Node{
			{Role: "push button", Label: "Send", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-send"},
		}, nil
	}
	var rings []string
	a.Point = func(x, y, w, h int, label string) error { rings = append(rings, label); return nil }
	f := &fakeActions{}
	a.doAction = func(ctx context.Context, ref string) (string, error) {
		f.clicked = append(f.clicked, ref)
		return "press", nil
	}
	// NewAgent defaults verify and extents to the tracker's (real accessibility bus); a fake ref would fail both unconditionally.
	a.verify = func(ctx context.Context, ref, role, label string, x, y, w, h int) error { return nil }
	a.extents = rectFromObserve(a)
	return a, f, &rings
}

func TestExecuteTool_Click_StopsBeforeIrreversibleWithoutConsent(t *testing.T) {
	a, f, rings := guardedClickAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	if len(f.clicked) != 0 {
		t.Errorf("click must not have run, clicked = %v", f.clicked)
	}
	if len(*rings) != 1 || (*rings)[0] != "Send" {
		t.Errorf("rings = %v, want the guarded element ringed once", *rings)
	}
	if !strings.HasPrefix(got, "Stopped before ") {
		t.Errorf("result = %q, want it to begin \"Stopped before \"", got)
	}
	if !strings.Contains(got, "Send") || !strings.Contains(got, "Compose") {
		t.Errorf("result = %q, want it to name the control and the window", got)
	}
	if !strings.Contains(got, `"yes, send it"`) {
		t.Errorf("result = %q, want the one-line question naming the exact phrase to say", got)
	}
}

func TestExecuteTool_Click_ProceedsWhenTheQuestionAlreadyConsented(t *testing.T) {
	a, f, _ := guardedClickAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	ctx := WithQuestion(context.Background(), "yes, send it")
	got := a.executeTool(ctx, "click", map[string]any{"n": float64(1)})
	if len(f.clicked) != 1 || f.clicked[0] != "r-send" {
		t.Errorf("click should have run with consent, clicked = %v", f.clicked)
	}
	if !strings.Contains(got, "clicked") {
		t.Errorf("result = %q", got)
	}
}

// An unguarded label (nothing in the stop-line list) needs no consent at all — most of what click does is navigation, not commitment.
func TestExecuteTool_Click_UnguardedNeedsNoConsent(t *testing.T) {
	a, f := actingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	if len(f.clicked) != 1 || !strings.Contains(got, "clicked") {
		t.Errorf("unguarded click should run without consent: clicked=%v got=%q", f.clicked, got)
	}
}

// A click that goes through leaves the field behind for type_text to read back as "the field with focus".
func TestExecuteTool_Click_RemembersTheFieldForTypeText(t *testing.T) {
	a, f := actingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	a.executeTool(context.Background(), "click", map[string]any{"n": float64(2)}) // the address entry
	if len(f.clicked) != 1 {
		t.Fatalf("clicked = %v, want the address entry pressed", f.clicked)
	}
	got := a.executeTool(context.Background(), "type_text", map[string]any{"text": "example.com", "enter": true})
	if len(f.typed) != 1 {
		t.Errorf("typing into an ordinary field should run, typed = %v, got %q", f.typed, got)
	}
}

// A field is refused outright, with no consent phrase able to unlock it, either because its own label names a secret — the whole point is that Ora never types a password, a card number or a code on the user's behalf — or because a password box carries no label at all (the accessibility walk blanks it), so its role alone tells type_text to refuse it.
func TestExecuteTool_TypeText_RefusesSecretFieldOutright(t *testing.T) {
	cases := []struct {
		name       string
		win        string
		node       act.Node
		question   string
		text       string
		wantSubstr string
	}{
		{
			// Billing, not Checkout: the window itself must not carry an action word, or the click that focuses the field would be stopped by that before type_text's own secret-field check is ever reached.
			name:       "labelled a card number",
			win:        "Billing",
			node:       act.Node{Role: "entry", Label: "Card number", X: 10, Y: 20, W: 200, H: 30, Showing: true, Ref: "r-card"},
			question:   "yes, type it, go ahead",
			text:       "4111111111111111",
			wantSubstr: "secrets",
		},
		{
			name:       "password role with no label",
			win:        "Sign in",
			node:       act.Node{Role: "password text", Label: "", X: 10, Y: 20, W: 200, H: 30, Showing: true, Ref: "r-pass"},
			text:       "hunter2",
			wantSubstr: "Stopped before ",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, f := actingAgent(t)
			a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
				return "brave", c.win, []act.Node{c.node}, nil
			}
			ctx := context.Background()
			if c.question != "" {
				ctx = WithQuestion(ctx, c.question)
			}
			a.executeTool(context.Background(), "observe_screen", map[string]any{})
			a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
			got := a.executeTool(ctx, "type_text", map[string]any{"text": c.text})
			if len(f.typed) != 0 {
				t.Errorf("must never type into a secret field, typed = %v", f.typed)
			}
			if !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, c.wantSubstr) {
				t.Errorf("result = %q, want an outright refusal containing %q", got, c.wantSubstr)
			}
		})
	}
}

// Before any click has happened, type_text has no field to blame — it must fall through to typing rather than refuse something it cannot describe.
func TestExecuteTool_TypeText_WithNoFocusedFieldYetTypes(t *testing.T) {
	a, f := actingAgent(t)
	got := a.executeTool(context.Background(), "type_text", map[string]any{"text": "ora"})
	if len(f.typed) != 1 || !strings.Contains(got, "typed") {
		t.Errorf("typed=%v got=%q, want it to type with nothing known about focus", f.typed, got)
	}
}

// click's own verify guard (added alongside the stop line) refuses to act when the node no longer matches what observe_screen described, before the stop-line check even runs.
func TestExecuteTool_Click_RefusesWhenVerifyFails(t *testing.T) {
	a, f := actingAgent(t)
	a.verify = func(ctx context.Context, ref, role, label string, x, y, w, h int) error {
		return context.DeadlineExceeded
	}
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	if len(f.clicked) != 0 {
		t.Errorf("click must not have run when verify fails, clicked = %v", f.clicked)
	}
	if !strings.Contains(got, "look again") {
		t.Errorf("result = %q, want it to say to look again", got)
	}
}

// The keyboard-focus read has three answers, not two: focused, not focused, and nothing either way. A definite not-focused with a readable element holding the keyboard that is no place to type is what the guard is for — the click landed somewhere the keys will not reach — and typing is refused.
func TestExecuteTool_TypeText_RefusesWhenTheFieldNoLongerHoldsTheKeyboard(t *testing.T) {
	a, f := actingAgent(t)
	a.focused = func(context.Context, string) (bool, error) { return false, nil }
	holdsKeyboard(t, act.Node{Role: "push button", Label: "Send", Ref: "r-send"}, true)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	a.executeTool(context.Background(), "click", map[string]any{"n": float64(2)})

	got := a.executeTool(context.Background(), "type_text", map[string]any{"text": "ora"})

	if len(f.typed) != 0 {
		t.Errorf("typed = %v, want nothing typed when the field definitely does not hold the keyboard", f.typed)
	}
	if !strings.Contains(got, "could not identify the field") {
		t.Errorf("result = %q, want the refusal", got)
	}
}

// A read that says nothing either way — the bus timed out, the element has gone, or the toolkit does not publish the focused bit on the node the walk listed, which Chromium and Electron often do not — is not evidence that the focus moved, and refusing on it stops typing into fields that are perfectly focused. The remembered click stands and the text goes in.
func TestExecuteTool_TypeText_TypesWhenTheFocusCannotBeRead(t *testing.T) {
	a, f := actingAgent(t)
	a.focused = func(context.Context, string) (bool, error) {
		return false, errors.New("the accessibility bus gave no state for r-address")
	}
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	a.executeTool(context.Background(), "click", map[string]any{"n": float64(2)})

	got := a.executeTool(context.Background(), "type_text", map[string]any{"text": "ora"})

	if len(f.typed) != 1 || !strings.Contains(got, "typed") {
		t.Errorf("typed = %v, result = %q, want the text typed on an unreadable focus rather than a refusal", f.typed, got)
	}
}
