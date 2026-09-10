package agent

import (
	"context"
	"testing"

	"ora/internal/tracker"
)

// Drawing needs the picture's frame, not the picture. A live voice session is told the frame in words — "here is the picture, 1280 wide and 698 high … one of its pixels is 1.50 screen pixels" — even though the image itself is never delivered, so a point it works out from that description maps onto the screen exactly as a sighted session's would.
// A real session on 2026-09-07 was asked to draw an octopus, sent a nine-point path and two small circles for eyes, and was refused; it then told the user "I can't draw for you" and stopped offering.
func TestDrawPoint_MapsThroughTheFrameEvenWhenTheImageWasNeverDelivered(t *testing.T) {
	a := &Agent{}
	ctx := liveScreenScope(context.Background())
	recordLook(ctx, tracker.Capture{W: 1280, H: 698, Scale: 1.5})

	x, y, errText := a.toScreenForDraw(ctx, 640, 349)
	if errText != "" {
		t.Fatalf("draw refused a point in a blind session: %s", errText)
	}
	if x == 0 && y == 0 {
		t.Error("the point mapped to the origin, so no frame was applied")
	}
}

// Clicking keeps the old rule: a point read off a picture nobody saw is a guess, and a guessed click presses the wrong thing. A guessed line only lands in the wrong place.
func TestClickPoint_StillRefusesWhenTheImageWasNeverDelivered(t *testing.T) {
	a := &Agent{}
	ctx := liveScreenScope(context.Background())
	recordLook(ctx, tracker.Capture{W: 1280, H: 698, Scale: 1.5})

	if _, _, errText := a.toScreen(ctx, 640, 349); errText == "" {
		t.Error("click_at accepted a coordinate from a session that never saw the picture")
	}
}

// With no look at all there is still no frame to map through, and saying so names the tool that produces one.
func TestDrawPoint_StillAsksForALookWhenNoneHasBeenTaken(t *testing.T) {
	a := &Agent{}
	ctx := liveScreenScope(context.Background())
	if _, _, errText := a.toScreenForDraw(ctx, 10, 10); errText == "" {
		t.Error("draw accepted a point with no picture behind it at all")
	}
}
