//go:build linux

package tracker

import (
	"bytes"
	"context"
	"fmt"
	"image"
	jpeg_ "image/jpeg"
	png_ "image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// TestScreenLayout reads the real monitor layout and checks it lines up with a real screenshot: every monitor rectangle inside the canvas, and the rectangles adding up to exactly the screenshot's own bounds. That equality is what decides whether captures get split per monitor or stay whole-canvas.
// Run on a live session: go test -run TestScreenLayout ./internal/tracker/...
func TestScreenLayout(t *testing.T) {
	mons, pointer := screenLayout()
	if len(mons) == 0 {
		t.Skip("no monitor layout available (headless or no XWayland)")
	}
	t.Logf("pointer at %v", pointer)

	shot, err := grabScreen(context.Background())
	if err != nil {
		t.Skipf("no screenshot available: %v", err)
	}
	cfg, err := png_.DecodeConfig(bytes.NewReader(shot))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	canvas := image.Rect(0, 0, cfg.Width, cfg.Height)

	union := image.Rectangle{}
	for _, m := range mons {
		t.Logf("monitor %v", m)
		union = union.Union(m)
	}
	if union != canvas {
		t.Fatalf("monitors cover %v but the screenshot is %v — captures would fall back to the whole canvas", union, canvas)
	}

	rects := orderMonitors(canvas, mons, pointer)
	if len(rects) != len(mons) {
		t.Fatalf("orderMonitors returned %d rects for %d monitors", len(rects), len(mons))
	}
}

// TestScreenFramesLive captures the real screen and reports what each stored frame costs, which is the check that a monitor's frame keeps its native resolution instead of being squeezed into a shared thumbnail.
// Set ORA_FRAME_DUMP to a directory to also write the frames out and look at them: ORA_FRAME_DUMP=/tmp/x go test -run TestScreenFramesLive ./internal/tracker/...
func TestScreenFramesLive(t *testing.T) {
	shot, err := grabScreen(context.Background())
	if err != nil {
		t.Skipf("no screenshot available: %v", err)
	}
	cfg, err := png_.DecodeConfig(bytes.NewReader(shot))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Logf("canvas %dx%d, %d bytes of PNG", cfg.Width, cfg.Height, len(shot))

	frames := screenFrames(shot)
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	dump := os.Getenv("ORA_FRAME_DUMP")
	for i, f := range frames {
		fc, err := jpeg_.DecodeConfig(bytes.NewReader(f))
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		t.Logf("frame %d: %dx%d, %d bytes", i, fc.Width, fc.Height, len(f))
		if fc.Width > jpegMaxWidth {
			t.Errorf("frame %d is %d wide, over the %d cap", i, fc.Width, jpegMaxWidth)
		}
		if dump != "" {
			path := filepath.Join(dump, fmt.Sprintf("frame-%d.jpg", i))
			if err := os.WriteFile(path, f, 0600); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s", path)
		}
	}
}

// TestScreenshotShell captures for real through gnome-shell and checks a PNG comes back. Skips anywhere the silent path is not available (no session bus, non-GNOME, org.gnome.Screenshot taken) — that is exactly the case where grabScreen falls back to the portal.
// Run on a live GNOME session: go test -run TestScreenshotShell ./internal/tracker/...
func TestScreenshotShell(t *testing.T) {
	png, err := screenshotShell(context.Background())
	if err != nil {
		t.Skipf("gnome-shell screenshot unavailable: %v", err)
	}
	if !bytes.HasPrefix(png, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("not a PNG: first bytes %q", png[:min(8, len(png))])
	}
	cfg, err := png_.DecodeConfig(bytes.NewReader(png))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Width == 0 || cfg.Height == 0 {
		t.Fatalf("empty image %dx%d", cfg.Width, cfg.Height)
	}
	t.Logf("captured %dx%d, %d bytes", cfg.Width, cfg.Height, len(png))
}

// When the five-second screenshot bound wins while the portal is still working, the portal goes on to write a PNG of the whole desktop and nothing used to delete it: the removal lived in readFileURI, which that path never reaches. The abandoned request is now drained and its file removed.
func TestDiscardLateShot_RemovesTheFileTheLateResponseNames(t *testing.T) {
	shot := filepath.Join(t.TempDir(), "screen.png")
	if err := os.WriteFile(shot, []byte("a picture of the whole desktop"), 0o600); err != nil {
		t.Fatal(err)
	}

	handle := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/x/ora_shot_1")
	sigCh := make(chan *dbus.Signal, 1)
	sigCh <- &dbus.Signal{Path: handle, Body: []interface{}{
		uint32(0),
		map[string]dbus.Variant{"uri": dbus.MakeVariant("file://" + shot)},
	}}

	discardLateShot(sigCh, handle, handle, 2*time.Second)

	if _, err := os.Stat(shot); !os.IsNotExist(err) {
		t.Fatalf("the abandoned portal screenshot is still on disk: %v", err)
	}
}

// A request that never answers must not keep the drain goroutine for the life of the process.
func TestDiscardLateShot_GivesUpWhenNoResponseArrives(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		discardLateShot(make(chan *dbus.Signal), dbus.ObjectPath("/a"), dbus.ObjectPath("/a"), 20*time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the drain never gave up on a portal request that answered nothing")
	}
}
