//go:build linux

package cmd

import (
	"bytes"
	"image/png"
	"testing"
)

// The status row uses a rendered, anti-aliased colored dot (Docker-style), not
// an emoji glyph: a square PNG with an opaque colored center and transparent
// corners, distinct per state.
func TestStatusDotPNG(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"active", dotActive},
		{"paused", dotPaused},
	} {
		img, err := png.Decode(bytes.NewReader(c.data))
		if err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		b := img.Bounds()
		if b.Dx() != statusDotSize || b.Dy() != statusDotSize {
			t.Fatalf("%s: want %dx%d, got %dx%d", c.name, statusDotSize, statusDotSize, b.Dx(), b.Dy())
		}
		// Center is opaque, corner is transparent (it's a circle, not a square).
		if _, _, _, a := img.At(statusDotSize/2, statusDotSize/2).RGBA(); a == 0 {
			t.Errorf("%s: center pixel is transparent", c.name)
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Errorf("%s: corner pixel is not transparent", c.name)
		}
	}

	if bytes.Equal(dotActive, dotPaused) {
		t.Error("active and paused dots must be visually distinct")
	}

	if got := (&dbusMenu{}).statusIcon(); !bytes.Equal(got, dotActive) {
		t.Error("unpaused menu must use the active dot")
	}
	m := &dbusMenu{}
	m.paused.Store(true)
	if got := m.statusIcon(); !bytes.Equal(got, dotPaused) {
		t.Error("paused menu must use the paused dot")
	}
}

// The watcher drops all items when it restarts and re-announces itself with a
// new owner. We must re-register on its return — but NOT when it disappears.
func TestShouldReregister_CoreDistinction(t *testing.T) {
	const sig = "org.freedesktop.DBus.NameOwnerChanged"
	const watcher = "org.kde.StatusNotifierWatcher"
	if !shouldReregister(sig, []interface{}{watcher, "", ":1.42"}) {
		t.Error("watcher reappearing (new owner) should trigger re-registration")
	}
	if shouldReregister(sig, []interface{}{watcher, ":1.42", ""}) {
		t.Error("watcher disappearing (empty new owner) must NOT trigger re-registration")
	}
}

// Guard against firing on unrelated signals or malformed bodies.
func TestShouldReregister_Guards(t *testing.T) {
	const noc = "org.freedesktop.DBus.NameOwnerChanged"
	const watcher = "org.kde.StatusNotifierWatcher"
	cases := []struct {
		name   string
		signal string
		body   []interface{}
		want   bool
	}{
		{"wrong signal name", "org.freedesktop.DBus.NameAcquired", []interface{}{watcher, "", ":1.42"}, false},
		{"unrelated service", noc, []interface{}{"org.example.Other", "", ":1.42"}, false},
		{"short body", noc, []interface{}{watcher, ""}, false},
		{"non-string fields", noc, []interface{}{42, 7, 9}, false},
		{"empty body", noc, []interface{}{}, false},
	}
	for _, c := range cases {
		if got := shouldReregister(c.signal, c.body); got != c.want {
			t.Errorf("%s: shouldReregister=%v want %v", c.name, got, c.want)
		}
	}
}

// The Linux tray must advertise the ORA logo as an icon pixmap, not a theme
// icon name. SNI pixmaps are ARGB32 in network byte order (A,R,G,B per pixel).
func TestTrayIconPixmap(t *testing.T) {
	pixmaps, err := trayIconPixmaps()
	if err != nil {
		t.Fatalf("trayIconPixmaps: %v", err)
	}
	if len(pixmaps) != 1 {
		t.Fatalf("want 1 pixmap, got %d", len(pixmaps))
	}
	p := pixmaps[0]
	if p.Width != 128 || p.Height != 128 {
		t.Fatalf("want 128x128, got %dx%d", p.Width, p.Height)
	}
	if want := int(p.Width * p.Height * 4); len(p.Data) != want {
		t.Fatalf("want %d bytes of ARGB data, got %d", want, len(p.Data))
	}

	// Alpha is the first byte of each pixel and the logo is opaque.
	for i := 0; i < len(p.Data); i += 4 {
		if p.Data[i] != 0xFF {
			t.Fatalf("pixel %d not opaque: alpha=%d", i/4, p.Data[i])
		}
	}

	// Real image data, not a blank fill.
	first := p.Data[1:4]
	varied := false
	for i := 4; i < len(p.Data); i += 4 {
		if p.Data[i+1] != first[0] || p.Data[i+2] != first[1] || p.Data[i+3] != first[2] {
			varied = true
			break
		}
	}
	if !varied {
		t.Fatal("icon has no color variation — decode/convert produced a flat image")
	}
}
