//go:build linux

package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"ora/internal/act"
)

// stateShowing is the AtspiStateType bit for STATE_SHOWING: the node and every ancestor are visible, so it is on screen or scrolled just off it.
const stateShowing uint = 25

// observeTimeout bounds one read of a window; a page that has not finished in this time is answered with what was read so far.
const observeTimeout = 4 * time.Second

// Observe reads the window that has focus (the last one before Ora took it, when Ora has it) into the nodes a model can act on. Input: a context. Output: the app's name, the window's title, one act.Node per node with an actionable role (see act.Actionable) in tree order, and an error when the accessibility bus is unreachable, the focused window has gone, or nothing on the desktop publishes an actionable node. When no window has taken focus since the daemon started it walks the desktop instead (see observeDesktop).
func Observe(ctx context.Context) (app, title string, nodes []act.Node, err error) {
	w := focus()
	if w == nil {
		return "", "", nil, errors.New("the accessibility bus is not reachable")
	}
	ctx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()
	ref, app, ok := w.state.get()
	if !ok || IsOraWindow(app, getName(ctx, w.conn, ref)) {
		// The watcher only learns focus from activation events after the daemon starts, so a fresh daemon, or one that has only seen Ora's own hover come and go, has nothing to walk. Fall back to the desktop tree and take the window with the most actionable nodes.
		return observeDesktop(ctx, w.conn)
	}
	title, err = readName(ctx, w.conn, ref)
	if err != nil {
		w.state.clear(ref)
		return "", "", nil, fmt.Errorf("the focused window is gone: %w", err)
	}
	visited := 0
	nodes = observeWalk(ctx, w.conn, ref, 0, &visited, nil)
	correctListing(ctx, w.conn, ref, nodes)
	return app, title, nodes, nil
}

// correctListing moves a whole listing into real screen pixels, once for the window it was read out of. Input: a context, the bus connection, the ref the walk started from, and the nodes it produced. Output: none; the nodes are moved in place, and are left alone when the desktop or the window cannot be read.
// This is the cheap half of the correction Extents makes for a single node. Doing it per node would mean a walk up to the window and a read of the desktop's work area for each of up to 150 of them, which would cost more than the walk's own budget; the shift is the same for every node in a window, so it is worked out once and added to all of them.
func correctListing(ctx context.Context, conn *dbus.Conn, ref aref, nodes []act.Node) {
	if len(nodes) == 0 {
		return
	}
	work, screen, ok := desktopBounds()
	if !ok {
		return
	}
	frame, ok := windowOf(ctx, conn, ref)
	if !ok {
		return
	}
	toScreen(nodes, frame, work, screen)
}

// toScreen moves every node of one window's listing by however far that window's rectangles are from the truth. Input: the nodes as the walk read them, the rectangle the window reported for itself, and the desktop's work area and whole screen. Output: none; the nodes are moved in place, by the shift windowShift works out, and a node with no rectangle is left at zero because there is nothing there to move.
func toScreen(nodes []act.Node, frame, work, screen rect) {
	dx, dy := windowShift(frame, work, screen, monitorRects())
	if dx == 0 && dy == 0 {
		return
	}
	for i := range nodes {
		if nodes[i].W <= 0 || nodes[i].H <= 0 {
			continue
		}
		nodes[i].X += dx
		nodes[i].Y += dy
	}
}

// observeWalk appends the actionable nodes under ref to out, depth first, bounded by maxDepth, maxNodes and the context. Only nodes with an actionable role pay for the name, text, extents and state reads; the rest cost one role call and a children call.
// The rectangles it records are as the window reported them, which is not always where the window is on the screen; correctListing puts the whole listing right once the walk is done.
func observeWalk(ctx context.Context, conn *dbus.Conn, ref aref, depth int, visited *int, out []act.Node) []act.Node {
	if depth > maxDepth || *visited >= maxNodes || ctx.Err() != nil {
		return out
	}
	*visited++
	role := getRoleName(ctx, conn, ref)
	if act.Actionable(role) {
		n := act.Node{Role: role, Label: getName(ctx, conn, ref), Ref: refString(ref)}
		if n.Label == "" {
			n.Label = strings.TrimSpace(getText(ctx, conn, ref))
		}
		if r, err := readExtents(ctx, conn, ref, coordScreen); err == nil {
			n.X, n.Y, n.W, n.H = r.X, r.Y, r.W, r.H
		}
		n.Showing = hasState(ctx, conn, ref, stateShowing)
		out = append(out, n)
	}
	children, err := getChildren(ctx, conn, ref)
	if err != nil {
		return out
	}
	for _, child := range children {
		if *visited >= maxNodes || ctx.Err() != nil {
			break
		}
		out = observeWalk(ctx, conn, child, depth+1, visited, out)
	}
	return out
}

// getExtents reads one node's rectangle in real screen pixels, through org.a11y.atspi.Component and then screenShift, which corrects a window that answers a request for screen coordinates with its own window coordinates. Output: x, y, width, height in pixels, all zero when the node has no Component interface, the call fails, or the node is not on the screen.
// This is the single-node path, the one Verify uses, and it pays a walk up to the window and a read of the desktop's work area on top of the node's own read. It has to correct, not skip: Verify compares what it reads here against the rectangle the listing showed, and the listing is in screen pixels, so an uncorrected read would differ by the height of the top bar and Verify would refuse every click on a maximized window as having moved. An observe pass uses correctListing instead, which is the same correction worked out once for a whole listing.
func getExtents(ctx context.Context, conn *dbus.Conn, ref aref) (x, y, w, h int) {
	got, err := readExtents(ctx, conn, ref, coordScreen)
	if err != nil || got.W <= 0 || got.H <= 0 {
		return 0, 0, 0, 0
	}
	dx, dy := screenShift(ctx, conn, ref)
	return got.X + dx, got.Y + dy, got.W, got.H
}

// walkWithOwnBudget runs one window's walk against a fresh maxNodes budget of its own, rather than one a caller shares across several windows, and logs — like walkWindow — when the bound rather than the end of the tree is what stopped it. Input: a context and the walk, which takes the visited counter it must count into. Output: whatever the walk produced.
func walkWithOwnBudget(ctx context.Context, walk func(visited *int) []act.Node) []act.Node {
	visited := 0
	nodes := walk(&visited)
	if visited >= maxNodes || ctx.Err() != nil {
		slog.Debug("a window's walk in the desktop-wide scan stopped on a bound, not the end of its tree",
			"nodes", visited, "node_cap", maxNodes, "deadline_passed", ctx.Err() != nil)
	}
	return nodes
}

// registryRoot is the accessible under which every application on the bus hangs.
var registryRoot = aref{Name: "org.a11y.atspi.Registry", Path: "/org/a11y/atspi/accessible/root"}

// observeDesktop walks every window of every application on the bus except the shell and Ora's own, and returns the one with the most actionable nodes, which on this desktop is the browser or editor the user is in. Output: like Observe; an error when nothing publishes an actionable node.
// ponytail: "most actionable nodes" is a stand-in for "in front", good enough for the seconds after a daemon start; replace with a real active-window read if the desktop ever exposes one.
func observeDesktop(ctx context.Context, conn *dbus.Conn) (app, title string, nodes []act.Node, err error) {
	apps, err := getChildren(ctx, conn, registryRoot)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot list the applications on the bus: %w", err)
	}
	bestKept := 0
	var bestWin aref
	for _, a := range apps {
		name := getName(ctx, conn, a)
		if skipWindow(name, "") {
			continue
		}
		windows, err := getChildren(ctx, conn, a)
		if err != nil {
			continue
		}
		for _, win := range windows {
			// Each window walked here gets a budget of its own, not one shared across the whole desktop: a first window big enough to spend the whole cap (gnome-shell's own tree came close on this desktop, see walkWindow) used to leave kept == 0 for every window walked after it, so it could never be chosen no matter how little it had worth keeping.
			walked := walkWithOwnBudget(ctx, func(visited *int) []act.Node {
				return observeWalk(ctx, conn, win, 0, visited, nil)
			})
			kept := keptCount(walked)
			if kept <= bestKept {
				continue
			}
			// The title is read only for a window that would win, because it costs a round trip and because it is the only thing that identifies Ora's own window when the compositor owns the frame.
			winTitle := getName(ctx, conn, win)
			if skipWindow(name, winTitle) {
				continue
			}
			bestKept, app, title, nodes, bestWin = kept, name, winTitle, walked, win
		}
	}
	if bestKept == 0 {
		return "", "", nil, errors.New("no window has taken focus yet, and nothing on the desktop publishes anything to act on")
	}
	// Only the window that won is corrected, since the listings of the ones that lost are thrown away.
	correctListing(ctx, conn, bestWin, nodes)
	return app, title, nodes, nil
}

// skipWindow reports whether observeDesktop should leave a window out of the running: the shell's own windows, and Ora's, which must never be what Ora describes back to the user.
// The title has to be part of the decision. On this desktop Ora's window reaches the accessibility bus twice over: once as the application "ora", and once as "mutter-x11-frames" — the compositor's frame process — with "Ora" as the title. The application name alone identifies only the first of those. Pass "" for the title to make the cheap application-level check before any window is read.
func skipWindow(app, title string) bool {
	return app == "gnome-shell" || IsOraWindow(app, title)
}

// typableRoles mirrors the roles act.Filter keeps without a label, because an empty box to type in is still a target. Kept here because act does not export it; keptCount's test holds the two rules together.
var typableRoles = map[string]bool{"entry": true, "text": true, "password text": true}

// keptCount is how many of a walked window's nodes act.Filter would list, without act.Filter's cap on the length of the list. Input: the nodes of one window. Output: the count.
// observeDesktop scores windows against each other with this rather than with len(act.Filter), because act.Filter stops at act.MaxItems: every window with that many actionable nodes or more scores the same, so a browser showing nine hundred of them ties with a chat window showing a hundred and fifty and the choice falls to whichever application the bus happened to list first.
func keptCount(nodes []act.Node) int {
	n := 0
	for _, node := range nodes {
		if !node.Showing || !act.Actionable(node.Role) || node.W <= 0 || node.H <= 0 {
			continue
		}
		if node.Label == "" && !typableRoles[node.Role] {
			continue
		}
		n++
	}
	return n
}
