//go:build linux

package tracker

import (
	"context"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// extractText returns the text content of the focused window via AT-SPI over D-Bus. Returns ("", nil) on any failure (no bus, no focused window, etc) — callers never see a non-nil error here.
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

// Walk bounds — deep enough to reach text in IDEs/terminals/rich native apps, but bounded so a pathological tree can't hang capture. captureTimeout is the hard ceiling regardless.
const (
	captureTimeout = 2500 * time.Millisecond
	maxDepth       = 14
	maxNodes       = 4000
	maxTextLen     = 100000
)

// enableATSPI makes GTK3/Qt/VTE apps build and expose their accessibility trees, by setting org.a11y.Status.IsEnabled only.
// Does NOT touch ScreenReaderEnabled — that flag turns on Orca and narrates keystrokes aloud, we just want the trees. Best-effort, failures ignored.
// Chromium still needs --force-renderer-accessibility at launch, so in-page browser content isn't covered here (vision tier handles that).
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

// atspiExtract does the real work so we can return errors internally without leaking them to the caller.
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
			// found the focused window — walk its subtree, then apply documentText's role-aware rule: browser chrome lives outside any DOCUMENT_WEB node, so keep only that when present; native apps have none and fall back to the full tree.
			visited := 0
			tree := buildA11yTree(ctx, conn, win, 0, &visited)
			if t := strings.TrimSpace(documentText(tree)); t != "" {
				if _, dup := seen[t]; !dup {
					seen[t] = struct{}{}
					parts = append(parts, t)
				}
			}
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

// extractMeetingWindow finds a call in progress anywhere on the desktop and reads it, whether or not it has focus, returning its app, its window title and its text. ok is false when no meeting window is open.
// This exists because the focused window is the wrong window during a meeting. On 2026-08-31 a thirty-nine minute standup produced twenty-seven episodes and not one of them was the call: the user spent it in ClickUp and a terminal, so the participant tiles, the "X is presenting" label and the meeting chat — the only things on the machine that name who is speaking — were never captured at all.
// It walks the same registry tree as atspiExtract and differs in one line: the window is chosen by name rather than by being active.
func extractMeetingWindow() (app, title, text string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	sess, err := dbus.SessionBus()
	if err != nil {
		return "", "", "", false
	}
	regObj := sess.Object("org.a11y.Bus", "/org/a11y/bus")
	var addr string
	if err := regObj.CallWithContext(ctx, "org.a11y.Bus.GetAddress", 0).Store(&addr); err != nil || addr == "" {
		return "", "", "", false
	}
	conn, err := dbus.Dial(addr)
	if err != nil {
		return "", "", "", false
	}
	defer conn.Close()
	if err := conn.Auth(nil); err != nil {
		return "", "", "", false
	}
	if err := conn.Hello(); err != nil {
		return "", "", "", false
	}

	root := aref{Name: "org.a11y.atspi.Registry", Path: "/org/a11y/atspi/accessible/root"}
	apps, err := getChildren(ctx, conn, root)
	if err != nil {
		return "", "", "", false
	}

	for _, a := range apps {
		if ctx.Err() != nil {
			break
		}
		wins, err := getChildren(ctx, conn, a)
		if err != nil {
			continue
		}
		appName := getName(ctx, conn, a)
		for _, win := range wins {
			if ctx.Err() != nil {
				break
			}
			winName := getName(ctx, conn, win)
			if !IsMeetingWindow(appName, winName) {
				continue
			}
			visited := 0
			tree := buildA11yTree(ctx, conn, win, 0, &visited)
			t := strings.TrimSpace(documentText(tree))
			if len(t) > maxTextLen {
				t = t[:maxTextLen]
			}
			return appName, winName, t, true
		}
	}
	return "", "", "", false
}

// atspiActiveWindow returns (app, title) of the focused window via AT-SPI, walking the same registry tree as atspiExtract but reading Names instead of text.
// Returns ("", "") on any failure — the caller maps that to Unknown.
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

// WindowTitleFor returns the title of a window belonging to the named application, or empty when the desktop does not report one.
//
// Input: the application's process name, such as "brave" or "chrome". Output: the title of one of its windows, preferring the longest, or empty when nothing matches.
//
// This asks the desktop rather than Ora's own history, because history lags: a call is joined seconds before the tracker next records a window. It is best-effort and often returns nothing — an application publishes an accessibility tree only if it was built or launched to, and Brave installed as a snap frequently publishes none at all even when launched with --force-renderer-accessibility. The caller falls back to history, which is why this failing is not a failure.
// X11 would list every window regardless, but this desktop is Wayland and XWayland reports an empty _NET_CLIENT_LIST, so there is nothing to read there.
// The longest title is preferred because a browser's several windows include short utility ones and the call is the window that names itself fully. Nothing here knows which applications host meetings; it answers only "what is this program showing".
func WindowTitleFor(ctx context.Context, app string) string {
	if app == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()

	sess, err := dbus.SessionBus()
	if err != nil {
		return ""
	}
	var addr string
	if err := sess.Object("org.a11y.Bus", "/org/a11y/bus").CallWithContext(ctx, "org.a11y.Bus.GetAddress", 0).Store(&addr); err != nil || addr == "" {
		return ""
	}
	conn, err := dbus.Dial(addr)
	if err != nil {
		return ""
	}
	defer conn.Close()
	if conn.Auth(nil) != nil || conn.Hello() != nil {
		return ""
	}

	root := aref{Name: "org.a11y.atspi.Registry", Path: "/org/a11y/atspi/accessible/root"}
	apps, err := getChildren(ctx, conn, root)
	if err != nil {
		return ""
	}
	want := strings.ToLower(app)
	best := ""
	for _, a := range apps {
		if ctx.Err() != nil {
			break
		}
		name := strings.ToLower(getName(ctx, conn, a))
		if name == "" || (!strings.Contains(name, want) && !strings.Contains(want, name)) {
			continue
		}
		wins, err := getChildren(ctx, conn, a)
		if err != nil {
			continue
		}
		for _, w := range wins {
			if t := strings.TrimSpace(getName(ctx, conn, w)); len(t) > len(best) {
				best = t
			}
		}
	}
	return best
}
