//go:build linux

package tracker

import (
	"context"
	"testing"
)

// Chromium names its actions press, click, jump, select, activate or doDefault depending on the node, always with showContextMenu beside them; the one to fire is the first of those, never the context menu, and index 0 when nothing recognisable is offered.
func TestPickAction(t *testing.T) {
	cases := []struct {
		names []string
		want  int
	}{
		{[]string{"press", "showContextMenu"}, 0},
		{[]string{"showContextMenu", "click"}, 1},
		{[]string{"showContextMenu", "jump"}, 1},
		{[]string{"select", "showContextMenu"}, 0},
		{[]string{"activate", "showContextMenu"}, 0},
		{[]string{"doDefault", "showContextMenu"}, 0},
		{[]string{"weird"}, 0},
	}
	for _, c := range cases {
		if got := pickAction(c.names); got != c.want {
			t.Errorf("pickAction(%v) = %d, want %d", c.names, got, c.want)
		}
	}
	if pickAction(nil) != -1 {
		t.Errorf("pickAction(nil) should be -1: nothing to do")
	}
}

// A ref travels as one string through act.Node and the model's numbered list; it has to come back as the same bus name and object path.
func TestRefRoundTrip(t *testing.T) {
	in := aref{Name: ":1.28", Path: "/org/a11y/atspi/accessible/1234"}
	out, err := parseARef(refString(in))
	if err != nil || out != in {
		t.Errorf("round trip = %+v, %v; want %+v", out, err, in)
	}
	if _, err := parseARef("nonsense"); err == nil {
		t.Error("a ref without a separator must be refused")
	}
}

// The staleness check is what stands between the model's numbered list and the screen it was made from. A list is a snapshot: the accessibility reference in it stays valid while the page scrolls under it, so an element that has merely moved still answers to its old number, and both the ring drawn for the user and the click fired afterwards are then aimed at whatever has taken its place. Comparing the rectangle catches that. It also tells two unlabelled entries in the same form apart, which the role and the label alone cannot do at all.
func TestVerifyAgainst(t *testing.T) {
	was := rect{X: 10, Y: 200, W: 300, H: 30}
	cases := []struct {
		name              string
		nowRole, nowLabel string
		now               rect
		role, label       string
		was               rect
		wantErr           bool
	}{
		{name: "unchanged", nowRole: "push button", nowLabel: "Send", now: was, role: "push button", label: "Send", was: was},
		{name: "gone", nowRole: "", now: was, role: "push button", label: "Send", was: was, wantErr: true},
		{name: "another role", nowRole: "link", nowLabel: "Send", now: was, role: "push button", label: "Send", was: was, wantErr: true},
		{name: "another label", nowRole: "push button", nowLabel: "Delete", now: was, role: "push button", label: "Send", was: was, wantErr: true},
		{name: "a few pixels of relayout", nowRole: "push button", nowLabel: "Send", now: rect{X: 12, Y: 202, W: 300, H: 30}, role: "push button", label: "Send", was: was, wantErr: false},
		{name: "the page scrolled a row", nowRole: "push button", nowLabel: "Send", now: rect{X: 10, Y: 240, W: 300, H: 30}, role: "push button", label: "Send", was: was, wantErr: true},
		{name: "moved sideways", nowRole: "push button", nowLabel: "Send", now: rect{X: 400, Y: 200, W: 300, H: 30}, role: "push button", label: "Send", was: was, wantErr: true},
		{name: "resized", nowRole: "push button", nowLabel: "Send", now: rect{X: 10, Y: 200, W: 80, H: 30}, role: "push button", label: "Send", was: was, wantErr: true},
		{name: "no longer showing", nowRole: "push button", nowLabel: "Send", now: rect{}, role: "push button", label: "Send", was: was, wantErr: true},
		// An empty box to type in is exactly what type_text aims at, and it carries no label at all, so the rectangle is the only thing that distinguishes it from the next box down the form.
		{name: "an unlabelled entry that stayed put", nowRole: "entry", now: was, role: "entry", was: was},
		{name: "the entry below the one that was listed", nowRole: "entry", now: rect{X: 10, Y: 250, W: 300, H: 30}, role: "entry", was: was, wantErr: true},
		// The list can show a node with no size only if it never had one; nothing then to compare against, so the rectangle is not part of the answer.
		{name: "nothing remembered to compare", nowRole: "entry", now: was, role: "entry", was: rect{}},
	}
	for _, c := range cases {
		err := verifyAgainst(c.nowRole, c.nowLabel, c.now, c.role, c.label, c.was)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: verifyAgainst(...) = %v, want error: %v", c.name, err, c.wantErr)
		}
	}
}

// Extents reads one element's rectangle so a ring can be drawn where the element is now. A ref that never came from a node list cannot be read at all, and that has to come back as an error rather than as the rectangle 0,0 0x0, which would put a ring in the corner of the screen.
func TestExtents_RefusesAMalformedRef(t *testing.T) {
	if _, _, _, _, err := Extents(context.Background(), "nonsense"); err == nil {
		t.Error("Extents must refuse a ref with no bus name and object path in it")
	}
}

// The accessibility bus is asked for screen coordinates and, from a client that cannot know where its own window is, answers with window coordinates instead: measured on this desk on 2026-09-05, Brave's frame reported 0,0 1920x1048 for coordinate type 0 while _NET_WORKAREA said the work area is 0,32 1920x1048, so every rectangle read out of that window was 32 pixels too high — the height of the desktop's top bar. windowShift is what puts them back.
// _NET_WORKAREA is one desktop-wide rectangle, so on a second monitor a maximized window matches neither it nor the whole screen and used to be left alone, keeping the whole top-bar error. Matching the window's width against each monitor's own width is what catches it.
func TestWindowShift(t *testing.T) {
	screen := rect{X: 0, Y: 0, W: 1920, H: 1080}
	work := rect{X: 0, Y: 32, W: 1920, H: 1048}
	one := []rect{{X: 0, Y: 0, W: 1920, H: 1080}}
	// Two 1920x1080 monitors side by side: the desktop canvas is 3840 wide and _NET_WORKAREA covers both, less the top bar.
	wideScreen := rect{X: 0, Y: 0, W: 3840, H: 1080}
	wideWork := rect{X: 0, Y: 32, W: 3840, H: 1048}
	two := []rect{{X: 0, Y: 0, W: 1920, H: 1080}, {X: 1920, Y: 0, W: 1920, H: 1080}}
	cases := []struct {
		name           string
		frame          rect
		work, screen   rect
		mons           []rect
		wantDX, wantDY int
	}{
		{name: "a maximized window the toolkit places at the origin", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, work: work, screen: screen, mons: one, wantDY: 32},
		{name: "a maximized window the toolkit already places correctly", frame: rect{X: 0, Y: 32, W: 1920, H: 1048}, work: work, screen: screen, mons: one},
		{name: "a full screen window covers the panel and is where it says", frame: rect{X: 0, Y: 0, W: 1920, H: 1080}, work: work, screen: screen, mons: one},
		{name: "a floating window cannot be placed and is left alone", frame: rect{X: 0, Y: 0, W: 800, H: 600}, work: work, screen: screen, mons: one},
		{name: "a desk with no panel needs no shift", frame: rect{X: 0, Y: 0, W: 1920, H: 1080}, work: screen, screen: screen, mons: one},
		{name: "nothing is read and nothing is shifted", frame: rect{}, work: rect{}, screen: rect{}},
		{name: "a panel down the left edge shifts x", frame: rect{X: 0, Y: 0, W: 1856, H: 1080}, work: rect{X: 64, Y: 0, W: 1856, H: 1080}, screen: screen, mons: one, wantDX: 64},
		{name: "no monitor list, so only the desktop-wide work area can be matched", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, work: work, screen: screen, wantDY: 32},
		// A window maximized on either monitor of a two-monitor desk is 1920x1048: as wide as one monitor, as tall as the work area, and matching neither the desktop-wide work area nor the whole canvas.
		{name: "maximized on a second monitor, reporting window coordinates", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, work: wideWork, screen: wideScreen, mons: two, wantDY: 32},
		{name: "maximized on a second monitor, reporting where it really is", frame: rect{X: 1920, Y: 32, W: 1920, H: 1048}, work: wideWork, screen: wideScreen, mons: two},
		{name: "maximized on the first monitor of two", frame: rect{X: 0, Y: 32, W: 1920, H: 1048}, work: wideWork, screen: wideScreen, mons: two},
		{name: "full screen on one monitor of two is where it says", frame: rect{X: 1920, Y: 0, W: 1920, H: 1080}, work: wideWork, screen: wideScreen, mons: two},
		{name: "a floating window on a two-monitor desk is left alone", frame: rect{X: 2200, Y: 300, W: 800, H: 600}, work: wideWork, screen: wideScreen, mons: two},
	}
	for _, c := range cases {
		dx, dy := windowShift(c.frame, c.work, c.screen, c.mons)
		if dx != c.wantDX || dy != c.wantDY {
			t.Errorf("%s: windowShift(%+v, %+v, %+v, %v) = %d,%d; want %d,%d", c.name, c.frame, c.work, c.screen, c.mons, dx, dy, c.wantDX, c.wantDY)
		}
	}
}
