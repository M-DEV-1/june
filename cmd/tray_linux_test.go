//go:build linux

package cmd

import (
	"fmt"

	"testing"
)

// The Linux tray must advertise the June logo as an icon pixmap, not a theme icon name. SNI pixmaps are ARGB32 in network byte order (A,R,G,B per pixel).
// IconPixmap is an array, a(iiay), and the host picks whichever entry is closest to its panel height. Until 2026-09-12 one 128x128 entry was all it had, so every shell scaled that down to around 22px with its own scaler and the face came out blurred and grey. Every panel size is now drawn at its own size (see packaging/make-icons.py) and offered, so a host with a 22px bar finds 22px and resamples nothing.
func TestTrayIconPixmap(t *testing.T) {
	pixmaps, err := trayIconPixmaps()
	if err != nil {
		t.Fatalf("trayIconPixmaps: %v", err)
	}
	if len(pixmaps) < 2 {
		t.Fatalf("want a pixmap for every panel size, got %d", len(pixmaps))
	}
	have := map[int32]bool{}
	for _, p := range pixmaps {
		if p.Width != p.Height {
			t.Errorf("pixmap %dx%d is not square", p.Width, p.Height)
		}
		if want := int(p.Width * p.Height * 4); len(p.Data) != want {
			t.Errorf("%dx%d: want %d bytes of ARGB data, got %d", p.Width, p.Height, want, len(p.Data))
		}
		have[p.Width] = true
	}
	// 22 and 24 are the panel heights a GNOME or KDE bar actually asks for; without one of them present the host is back to scaling something else down.
	if !have[22] && !have[24] {
		t.Errorf("no pixmap at a panel height: got %v", have)
	}

	// The largest pixmap is the specimen for the checks below. At 16px every stem is about a pixel wide with anti-aliasing on both sides of it, so no pixel there reaches full opacity — true of any small icon, and nothing to do with what these assertions are about.
	p := pixmaps[len(pixmaps)-1]

	// Alpha is the first byte of each pixel, which is what this checks by finding both kinds: the icon is the bare face on transparency, so its ink is fully opaque and its background is fully clear.
	// Until 2026-09-12 this asserted every pixel was opaque, which described the art of the day — a purple face on a solid black tile — rather than anything the protocol asks for. A tray icon has to sit on a bar that may be light or dark, so an opaque tile of either colour is wrong on one of them, and SNI pixmaps are ARGB32 precisely so alpha can say so. Having both values present is also the only way to tell alpha really is in the first byte rather than the last.
	var opaque, clear int
	for i := 0; i < len(p.Data); i += 4 {
		switch p.Data[i] {
		case 0xFF:
			opaque++
		case 0x00:
			clear++
		}
	}
	if opaque == 0 {
		t.Error("no fully opaque pixel, so nothing was actually drawn")
	}
	if clear == 0 {
		t.Error("no fully clear pixel, so the icon is a solid tile and will be wrong on a bar of the opposite shade")
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
