//go:build linux

package tracker

import (
	"context"
	"image"
	"strings"
	"testing"
)

// A look sends the brain a picture of the window in front, and CaptureFront filtered nothing at all: with KeePassXC or 1Password in front the picture is of the open vault. The blocklist the capture loop applies has to reach this path too, and it has to refuse before the screen is grabbed rather than after, since the fallback region when a window reports no rectangle is the whole screen.
func TestCaptureFront_RefusesABlocklistedApplication(t *testing.T) {
	restore := frontApp
	frontApp = func() (string, bool) { return "KeePassXC", true }
	SetBlocklist([]string{"keepassxc"})
	t.Cleanup(func() { frontApp = restore; SetBlocklist(nil) })

	_, err := CaptureFront(context.Background())
	if err == nil || !strings.Contains(err.Error(), "blocklist") {
		t.Errorf("CaptureFront on a blocked application = %v, want a refusal naming the blocklist", err)
	}
}

// The model reads a point off the picture and click_at hands it to the portal, which takes logical desktop pixels and maps them onto the granted monitor (see toStream in internal/input/portal.go). The gnome-shell screenshot is in device pixels, so on a 2x display every coordinate ToScreen produced was twice the one the pointer wanted: a click on something in the middle of the screen landed off the bottom right of it, or was refused as outside the monitor. The ratio between the screenshot's own width and the desktop's logical width is what puts it back, and it is 1 on an unscaled display, where nothing may move at all.
func TestToScreen_IsInLogicalDesktopPixels(t *testing.T) {
	// A 1920x1080 desktop, captured whole. At 1x the screenshot is 1920 across, at 2x it is 3840 and the same picture of it is scaled down to maxLookSide either way.
	cases := []struct {
		name            string
		shotW, logicalW int
		capture         Capture
		inX, inY        int
		wantX, wantY    int
		wantScale       float64
		wantOriginX     int
	}{
		{
			name: "an unscaled display is left exactly as it was", shotW: 1920, logicalW: 1920,
			capture: Capture{X: 0, Y: 0, W: 1280, H: 720, Scale: 1.5},
			inX:     640, inY: 360,
			wantX: 960, wantY: 540, wantScale: 1.5, wantOriginX: 0,
		},
		{
			// 3840 device pixels wide scaled to 1280 across is a Scale of 3 in device pixels, which is 1.5 logical pixels to one image pixel; the middle of the picture is the middle of a 1920x1080 logical desktop.
			name: "a 2x display, captured whole", shotW: 3840, logicalW: 1920,
			capture: Capture{X: 0, Y: 0, W: 1280, H: 720, Scale: 3},
			inX:     640, inY: 360,
			wantX: 960, wantY: 540, wantScale: 1.5, wantOriginX: 0,
		},
		{
			// A window on the bottom right quarter of a 2x display: the crop starts at 1920,1080 device pixels, which is 960,540 logical.
			name: "a 2x display, cropped to the front window", shotW: 3840, logicalW: 1920,
			capture: Capture{X: 1920, Y: 1080, W: 1280, H: 720, Scale: 1.5},
			inX:     0, inY: 0,
			wantX: 960, wantY: 540, wantScale: 0.75, wantOriginX: 960,
		},
		{
			name: "the desktop could not be read, so the picture is left alone", shotW: 3840, logicalW: 0,
			capture: Capture{X: 100, Y: 200, W: 1280, H: 720, Scale: 3},
			inX:     10, inY: 10,
			wantX: 130, wantY: 230, wantScale: 3, wantOriginX: 100,
		},
	}
	for _, c := range cases {
		got := toLogical(c.capture, shotScale(c.shotW, c.logicalW))
		if got.Scale != c.wantScale || got.X != c.wantOriginX {
			t.Errorf("%s: capture came out at %d,%d scale %v; want origin x %d scale %v", c.name, got.X, got.Y, got.Scale, c.wantOriginX, c.wantScale)
		}
		if got.W != c.capture.W || got.H != c.capture.H {
			t.Errorf("%s: the image's own size changed to %dx%d; it is in image pixels and must not move", c.name, got.W, got.H)
		}
		x, y := got.ToScreen(c.inX, c.inY)
		if x != c.wantX || y != c.wantY {
			t.Errorf("%s: ToScreen(%d, %d) = %d,%d; want %d,%d", c.name, c.inX, c.inY, x, y, c.wantX, c.wantY)
		}
	}
}

// frontWindowRect answers in logical desktop pixels, because that is what the accessibility bus and _NET_WORKAREA are in, and it is intersected with the screenshot's bounds to crop the picture. On a 2x display those two are different units: a window filling the right-hand half of the desktop named the middle of the screenshot instead, so the look was a picture of the wrong part of the screen.
func TestAtShotScale_GrowsALogicalRectangleToTheScreenshot(t *testing.T) {
	front := image.Rect(960, 0, 1920, 1080)
	if got := atShotScale(front, shotScale(1920, 1920)); got != front {
		t.Errorf("an unscaled display moved the crop to %v, want %v", got, front)
	}
	want := image.Rect(1920, 0, 3840, 2160)
	if got := atShotScale(front, shotScale(3840, 1920)); got != want {
		t.Errorf("a 2x display cropped %v, want %v", got, want)
	}
}
