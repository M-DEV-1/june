package agent

import (
	"context"
	"strings"
	"testing"

	"ora/internal/tracker"
)

// A typed ask's state must default to maxLooksPerAsk when nothing sets maxLooks explicitly — the zero value of a fresh askLookState, and the throwaway lookStateFrom hands back when a tool is driven directly.
func TestAskLookState_LookCapDefaultsToMaxLooksPerAsk(t *testing.T) {
	s := &askLookState{}
	if got := s.lookCap(); got != maxLooksPerAsk {
		t.Errorf("lookCap() = %d on a zero-value state, want the default of %d", got, maxLooksPerAsk)
	}
}

// A state built with an explicit maxLooks must use it instead of the default, which is how the voice scope gets a bigger budget than a typed ask without touching looksLeft's default path.
func TestAskLookState_LookCapUsesExplicitMaxLooks(t *testing.T) {
	s := &askLookState{maxLooks: voiceMaxLooksPerTurn}
	if got := s.lookCap(); got != voiceMaxLooksPerTurn {
		t.Errorf("lookCap() = %d on a state with maxLooks set to %d, want that value back", got, voiceMaxLooksPerTurn)
	}
}

// looksLeft and looksCap must honor a budget bigger than the typed-ask default of two — the case a five-of-six-refused voice session hit before the cap became part of the state instead of a package const.
func TestLooksLeft_HonorsALargerBudget(t *testing.T) {
	ctx := context.WithValue(context.Background(), askLookStateKey{}, &askLookState{maxLooks: voiceMaxLooksPerTurn})

	for i := 0; i < voiceMaxLooksPerTurn; i++ {
		if !looksLeft(ctx) {
			t.Fatalf("look %d refused while still inside the budget of %d", i+1, voiceMaxLooksPerTurn)
		}
		recordLook(ctx, tracker.Capture{W: 1280, H: 698})
	}
	if looksLeft(ctx) {
		t.Fatal("the budget never ran out, so it is not being counted at all")
	}
	if got := looksCap(ctx); got != voiceMaxLooksPerTurn {
		t.Errorf("looksCap(ctx) = %d, want the state's own budget of %d, not the typed-ask default", got, voiceMaxLooksPerTurn)
	}
}

// A typed ask's state, built through withAskLookState, must still cap at the smaller maxLooksPerAsk — the voice budget must not leak into a text ask that never asked for it.
func TestLooksLeft_TypedAskKeepsTheSmallerDefaultBudget(t *testing.T) {
	ctx := withAskLookState(context.Background())

	for i := 0; i < maxLooksPerAsk; i++ {
		if !looksLeft(ctx) {
			t.Fatalf("look %d refused while still inside the typed-ask budget of %d", i+1, maxLooksPerAsk)
		}
		recordLook(ctx, tracker.Capture{W: 1280, H: 698})
	}
	if looksLeft(ctx) {
		t.Fatal("a typed ask was allowed more looks than maxLooksPerAsk")
	}
	if got := looksCap(ctx); got != maxLooksPerAsk {
		t.Errorf("looksCap(ctx) = %d for a typed ask, want %d", got, maxLooksPerAsk)
	}
}

// resetLooks must give the allowance back without disturbing which budget is in effect, so a voice turn boundary restores six looks, not the typed-ask default of two.
func TestAskLookState_ResetLooksKeepsItsOwnBudget(t *testing.T) {
	s := &askLookState{maxLooks: voiceMaxLooksPerTurn}
	s.looks = voiceMaxLooksPerTurn

	s.resetLooks()

	if s.looks != 0 {
		t.Errorf("looks = %d after resetLooks, want 0", s.looks)
	}
	if got := s.lookCap(); got != voiceMaxLooksPerTurn {
		t.Errorf("lookCap() = %d after resetLooks, want the budget unchanged at %d", got, voiceMaxLooksPerTurn)
	}
}

// frameOnly must fire on the exact three-button example a Chromium window with no accessible content produces: nothing but its own Minimize, Restore and Close controls.
func TestFrameOnly_FiresOnWindowFrameButtonsOnly(t *testing.T) {
	lines := []string{
		`[1] push button "Minimize" (51,52)`,
		`[2] push button "Restore" (100,52)`,
		`[3] push button "Close" (150,52)`,
	}
	if !frameOnly(lines) {
		t.Errorf("frameOnly(%v) = false, want true", lines)
	}
}

// frameOnly must not fire on a normal listing that has real content alongside the frame buttons.
func TestFrameOnly_DoesNotFireWhenRealContentIsPresent(t *testing.T) {
	lines := []string{
		`[1] push button "Minimize" (51,52)`,
		`[2] push button "Close" (150,52)`,
		`[3] push button "Play" (400,300)`,
	}
	if frameOnly(lines) {
		t.Errorf("frameOnly(%v) = true, want false", lines)
	}
}

// An empty listing counts as frame-only too: a window with no readable content shows the same nothing whether or not it happens to also publish frame buttons.
func TestFrameOnly_FiresOnEmptyListing(t *testing.T) {
	if !frameOnly(nil) {
		t.Errorf("frameOnly(nil) = false, want true")
	}
}

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

// The hint costs a browser look about forty tokens, so it must not be paid on a window that has no tabs to close.
func TestObserveResult_NoBrowserHintForAnAppWithNoTabs(t *testing.T) {
	lines := []string{`[1] push button "Send" (50,35)`}
	for _, app := range []string{"Mail", "Spotify", "Microsoft Teams"} {
		got := observeResult(app, "Inbox", lines, screenSnapshot{})
		if strings.Contains(got, browserKeysHint) {
			t.Errorf("observeResult(%q) = %q, want no browser keys hint", app, got)
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

// On 2026-09-12 the user said "you can delete note". Ora called query_memory, got [note#312] back, then said "Understood, I've deleted that note about the supplement then" and never called revise. The note is still in the store. The system prompt already said to fix memory in the same turn; it lost against nine thousand other tokens. The affordance has to sit on the result the model is reading at the moment it decides.
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

// A result with nothing revise can act on must not pay for the line. Only notes carry a ref revise takes.
func TestNoReviseAffordanceWithoutANoteRef(t *testing.T) {
	for _, result := range []string{
		`[summary (Thu Sep 3 15:15, 8d ago)] chatted with Vexil Quorin on Microsoft Teams`,
		"no memory matches",
		"",
	} {
		if got := withReviseHint(result); strings.Contains(got, reviseHint) {
			t.Errorf("withReviseHint(%q) = %q, want no revise hint", result, got)
		}
	}
}
