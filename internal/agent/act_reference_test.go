package agent

import (
	"strings"
	"testing"
	"time"

	"june/internal/db"
)

// refNow is the clock every test in this file renders against, so the ages in the expected strings are fixed rather than whatever the machine says today.
var refNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// TestRenderActReferenceNeverQuotesWhatWasTyped checks the one thing that must never come back out: a step that typed says what it typed into, never what was typed, because what the user dictated can be a passphrase or a private message.
func TestRenderActReferenceNeverQuotesWhatWasTyped(t *testing.T) {
	m := db.ActMatch{
		When:  refNow.Add(-time.Hour),
		Score: 1,
		Run: db.ActRun{
			Question: "search for the thing",
			Steps: []db.ActStep{
				{Name: "click", Args: map[string]any{"n": float64(9)}, Result: `clicked [9] entry "Address and search bar" via activate`},
				// A row written before the storage layer started dropping it still carries the text, so the rendering must ignore it rather than trust it is gone.
				{Name: "type_text", Args: map[string]any{"enter": true, "text": "hunter2-my-password"}, Result: "typed 19 characters"},
			},
		},
	}
	got := RenderActReference(m, refNow)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("RenderActReference = %q, which carries what was typed", got)
	}
	if !strings.HasSuffix(got, "typed into the box in front and pressed Enter.") {
		t.Errorf("RenderActReference = %q, want it to end by saying what was typed into", got)
	}
}
