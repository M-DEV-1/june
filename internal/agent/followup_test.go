package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	a.Draw = func(_, shape string, points [][2]int, x, y, w, h int, label string) error { return nil }

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
