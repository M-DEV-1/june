package agent

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"june/internal/act"
	"strings"
	"testing"

	"june/internal/tracker"
)

// shot is a flat grey picture, with one block painted over when block is set, encoded the way a capture is. The capture starts at 0,32 on the screen at scale 2, like the one lookingAgent hands out.
func shot(t *testing.T, block *image.Rectangle) tracker.Capture {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for i := range img.Pix {
		img.Pix[i] = 128
	}
	if block != nil {
		for y := block.Min.Y; y < block.Max.Y; y++ {
			for x := block.Min.X; x < block.Max.X; x++ {
				img.Set(x, y, color.RGBA{255, 255, 255, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return tracker.Capture{Data: buf.Bytes(), Mime: "image/png", X: 0, Y: 32, W: 200, H: 200, Scale: 2}
}

// A press whose surroundings did not change comes back as an error, so the model retries instead of moving on from a click that landed on nothing.
func TestExecuteTool_ClickAt_ReportsAPressThatChangedNothing(t *testing.T) {
	a, in := typingAgent(t)
	pressSettle, tapLead = 0, 0
	still := shot(t, nil)
	a.capture = func(ctx context.Context) (tracker.Capture, error) { return still, nil }
	ctx := lookedAt(t, a)
	got := a.executeTool(ctx, "click_at", map[string]any{"x": 100.0, "y": 100.0})
	if len(in.calls) != 1 {
		t.Fatalf("pointer = %v, want the click sent", in.calls)
	}
	if !strings.HasPrefix(got, "error") || !strings.Contains(got, "nothing on the screen changed") {
		t.Errorf("result = %q, want the miss reported", got)
	}
}

// A press that changes the screen somewhere other than under the pointer still landed on something. The 120-pixel box only sees a highlight or a menu opening next to the button; a link that navigates the page, a tab that switches, a button that opens a panel on the other side of the window all leave the box identical and the rest of the screen different. Nineteen times in the week to 2026-09-12 the loop was told "it landed on nothing" and went to look again after a press that had worked.
func TestExecuteTool_ClickAt_APressThatChangedTheScreenElsewhereIsNotAMiss(t *testing.T) {
	a, _ := typingAgent(t)
	pressSettle, tapLead = 0, 0
	still := shot(t, nil)
	// The point is at image 100,100; the box around it is image 70..130. This block is well outside it, and is 9% of the screen.
	elsewhere := image.Rect(0, 0, 60, 60)
	after := shot(t, &elsewhere)
	a.capture = func(ctx context.Context) (tracker.Capture, error) { return still, nil }
	ctx := lookedAt(t, a) // takes a picture of its own, so the press's own pair is wired up after it
	shots := 0
	a.capture = func(ctx context.Context) (tracker.Capture, error) {
		shots++
		if shots == 1 {
			return still, nil
		}
		return after, nil
	}
	got := a.executeTool(ctx, "click_at", map[string]any{"x": 100.0, "y": 100.0})
	if strings.Contains(got, "landed on nothing") {
		t.Errorf("result = %q, want the press accepted: the screen changed, just not under the pointer", got)
	}
}

// A change the accessibility list cannot show, a shell overlay, a top-bar indicator, a video starting, is checked on the pixels: the step's pre-check keeps a picture, and wait_for passes when a later picture differs from it. On 2026-09-09 a job pressed GNOME's recording chord three times and each list-based check failed on a window whose list was empty.
func TestWaitFor_ScreenChangedComparesAgainstThePictureKeptBeforeTheAction(t *testing.T) {
	a, _ := typingAgent(t)
	pressPoll = 0
	still := shot(t, nil)
	lit := image.Rect(90, 90, 130, 130)
	current := still
	a.capture = func(ctx context.Context) (tracker.Capture, error) { return current, nil }
	ctx := context.Background()
	check := act.Check{Kind: act.ScreenChanged, Value: "a recording indicator"}
	if a.CheckHolds(ctx, check) {
		t.Error("a screen_changed check can never already hold")
	}
	got := a.executeTool(ctx, "wait_for", map[string]any{"kind": act.ScreenChanged, "value": "a recording indicator", "timeout_ms": 50.0})
	if !strings.HasPrefix(got, act.WaitFailPrefix) {
		t.Errorf("unchanged screen: %q, want the change not to have come", got)
	}
	current = shot(t, &lit)
	got = a.executeTool(ctx, "wait_for", map[string]any{"kind": act.ScreenChanged, "value": "a recording indicator", "timeout_ms": 50.0})
	if !strings.HasPrefix(got, act.WaitPassPrefix) {
		t.Errorf("changed screen: %q, want the change seen", got)
	}
}
