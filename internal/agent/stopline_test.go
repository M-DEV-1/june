package agent

import (
	"context"
	"strings"
	"testing"

	"june/internal/act"
)

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

// A field is refused outright, with no consent phrase able to unlock it, either because its own label names a secret — the whole point is that June never types a password, a card number or a code on the user's behalf — or because a password box carries no label at all (the accessibility walk blanks it), so its role alone tells type_text to refuse it.
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

	got := a.executeTool(context.Background(), "type_text", map[string]any{"text": "june"})

	if len(f.typed) != 0 {
		t.Errorf("typed = %v, want nothing typed when the field definitely does not hold the keyboard", f.typed)
	}
	if !strings.Contains(got, "no place to type") {
		t.Errorf("result = %q, want the refusal", got)
	}
}
