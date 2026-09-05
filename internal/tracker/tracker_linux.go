//go:build linux

package tracker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

type linuxTracker struct {
	getActive func() (*Activity, error)
	xconn     *xgb.Conn
}

// backend identifies the active-window detection strategy chosen for this session.
type backend int

const (
	backendNone backend = iota
	backendSway
	backendHypr
	backendATSPI
	backendX11
)

// selectBackend picks a strategy from session capabilities. AT-SPI takes priority over X11 on Wayland because XWayland's _NET_ACTIVE_WINDOW reports 0x0 for native Wayland windows, making X11 useless there. On native X11, X11 is preferred.
func selectBackend(haveSway, haveHypr, wayland, haveX11 bool) backend {
	switch {
	case haveSway:
		return backendSway
	case haveHypr:
		return backendHypr
	case wayland:
		return backendATSPI
	case haveX11:
		return backendX11
	default:
		return backendNone
	}
}

func New() (Tracker, error) {
	t := &linuxTracker{}

	// Coax GTK3/Qt/VTE apps into exposing their accessibility trees.
	enableATSPI()

	haveSway := false
	if sock := os.Getenv("SWAYSOCK"); sock != "" {
		if _, err := os.Stat(sock); err == nil {
			haveSway = true
		}
	}
	haveHypr := os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") != ""
	wayland := os.Getenv("XDG_SESSION_TYPE") == "wayland" || os.Getenv("WAYLAND_DISPLAY") != ""
	haveX11 := os.Getenv("XDG_SESSION_TYPE") == "x11" || os.Getenv("DISPLAY") != ""

	switch selectBackend(haveSway, haveHypr, wayland, haveX11) {
	case backendSway:
		t.getActive = t.swayWindow
		return t, nil
	case backendHypr:
		t.getActive = t.hyprWindow
		return t, nil
	case backendATSPI:
		t.getActive = t.atspiWindow
		return t, nil
	case backendX11:
		conn, err := xgb.NewConn()
		if err == nil {
			t.xconn = conn
			t.getActive = t.x11Window
			return t, nil
		}
		// X11 advertised but unreachable — fall through to generic
	}

	t.getActive = func() (*Activity, error) {
		return Normalize("Unknown", "Unknown"), nil
	}
	return t, nil
}

// atspiWindow reports the focused window via AT-SPI over D-Bus — the only pure-Go path that sees native Wayland windows on GNOME/KDE. Falls back to "Unknown" when the accessibility bus is unavailable.
func (t *linuxTracker) atspiWindow() (*Activity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()

	app, title := atspiActiveWindow(ctx)
	if app == "" && title == "" {
		return Normalize("Unknown", "Unknown"), nil
	}
	return Normalize(app, title), nil
}

func (t *linuxTracker) GetActiveWindow() (*Activity, error) {
	return t.getActive()
}

// x11Window reads _NET_ACTIVE_WINDOW from the root, then fetches title + class.
func (t *linuxTracker) x11Window() (*Activity, error) {
	unknown := Normalize("Unknown", "Unknown")
	conn := t.xconn
	setup := xproto.Setup(conn)
	root := setup.DefaultScreen(conn).Root

	activeAtom, err := internAtom(conn, "_NET_ACTIVE_WINDOW")
	if err != nil {
		return unknown, nil
	}

	prop, err := xproto.GetProperty(conn, false, root, activeAtom, xproto.AtomWindow, 0, 1).Reply()
	if err != nil || prop.ValueLen == 0 {
		return unknown, nil
	}
	activeWin := xproto.Window(binary.LittleEndian.Uint32(prop.Value))

	title := x11StringProp(conn, activeWin, "_NET_WM_NAME", "UTF8_STRING")
	if title == "" {
		title = x11StringProp(conn, activeWin, "WM_NAME", "STRING")
	}

	// WM_CLASS is two null-terminated strings: instance\0class\0
	classProp := x11RawProp(conn, activeWin, "WM_CLASS", "STRING")
	app := ""
	if len(classProp) > 0 {
		parts := splitNull(classProp)
		if len(parts) >= 2 {
			app = parts[1]
		} else if len(parts) == 1 {
			app = parts[0]
		}
	}

	return Normalize(app, title), nil
}

func internAtom(conn *xgb.Conn, name string) (xproto.Atom, error) {
	reply, err := xproto.InternAtom(conn, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0, err
	}
	return reply.Atom, nil
}

func x11StringProp(conn *xgb.Conn, win xproto.Window, atomName, typeName string) string {
	atom, err := internAtom(conn, atomName)
	if err != nil {
		return ""
	}
	typeAtom, err := internAtom(conn, typeName)
	if err != nil {
		return ""
	}
	prop, err := xproto.GetProperty(conn, false, win, atom, typeAtom, 0, (1<<32)-1).Reply()
	if err != nil || prop.ValueLen == 0 {
		return ""
	}
	return string(prop.Value)
}

func x11RawProp(conn *xgb.Conn, win xproto.Window, atomName, typeName string) []byte {
	atom, err := internAtom(conn, atomName)
	if err != nil {
		return nil
	}
	typeAtom, err := internAtom(conn, typeName)
	if err != nil {
		return nil
	}
	prop, err := xproto.GetProperty(conn, false, win, atom, typeAtom, 0, (1<<32)-1).Reply()
	if err != nil {
		return nil
	}
	return prop.Value
}

func splitNull(b []byte) []string {
	var parts []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				parts = append(parts, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		parts = append(parts, string(b[start:]))
	}
	return parts
}

// swayWindow sends GET_TREE over the sway IPC socket and finds the focused node.
func (t *linuxTracker) swayWindow() (*Activity, error) {
	sock := os.Getenv("SWAYSOCK")
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return Normalize("Unknown", "Unknown"), nil
	}
	defer conn.Close()

	// sway IPC framing: magic(6) + len(4 LE) + type(4 LE) + payload
	const magic = "i3-ipc"
	const msgGetTree = uint32(4)
	msg := make([]byte, 6+4+4)
	copy(msg[0:], magic)
	binary.LittleEndian.PutUint32(msg[6:], 0) // payload length = 0
	binary.LittleEndian.PutUint32(msg[10:], msgGetTree)

	if _, err := conn.Write(msg); err != nil {
		return Normalize("Unknown", "Unknown"), nil
	}

	header := make([]byte, 14)
	if _, err := io.ReadFull(conn, header); err != nil {
		return Normalize("Unknown", "Unknown"), nil
	}
	bodyLen := binary.LittleEndian.Uint32(header[6:10])
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(conn, body); err != nil {
		return Normalize("Unknown", "Unknown"), nil
	}

	node := findFocusedSwayNode(body)
	if node == nil {
		return Normalize("Unknown", "Unknown"), nil
	}
	app := node.AppID
	if app == "" {
		app = node.WindowProperties.Class
	}
	return Normalize(app, node.Name), nil
}

type swayNode struct {
	Focused          bool   `json:"focused"`
	Name             string `json:"name"`
	AppID            string `json:"app_id"`
	WindowProperties struct {
		Class string `json:"class"`
	} `json:"window_properties"`
	Nodes         []swayNode `json:"nodes"`
	FloatingNodes []swayNode `json:"floating_nodes"`
}

func findFocusedSwayNode(data []byte) *swayNode {
	var root swayNode
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	return walkSwayNodes(&root)
}

func walkSwayNodes(n *swayNode) *swayNode {
	if n.Focused {
		return n
	}
	for i := range n.Nodes {
		if found := walkSwayNodes(&n.Nodes[i]); found != nil {
			return found
		}
	}
	for i := range n.FloatingNodes {
		if found := walkSwayNodes(&n.FloatingNodes[i]); found != nil {
			return found
		}
	}
	return nil
}

// hyprSocketTimeout bounds the dial and the whole exchange with Hyprland's IPC socket, which answers in well under a millisecond when it answers at all.
const hyprSocketTimeout = time.Second

// hyprWindow queries the active window via Hyprland's IPC socket.
func (t *linuxTracker) hyprWindow() (*Activity, error) {
	sig := os.Getenv("HYPRLAND_INSTANCE_SIGNATURE")
	sockPath := fmt.Sprintf("/tmp/hypr/%s/.socket.sock", sig)
	if _, err := os.Stat(sockPath); err != nil {
		// newer hyprland may use XDG_RUNTIME_DIR
		runtime := os.Getenv("XDG_RUNTIME_DIR")
		sockPath = fmt.Sprintf("%s/hypr/%s/.socket.sock", runtime, sig)
	}

	conn, err := net.DialTimeout("unix", sockPath, hyprSocketTimeout)
	if err != nil {
		return Normalize("Unknown", "Unknown"), nil
	}
	defer conn.Close()
	// The tracker samples on a fixed interval, so a compositor that stops answering must cost one missed sample, not a hung loop.
	_ = conn.SetDeadline(time.Now().Add(hyprSocketTimeout))

	if _, err := conn.Write([]byte("j/activewindow")); err != nil {
		return Normalize("Unknown", "Unknown"), nil
	}

	data, err := io.ReadAll(conn)
	if err != nil {
		return Normalize("Unknown", "Unknown"), nil
	}

	var win struct {
		Class string `json:"class"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(data, &win); err != nil {
		return Normalize("Unknown", "Unknown"), nil
	}
	return Normalize(win.Class, win.Title), nil
}
