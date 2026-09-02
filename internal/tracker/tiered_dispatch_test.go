package tracker

import "testing"

// TestResolveCapture_AlternatingTiersOnUnchangedScreenDoesNotReemit is the regression test for FINDING 10:
// a single shared "last text" compared across both the accessibility tier and the vision tier meant a vision
// capture's stored description (of the screen) never matched the next accessibility capture's raw a11y text
// of that same, unchanged screen — so the diff always looked like a change and the episode re-emitted, even
// though nothing on screen actually changed. Tracking the last text per tier fixes this.
func TestResolveCapture_AlternatingTiersOnUnchangedScreenDoesNotReemit(t *testing.T) {
	var lastA11yText, lastVisionText string

	// First capture: vision tier fires (e.g. thin a11y text) and describes the screen.
	out := resolveCapture(&lastA11yText, &lastVisionText, "thin", true, "a browser showing a login form", Sight{}, nil)
	if out.text == "" {
		t.Fatal("expected the first vision capture to emit a description")
	}

	// Second capture: same unchanged screen, but this time the accessibility tier is used (e.g. vision
	// rate-limited). The raw a11y text differs from the vision description by construction — that must not,
	// on its own, cause a re-emit; only a real accessibility-tier text change should re-emit.
	out = resolveCapture(&lastA11yText, &lastVisionText, "thin", false, "", Sight{}, nil)
	if out.text != "" {
		t.Errorf("alternating from vision tier to accessibility tier on an unchanged screen re-emitted: %q", out.text)
	}

	// Third capture: accessibility tier again, text genuinely changed — must emit.
	out = resolveCapture(&lastA11yText, &lastVisionText, "different now", false, "", Sight{}, nil)
	if out.text != "different now" {
		t.Errorf("expected a genuine accessibility-tier text change to emit, got %q", out.text)
	}
}
