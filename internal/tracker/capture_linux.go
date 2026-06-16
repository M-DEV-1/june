//go:build linux

package tracker

import (
	"context"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// captureScreen is a no-op on Linux — AT-SPI reads live accessible text, no pixel capture needed.
func captureScreen() ([]byte, error) {
	return nil, nil
}

// extractText returns the text content of the focused window via AT-SPI over D-Bus.
// Returns ("", nil) when the accessibility bus is unavailable, no window is focused,
// or any other error occurs — callers must never see a non-nil error from this function.
//
// Per-app requirements:
//   - GTK3: set toolkit-accessibility=true in ~/.config/gtk-3.0/settings.ini, or use
//     GNOME Settings > Accessibility > Enable, or pass per-app env AT_SPI_BUS_ADDRESS.
//   - GTK4: accessibility is on by default.
//   - Qt: set QT_ACCESSIBILITY=1 in the environment.
//   - Electron: launch with --force-renderer-accessibility.
func extractText() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	text, _ := atspiExtract(ctx)
	return text, nil
}

// Walk bounds. Deep enough to reach text inside IDEs, terminals, and rich
// native apps (their a11y trees nest far below the old depth-6 cap) while still
// bounded so a pathological tree can't hang capture. captureTimeout is the hard
// ceiling regardless of these.
const (
	captureTimeout = 2500 * time.Millisecond
	maxDepth       = 14
	maxNodes       = 4000
	maxTextLen     = 100000
)

// enableATSPI makes GTK3/Qt/VTE apps build and expose their accessibility trees
// by setting only org.a11y.Status.IsEnabled. It deliberately does NOT touch
// ScreenReaderEnabled — that flag activates a talking screen reader (Orca), which
// narrates keystrokes aloud. We want to read trees silently, not announce them.
// Best-effort: any failure is ignored. Chromium browsers still need
// --force-renderer-accessibility at launch, so this does not cover in-page
// browser content (the vision tier handles those).
func enableATSPI() {
	sess, err := dbus.SessionBus()
	if err != nil {
		return
	}
	obj := sess.Object("org.a11y.Bus", "/org/a11y/bus")
	obj.Call("org.freedesktop.DBus.Properties.Set", 0,
		"org.a11y.Status", "IsEnabled", dbus.MakeVariant(true))
}

// aref is an AT-SPI accessible reference: the bus name + object path tuple.
type aref struct {
	Name string
	Path dbus.ObjectPath
}

// atspiExtract does the real work so we can return errors internally without
// leaking them to the caller.
func atspiExtract(ctx context.Context) (string, error) {
	// Step 1: get the a11y bus address from the session bus.
	sess, err := dbus.SessionBus()
	if err != nil {
		return "", err
	}

	regObj := sess.Object("org.a11y.Bus", "/org/a11y/bus")
	var addr string
	if err := regObj.CallWithContext(ctx, "org.a11y.Bus.GetAddress", 0).Store(&addr); err != nil {
		return "", err
	}
	if addr == "" {
		return "", nil
	}

	// Step 2: dial the dedicated AT-SPI bus (separate from the session bus).
	conn, err := dbus.Dial(addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.Auth(nil); err != nil {
		return "", err
	}
	if err := conn.Hello(); err != nil {
		return "", err
	}

	// Step 3: enumerate application-level accessibles from the registry root.
	root := aref{Name: "org.a11y.atspi.Registry", Path: "/org/a11y/atspi/accessible/root"}
	apps, err := getChildren(ctx, conn, root)
	if err != nil {
		return "", err
	}

	var parts []string
	seen := make(map[string]struct{})

	for _, app := range apps {
		if ctx.Err() != nil {
			break
		}
		wins, err := getChildren(ctx, conn, app)
		if err != nil {
			continue
		}
		for _, win := range wins {
			if ctx.Err() != nil {
				break
			}
			if !hasState(ctx, conn, win, stateActive) {
				continue
			}
			// Found the focused top-level window — walk its subtree.
			visited := 0
			dfsText(ctx, conn, win, 0, visited, seen, &parts)
			break
		}
	}

	if len(parts) == 0 {
		return "", nil
	}

	result := strings.Join(parts, "\n")
	if len(result) > maxTextLen {
		result = result[:maxTextLen]
	}
	return strings.TrimSpace(result), nil
}

// atspiActiveWindow returns (app, title) of the focused top-level window via
// AT-SPI. It walks the same registry tree as atspiExtract but reads accessible
// Names instead of text. Returns ("", "") on any failure — the caller maps that
// to Unknown.
func atspiActiveWindow(ctx context.Context) (string, string) {
	sess, err := dbus.SessionBus()
	if err != nil {
		return "", ""
	}

	regObj := sess.Object("org.a11y.Bus", "/org/a11y/bus")
	var addr string
	if err := regObj.CallWithContext(ctx, "org.a11y.Bus.GetAddress", 0).Store(&addr); err != nil || addr == "" {
		return "", ""
	}

	conn, err := dbus.Dial(addr)
	if err != nil {
		return "", ""
	}
	defer conn.Close()
	if err := conn.Auth(nil); err != nil {
		return "", ""
	}
	if err := conn.Hello(); err != nil {
		return "", ""
	}

	root := aref{Name: "org.a11y.atspi.Registry", Path: "/org/a11y/atspi/accessible/root"}
	apps, err := getChildren(ctx, conn, root)
	if err != nil {
		return "", ""
	}

	for _, app := range apps {
		if ctx.Err() != nil {
			break
		}
		wins, err := getChildren(ctx, conn, app)
		if err != nil {
			continue
		}
		for _, win := range wins {
			if ctx.Err() != nil {
				break
			}
			if !hasState(ctx, conn, win, stateActive) {
				continue
			}
			return getName(ctx, conn, app), getName(ctx, conn, win)
		}
	}
	return "", ""
}

// getName reads the org.a11y.atspi.Accessible "Name" property of an accessible.
func getName(ctx context.Context, conn *dbus.Conn, ref aref) string {
	obj := conn.Object(ref.Name, ref.Path)
	var v dbus.Variant
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
		"org.a11y.atspi.Accessible", "Name").Store(&v); err != nil {
		return ""
	}
	s, _ := v.Value().(string)
	return strings.TrimSpace(s)
}

// stateActive is the AtspiStateType bit index for STATE_ACTIVE (focused top-level window).
const stateActive uint = 1

// hasState checks whether the accessible has a given AtspiStateType bit set.
// State bits are packed into two uint32 words: bit n lives at word[n/32], bit (n%32).
func hasState(ctx context.Context, conn *dbus.Conn, ref aref, bit uint) bool {
	obj := conn.Object(ref.Name, ref.Path)
	var states []uint32
	if err := obj.CallWithContext(ctx, "org.a11y.atspi.Accessible.GetState", 0).Store(&states); err != nil {
		return false
	}
	word := bit / 32
	if int(word) >= len(states) {
		return false
	}
	return states[word]&(1<<(bit%32)) != 0
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

// dfsText walks the accessible subtree from ref, collecting text from every
// node that implements org.a11y.atspi.Text. Bounded by maxDepth, maxNodes, and
// the context deadline.
func dfsText(ctx context.Context, conn *dbus.Conn, ref aref, depth, visited int, seen map[string]struct{}, parts *[]string) int {
	if depth > maxDepth || visited >= maxNodes || ctx.Err() != nil {
		return visited
	}
	visited++

	ifaces := getInterfaces(ctx, conn, ref)
	for _, iface := range ifaces {
		if iface == "org.a11y.atspi.Text" {
			t := strings.TrimSpace(getText(ctx, conn, ref))
			if t != "" {
				if _, dup := seen[t]; !dup {
					seen[t] = struct{}{}
					*parts = append(*parts, t)
				}
			}
			break
		}
	}

	children, err := getChildren(ctx, conn, ref)
	if err != nil {
		return visited
	}
	for _, child := range children {
		visited = dfsText(ctx, conn, child, depth+1, visited, seen, parts)
		if visited >= maxNodes || ctx.Err() != nil {
			break
		}
	}
	return visited
}
