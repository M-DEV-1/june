package agent

import (
	"strings"
	"testing"
)

// observeResult must append the frame-only hint when the listing is nothing but frame buttons.
func TestObserveResult_AppendsFrameOnlyHintWhenFrameOnly(t *testing.T) {
	lines := []string{
		`[1] push button "Minimize" (51,52)`,
		`[2] push button "Restore" (100,52)`,
		`[3] push button "Close" (150,52)`,
	}
	got := observeResult("Brave Browser", "(1179) Elixir compilation auto optimization - YouTube - Brave", lines, screenSnapshot{})
	if !strings.Contains(got, frameOnlyHint) {
		t.Errorf("observeResult() = %q, want it to contain the frame-only hint", got)
	}
}

// observeResult must not append the frame-only hint on a normal listing with real content.
func TestObserveResult_NoFrameOnlyHintWithRealContent(t *testing.T) {
	lines := []string{
		`[1] push button "Send" (50,35)`,
	}
	got := observeResult("Mail", "Inbox", lines, screenSnapshot{})
	if strings.Contains(got, frameOnlyHint) {
		t.Errorf("observeResult() = %q, want it to not contain the frame-only hint", got)
	}
}

// A browser's tabs and address bar are never in the accessibility listing, so without being told, a model reads their absence as "I cannot close a tab" and says so out loud — which is what happened three times on 2026-09-12. The hint has to ride the look that found the browser.
func TestObserveResult_TellsABrowserItsTabsAreReachedByKey(t *testing.T) {
	lines := []string{`[1] link "Careers" (300,200)`}
	for _, app := range []string{"Brave Browser", "Chromium", "Google Chrome", "Firefox"} {
		got := observeResult(app, "Jobs | LinkedIn", lines, screenSnapshot{})
		if !strings.Contains(got, browserKeysHint) {
			t.Errorf("observeResult(%q) = %q, want it to carry the browser keys hint", app, got)
		}
	}
}

// The short answers a repeat look gets exist to save tokens, and the hint is already sitting in the conversation from the full listing of this same window, so repeating it on every look would spend those tokens back for nothing. A window it has not described yet always renders as a full listing, which is where the hint lands.
func TestObserveResult_DoesNotRepeatTheBrowserHintOnAnUnchangedLook(t *testing.T) {
	lines := []string{`[1] link "Careers" (300,200)`}
	before := screenSnapshot{app: "Brave Browser", title: "Jobs | LinkedIn", lines: lines}
	got := observeResult("Brave Browser", "Jobs | LinkedIn", lines, before)
	if !strings.Contains(got, unchangedScreenMarker) {
		t.Fatalf("observeResult() = %q, want the unchanged answer this test is about", got)
	}
	if strings.Contains(got, browserKeysHint) {
		t.Errorf("observeResult() = %q, want no browser keys hint on the unchanged answer", got)
	}
}

// On 2026-09-12 the user said "you can delete note". June called query_memory, got [note#312] back, then said "Understood, I've deleted that note about the supplement then" and never called revise. The note is still in the store. The system prompt already said to fix memory in the same turn; it lost against nine thousand other tokens. The affordance has to sit on the result the model is reading at the moment it decides.
func TestNoteRefsCarryTheReviseAffordance(t *testing.T) {
	lines := []string{
		`[note#312] The user takes one scoop of a supplement every morning.`,
		`[summary (Thu Sep 3 15:15, 8d ago)] chatted with Vexil Quorin on Microsoft Teams`,
	}
	got := withReviseHint(strings.Join(lines, "\n"))
	if !strings.Contains(got, reviseHint) {
		t.Errorf("withReviseHint() = %q, want the revise hint on a result holding a note ref", got)
	}
	// The hits themselves must come through untouched; the hint is an addition, never a rewrite.
	for _, line := range lines {
		if !strings.Contains(got, line) {
			t.Errorf("withReviseHint() = %q, want it to still contain %q", got, line)
		}
	}
}
