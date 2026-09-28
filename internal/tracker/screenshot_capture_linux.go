//go:build linux

package tracker

import (
	"bytes"
	"context"
	"fmt"
	"image"
)

// minLookSide is the smallest a front window may measure and still be captured on its own. Anything smaller is a window that reported nonsense — a Wayland client answering with a rectangle it made up — and the whole screen is sent instead, which is never wrong, only wider.
const minLookSide = 200

// CaptureFront returns the pixels of the window in front, scaled and encoded for a model to look at, with the origin and scale that map a point in the image back to a point on the screen, in the logical desktop pixels the portal's pointer takes.
// Input: a context, which bounds the screenshot call. Output: the capture, or an error when the application in front is on the blocklist, or the screen cannot be grabbed or decoded. The region is the whole screen whenever the front window's rectangle cannot be read or does not make sense, which costs some resolution and nothing else — the origin says which of the two happened.
func CaptureFront(ctx context.Context) (Capture, error) {
	// The blocklist is checked before the screen is grabbed, not after: when a window reports no rectangle the region falls back to the whole screen, so a refusal that came later would already have the blocked window's pixels in hand.
	if app, ok := frontApp(); ok && Blocklisted(app) {
		return Capture{}, blockedRead(app)
	}
	shot, err := grabScreen(ctx)
	if err != nil {
		return Capture{}, err
	}
	img, _, err := image.Decode(bytes.NewReader(shot))
	if err != nil {
		return Capture{}, fmt.Errorf("decode screenshot: %w", err)
	}
	// The screenshot is in the display's own device pixels while everything else here — the accessibility bus, _NET_WORKAREA, the portal's pointer — is in logical desktop pixels. On an unscaled display the two are the same and this ratio is 1.
	logicalW := 0
	if d, ok := deskNow(); ok {
		logicalW = d.screen.W
	}
	ratio := shotScale(img.Bounds().Dx(), logicalW)
	region := img.Bounds()
	if front, ok := frontWindowRect(ctx); ok {
		if in := atShotScale(front, ratio).Intersect(img.Bounds()); in.Dx() >= minLookSide && in.Dy() >= minLookSide {
			region = in
		}
	}
	c, err := encodeCapture(img, region)
	if err != nil {
		return Capture{}, err
	}
	return toLogical(c, ratio), nil
}

// shotScale is how many screenshot pixels there are to one logical desktop pixel. Input: the screenshot's width in its own pixels and the desktop's logical width, which is 0 when X could not be read. Output: the ratio between the two, and 1 whenever either width is missing, which leaves an unscaled display untouched.
func shotScale(shotW, logicalW int) float64 {
	if shotW <= 0 || logicalW <= 0 {
		return 1
	}
	return float64(shotW) / float64(logicalW)
}

// toLogical rewrites a capture's mapping from the screenshot's own pixels into the logical desktop pixels the portal's pointer takes. Input: the capture encodeCapture made, whose origin and scale are both in screenshot pixels, and the screenshot-to-logical ratio. Output: the capture with X, Y and Scale divided by that ratio, so ToScreen answers in logical pixels; the capture unchanged at a ratio of 1. W and H are the image's own size in image pixels and do not move.
func toLogical(c Capture, ratio float64) Capture {
	if ratio <= 0 || ratio == 1 {
		return c
	}
	c.X = int(float64(c.X) / ratio)
	c.Y = int(float64(c.Y) / ratio)
	c.Scale /= ratio
	return c
}

// atShotScale grows a rectangle measured in logical desktop pixels to the screenshot's own pixels, so it names the same part of the picture. Input: the rectangle and the screenshot-to-logical ratio. Output: the rectangle multiplied by that ratio, unchanged at a ratio of 1.
func atShotScale(r image.Rectangle, ratio float64) image.Rectangle {
	if ratio <= 0 || ratio == 1 {
		return r
	}
	return image.Rect(int(float64(r.Min.X)*ratio), int(float64(r.Min.Y)*ratio), int(float64(r.Max.X)*ratio), int(float64(r.Max.Y)*ratio))
}

// frontApp names the application whose window has focus, for the check a capture makes before it grabs anything. Output: the application name and true, or false when the accessibility bus is unreachable or no window has taken focus. It is a variable so a test can stand in for the bus.
var frontApp = func() (string, bool) {
	w := focus()
	if w == nil {
		return "", false
	}
	_, app, ok := w.state.get()
	return app, ok
}

// frontWindowRect reads where the window in front sits on the screen, so a look is of that window rather than of the whole desktop. Input: a context. Output: the rectangle in screen coordinates, and false when no window has focus on the accessibility bus or it reports no rectangle.
// It is the same read observe_screen's listing is corrected by (see correctListing): the window's own frame, moved by windowShift, because a native Wayland client cannot know where it is and answers with its own window coordinates.
func frontWindowRect(ctx context.Context) (image.Rectangle, bool) {
	w := focus()
	if w == nil {
		return image.Rectangle{}, false
	}
	ref, _, ok := w.state.get()
	// The compositor's focused window wins here for the same reason it does in Observe: a look must be of the window the user sees in front, and the bus's own idea of focus drifts between two windows of one application.
	if fref, _, found := compositorFront(ctx, w.conn); found {
		ref, ok = fref, true
	}
	if !ok {
		return image.Rectangle{}, false
	}
	frame, _, ok := windowOf(ctx, w.conn, ref)
	if !ok || frame.W <= 0 || frame.H <= 0 {
		return image.Rectangle{}, false
	}
	dx, dy, _ := shiftOf(ctx, w.conn, ref)
	return image.Rect(frame.X+dx, frame.Y+dy, frame.X+frame.W+dx, frame.Y+frame.H+dy), true
}
