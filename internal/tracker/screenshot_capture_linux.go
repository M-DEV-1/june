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

// CaptureFront returns the pixels of the window in front, scaled and encoded for a model to look at, with the origin and scale that map a point in the image back to a point on the screen.
// Input: a context, which bounds the screenshot call. Output: the capture, or an error when the screen cannot be grabbed or decoded. The region is the whole screen whenever the front window's rectangle cannot be read or does not make sense, which costs some resolution and nothing else — the origin says which of the two happened.
func CaptureFront(ctx context.Context) (Capture, error) {
	shot, err := grabScreen(ctx)
	if err != nil {
		return Capture{}, err
	}
	img, _, err := image.Decode(bytes.NewReader(shot))
	if err != nil {
		return Capture{}, fmt.Errorf("decode screenshot: %w", err)
	}
	region := img.Bounds()
	if front, ok := frontWindowRect(ctx); ok {
		if in := front.Intersect(img.Bounds()); in.Dx() >= minLookSide && in.Dy() >= minLookSide {
			region = in
		}
	}
	return encodeCapture(img, region)
}

// frontWindowRect reads where the window in front sits on the screen, so a look is of that window rather than of the whole desktop. Input: a context. Output: the rectangle in screen coordinates, and false when no window has focus on the accessibility bus or it reports no rectangle.
// It is the same read observe_screen's listing is corrected by (see correctListing): the window's own frame, moved by windowShift, because a native Wayland client cannot know where it is and answers with its own window coordinates.
func frontWindowRect(ctx context.Context) (image.Rectangle, bool) {
	w := focus()
	if w == nil {
		return image.Rectangle{}, false
	}
	ref, _, ok := w.state.get()
	if !ok {
		return image.Rectangle{}, false
	}
	frame, ok := windowOf(ctx, w.conn, ref)
	if !ok || frame.W <= 0 || frame.H <= 0 {
		return image.Rectangle{}, false
	}
	dx, dy := 0, 0
	if work, screen, ok := desktopBounds(); ok {
		dx, dy = windowShift(frame, work, screen, monitorRects())
	}
	return image.Rect(frame.X+dx, frame.Y+dy, frame.X+frame.W+dx, frame.Y+frame.H+dy), true
}
