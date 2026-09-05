//go:build linux

package cmd

import (
	"bytes"
	"fmt"
	"image/png"
	"testing"
)

// The status row uses a rendered, anti-aliased colored dot (Docker-style), not an emoji glyph: a square PNG with an opaque colored center and transparent corners, distinct per state.
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

// The watcher drops all items when it restarts and re-announces itself with a new owner, so we must re-register on its return — but not when it disappears, and never on an unrelated signal or a malformed body.
func TestShouldReregister(t *testing.T) {
	const noc = "org.freedesktop.DBus.NameOwnerChanged"
	const watcher = "org.kde.StatusNotifierWatcher"
	cases := []struct {
		name   string
		signal string
		body   []interface{}
		want   bool
	}{
		{"watcher reappearing with a new owner", noc, []interface{}{watcher, "", ":1.42"}, true},
		{"watcher disappearing (empty new owner)", noc, []interface{}{watcher, ":1.42", ""}, false},
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

// The Linux tray must advertise the ORA logo as an icon pixmap, not a theme icon name. SNI pixmaps are ARGB32 in network byte order (A,R,G,B per pixel).
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

// The recording item is a toggle, so its label always names what the click will do.
func TestMeetingLabel(t *testing.T) {
	if got := meetingLabel(false); got != "Start meeting recording" {
		t.Errorf("idle label = %q", got)
	}
	if got := meetingLabel(true); got != "Stop meeting recording" {
		t.Errorf("recording label = %q", got)
	}
}

// A nil recorder (the daemon never got far enough to wire one up) must not panic the menu, and must read as not recording.
func TestMenuRecording_NilRecorderIsSafe(t *testing.T) {
	m := &dbusMenu{}
	if m.recording() {
		t.Error("a menu with no recorder must not report a recording in progress")
	}
	if got := labelOf(t, m.items(), menuMeeting); got != "Start meeting recording" {
		t.Errorf("meeting item label = %q", got)
	}
}

// GetLayout, GetGroupProperties and GetProperty all serve the same menu, so a label change must show up identically in all three — they used to hardcode their own copies.
func TestMenuViewsAgree(t *testing.T) {
	for _, paused := range []bool{false, true} {
		m := &dbusMenu{}
		m.paused.Store(paused)

		_, root, derr := m.GetLayout(0, -1, nil)
		if derr != nil {
			t.Fatalf("GetLayout: %v", derr)
		}
		items := m.items()
		if len(root.Children) != len(items) {
			t.Fatalf("layout has %d children, items() has %d", len(root.Children), len(items))
		}

		group, derr := m.GetGroupProperties(nil, nil)
		if derr != nil {
			t.Fatalf("GetGroupProperties: %v", derr)
		}
		if len(group) != len(items) {
			t.Fatalf("GetGroupProperties returned %d items, want %d", len(group), len(items))
		}

		for i, it := range items {
			child, ok := root.Children[i].Value().(dbusMenuLayout)
			if !ok {
				t.Fatalf("child %d is not a dbusMenuLayout", i)
			}
			if child.ID != it.ID {
				t.Errorf("child %d id = %d, want %d", i, child.ID, it.ID)
			}
			if group[i].ID != it.ID {
				t.Errorf("group %d id = %d, want %d", i, group[i].ID, it.ID)
			}
			for name, want := range it.Properties {
				got, derr := m.GetProperty(it.ID, name)
				if derr != nil {
					t.Fatalf("GetProperty(%d, %s): %v", it.ID, name, derr)
				}
				if fmt.Sprint(got.Value()) != fmt.Sprint(want.Value()) {
					t.Errorf("item %d property %q: GetProperty gave %v, items() gave %v", it.ID, name, got.Value(), want.Value())
				}
			}
		}
	}
}

// The menu order puts opening the window and the recording toggle with the other actions, above the separator and Quit. Ora shows one tray icon, so what the window's own removed menu offered lives here, except showing the hover, which is what the keyboard shortcut is for.
func TestMenuOrder(t *testing.T) {
	var ids []int32
	for _, it := range (&dbusMenu{}).items() {
		ids = append(ids, it.ID)
	}
	want := []int32{menuStatus, menuSep1, menuOpen, menuPause, menuMeeting, menuSep2, menuQuit}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Errorf("menu order = %v, want %v", ids, want)
	}
}

func labelOf(t *testing.T, items []dbusMenuItemProps, id int32) string {
	t.Helper()
	for _, it := range items {
		if it.ID == id {
			s, _ := it.Properties["label"].Value().(string)
			return s
		}
	}
	t.Fatalf("no menu item with id %d", id)
	return ""
}
