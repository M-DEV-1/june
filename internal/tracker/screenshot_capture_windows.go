package tracker

import (
	"context"
	"fmt"
	"image"
	"unsafe"

	"golang.org/x/sys/windows"
)

// minLookSide is the smallest a front window may measure and still be captured on its own, the same floor as on Linux. A smaller frame is a window that is minimized or not really there, and the whole desktop is sent instead.
const minLookSide = 200

// CaptureFront returns the pixels of the window in front, scaled and encoded for a model to look at, with the origin and scale that map a point in the image back to a point on the screen in physical virtual-desktop pixels, the ones SetCursorPos and SendInput take in this per-monitor DPI aware process.
// Input: a context, unused because a GDI grab does not wait on anything. Output: the capture, or an error when the application in front is on the blocklist or the screen cannot be grabbed. The region is the whole virtual desktop whenever the front window's frame cannot be read or is too small.
func CaptureFront(_ context.Context) (Capture, error) {
	hwnd := windows.GetForegroundWindow()
	app := windowApp(uintptr(hwnd))
	// The blocklist is checked before the grab, as on Linux, so a refused window's pixels are never in hand.
	if app != "" && Blocklisted(app) {
		return Capture{}, fmt.Errorf("%s is on this machine's blocklist, so its windows are not read or pictured", app)
	}
	// When June's own hover has focus the guard below hides it, so its frame would crop to whatever happens to sit behind it; the whole desktop is sent instead.
	if IsJuneWindow(app, "") {
		hwnd = 0
	}
	defer standAside()()
	img, origin, err := grabVirtual()
	if err != nil {
		return Capture{}, err
	}
	region := img.Bounds()
	if frame, ok := windowFrame(hwnd); ok {
		if in := frame.Sub(origin).Intersect(img.Bounds()); in.Dx() >= minLookSide && in.Dy() >= minLookSide {
			region = in
		}
	}
	c, err := encodeCapture(img, region)
	if err != nil {
		return Capture{}, err
	}
	// encodeCapture's origin is in the picture's own pixels; adding the desktop's top-left corner makes ToScreen answer in screen pixels when a monitor sits left of or above the primary.
	c.X += origin.X
	c.Y += origin.Y
	return c, nil
}

// windowFrame reads the rectangle a window visibly occupies on the desktop, in physical pixels. DWMWA_EXTENDED_FRAME_BOUNDS leaves out the invisible resize border GetWindowRect counts on Windows 10 and 11, which is several pixels of whatever sits behind the window on each side. Input: a window handle. Output: the rectangle, and false when there is no window or DWM cannot say.
func windowFrame(hwnd windows.HWND) (image.Rectangle, bool) {
	if hwnd == 0 {
		return image.Rectangle{}, false
	}
	var r windows.Rect
	if err := windows.DwmGetWindowAttribute(hwnd, windows.DWMWA_EXTENDED_FRAME_BOUNDS, unsafe.Pointer(&r), uint32(unsafe.Sizeof(r))); err != nil {
		return image.Rectangle{}, false
	}
	rect := image.Rect(int(r.Left), int(r.Top), int(r.Right), int(r.Bottom))
	return rect, !rect.Empty()
}
