//go:build linux

package tracker

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ora/internal/util"

	"github.com/godbus/dbus/v5"
)

// aref is an AT-SPI accessible reference: the bus name + object path tuple.
type aref struct {
	Name string
	Path dbus.ObjectPath
}

// Walk bounds — deep enough to reach text in IDEs/terminals/rich native apps, but bounded so a pathological tree can't hang capture. captureTimeout is the hard ceiling regardless.
const (
	// maxDepth is how far down an accessibility tree the walk goes. A web app nests deeply: on 2026-09-03 the Teams window in Chrome had its document at depth 7 and its text, the people list included, down to depth 26, and at 14 the read returned eleven characters. The node cap and the deadline are what bound the cost, so this only needs to clear the deepest page seen.
	maxDepth   = 40
	maxNodes   = 4000
	maxTextLen = 100000
)

// enableATSPI makes GTK3/Qt/VTE apps build and expose their accessibility trees, by setting org.a11y.Status.IsEnabled only.
// Does NOT touch ScreenReaderEnabled — that flag turns on Orca and narrates keystrokes aloud, we just want the trees. Best-effort, failures ignored.
// Chromium still needs --force-renderer-accessibility at launch, so in-page browser content isn't covered here (vision tier handles that).
func enableATSPI() {
	sess, err := dbus.SessionBus()
	if err != nil {
		return
	}
	// Bounded because this runs on tracker.New(), on the daemon's startup path: an org.a11y.Bus that accepted the message and never replied held a plain Call for good, and the daemon would never finish starting.
	ctx, cancel := context.WithTimeout(context.Background(), dbusProbeTimeout)
	defer cancel()
	obj := sess.Object("org.a11y.Bus", "/org/a11y/bus")
	obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Set", 0, //nolint:errcheck — best-effort switch, failures are expected on a desktop without accessibility
		"org.a11y.Status", "IsEnabled", dbus.MakeVariant(true))
}

// busDialer opens a connection to the accessibility bus. Input: a context bounding the dial. Output: a live connection the caller must close, or an error when the bus is absent or unreachable.
type busDialer func(ctx context.Context) (*dbus.Conn, error)

// dialBus is the dialler every reader in this file goes through, or nil for the real one. It is swappable so a test can stand in a bus that accepts the connection and then never answers, which is the failure the deadlines below exist for. It is atomic rather than a plain variable because a read abandoned at its deadline is still inside this function when a test puts the real dialler back.
var dialBus atomic.Pointer[busDialer]

// dialTheBus dials the accessibility bus through whichever dialler is installed. Input: a context bounding the dial. Output: a live connection the caller must close, or an error.
func dialTheBus(ctx context.Context) (*dbus.Conn, error) {
	if d := dialBus.Load(); d != nil {
		return (*d)(ctx)
	}
	return dialA11y(ctx)
}

// dialA11y opens a connection to the accessibility bus, whose address it asks the session bus for. Input: a context bounding both calls. Output: a live, authenticated connection the caller must close, or an error when the bus is absent or unreachable.
func dialA11y(ctx context.Context) (*dbus.Conn, error) {
	sess, err := dbus.SessionBus()
	if err != nil {
		return nil, err
	}
	var addr string
	if err := sess.Object("org.a11y.Bus", "/org/a11y/bus").CallWithContext(ctx, "org.a11y.Bus.GetAddress", 0).Store(&addr); err != nil {
		return nil, err
	}
	if addr == "" {
		return nil, dbus.ErrClosed
	}
	conn, err := dbus.Dial(addr)
	if err != nil {
		return nil, err
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// pidCache remembers the pid behind an accessibility-bus unique name, since a connection's unique name never changes owner for as long as that connection lives, so one lookup per name is all GetConnectionUnixProcessID is ever worth.
var pidCache sync.Map

// busPid asks the accessibility bus which process owns a connection's unique name, such as ":1.61" — the same identity a node's ref carries as its bus name. Input: a context bounding the call, the live connection and the unique name. Output: the pid and true, cached after the first successful read; 0 and false when the bus cannot answer.
func busPid(ctx context.Context, conn *dbus.Conn, name string) (uint32, bool) {
	if v, ok := pidCache.Load(name); ok {
		return v.(uint32), true
	}
	var pid uint32
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetConnectionUnixProcessID", 0, name).Store(&pid); err != nil {
		return 0, false
	}
	pidCache.Store(name, pid)
	return pid, true
}

// getParent reads the accessible's Parent property, which for a top-level window is its application. Output: the zero aref when the property cannot be read.
func getParent(ctx context.Context, conn *dbus.Conn, ref aref) aref {
	obj := conn.Object(ref.Name, ref.Path)
	var v dbus.Variant
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
		"org.a11y.atspi.Accessible", "Parent").Store(&v); err != nil {
		return aref{}
	}
	var p struct {
		Name string
		Path dbus.ObjectPath
	}
	if err := dbus.Store([]interface{}{v.Value()}, &p); err != nil {
		return aref{}
	}
	return aref{Name: p.Name, Path: p.Path}
}

// walkWindow walks one window's accessibility subtree and says, at debug, when a bound rather than the end of the tree is what stopped it. Input: a context carrying the walk's budget, a live bus connection and the window accessible. Output: the tree that was built, partial when a bound was hit.
// It is logged because a truncated walk returns short text and is indistinguishable, from the outside, from a window that simply had little in it. Measured on this desktop on 2026-09-05: the focused window's walk costs 0.5-80 ms over 2-400 nodes and reaches depth 22 at most, well inside every bound; the one walk that comes near them is gnome-shell's own tree, at 3,836-3,933 nodes against the 4,000 cap and 0.87-1.94 s against the 2.5 s budget, and only the pre-focus-signal fallback ever walks it.
func walkWindow(ctx context.Context, conn *dbus.Conn, win aref) a11yNode {
	visited := 0
	started := time.Now()
	tree := buildA11yTree(ctx, conn, win, 0, &visited)
	if visited >= maxNodes || ctx.Err() != nil {
		slog.Debug("the accessibility walk stopped on a bound, not on the end of the tree",
			"nodes", visited, "node_cap", maxNodes, "elapsed", time.Since(started), "deadline_passed", ctx.Err() != nil)
	}
	return tree
}

// trimText strips surrounding whitespace from captured text and caps it at maxTextLen runes. Input: the text a walk produced. Output: the trimmed, capped text.
// The cap is in runes rather than bytes because a byte cut lands inside a rune on any screen that is not pure ASCII, and the invalid UTF-8 that produced went into episodes.screen_text and its FTS5 index.
func trimText(s string) string {
	return strings.TrimSpace(util.Runes(s, maxTextLen))
}

// getName reads the org.a11y.atspi.Accessible "Name" property of an accessible, returning "" when it cannot be read.
func getName(ctx context.Context, conn *dbus.Conn, ref aref) string {
	name, _ := readName(ctx, conn, ref)
	return name
}

// readName is getName with the error kept, for callers that need to tell "this window is called nothing" apart from "this window is gone".
func readName(ctx context.Context, conn *dbus.Conn, ref aref) (string, error) {
	obj := conn.Object(ref.Name, ref.Path)
	var v dbus.Variant
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
		"org.a11y.atspi.Accessible", "Name").Store(&v); err != nil {
		return "", err
	}
	s, _ := v.Value().(string)
	return strings.TrimSpace(s), nil
}

// stateActive is the AtspiStateType bit index for STATE_ACTIVE, which a top-level window keeps while the window manager treats it as the active one.
const stateActive uint = 1

// stateFocused is the AtspiStateType bit index for STATE_FOCUSED, which says the object holds the keyboard focus right now. Only one window on the desktop should carry it, which is what makes it worth reading next to STATE_ACTIVE — that bit is left set on windows that have lost the focus.
const stateFocused uint = 12

// readStates reads an accessible's packed state words. Input: a context bounding the call, a live connection and the accessible. Output: the two uint32 words, or nil when the accessible cannot be read.
func readStates(ctx context.Context, conn *dbus.Conn, ref aref) []uint32 {
	obj := conn.Object(ref.Name, ref.Path)
	var states []uint32
	if err := obj.CallWithContext(ctx, "org.a11y.atspi.Accessible.GetState", 0).Store(&states); err != nil {
		return nil
	}
	return states
}

// stateSet reports whether a state bit is set in packed state words. Input: the words and the AtspiStateType bit index. Output: whether the bit is set; state bits are packed into two uint32 words, so bit n lives at word[n/32], bit (n%32).
func stateSet(states []uint32, bit uint) bool {
	word := bit / 32
	if int(word) >= len(states) {
		return false
	}
	return states[word]&(1<<(bit%32)) != 0
}

// hasState checks whether the accessible has a given AtspiStateType bit set.
func hasState(ctx context.Context, conn *dbus.Conn, ref aref, bit uint) bool {
	return stateSet(readStates(ctx, conn, ref), bit)
}

// activeAndFocused reads one window's STATE_ACTIVE and STATE_FOCUSED bits together. Input: a context bounding the call, a live connection and the window. Output: the two bits, both false when the window cannot be read. Reading them from one GetState keeps the second opinion free: it is the same bus call the active check already made.
func activeAndFocused(ctx context.Context, conn *dbus.Conn, ref aref) (active, focused bool) {
	states := readStates(ctx, conn, ref)
	return stateSet(states, stateActive), stateSet(states, stateFocused)
}

// getChildren calls GetChildren on an accessible and returns the child refs.
func getChildren(ctx context.Context, conn *dbus.Conn, ref aref) ([]aref, error) {
	obj := conn.Object(ref.Name, ref.Path)
	var raw []struct {
		Name string
		Path dbus.ObjectPath
	}
	if err := obj.CallWithContext(ctx, "org.a11y.atspi.Accessible.GetChildren", 0).Store(&raw); err != nil {
		return nil, err
	}
	refs := make([]aref, len(raw))
	for i, r := range raw {
		refs[i] = aref{Name: r.Name, Path: r.Path}
	}
	return refs, nil
}

// getInterfaces returns the D-Bus interface names the accessible implements.
func getInterfaces(ctx context.Context, conn *dbus.Conn, ref aref) []string {
	obj := conn.Object(ref.Name, ref.Path)
	var ifaces []string
	obj.CallWithContext(ctx, "org.a11y.atspi.Accessible.GetInterfaces", 0).Store(&ifaces) //nolint:errcheck
	return ifaces
}

// getText calls GetText(0, -1) on an accessible that implements org.a11y.atspi.Text.
func getText(ctx context.Context, conn *dbus.Conn, ref aref) string {
	obj := conn.Object(ref.Name, ref.Path)
	var text string
	obj.CallWithContext(ctx, "org.a11y.atspi.Text.GetText", 0, int32(0), int32(-1)).Store(&text) //nolint:errcheck
	return text
}

// getRoleName reads the canonical AT-SPI role name of an accessible — e.g. "document web", "tool bar", "push button". Using the string form (vs. GetRole's uint32) avoids needing a numeric constant table. Best-effort: any error yields "".
func getRoleName(ctx context.Context, conn *dbus.Conn, ref aref) string {
	obj := conn.Object(ref.Name, ref.Path)
	var role string
	obj.CallWithContext(ctx, "org.a11y.atspi.Accessible.GetRoleName", 0).Store(&role) //nolint:errcheck
	return role
}

// buildA11yTree walks the accessible subtree from ref into an in-memory a11yNode tree (role, own text if it implements org.a11y.atspi.Text, and children), so documentText (document_text.go) can decide what to keep.
// Bounded by maxDepth, maxNodes (via the visited counter), and the context deadline.
func buildA11yTree(ctx context.Context, conn *dbus.Conn, ref aref, depth int, visited *int) a11yNode {
	if depth > maxDepth || *visited >= maxNodes || ctx.Err() != nil {
		return a11yNode{}
	}
	*visited++

	node := a11yNode{Role: getRoleName(ctx, conn, ref)}

	ifaces := getInterfaces(ctx, conn, ref)
	for _, iface := range ifaces {
		if iface == "org.a11y.atspi.Text" {
			node.Text = strings.TrimSpace(getText(ctx, conn, ref))
			break
		}
	}

	children, err := getChildren(ctx, conn, ref)
	if err != nil {
		return node
	}
	for _, child := range children {
		if *visited >= maxNodes || ctx.Err() != nil {
			break
		}
		node.Children = append(node.Children, buildA11yTree(ctx, conn, child, depth+1, visited))
	}
	return node
}
