package tracker

import (
	"bytes"
	"context"
	"image"
	"testing"
)

// Smoke test for the Windows capture glue, run on a real unlocked Windows desktop: the grab must decode to the size of the virtual desktop, and the monitors screenLayout reports must tile that picture exactly, or every stored frame silently falls back to one whole-desktop image.
func TestWindowsScreenCapture(t *testing.T) {
	shot, err := grabScreen(context.Background())
	if err != nil {
		t.Fatalf("grabScreen: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(shot))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	desk := virtualDesktop()
	if img.Bounds().Dx() != desk.Dx() || img.Bounds().Dy() != desk.Dy() {
		t.Fatalf("picture %v, virtual desktop %v", img.Bounds(), desk)
	}
	mons, point := screenLayout()
	if orderMonitors(img.Bounds(), mons, point) == nil {
		t.Errorf("monitors %v do not tile the picture %v", mons, img.Bounds())
	}
	t.Logf("desktop %v, monitors %v, point %v, frames %d", desk, mons, point, len(screenFrames(shot)))

	c, err := CaptureFront(context.Background())
	if err != nil {
		t.Fatalf("CaptureFront: %v", err)
	}
	t.Logf("front capture at %d,%d, %dx%d image, scale %.2f", c.X, c.Y, c.W, c.H, c.Scale)

	idle, err := winInputIdle()
	if err != nil {
		t.Errorf("input idle: %v", err)
	}
	if winSessionLocked() {
		t.Errorf("session reads as locked on an unlocked desktop")
	}
	t.Logf("idle %v", idle)
}
