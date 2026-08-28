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

	"github.com/godbus/dbus/v5"
)

func TestResponseURI(t *testing.T) {
	ok := []interface{}{
		uint32(0),
		map[string]dbus.Variant{"uri": dbus.MakeVariant("file:///tmp/shot.png")},
	}
	uri, err := responseURI(ok)
	if err != nil {
		t.Fatalf("success case: %v", err)
	}
	if uri != "file:///tmp/shot.png" {
		t.Fatalf("uri=%q", uri)
	}

	denied := []interface{}{uint32(1), map[string]dbus.Variant{}}
	if _, err := responseURI(denied); err == nil {
		t.Error("denied (code 1) should error")
	}

	noURI := []interface{}{uint32(0), map[string]dbus.Variant{}}
	if _, err := responseURI(noURI); err == nil {
		t.Error("missing uri should error")
	}

	if _, err := responseURI([]interface{}{uint32(0)}); err == nil {
		t.Error("short body should error")
	}
}

func TestScreenshotGrantedDenied(t *testing.T) {
	cases := []struct {
		name      string
		perms     map[string][]string
		wantGrant bool
		wantDeny  bool
	}{
		{"unsandboxed allow", map[string][]string{"": {"yes"}}, true, false},
		{"unsandboxed deny", map[string][]string{"": {"no"}}, false, true},
		{"named app allow", map[string][]string{"ora": {"yes"}}, true, false},
		{"never asked", map[string][]string{}, false, false},
		{"nil perms", nil, false, false},
		{"grant wins over unrelated", map[string][]string{"": {"yes"}, "other": {"no"}}, true, false},
	}
	for _, c := range cases {
		if got := screenshotGranted(c.perms); got != c.wantGrant {
			t.Errorf("%s: screenshotGranted=%v want %v", c.name, got, c.wantGrant)
		}
		if got := screenshotDenied(c.perms); got != c.wantDeny {
			t.Errorf("%s: screenshotDenied=%v want %v", c.name, got, c.wantDeny)
		}
	}
}

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
