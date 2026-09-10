package agent

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"ora/internal/act"
	"strings"
	"testing"
	"time"

	"ora/internal/tracker"
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

// On 2026-09-08 a click on the JBL row in Settings landed on the window header, and the loop reported success because it re-read the same accessibility numbers it had aimed with. The pixels around the point are the one sensor the aim did not use: a press that changes nothing there is a miss, one that changes them is not, and a picture that cannot be read says nothing either way.
func TestChangedAround(t *testing.T) {
	still := shot(t, nil)
	lit := image.Rect(90, 90, 130, 130)
	changed := shot(t, &lit)
	// The point is at image 100,100, which is screen 200,232.
	if got, known := changedAround(still, changed, 200, 232); !known || !got {
		t.Errorf("a highlight under the point: changed=%v known=%v, want changed", got, known)
	}
	if got, known := changedAround(still, still, 200, 232); !known || got {
		t.Errorf("the same picture twice: changed=%v known=%v, want unchanged", got, known)
	}
	if _, known := changedAround(still, changed, 2000, 2000); known {
		t.Error("a point outside the picture must not be judged")
	}
	if _, known := changedAround(tracker.Capture{Data: []byte("fake-jpeg-bytes")}, still, 200, 232); known {
		t.Error("a picture that will not decode must not be judged")
	}
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

// The check returns the moment a picture shows the change, and only waits the full settle when nothing changes: a press that lands is not made to pay for one that might not.
func TestPressCheck_ReturnsOnTheFirstPictureThatChanged(t *testing.T) {
	a, _ := typingAgent(t)
	pressSettle, pressPoll = time.Second, 0
	still := shot(t, nil)
	lit := image.Rect(90, 90, 130, 130)
	shots := 0
	a.capture = func(ctx context.Context) (tracker.Capture, error) {
		shots++
		if shots >= 3 {
			return shot(t, &lit), nil
		}
		return still, nil
	}
	started := time.Now()
	if got := a.pressCheck(context.Background(), a.beforePress(context.Background()), 200, 232); got != "" {
		t.Errorf("check = %q, want the change seen", got)
	}
	if shots != 3 || time.Since(started) > 500*time.Millisecond {
		t.Errorf("took %d pictures in %v, want the change caught on the second look after the press without the full settle", shots, time.Since(started))
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
