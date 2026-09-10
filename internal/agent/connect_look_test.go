package agent

import (
	"context"
	"strings"
	"testing"

	"ora/internal/act"
	"ora/internal/tracker"
)

// A live voice session's tool calls must share one screen state, or every one of them gets a throwaway and draw can never tell that look ran at all.
// Before this, withAskLookState was attached on every typed ask path (ask.go, claude.go, codex.go, agy.go) and on none of the live path, so a real session on 2026-09-07 called look, got a picture back, called draw three seconds later and was told "I need to look at the screen first" — then looked again and was told the same thing. That is a loop with no exit, which is why the session reported its screen tools broken.
func TestLiveScreenScope_DrawKnowsThatLookRan(t *testing.T) {
	ctx := liveScreenScope(context.Background())
	recordLook(ctx, tracker.Capture{W: 1280, H: 698})

	_, ok, blind := lookSeen(ctx)
	if ok {
		t.Fatal("the live session was handed a picture it cannot display")
	}
	// The Live API carries tool results as text, so the picture is never delivered and no further look will change that. The honest refusal says so and names the path that does work; the loop said "look first" forever.
	if !blind {
		t.Errorf("draw would answer %q and send the model round the same loop again", needLookFirst)
	}
	if !strings.Contains(cannotSeePictures, "observe_screen") {
		t.Error("the refusal a blind session gets does not name the tool that does work")
	}
}

// The allowance is per turn, not per session: maxLooksPerAsk is 2, and a voice conversation running for twenty minutes on one allowance could look twice and never again.
func TestLiveScreenScope_RefreshesTheLookAllowanceEachTurn(t *testing.T) {
	ctx := liveScreenScope(context.Background())
	for i := 0; i < maxLooksPerAsk; i++ {
		if !looksLeft(ctx) {
			t.Fatalf("look %d refused while still inside the allowance", i+1)
		}
		recordLook(ctx, tracker.Capture{W: 1280, H: 698})
	}
	if looksLeft(ctx) {
		t.Fatal("the allowance never ran out, so it is not being counted at all")
	}

	endLiveTurn(ctx)
	if !looksLeft(ctx) {
		t.Error("the allowance did not come back at the turn boundary, so a long session can look only twice in total")
	}
}

// The numbered list observe_screen produced must survive a turn boundary: the model lists the screen, replies to the user, and is then asked to draw around item 3.
func TestLiveScreenScope_KeepsWhatObserveScreenListedAcrossATurn(t *testing.T) {
	a := &Agent{}
	ctx := liveScreenScope(context.Background())
	a.rememberScreen(ctx, []act.Item{{N: 1, X: 5, Y: 6, W: 7, H: 8}}, screenSnapshot{})

	endLiveTurn(ctx)

	if got := a.seen(ctx); len(got) != 1 {
		t.Errorf("observe_screen's list is %d long after the turn ended, want the one item still there to draw around", len(got))
	}
}
