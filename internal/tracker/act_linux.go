//go:build linux

package tracker

import (
	"context"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"strings"
	"sync"
	"time"

	"june/internal/act"

	"github.com/godbus/dbus/v5"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// actTimeout bounds one entry point that acts on or measures an element. An application that has stopped answering the accessibility bus is exactly the state a model clicks into, and the caller's context comes from a whole ask or a whole job, so without a bound of its own one wedged toolkit holds that ask up for as long as it lasts. It is the same few seconds Observe gives a whole window walk.
const actTimeout = 4 * time.Second

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
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
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

// coordScreen is ATSPI_COORD_TYPE_SCREEN, the coordinate type that asks org.a11y.atspi.Component for a rectangle on the screen rather than inside its own window, which is coordWindow (ATSPI_COORD_TYPE_WINDOW).
const (
	coordScreen uint32 = 0
	coordWindow uint32 = 1
)

// readPlace reads one node's rectangle the way readExtents does, but in the coordinates the listing and the click both use: the screen answer, or the window answer when the screen answer put the node at the origin. A GTK4 client answers coordScreen with 0,0 for every widget (measured on this desk on 2026-09-08: gnome-control-center's "Search" button was 0,0 34x34 for the screen type and 125,11 34x34 for the window type), so without this every row of a list sat at one point and a click aimed at it landed on the window's header. A client that cannot place its widgets on the screen cannot place its window either and reports the frame at 0,0 too, so the window answer is in the same space windowShift already corrects. Input: a context, the bus connection and the node's ref. Output: the rectangle, or an error when the node no longer answers.
func readPlace(ctx context.Context, conn *dbus.Conn, ref aref) (rect, error) {
	got, err := readExtents(ctx, conn, ref, coordScreen)
	if err != nil {
		return rect{}, err
	}
	return placeOf(got, func() rect {
		w, err := readExtents(ctx, conn, ref, coordWindow)
		if err != nil {
			return got
		}
		return w
	}), nil
}

// placeOf picks between the screen answer and the window answer for one node. Input: the screen rectangle, and a function reading the window rectangle, called only when needed. Output: the window rectangle when the screen one sits at 0,0 with a real size, the screen one otherwise.
func placeOf(screen rect, window func() rect) rect {
	if screen.X == 0 && screen.Y == 0 && screen.W > 0 && screen.H > 0 {
		return window()
	}
	return screen
}

// Extents reads where an element is on the screen right now, so a ring is drawn around the element rather than around the rectangle it occupied when observe_screen made its list. Input: a context and the node's Ref from act.Node. Output: the rectangle in real screen pixels, or an error when the ref is malformed, the accessibility bus is unreachable, or the element no longer answers.
// The bus is asked for screen coordinates and does not always answer with them, so the answer is corrected before it is handed back: see screenShift.
func Extents(ctx context.Context, ref string) (x, y, w, h int, err error) {
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
	r, err := parseARef(ref)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	conn, err := actionConn()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	got, err := readPlace(ctx, conn, r)
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

// windowPlacer answers where a window really is on the screen, in logical pixels, and false when it cannot say. Input: the pid of the process that owns the window, and its title. Output: the window's frame and true, false when it cannot be matched. nil until UseWindowPlacer wires one in; the daemon wires the bundled shell extension's window list, which is the only thing on a Wayland desk that knows.
var windowPlacer func(ctx context.Context, pid uint32, title string) (x, y, w, h int, ok bool)

// UseWindowPlacer wires in the reader of real window positions. Input: a function answering the frame of the window owned by that pid, preferring the one titled title when the pid owns more than one, and false when there is none or nothing can say. Output: none. Call it before the first observe; a placer is asked on every correction and may answer false until the shell extension is reachable.
func UseWindowPlacer(f func(ctx context.Context, pid uint32, title string) (x, y, w, h int, ok bool)) {
	windowPlacer = f
}

// shiftOf is how far a node's window has to move to be where it really is on the screen. Input: a context, the bus connection and the node's ref. Output: the shift to add to x and to y, and false when the node's window cannot be found. The placer's answer wins when it has one, since it is a measurement; the size-based guess in windowShift is the fallback for a desk with no extension.
func shiftOf(ctx context.Context, conn *dbus.Conn, ref aref) (dx, dy int, ok bool) {
	frame, win, ok := windowOf(ctx, conn, ref)
	if !ok {
		return 0, 0, false
	}
	pid, _ := busPid(ctx, conn, win.Name)
	if dx, dy, ok := shiftFromPlacer(ctx, pid, getName(ctx, conn, win), frame); ok {
		return dx, dy, true
	}
	d, ok := deskNow()
	if !ok {
		return 0, 0, false
	}
	dx, dy = windowShift(frame, d)
	return dx, dy, true
}

// shiftFromPlacer asks the wired window placer for one node's window shift, matched by the pid of the process that owns the accessibility connection rather than by title alone: a title that carries a live status such as a memory count changes between the listing and the click, and a title match that must be exact then misses the window it just found by pid, falling back to a size-based guess that can differ from the real shift by a monitor's width. Input: a context, the window's pid and title, and the frame the accessibility bus reported for it. Output: the shift, and true when the placer is wired and has an answer for this window; false otherwise, which leaves the size-based guess in windowShift as the only source left.
func shiftFromPlacer(ctx context.Context, pid uint32, title string, frame rect) (dx, dy int, ok bool) {
	if windowPlacer == nil {
		return 0, 0, false
	}
	x, y, _, _, ok := windowPlacer(ctx, pid, title)
	if !ok {
		return 0, 0, false
	}
	return x - frame.X, y - frame.Y, true
}

// windowOf walks up from a node to the top-level window it is drawn in. Input: a context, the bus connection and the node's ref. Output: that window's rectangle as the bus reports it in screen coordinates, the window's ref, and true; false when the chain breaks or runs past maxParentHops before a window is reached.
func windowOf(ctx context.Context, conn *dbus.Conn, ref aref) (rect, aref, bool) {
	at := ref
	for hop := 0; hop < maxParentHops; hop++ {
		if windowRoles[getRoleName(ctx, conn, at)] {
			r, err := readExtents(ctx, conn, at, coordScreen)
			if err != nil {
				return rect{}, aref{}, false
			}
			return r, at, true
		}
		parent := getParent(ctx, conn, at)
		if parent.Name == "" || parent.Path == "" || parent.Path == "/org/a11y/atspi/null" {
			return rect{}, aref{}, false
		}
		at = parent
	}
	return rect{}, aref{}, false
}

// screenShift is how far a rectangle this node's window reports has to move to land where that window really is on the screen. Input: a context, the bus connection and the node's ref. Output: how much to add to x and to y, and 0,0 whenever nothing here can say. See shiftOf for where the answer comes from.
func screenShift(ctx context.Context, conn *dbus.Conn, ref aref) (dx, dy int) {
	dx, dy, _ = shiftOf(ctx, conn, ref)
	return dx, dy
}

// desk is the desktop the windows are laid out on: the work area, the whole canvas, the monitors making it up, and where the pointer is (-1,-1 when X could not say), all in the same screen pixels a whole-screen screenshot is in.
type desk struct {
	work, screen rect
	mons         []rect
	pointer      image.Point
}

// readDesk reads the desktop from X, one connection for _NET_WORKAREA and one for RandR. Output: the desktop, and false when X is unreachable or publishes no work area. It is a variable so a test can count how often the real read happens.
var readDesk = func() (desk, bool) {
	work, screen, ok := desktopBounds()
	if !ok {
		return desk{}, false
	}
	mons, pointer := screenLayout()
	return desk{work: work, screen: screen, mons: monitorRects(mons), pointer: pointer}, true
}

// deskTTL is how long one read of the desktop stands before it is taken again. Monitors are plugged in and panels resized in seconds; a show_marks call reads the rectangle of up to forty elements inside a few hundred milliseconds, and each of those reads used to open two X connections of its own.
const deskTTL = 2 * time.Second

// deskCache holds the last desktop read and when it was taken, so the reads one tool call makes share one answer.
var deskCache struct {
	mu   sync.Mutex
	at   time.Time
	desk desk
	ok   bool
}

// deskNow hands back the desktop, reading it from X at most once every deskTTL. Output: the desktop and true, or false when the last read failed — a failed read is cached too, so a machine with no X is not asked forty times a call.
func deskNow() (desk, bool) {
	deskCache.mu.Lock()
	defer deskCache.mu.Unlock()
	if !deskCache.at.IsZero() && time.Since(deskCache.at) < deskTTL {
		return deskCache.desk, deskCache.ok
	}
	deskCache.desk, deskCache.ok = readDesk()
	deskCache.at = time.Now()
	return deskCache.desk, deskCache.ok
}

// forgetDesk drops the cached desktop so the next deskNow reads X again. It exists for the tests, which need a known starting point and must not leave one behind.
func forgetDesk() {
	deskCache.mu.Lock()
	defer deskCache.mu.Unlock()
	deskCache.at = time.Time{}
}

// MonitorLogicalSize reports how big the monitor holding a desktop point is, in the logical pixels the accessibility bus, the work area and the portal's pointer all work in. Input: the point in those logical desktop pixels, which for the portal is the top-left corner its granted stream reported. Output: the monitor's logical width and height, and false when the desktop cannot be read or no monitor covers that point.
// It exists for the portal's pointer mapping (see input.UseMonitorLayout): a screen-cast stream is sized in the monitor's device pixels, and dividing the two is the only way to know how many stream pixels one logical pixel is worth on a scaled display.
func MonitorLogicalSize(x, y int) (w, h int, ok bool) {
	d, ok := deskNow()
	if !ok {
		return 0, 0, false
	}
	m, ok := monitorAt(x, y, d.mons)
	if !ok {
		return 0, 0, false
	}
	return m.W, m.H, true
}

// monitorRects converts the monitor rectangles screenLayout reports into the rectangle type this file works in. Input: the monitors as image rectangles. Output: one rect per monitor, empty when the list is empty.
func monitorRects(mons []image.Rectangle) []rect {
	out := make([]rect, 0, len(mons))
	for _, m := range mons {
		out = append(out, rect{X: m.Min.X, Y: m.Min.Y, W: m.Dx(), H: m.Dy()})
	}
	return out
}

// windowShift works out how far one window's rectangles are from the truth. Input: the window's rectangle as the bus reported it, and the desktop it is on. Output: how much to add to x and to y.
// A window the size of the whole screen, or of one whole monitor, is full screen and is where it says it is. A window as tall as the work area and as wide as either the work area or some monitor is maximized, and a maximized window's top-left corner is the work area's top-left corner on the monitor it sits on, so the gap between the two is the whole error. A window of any other size is one this cannot place from its size alone, and is left alone rather than moved by a guess.
// Each monitor's own work area is not readable here — _NET_WORKAREA is one desktop-wide rectangle covering every monitor — so the maximized test is the tolerant one: the height must match that single work area's height whichever monitor the window is on, and the width has only to match one monitor. Which monitor the window sits on is windowMonitor's question.
func windowShift(frame rect, d desk) (dx, dy int) {
	work, screen, mons := d.work, d.screen, d.mons
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
	if m, ok := windowMonitor(frame, mons, d.pointer); ok {
		x, y = m.X+work.X, m.Y+work.Y
	}
	return x - frame.X, y - frame.Y
}

// windowMonitor picks the monitor a window is on. Input: the window's rectangle as the bus reported it, the monitors, and the pointer's position (-1,-1 when X could not say). Output: that monitor and true, or false when nothing here can place the window.
// The reported corner answers it whenever the window can say where it is. A native Wayland client cannot: it answers 0,0, which on a desk with more than one monitor names the monitor at the desktop origin whatever monitor the window is really on — a maximized Brave on the right-hand monitor got no x shift and every rectangle in its listing named a point one monitor's width to the left of the element it was for. The pointer is then the second opinion, and it is the same stand-in screenLayout already uses to say which monitor a stored frame came from.
// ponytail: the pointer is wrong when the user's hand is on one monitor and the keyboard focus on another. The extension's window list settles that better when it is wired: shiftFromPlacer asks it for the window's real frame, by pid, before windowShift (and this monitor guess inside it) ever runs, so this pointer-based guess is now only the fallback for a desk with no extension reachable.
func windowMonitor(frame rect, mons []rect, pointer image.Point) (rect, bool) {
	if frame.X == 0 && frame.Y == 0 && len(mons) > 1 {
		if m, ok := monitorAt(pointer.X, pointer.Y, mons); ok {
			return m, true
		}
		unplacedWindow.Do(func() {
			slog.Debug("a window reported its corner as 0,0 on a desk with more than one monitor and the pointer could not be read, so its rectangles stay on the monitor at the desktop origin", "monitors", len(mons))
		})
	}
	return monitorAt(frame.X, frame.Y, mons)
}

// unplacedWindow keeps the note about a window no source could place to one line in the log, since it would otherwise be written once per element read.
var unplacedWindow sync.Once

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

// Verify checks that a node still is what observe_screen described, since a toolkit can hand a recycled object path to a different element after a page re-renders. Input: a context, the node's Ref, and the role and label the list showed; x, y, w and h are accepted for compatibility with the caller's fixed signature but no longer read or compared — a click fires the accessibility action on the ref itself, so a stale rectangle never stopped it from landing on the right element, and it only ever caused a refusal when a page changed under the node between the listing and the click, such as a live status word changing in a window's title. Output: nil when the role and the label still match (the label check is skipped when the list showed none, and for a content role, whose label is the node's own contents), or an error naming what changed or that the node has gone.
func Verify(ctx context.Context, ref, role, label string, x, y, w, h int) error {
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
	r, err := parseARef(ref)
	if err != nil {
		return err
	}
	conn, err := actionConn()
	if err != nil {
		return err
	}
	nowRole := getRoleName(ctx, conn, r)
	// The label costs a round trip, so it is not read when there is nothing in the list to compare it against, and not read at all for a content role, whose label is not compared.
	nowLabel := ""
	if label != "" && !act.ContentRole(role) {
		nowLabel = getName(ctx, conn, r)
		if nowLabel == "" {
			nowLabel = strings.TrimSpace(getText(ctx, conn, r))
		}
	}
	return VerifyAgainst(nowRole, nowLabel, role, label)
}

// Focused reports whether an element holds the keyboard focus right now, so a caller about to type can check that the box it is typing into is the box its guards were applied to: a click that opened a dialog, or an application that moved the focus itself, leaves the remembered element no longer the one the keys reach. Input: a context and the node's Ref from act.Node. Output: true when STATE_FOCUSED is set on that element, false when the element answered and the bit is not set, and an error when the answer says nothing either way — a malformed ref, an unreachable bus, or a GetState that timed out, named an element that has gone, or came back with no state words at all.
// The empty answer is an error rather than a false because the two are not the same thing to the caller: a toolkit that does not publish the bit on the node the walk listed, which Chromium and Electron often do not, would otherwise read as "some other element has the keyboard" and stop a legitimate typing.
func Focused(ctx context.Context, ref string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
	r, err := parseARef(ref)
	if err != nil {
		return false, err
	}
	conn, err := actionConn()
	if err != nil {
		return false, err
	}
	held, err := focusedFromStates(readStates(ctx, conn, r), ref)
	if err != nil || held {
		return held, err
	}
	// The bit is not on this node, which is not the same as the keyboard being elsewhere: Chromium and Electron publish it on a node under the one the walk listed, so a text input reads as unfocused while the keys are going straight into it. The subtree is walked before the answer is no.
	budget := maxNodes
	_, found := findFocused(r, 0, &budget, busFocus(ctx, conn))
	return found, nil
}

// FocusedElement reports which element of the window in front holds the keyboard. Input: a context. Output: that element's ref, role and name as an act.Node, and false when the accessibility bus is unreachable, no window is in front, or nothing in that window's tree carries STATE_FOCUSED.
// It exists because reading the bit on one remembered element cannot say where the keyboard went when the answer is no: a click made at a point on the screen leaves no element to read at all, and a page that moved the focus into its own search box leaves the remembered field reading unfocused with nothing to name in its place. A caller about to type asks the window itself who holds the keys.
func FocusedElement(ctx context.Context) (act.Node, bool) {
	w := focus()
	if w == nil {
		return act.Node{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
	win, _, ok := w.state.get()
	if !ok {
		return act.Node{}, false
	}
	budget := maxNodes
	hit, ok := findFocused(win, 0, &budget, busFocus(ctx, w.conn))
	if !ok {
		return act.Node{}, false
	}
	role := getRoleName(ctx, w.conn, hit)
	if kid, kidRole, ok := editableChild(ctx, w.conn, hit, role); ok {
		hit, role = kid, kidRole
	}
	return act.Node{Role: role, Label: getName(ctx, w.conn, hit), Ref: refString(hit)}, true
}

// busFocus is the read findFocused makes over the accessibility bus. Input: a context bounding the calls and a live connection. Output: a function giving one element's focused bit and, when the bit is not set, its children — a focused element's children are never walked, so they are never read.
func busFocus(ctx context.Context, conn *dbus.Conn) func(aref) (bool, []aref) {
	return func(r aref) (bool, []aref) {
		if stateSet(readStates(ctx, conn, r), stateFocused) {
			return true, nil
		}
		kids, _ := getChildren(ctx, conn, r)
		return false, kids
	}
}

// findFocused walks a tree depth first for the element that holds the keyboard. Input: the element to start at, how deep this call already is, how many more elements the whole walk may read, and a read giving one element's focused bit and children. Output: the element carrying STATE_FOCUSED and true, or false when nothing at or under the start carries it and when the depth bound or the budget ran out first.
func findFocused(ref aref, depth int, budget *int, read func(aref) (bool, []aref)) (aref, bool) {
	if depth > maxDepth || *budget <= 0 {
		return aref{}, false
	}
	*budget--
	focused, kids := read(ref)
	if focused {
		return ref, true
	}
	for _, kid := range kids {
		if hit, ok := findFocused(kid, depth+1, budget, read); ok {
			return hit, true
		}
	}
	return aref{}, false
}

// editableChild resolves a combo box that holds the keyboard to the box inside it the keys actually reach. Input: a context, the connection, the element carrying the focused bit and the role it publishes. Output: that child and its role, and false when the element is not a combo box or holds no child of a role text goes into.
// A combo box that can be typed into — a browser's address bar, a form's country picker with a filter — is a text box in a frame, and it is the child's role, not the frame's, that says the keys have somewhere to land.
func editableChild(ctx context.Context, conn *dbus.Conn, ref aref, role string) (aref, string, bool) {
	if role != "combo box" {
		return aref{}, "", false
	}
	kids, _ := getChildren(ctx, conn, ref)
	for _, kid := range kids {
		if r := getRoleName(ctx, conn, kid); typableRoles[r] {
			return kid, r, true
		}
	}
	return aref{}, "", false
}

// focusedFromStates turns one GetState answer into the focus verdict. Input: the packed state words, which readStates returns as nil for every failure, and the ref they were read for. Output: whether STATE_FOCUSED is set, or an error when there are no words to read it from.
func focusedFromStates(states []uint32, ref string) (bool, error) {
	if len(states) == 0 {
		return false, fmt.Errorf("the accessibility bus gave no state for %s: the element may have gone, the read may have timed out, or its toolkit may not publish the focused bit", ref)
	}
	return stateSet(states, stateFocused), nil
}

// scrollAnywhere is ATSPI_SCROLL_ANYWHERE: bring the node into view wherever is cheapest.
const scrollAnywhere uint32 = 6

// ScrollTo scrolls a node into view. Input: a context and the node's Ref. Output: an error when the ref is malformed, the node has gone, or the toolkit could not scroll.
func ScrollTo(ctx context.Context, ref string) error {
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
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
