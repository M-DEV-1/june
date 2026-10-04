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
// Input: a context, unused because a GDI grab does not wait on anything. Output: the capture, or an error when the application in front, or for a picture of the whole desktop any application with a window showing, is on the blocklist, or the screen cannot be grabbed. The region is the whole virtual desktop whenever the foreground window's frame cannot be read or is too small.
// When June's own window is in front, the window in front is the one the user came from (see frontWindow), as on Linux, where the focus watcher answers with the window June took focus from: the guard below takes June's window off the screen for the picture, so that window is what the picture shows. Sending the whole desktop instead put it in the picture without the blocklist ever being asked about it.
func CaptureFront(_ context.Context) (Capture, error) {
	hwnd := windows.GetForegroundWindow()
	app := windowApp(uintptr(hwnd))
	chosen := false
	if IsJuneWindow(app, "") {
		hwnd, app = 0, ""
		if w, ok := frontWindow(); ok {
			hwnd, app, chosen = w.hwnd, trimExe(w.exe), true
		}
	}
	// The blocklist is checked before the grab, as on Linux, so a refused window's pixels are never in hand.
	if app != "" && Blocklisted(app) {
		return Capture{}, fmt.Errorf("%s is on this machine's blocklist, so its windows are not read or pictured", app)
	}
	var frame image.Rectangle
	whole := true
	if f, ok := windowFrame(hwnd); ok {
		in := f.Intersect(virtualDesktop())
		// A window June chose is pictured on its own however small it is: frontWindow answers only with an open, unminimised window that is not a popup, and widening a small dialog to the whole desktop put every other window open on it in front of the model.
		if (in.Dx() >= minLookSide && in.Dy() >= minLookSide) || (chosen && !in.Empty()) {
			frame, whole = in, false
		}
	} else if chosen {
		return Capture{}, fmt.Errorf("could not read where %s's window is on the screen, so it was not pictured", app)
	}
	// A picture of the whole desktop shows every window open on it, so each of them is asked about, not only the one in front.
	if whole {
		if blocked := blocklistedOnScreen(); blocked != "" {
			return Capture{}, fmt.Errorf("%s has a window showing and is on this machine's blocklist, so the screen was not pictured", blocked)
		}
	}
	defer standAside()()
	img, origin, err := grabVirtual()
	if err != nil {
		return Capture{}, err
	}
	region := img.Bounds()
	if !whole {
		region = frame.Sub(origin).Intersect(region)
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

// blocklistedOnScreen names an application on the blocklist with a window showing on the current virtual desktop. Output: its name without ".exe", "" when there is none or no blocklist is set.
func blocklistedOnScreen() string {
	if list := blocklist.Load(); list == nil || len(*list) == 0 {
		return ""
	}
	for _, h := range topWindows() {
		if !winListable(h) || winIconic(h) {
			continue
		}
		if app := trimExe(winOf(h).exe); app != "" && Blocklisted(app) {
			return app
		}
	}
	return ""
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
