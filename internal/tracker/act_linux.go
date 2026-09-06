//go:build linux

package tracker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ora/internal/act"

	"github.com/godbus/dbus/v5"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// refString packs an accessible's bus name and object path into the one string act.Node carries. Input: the ref. Output: "name|path".
func refString(ref aref) string { return ref.Name + "|" + string(ref.Path) }

// parseARef is refString's inverse. Input: "name|path". Output: the ref, or an error when the string has no separator.
func parseARef(s string) (aref, error) {
	name, path, ok := strings.Cut(s, "|")
	if !ok || name == "" || path == "" {
		return aref{}, fmt.Errorf("not a node ref: %q", s)
	}
	return aref{Name: name, Path: dbus.ObjectPath(path)}, nil
}

// preferredActions is the order to try a node's actions in; Chromium names the primary one press, click, jump, select, activate or doDefault depending on the role.
var preferredActions = []string{"press", "click", "jump", "select", "activate", "doDefault"}

// pickAction chooses which of a node's actions to fire. Input: the action names as the node lists them. Output: the index of the first preferred name present, else the first name that is not the context menu, else 0; -1 for no actions at all.
func pickAction(names []string) int {
	if len(names) == 0 {
		return -1
	}
	for _, want := range preferredActions {
		for i, n := range names {
			if n == want {
				return i
			}
		}
	}
	for i, n := range names {
		if n != "showContextMenu" {
			return i
		}
	}
	return 0
}

// actionConn is the bus connection actions go over: the focus watcher's, since it is already open. Output: an error when there is no accessibility bus.
func actionConn() (*dbus.Conn, error) {
	w := focus()
	if w == nil {
		return nil, errors.New("the accessibility bus is not reachable")
	}
	return w.conn, nil
}

// DoAction fires a node's primary accessibility action, which is what a click does without moving the pointer. Input: a context and the node's Ref from act.Node. Output: the name of the action fired, or an error when the ref is malformed, the node has gone, has no actions, or the toolkit reports the action failed.
func DoAction(ctx context.Context, ref string) (string, error) {
	r, err := parseARef(ref)
	if err != nil {
		return "", err
	}
	conn, err := actionConn()
	if err != nil {
		return "", err
	}
	obj := conn.Object(r.Name, r.Path)
	names, err := actionNames(ctx, obj)
	if err != nil {
		return "", err
	}
	i := pickAction(names)
	if i < 0 {
		return "", errors.New("the element offers no action to fire")
	}
	var ok bool
	if err := obj.CallWithContext(ctx, "org.a11y.atspi.Action.DoAction", 0, int32(i)).Store(&ok); err != nil {
		return "", fmt.Errorf("%s failed: %w", names[i], err)
	}
	if !ok {
		return "", fmt.Errorf("the element refused %s", names[i])
	}
	return names[i], nil
}

// actionNames lists a node's actions one name at a time, through the NActions property and GetName, and never through GetActions. Input: a context and the node's bus object. Output: the action names in index order, or an error when the node no longer answers. GetActions replies with a(sss), and on 2026-09-05 Brave (Chromium, snap) aborted inside its D-Bus library three times in a row while building that reply for a web-page node ("Array or variant type requires that type end_struct be written, but string was written"), which closed the user's browser each time the model clicked; NActions and GetName are the calls Orca and libatspi make and reply with an int and a string.
func actionNames(ctx context.Context, obj dbus.BusObject) ([]string, error) {
	var n int32
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.a11y.atspi.Action", "NActions").Store(&n); err != nil {
		return nil, fmt.Errorf("the element no longer answers: %w", err)
	}
	names := make([]string, 0, n)
	for i := int32(0); i < n; i++ {
		var name string
		if err := obj.CallWithContext(ctx, "org.a11y.atspi.Action.GetName", 0, i).Store(&name); err != nil {
			return nil, fmt.Errorf("the element no longer answers: %w", err)
		}
		names = append(names, name)
	}
	return names, nil
}

// rect is an element's rectangle in screen pixels, as org.a11y.atspi.Component reports it.
type rect struct{ X, Y, W, H int }

// coordScreen is ATSPI_COORD_TYPE_SCREEN, the coordinate type that asks org.a11y.atspi.Component for a rectangle on the screen rather than inside its own window (which is type 1, ATSPI_COORD_TYPE_WINDOW).
const coordScreen uint32 = 0

// Extents reads where an element is on the screen right now, so a ring is drawn around the element rather than around the rectangle it occupied when observe_screen made its list. Input: a context and the node's Ref from act.Node. Output: the rectangle in real screen pixels, or an error when the ref is malformed, the accessibility bus is unreachable, or the element no longer answers.
// The bus is asked for screen coordinates and does not always answer with them, so the answer is corrected before it is handed back: see screenShift.
func Extents(ctx context.Context, ref string) (x, y, w, h int, err error) {
	r, err := parseARef(ref)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	conn, err := actionConn()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	got, err := readExtents(ctx, conn, r, coordScreen)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	dx, dy := screenShift(ctx, conn, r)
	return got.X + dx, got.Y + dy, got.W, got.H, nil
}

// readExtents asks one node for its rectangle. Input: a context, the bus connection, the node's ref and the coordinate type (coordScreen or coordWindow). Output: the rectangle, or an error when the node no longer answers.
func readExtents(ctx context.Context, conn *dbus.Conn, ref aref, coord uint32) (rect, error) {
	var got struct{ X, Y, W, H int32 }
	if err := conn.Object(ref.Name, ref.Path).CallWithContext(ctx, "org.a11y.atspi.Component.GetExtents", 0, coord).Store(&got); err != nil {
		return rect{}, fmt.Errorf("the element no longer answers: %w", err)
	}
	return rect{X: int(got.X), Y: int(got.Y), W: int(got.W), H: int(got.H)}, nil
}

// windowRoles are the roles a top-level window carries on the accessibility bus; the walk up from a node stops at the first node with one of them.
var windowRoles = map[string]bool{"frame": true, "window": true, "dialog": true, "alert": true}

// maxParentHops bounds the walk from a node up to its window, so a tree that answers with a cycle cannot spin here for ever. Measured on this desk on 2026-09-05, Brave's address bar sits seven levels under its frame, so this is several times what a real page needs.
const maxParentHops = 40

// windowOf walks up from a node to the top-level window it is drawn in. Input: a context, the bus connection and the node's ref. Output: that window's rectangle as the bus reports it in screen coordinates, and true; false when the chain breaks or runs past maxParentHops before a window is reached.
func windowOf(ctx context.Context, conn *dbus.Conn, ref aref) (rect, bool) {
	at := ref
	for hop := 0; hop < maxParentHops; hop++ {
		if windowRoles[getRoleName(ctx, conn, at)] {
			r, err := readExtents(ctx, conn, at, coordScreen)
			if err != nil {
				return rect{}, false
			}
			return r, true
		}
		parent := getParent(ctx, conn, at)
		if parent.Name == "" || parent.Path == "" || parent.Path == "/org/a11y/atspi/null" {
			return rect{}, false
		}
		at = parent
	}
	return rect{}, false
}

// screenShift is how far a rectangle this node's window reports has to move to land where that window really is on the screen. Input: a context, the bus connection and the node's ref. Output: how much to add to x and to y, and 0,0 whenever nothing here can say.
// Coordinate type 0 does mean screen coordinates: measured on this desk on 2026-09-05, Ora's own GTK window answered 0,32 1920x1048 for it and a Brave popup answered 1551,110, both of which are where those windows really were. But a client that cannot know where its own window sits — which is every native Wayland client, Chromium and GTK included — answers with window coordinates instead, and Brave's maximized frame came back as 0,0 1920x1048 when the work area starts at y=32. Every rectangle read out of that window was then 32 pixels too high, so a ring drawn from one landed a top bar's height above the thing it was naming.
func screenShift(ctx context.Context, conn *dbus.Conn, ref aref) (dx, dy int) {
	work, screen, ok := desktopBounds()
	if !ok {
		return 0, 0
	}
	frame, ok := windowOf(ctx, conn, ref)
	if !ok {
		return 0, 0
	}
	return windowShift(frame, work, screen, monitorRects())
}

// monitorRects reads the monitors making up the desktop, in the same screen pixels desktopBounds reports. Input: none; it asks RandR through screenLayout. Output: one rectangle per monitor, or nil when X or RandR is unreachable.
func monitorRects() []rect {
	mons, _ := screenLayout()
	out := make([]rect, 0, len(mons))
	for _, m := range mons {
		out = append(out, rect{X: m.Min.X, Y: m.Min.Y, W: m.Dx(), H: m.Dy()})
	}
	return out
}

// windowShift works out how far one window's rectangles are from the truth. Input: the window's rectangle as the bus reported it, the desktop's work area, the whole screen, and the monitors it is made of (nil when RandR cannot be read), all in pixels. Output: how much to add to x and to y.
// A window the size of the whole screen, or of one whole monitor, is full screen and is where it says it is. A window as tall as the work area and as wide as either the work area or some monitor is maximized, and a maximized window's top-left corner is the work area's top-left corner on the monitor it sits on, so the gap between the two is the whole error. A window of any other size is one this cannot place from its size alone, and is left alone rather than moved by a guess.
// Each monitor's own work area is not readable here — _NET_WORKAREA is one desktop-wide rectangle covering every monitor — so the maximized test is the tolerant one: the height must match that single work area's height whichever monitor the window is on, and the width has only to match one monitor. The monitor is taken to be the one the reported top-left corner falls in, which for a native Wayland client answering 0,0 is the monitor at the desktop origin; that is right for the top bar's height, which is the error being corrected, and cannot tell a window maximized on a second monitor apart from one maximized on the first.
func windowShift(frame, work, screen rect, mons []rect) (dx, dy int) {
	if frame.W <= 0 || frame.H <= 0 || work.W <= 0 || work.H <= 0 {
		return 0, 0
	}
	if frame.W == screen.W && frame.H == screen.H {
		return 0, 0
	}
	for _, m := range mons {
		if frame.W == m.W && frame.H == m.H {
			return 0, 0
		}
	}
	if frame.H != work.H || !maximizedWidth(frame.W, work, mons) {
		return 0, 0
	}
	x, y := work.X, work.Y
	if m, ok := monitorAt(frame.X, frame.Y, mons); ok {
		x, y = m.X+work.X, m.Y+work.Y
	}
	return x - frame.X, y - frame.Y
}

// maximizedWidth reports whether a window that wide is as wide as something it could be maximized across. Input: the window's width, the desktop work area and the monitors. Output: true when it matches the work area (a one-monitor desk, or a window maximized across the lot) or any one monitor.
func maximizedWidth(w int, work rect, mons []rect) bool {
	if w == work.W {
		return true
	}
	for _, m := range mons {
		if w == m.W {
			return true
		}
	}
	return false
}

// monitorAt finds the monitor a point falls in. Input: the point and the monitors. Output: that monitor and true, or false when no monitor covers the point (including when the monitor list could not be read).
func monitorAt(x, y int, mons []rect) (rect, bool) {
	for _, m := range mons {
		if x >= m.X && x < m.X+m.W && y >= m.Y && y < m.Y+m.H {
			return m, true
		}
	}
	return rect{}, false
}

// desktopBounds reads the desktop's work area and the whole screen in real screen pixels. Input: none; it reads _NET_WORKAREA off the X root window, which under Wayland answers through XWayland — mutter keeps that in step with the real layout, the same way screenLayout reads the monitors. Output: the work area, the screen, and false when X is unreachable or publishes no work area.
func desktopBounds() (work, screen rect, ok bool) {
	conn, err := xgb.NewConn()
	if err != nil {
		return rect{}, rect{}, false
	}
	defer conn.Close()
	root := xproto.Setup(conn).DefaultScreen(conn)
	screen = rect{W: int(root.WidthInPixels), H: int(root.HeightInPixels)}
	atom, err := xproto.InternAtom(conn, true, uint16(len(netWorkArea)), netWorkArea).Reply()
	if err != nil || atom.Atom == 0 {
		return rect{}, rect{}, false
	}
	// Four 32-bit numbers: x, y, width and height of the first workspace's work area, which is the one every window on this desk is laid out in.
	prop, err := xproto.GetProperty(conn, false, root.Root, atom.Atom, xproto.GetPropertyTypeAny, 0, 4).Reply()
	if err != nil || len(prop.Value) < 16 {
		return rect{}, rect{}, false
	}
	work = rect{
		X: int(int32(xgb.Get32(prop.Value[0:]))),
		Y: int(int32(xgb.Get32(prop.Value[4:]))),
		W: int(int32(xgb.Get32(prop.Value[8:]))),
		H: int(int32(xgb.Get32(prop.Value[12:]))),
	}
	return work, screen, true
}

// netWorkArea is the root-window property every EWMH desktop publishes its work area in: the screen less whatever its own panels have reserved.
const netWorkArea = "_NET_WORKAREA"

// movedBy is how far, in pixels, an element's rectangle may differ from the one observe_screen recorded and still count as the same element in the same place. It covers the rounding a scaled display introduces and little else: a list that has scrolled moves a row by at least its own height, and two entries stacked in a form sit further apart than this even when neither carries a label.
const movedBy = 8

// Verify checks that a node still is what observe_screen described, since a toolkit can hand a recycled object path to a different element after a page re-renders, and since a page that scrolls under the list leaves every number pointing at the right element in the wrong place. Input: a context, the node's Ref, the role and label the list showed, and the rectangle it showed. Output: nil when the role, the label and the rectangle all still match (the label check is skipped when the list showed none and when the role is a content role, whose label is the node's own contents, the rectangle check when the list showed no size), or an error naming what changed or that the node has gone.
func Verify(ctx context.Context, ref, role, label string, x, y, w, h int) error {
	r, err := parseARef(ref)
	if err != nil {
		return err
	}
	conn, err := actionConn()
	if err != nil {
		return err
	}
	nowRole := getRoleName(ctx, conn, r)
	// The label and the rectangle each cost a round trip, so neither is read when there is nothing in the list to compare it against, and the label is not read at all for a content role, whose label is not compared.
	nowLabel := ""
	if label != "" && !act.ContentRole(role) {
		nowLabel = getName(ctx, conn, r)
		if nowLabel == "" {
			nowLabel = strings.TrimSpace(getText(ctx, conn, r))
		}
	}
	now := rect{}
	if w > 0 && h > 0 {
		now.X, now.Y, now.W, now.H = getExtents(ctx, conn, r)
	}
	return verifyAgainst(nowRole, nowLabel, now, role, label, rect{X: x, Y: y, W: w, H: h})
}

// verifyAgainst compares what an element is now with what observe_screen recorded for it. Input: the role, label and rectangle read from the element just now, then the role, label and rectangle the numbered list showed. Output: nil when they still describe the same element in the same place, or an error naming what changed; an empty label in the list means the list held none, and a rectangle of no size in the list means it held none either, and neither is then compared; a content role's label is not compared at all.
func verifyAgainst(nowRole, nowLabel string, now rect, role, label string, was rect) error {
	if nowRole == "" {
		return errors.New("the element has gone")
	}
	if nowRole != role {
		return fmt.Errorf("it is now a %s, not a %s", nowRole, role)
	}
	// A content role's label is the node's own contents — what is typed in an entry, what a run of page text says — not a name anybody chose for it, so it changes whenever the user types and says nothing about whether this is still the same element. The role and the rectangle still do. Comparing it refused a click on a box the user had just typed into, which is the ordinary thing to happen between listing a box and clicking it.
	if label != "" && !act.ContentRole(role) && nowLabel != label {
		return fmt.Errorf("it is now labelled %q, not %q", nowLabel, label)
	}
	if was.W <= 0 || was.H <= 0 {
		return nil
	}
	if now.W <= 0 || now.H <= 0 {
		return errors.New("it is no longer showing on the screen")
	}
	if moved(now, was) {
		return fmt.Errorf("it is now at %d,%d %dx%d, not %d,%d %dx%d: the page has moved under the list", now.X, now.Y, now.W, now.H, was.X, was.Y, was.W, was.H)
	}
	return nil
}

// moved reports whether two rectangles are too far apart to be the same element in the same place. Input: the rectangle read from the element now and the one observe_screen recorded. Output: true when any of the four numbers differs by more than movedBy pixels.
func moved(now, was rect) bool {
	return away(now.X, was.X) || away(now.Y, was.Y) || away(now.W, was.W) || away(now.H, was.H)
}

// away reports whether two pixel counts differ by more than movedBy.
func away(a, b int) bool { return a-b > movedBy || b-a > movedBy }

// scrollAnywhere is ATSPI_SCROLL_ANYWHERE: bring the node into view wherever is cheapest.
const scrollAnywhere uint32 = 6

// ScrollTo scrolls a node into view. Input: a context and the node's Ref. Output: an error when the ref is malformed, the node has gone, or the toolkit could not scroll.
func ScrollTo(ctx context.Context, ref string) error {
	r, err := parseARef(ref)
	if err != nil {
		return err
	}
	conn, err := actionConn()
	if err != nil {
		return err
	}
	var ok bool
	if err := conn.Object(r.Name, r.Path).CallWithContext(ctx, "org.a11y.atspi.Component.ScrollTo", 0, scrollAnywhere).Store(&ok); err != nil {
		return fmt.Errorf("the element no longer answers: %w", err)
	}
	if !ok {
		return errors.New("the element could not be scrolled into view")
	}
	return nil
}
