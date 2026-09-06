//go:build linux

package tracker

import (
	"context"
	"image"
	"testing"

	"github.com/godbus/dbus/v5"
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
		// For a content role the label is the node's own contents, not a name somebody chose for it, so it changes whenever the user types — and typing into a box is the ordinary thing to do between listing it and clicking it. The role and the rectangle still say it is the same box. Comparing the contents refused those clicks and sent the model to look at the screen again.
		{name: "the user typed a character into the listed entry", nowRole: "entry", nowLabel: "hello!", now: was, role: "entry", label: "hello", was: was},
		{name: "the paragraph that was listed was edited", nowRole: "text", nowLabel: "Dear Bob,", now: was, role: "text", label: "Dear Bo", was: was},
		// A control's label is a name, so a changed one still means a different element.
		{name: "another label on a button", nowRole: "push button", nowLabel: "Delete", now: was, role: "push button", label: "Send", was: was, wantErr: true},
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
	// The pointer is only consulted for a frame reporting 0,0 on a desk with more than one monitor; nowhere is where X answers when it cannot be read.
	nowhere := image.Pt(-1, -1)
	onFirst := image.Pt(400, 500)
	cases := []struct {
		name           string
		frame          rect
		d              desk
		wantDX, wantDY int
	}{
		{name: "a maximized window the toolkit places at the origin", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, d: desk{work: work, screen: screen, mons: one, pointer: nowhere}, wantDY: 32},
		{name: "a maximized window the toolkit already places correctly", frame: rect{X: 0, Y: 32, W: 1920, H: 1048}, d: desk{work: work, screen: screen, mons: one, pointer: nowhere}},
		{name: "a full screen window covers the panel and is where it says", frame: rect{X: 0, Y: 0, W: 1920, H: 1080}, d: desk{work: work, screen: screen, mons: one, pointer: nowhere}},
		{name: "a floating window cannot be placed and is left alone", frame: rect{X: 0, Y: 0, W: 800, H: 600}, d: desk{work: work, screen: screen, mons: one, pointer: nowhere}},
		{name: "a desk with no panel needs no shift", frame: rect{X: 0, Y: 0, W: 1920, H: 1080}, d: desk{work: screen, screen: screen, mons: one, pointer: nowhere}},
		{name: "nothing is read and nothing is shifted", frame: rect{}, d: desk{pointer: nowhere}},
		{name: "a panel down the left edge shifts x", frame: rect{X: 0, Y: 0, W: 1856, H: 1080}, d: desk{work: rect{X: 64, Y: 0, W: 1856, H: 1080}, screen: screen, mons: one, pointer: nowhere}, wantDX: 64},
		{name: "no monitor list, so only the desktop-wide work area can be matched", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, d: desk{work: work, screen: screen, pointer: nowhere}, wantDY: 32},
		// A window maximized on either monitor of a two-monitor desk is 1920x1048: as wide as one monitor, as tall as the work area, and matching neither the desktop-wide work area nor the whole canvas.
		{name: "maximized on a second monitor, reporting window coordinates", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, d: desk{work: wideWork, screen: wideScreen, mons: two, pointer: image.Pt(2500, 500)}, wantDX: 1920, wantDY: 32},
		{name: "maximized on the first monitor of two, reporting window coordinates", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, d: desk{work: wideWork, screen: wideScreen, mons: two, pointer: onFirst}, wantDY: 32},
		{name: "maximized on a second monitor, reporting where it really is", frame: rect{X: 1920, Y: 32, W: 1920, H: 1048}, d: desk{work: wideWork, screen: wideScreen, mons: two, pointer: nowhere}},
		{name: "maximized on the first monitor of two", frame: rect{X: 0, Y: 32, W: 1920, H: 1048}, d: desk{work: wideWork, screen: wideScreen, mons: two, pointer: nowhere}},
		{name: "full screen on one monitor of two is where it says", frame: rect{X: 1920, Y: 0, W: 1920, H: 1080}, d: desk{work: wideWork, screen: wideScreen, mons: two, pointer: nowhere}},
		{name: "a floating window on a two-monitor desk is left alone", frame: rect{X: 2200, Y: 300, W: 800, H: 600}, d: desk{work: wideWork, screen: wideScreen, mons: two, pointer: nowhere}},
	}
	for _, c := range cases {
		dx, dy := windowShift(c.frame, c.d)
		if dx != c.wantDX || dy != c.wantDY {
			t.Errorf("%s: windowShift(%+v, %+v) = %d,%d; want %d,%d", c.name, c.frame, c.d, dx, dy, c.wantDX, c.wantDY)
		}
	}
}

// A native Wayland client answers 0,0 when asked where its own window is, so on a desk with more than one monitor the corner it reports names the monitor at the desktop origin whatever monitor the window is really on: a maximized Brave on the right-hand monitor got dx=0 and every rectangle in its listing named a point 1920 pixels to the left of the element it was for. The pointer is the second opinion, and it is the same stand-in screenLayout already uses to pick the monitor a stored frame came from.
func TestWindowMonitor(t *testing.T) {
	first := rect{X: 0, Y: 0, W: 1920, H: 1080}
	second := rect{X: 1920, Y: 0, W: 1920, H: 1080}
	two := []rect{first, second}
	cases := []struct {
		name    string
		frame   rect
		mons    []rect
		pointer image.Point
		want    rect
		wantOK  bool
	}{
		{name: "a window that says where it is", frame: rect{X: 1920, Y: 32, W: 1920, H: 1048}, mons: two, pointer: image.Pt(400, 500), want: second, wantOK: true},
		{name: "a window that cannot say, with the pointer on the second monitor", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, mons: two, pointer: image.Pt(2500, 500), want: second, wantOK: true},
		{name: "a window that cannot say, with the pointer on the first monitor", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, mons: two, pointer: image.Pt(400, 500), want: first, wantOK: true},
		{name: "a window that cannot say, and no pointer either", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, mons: two, pointer: image.Pt(-1, -1), want: first, wantOK: true},
		// One monitor: the corner cannot name the wrong screen, so the pointer is never asked and a pointer on another desk cannot move anything.
		{name: "one monitor is the only answer there is", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, mons: []rect{first}, pointer: image.Pt(2500, 500), want: first, wantOK: true},
		{name: "no monitor list at all", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, pointer: image.Pt(400, 500)},
		{name: "a window off every monitor", frame: rect{X: 9000, Y: 9000, W: 800, H: 600}, mons: two, pointer: image.Pt(-1, -1)},
	}
	for _, c := range cases {
		got, ok := windowMonitor(c.frame, c.mons, c.pointer)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("%s: windowMonitor(%+v, %v, %v) = %+v, %v; want %+v, %v", c.name, c.frame, c.mons, c.pointer, got, ok, c.want, c.wantOK)
		}
	}
}

// show_marks reads the rectangle of up to forty items in one call, and each of those reads went through screenShift, which opened one X connection for _NET_WORKAREA and another for RandR every time: eighty connections for one call, all of them asking a layout that changes when a monitor is plugged in and not otherwise. deskNow reads it once and hands the same answer back for deskTTL.
func TestDeskNow_ReadsTheDesktopOncePerCall(t *testing.T) {
	reads := 0
	restore := readDesk
	readDesk = func() (desk, bool) {
		reads++
		return desk{work: rect{W: 1920, H: 1048}, screen: rect{W: 1920, H: 1080}, pointer: image.Pt(-1, -1)}, true
	}
	t.Cleanup(func() { readDesk = restore; forgetDesk() })
	forgetDesk()
	for i := 0; i < 40; i++ {
		if _, ok := deskNow(); !ok {
			t.Fatalf("read %d came back with nothing", i)
		}
	}
	if reads != 1 {
		t.Errorf("forty rectangle reads asked X %d times, want 1", reads)
	}
	// The cache has to expire, or a monitor plugged in mid-session would never be noticed.
	forgetDesk()
	if _, ok := deskNow(); !ok || reads != 2 {
		t.Errorf("after the cache expired X was asked %d times, want 2", reads)
	}
}

// A ref that never came from a node list cannot be read at all, and Focused has to say so rather than answer false, which the caller would read as "some other element has the keyboard" and refuse a legitimate typing.
func TestFocused_RefusesAMalformedRef(t *testing.T) {
	if _, err := Focused(context.Background(), "nonsense"); err == nil {
		t.Error("Focused must refuse a ref with no bus name and object path in it")
	}
}

// readStates answers nil for every way a read can fail — a D-Bus timeout, an element that has gone, a toolkit that publishes no state — and reporting that as "not focused" is what made a caller refuse to type into fields that were perfectly focused. An empty answer is an error; a real answer with the bit clear is a plain false.
func TestFocusedFromStates_TellsAnEmptyReadFromANotFocusedOne(t *testing.T) {
	if _, err := focusedFromStates(nil, "r-address"); err == nil {
		t.Error("a state read that came back with nothing must be an error, not a false")
	}
	held, err := focusedFromStates([]uint32{1 << stateFocused, 0}, "r-address")
	if err != nil || !held {
		t.Errorf("focusedFromStates(focused) = (%v, %v), want (true, nil)", held, err)
	}
	held, err = focusedFromStates([]uint32{0, 0}, "r-address")
	if err != nil || held {
		t.Errorf("focusedFromStates(not focused) = (%v, %v), want (false, nil)", held, err)
	}
}

// A Chromium or Electron text input often leaves STATE_FOCUSED off the node the accessibility walk listed and sets it on a child of that node instead, so reading the bit on the listed node alone answers "something else has the keyboard" for a box the keys are going straight into. The walk has to look under the node before it says no.
func TestFindFocused(t *testing.T) {
	ref := func(path string) aref { return aref{Name: ":1.7", Path: dbus.ObjectPath(path)} }
	kids := map[string][]string{"/win": {"/bar", "/page"}, "/page": {"/input"}, "/input": {"/inner"}}
	// read answers for one window as the bus would: the node at focused carries the bit and every node has the children kids gives it.
	read := func(focused string) func(aref) (bool, []aref) {
		return func(r aref) (bool, []aref) {
			if string(r.Path) == focused {
				return true, nil
			}
			out := make([]aref, 0, len(kids[string(r.Path)]))
			for _, k := range kids[string(r.Path)] {
				out = append(out, ref(k))
			}
			return false, out
		}
	}
	cases := []struct {
		name    string
		focused string
		want    string
	}{
		{name: "the bit sits on a child of the listed input", focused: "/inner", want: "/inner"},
		{name: "the bit sits on the node itself", focused: "/input", want: "/input"},
		{name: "nothing in the window carries the bit", focused: "/elsewhere"},
	}
	for _, c := range cases {
		budget := maxNodes
		got, ok := findFocused(ref("/win"), 0, &budget, read(c.focused))
		if ok != (c.want != "") || (ok && string(got.Path) != c.want) {
			t.Errorf("%s: findFocused = %q, %v; want %q, %v", c.name, got.Path, ok, c.want, c.want != "")
		}
	}
}

// A walk that reads every node of a window must stop on its budget rather than on the size of the tree, since the tree it is given is a whole browser page.
func TestFindFocused_StopsOnItsBudget(t *testing.T) {
	reads := 0
	// Each node has one child for ever, so only the budget can end this walk.
	read := func(r aref) (bool, []aref) {
		reads++
		return false, []aref{{Name: r.Name, Path: r.Path + "/x"}}
	}
	budget := 5
	if _, ok := findFocused(aref{Name: ":1.7", Path: "/win"}, 0, &budget, read); ok {
		t.Error("findFocused found a focused element in a tree that has none")
	}
	if reads > 5 {
		t.Errorf("the walk read %d elements, want at most the 5 it was given", reads)
	}
}
