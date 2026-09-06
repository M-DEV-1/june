package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The user's live thread on 2026-09-05: turn 1 "ring the refresh button" ringed item [n] "Reload"; turn 2 "draw a circle around it" resolved "it" against the fresh screen listing instead of turn 1's target and circled the address bar. isBareReference is the check that decides when a question needs the last-target reminder at all: a question that already names its own thing to act on ("click the address bar") gets no reminder, since one would only get in the way.
func TestIsBareReference(t *testing.T) {
	cases := []struct {
		question string
		want     bool
	}{
		{"draw a circle around it", true},
		{"ring it", true},
		{"do that again", true},
		{"click the same one", true},
		{"ring this", true},
		{"ring the refresh button", false},
		{"click the address bar", false},
		{"read the numbers on screen", false},
		{"draw a circle around the reload button", false},
	}
	for _, c := range cases {
		if got := isBareReference(c.question); got != c.want {
			t.Errorf("isBareReference(%q) = %v, want %v", c.question, got, c.want)
		}
	}
}

// A turn's screen target must survive an observe_screen call within the same ask (draw and point_at both call observe_screen first, or ran one earlier in the same turn) and across two separate asks answered by the same agent, which is the one piece of screen memory that outlives an ask.
func TestScreenTarget_SurvivesObserveScreenAndCarriesToTheNextAsk(t *testing.T) {
	a, rings := observingAgent(t)
	if _, ok := a.screenTarget(); ok {
		t.Fatal("a fresh agent must remember no target")
	}

	got := a.executeTool(t.Context(), "observe_screen", map[string]any{})
	if strings.HasPrefix(got, "error:") {
		t.Fatalf("observe_screen: %s", got)
	}
	got = a.executeTool(t.Context(), "point_at", map[string]any{"n": float64(1)})
	if !strings.Contains(got, "ringed [1]") {
		t.Fatalf("point_at: %s", got)
	}
	if len(*rings) != 1 {
		t.Fatalf("%d rings drawn, want 1", len(*rings))
	}

	target, ok := a.screenTarget()
	if !ok {
		t.Fatal("expected a remembered target after point_at")
	}
	if target.Label != "Merge" || target.Role != "push button" {
		t.Errorf("target = %+v, want the Merge button", target)
	}
	if target.Window == "" {
		t.Error("target carried no window")
	}
	if !target.HasRect {
		t.Error("target from point_at should carry a rectangle")
	}

	// A later observe_screen in a new ask must not wipe out the target: it is what the next ask's bare "it" resolves against, and the whole point is that a fresh screen read is not what "it" means.
	a.executeTool(t.Context(), "observe_screen", map[string]any{})
	again, ok := a.screenTarget()
	if !ok || again.Label != "Merge" {
		t.Errorf("target after a later observe_screen = %+v, ok=%v, want it to survive", again, ok)
	}
}

// draw's "on" form must name the item it drew around, the way point_at and click already do, so a later ask has something to remember and so the model can tell from the tool's own result whether it drew around the right thing.
func TestDraw_OnItemNamesTheItemInItsResult(t *testing.T) {
	a, _ := observingAgent(t)
	a.Draw = func(shape string, points [][2]int, x, y, w, h int, label string) error { return nil }
	a.executeTool(t.Context(), "observe_screen", map[string]any{})
	got := a.executeTool(t.Context(), "draw", map[string]any{"shape": "circle", "on": float64(1)})
	if !strings.Contains(got, "Merge") {
		t.Errorf("draw result = %q, want it to name the item drawn around", got)
	}
	target, ok := a.screenTarget()
	if !ok || target.Label != "Merge" {
		t.Errorf("target after draw(on) = %+v, ok=%v, want the Merge button remembered", target, ok)
	}
}

// TestDraw_MismatchNote drives the same turn-1-rings-Merge, turn-2-draws setup for three questions and checks the mismatch note each one must or must not produce. The regression is the night of 2026-09-05: turn 1 rings one item, turn 2 says "draw a circle around it" and the model (wrongly, for the first row) picks a different item — the draw tool result must say so, one line, so the model can correct itself inside the same ask rather than the ring standing as the last word on what "it" meant. The note must not fire when the question already named its own target (naming a different button on purpose is not a mistake to flag) or when the picked item is in fact the remembered one (a correct answer earns no correction).
func TestDraw_MismatchNote(t *testing.T) {
	cases := []struct {
		name        string
		question    string
		on          float64
		wantMatch   bool // note names both "Merge" (remembered) and "Checks" (picked)
		wantNoMatch bool
	}{
		{name: "bare reference picks a different item", question: "draw a circle around it", on: 2, wantMatch: true},
		{name: "question names its own target", question: "draw a circle around the checks link", on: 2, wantNoMatch: true},
		{name: "picked item matches the remembered one", question: "draw a circle around it", on: 1, wantNoMatch: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := observingAgent(t)
			a.Draw = func(shape string, points [][2]int, x, y, w, h int, label string) error { return nil }
			a.executeTool(t.Context(), "observe_screen", map[string]any{})
			a.executeTool(t.Context(), "point_at", map[string]any{"n": float64(1)}) // rings [1] Merge

			ctx := WithQuestion(t.Context(), c.question)
			got := a.executeTool(ctx, "draw", map[string]any{"shape": "circle", "on": c.on})
			if c.wantMatch && (!strings.Contains(got, `you were asked about "Merge"`) || !strings.Contains(got, `this is "Checks"`)) {
				t.Errorf("draw result = %q, want a mismatch note naming both items", got)
			}
			if c.wantNoMatch && strings.Contains(got, "you were asked about") {
				t.Errorf("draw result = %q, want no mismatch note", got)
			}
		})
	}
}

// End-to-end regression for the night of 2026-09-05: turn 1 rings the Merge button, and turn 2 in the same conversation — same agent, since the remembered target is agent-scoped where the rest of the screen state belongs to one ask — says "draw a circle around it". The request the second askText round sends must carry a sentence naming what was last pointed at, so a smart model answers over the right item instead of guessing from a fresh screen read.
func TestAskText_BareFollowUpCarriesTheLastTargetHint(t *testing.T) {
	var bodies []string
	call := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		call++
		switch call {
		case 1:
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"observe_screen","args":{}}}]}}]}`)
		case 2:
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"point_at","args":{"n":1}}}]}}]}`)
		case 3:
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Ringed the merge button."}]}}]}`)
		case 4:
			// The numbered list belongs to one ask (see askLookState), so the second turn walks the screen for itself before it names a number.
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"observe_screen","args":{}}}]}}]}`)
		case 5:
			// Deliberately the wrong item, [2] Checks rather than [1] Merge, to also exercise the mismatch note the tool result carries back.
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"draw","args":{"shape":"circle","on":2}}}]}}]}`)
		default:
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Drew a circle."}]}}]}`)
		}
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })
	a, _ := observingAgent(t)
	a.Draw = func(shape string, points [][2]int, x, y, w, h int, label string) error { return nil }

	tr1, err := a.askText(t.Context(), "gemini-test", nil, "ring the merge button")
	if err != nil {
		t.Fatal(err)
	}
	if tr1.LastTarget == nil || tr1.LastTarget.Label != "Merge" {
		t.Fatalf("turn 1 trace LastTarget = %+v, want the Merge button", tr1.LastTarget)
	}

	tr2, err := a.askText(t.Context(), "gemini-test", nil, "draw a circle around it")
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 6 {
		t.Fatalf("%d requests, want 6", len(bodies))
	}
	if !strings.Contains(bodies[3], "Merge") {
		t.Errorf("the follow-up's own request did not carry the last-target hint: %s", bodies[3])
	}
	var drewResult string
	for _, hop := range tr2.ToolHops {
		if hop.Name == "draw" {
			drewResult = hop.Result
		}
	}
	if !strings.Contains(drewResult, `you were asked about "Merge"`) {
		t.Errorf("draw hop result = %q, want the mismatch note", drewResult)
	}
	if tr2.LastTarget == nil || tr2.LastTarget.Label != "Checks" {
		t.Fatalf("turn 2 trace LastTarget = %+v, want it updated to the Checks link that was actually drawn on", tr2.LastTarget)
	}
}

// The last-target hint belongs to the window it was made in: a target remembered from a mail window says nothing about a bare "it" asked over a browser, so the hint is only added when the window in front now is the one the target came from, or when no front window can be read at all.
func TestHintApplies_OnlyInTheTargetsOwnWindow(t *testing.T) {
	remembered := ScreenTarget{Label: "Submit", Role: "push button", Window: "MailClient · Compose"}
	if hintApplies(remembered, "Brave · Shopping Cart") {
		t.Error("hint applied over a different window")
	}
	if !hintApplies(remembered, "MailClient · Compose") {
		t.Error("hint dropped in the target's own window")
	}
	if !hintApplies(remembered, "") {
		t.Error("hint dropped when the front window could not be read")
	}
}
