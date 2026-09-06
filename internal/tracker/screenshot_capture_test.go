package tracker

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// The lock gate consults the real GNOME session over D-Bus, so with the screen actually locked every daemon test on this machine would see no activity at all. Unit tests are about the daemon's logic, not the tester's screen state, so the gate is pinned open for the whole test binary.
func init() { sessionLocked = func() bool { return false } }

// testImage builds a w by h image with a distinct colour in each quarter, so a scaled copy can be checked for having kept the layout rather than only the size.
func testImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{R: 20, G: 20, B: 20, A: 255}
			if x >= w/2 {
				c.R = 220
			}
			if y >= h/2 {
				c.B = 220
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// A capture is scaled so its longer side is at most maxLookSide, and it says both where its top-left corner sits on the screen and how many screen pixels one of its own pixels is worth — without those two a point read off the image cannot be turned back into the point on the screen it names.
func TestEncodeCapture_ScalesAndReportsWhereItCameFrom(t *testing.T) {
	img := testImage(2560, 1440)
	got, err := encodeCapture(img, image.Rect(0, 32, 2560, 1440))
	if err != nil {
		t.Fatalf("encodeCapture: %v", err)
	}
	if got.W != maxLookSide {
		t.Errorf("width = %d, want the longer side scaled to %d", got.W, maxLookSide)
	}
	if got.H != 704 {
		t.Errorf("height = %d, want 1408 screen pixels at the same scale, which is 704", got.H)
	}
	if got.X != 0 || got.Y != 32 {
		t.Errorf("origin = %d,%d, want the region's own top-left 0,32", got.X, got.Y)
	}
	if got.Scale != 2.0 {
		t.Errorf("scale = %v, want 2 screen pixels per captured pixel", got.Scale)
	}
	// The point 640,320 in the image is 1280,672 on the screen: 640*2 across, 320*2 down from y=32.
	x, y := got.ToScreen(640, 320)
	if x != 1280 || y != 672 {
		t.Errorf("ToScreen(640,320) = %d,%d, want 1280,672", x, y)
	}
}

// A region already smaller than the cap is sent at its own size: scaling it up would cost bytes and add nothing.
func TestEncodeCapture_LeavesASmallRegionAlone(t *testing.T) {
	got, err := encodeCapture(testImage(1000, 700), image.Rect(100, 50, 900, 650))
	if err != nil {
		t.Fatalf("encodeCapture: %v", err)
	}
	if got.W != 800 || got.H != 600 || got.Scale != 1 {
		t.Errorf("got %dx%d at scale %v, want 800x600 at scale 1", got.W, got.H, got.Scale)
	}
	if got.X != 100 || got.Y != 50 {
		t.Errorf("origin = %d,%d, want 100,50", got.X, got.Y)
	}
	x, y := got.ToScreen(10, 10)
	if x != 110 || y != 60 {
		t.Errorf("ToScreen(10,10) = %d,%d, want 110,60", x, y)
	}
}

// The bytes have to be a real JPEG of the stated size, and small enough that a whole screen of them is not the largest thing in the request.
func TestEncodeCapture_IsAJPEGUnderTheSizeCap(t *testing.T) {
	got, err := encodeCapture(testImage(1920, 1080), image.Rect(0, 0, 1920, 1080))
	if err != nil {
		t.Fatalf("encodeCapture: %v", err)
	}
	if got.Mime != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", got.Mime)
	}
	if len(got.Data) > maxLookBytes {
		t.Errorf("size = %d bytes, want at most %d", len(got.Data), maxLookBytes)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatalf("the bytes are not a JPEG: %v", err)
	}
	if decoded.Bounds().Dx() != got.W || decoded.Bounds().Dy() != got.H {
		t.Errorf("decoded %v, want the %dx%d the capture reports", decoded.Bounds(), got.W, got.H)
	}
}

// A point outside the image is not a point on the screen, and the draw tool leans on this to refuse coordinates the model guessed rather than read off a look.
func TestCapture_HoldsOnlyItsOwnPixels(t *testing.T) {
	c := Capture{X: 0, Y: 32, W: 1280, H: 704, Scale: 2}
	for _, p := range [][2]int{{-1, 0}, {0, -1}, {1281, 10}, {10, 705}, {1280, 10}, {10, 704}} {
		if c.Holds(p[0], p[1]) {
			t.Errorf("Holds(%d,%d) = true, want false for a point outside a %dx%d image", p[0], p[1], c.W, c.H)
		}
	}
	for _, p := range [][2]int{{0, 0}, {1279, 703}, {640, 320}} {
		if !c.Holds(p[0], p[1]) {
			t.Errorf("Holds(%d,%d) = false, want true", p[0], p[1])
		}
	}
}
