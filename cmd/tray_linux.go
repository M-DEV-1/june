//go:build linux

package cmd

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"net"
	"os"
	"sync/atomic"

	"ora/internal/recorder"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

//go:embed tray_icon_linux.png
var trayIconPNG []byte

// statusDotSize is the side length of the rendered status indicator (px).
const statusDotSize = 16

// statusDotPNG renders an anti-aliased filled circle of colour c on a transparent square — a Docker-style status dot for a dbusmenu item's icon-data property, rather than an emoji glyph in the label text.
func statusDotPNG(c color.RGBA) []byte {
	const ss = 4 // supersampling factor for smooth edges
	img := image.NewRGBA(image.Rect(0, 0, statusDotSize, statusDotSize))
	center := float64(statusDotSize) / 2
	radius := center - 1.5 // small padding from the edge
	for y := 0; y < statusDotSize; y++ {
		for x := 0; x < statusDotSize; x++ {
			hits := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := float64(x) + (float64(sx)+0.5)/ss
					py := float64(y) + (float64(sy)+0.5)/ss
					dx, dy := px-center, py-center
					if dx*dx+dy*dy <= radius*radius {
						hits++
					}
				}
			}
			if hits == 0 {
				continue
			}
			cov := float64(hits) / float64(ss*ss)
			// image.RGBA is alpha-premultiplied, so scale RGB by coverage too.
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(float64(c.R) * cov),
				G: uint8(float64(c.G) * cov),
				B: uint8(float64(c.B) * cov),
				A: uint8(cov * 255),
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

var (
	dotActive = statusDotPNG(color.RGBA{R: 0x3f, G: 0xb9, B: 0x50, A: 0xff}) // green: tracking
	dotPaused = statusDotPNG(color.RGBA{R: 0xd2, G: 0x99, B: 0x22, A: 0xff}) // amber: paused
)

// trayIconPixmaps decodes the embedded ORA logo into a single SNI icon pixmap.
// SNI pixmaps are ARGB32 in network byte order: A,R,G,B per pixel.
func trayIconPixmaps() ([]sniPixmap, error) {
	img, _, err := image.Decode(bytes.NewReader(trayIconPNG))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)

	data := make([]byte, w*h*4)
	for i, p := 0, 0; i < len(rgba.Pix); i, p = i+4, p+4 {
		data[p] = rgba.Pix[i+3]   // A
		data[p+1] = rgba.Pix[i]   // R
		data[p+2] = rgba.Pix[i+1] // G
		data[p+3] = rgba.Pix[i+2] // B
	}
	return []sniPixmap{{Width: int32(w), Height: int32(h), Data: data}}, nil
}

// sniToolTip matches the D-Bus signature (s a(iiay) s s) for StatusNotifierItem ToolTip.
type sniToolTip struct {
	IconName    string
	IconPixmaps []sniPixmap
	Title       string
	Description string
}

// sniPixmap matches a(iiay) — an array of (int32, int32, []byte) icon pixmaps.
type sniPixmap struct {
	Width  int32
	Height int32
	Data   []byte
}

// sniItem implements the exported methods of org.kde.StatusNotifierItem.
// Properties are handled separately via prop.Export.
type sniItem struct{}

func (s *sniItem) Activate(x, y int32) *dbus.Error          { return nil }
func (s *sniItem) SecondaryActivate(x, y int32) *dbus.Error { return nil }
func (s *sniItem) ContextMenu(x, y int32) *dbus.Error       { return nil }
func (s *sniItem) Scroll(delta int32, orientation string) *dbus.Error {
	return nil
}

// dbusMenuLayout is the (ia{sv}av) tuple returned by GetLayout.
// godbus marshals exported struct fields positionally.
type dbusMenuLayout struct {
	ID         int32
	Properties map[string]dbus.Variant
	Children   []dbus.Variant
}

// dbusMenu implements com.canonical.dbusmenu at /MenuBar.
// Item IDs:
//
//	0 = root
//	1 = Status indicator  (disabled; label reflects tracking state)
//	2 = separator
//	3 = Pause / Resume Tracking  (label toggled by paused flag)
//	6 = Start / Stop meeting recording  (label toggled by the recorder's own state)
//	4 = separator
//	5 = Quit Ora
type dbusMenu struct {
	quitCh  chan<- struct{}
	conn    *dbus.Conn // needed to emit LayoutUpdated when pause label changes
	rec     *recorder.Recorder
	paused  atomic.Bool
	menuRev atomic.Uint32
}

// menu item IDs, in the order they appear.
const (
	menuStatus  int32 = 1
	menuSep1    int32 = 2
	menuPause   int32 = 3
	menuMeeting int32 = 6
	menuSep2    int32 = 4
	menuQuit    int32 = 5
)

// meetingLabel is the recording menu item's text — a toggle, so the label always names the action the click performs.
func meetingLabel(recording bool) string {
	if recording {
		return "Stop meeting recording"
	}
	return "Start meeting recording"
}

// recording reports whether a meeting is being recorded. Nil-safe: the recorder is only wired in once the daemon's store exists, and the tray must still render before then.
func (m *dbusMenu) recording() bool {
	return m.rec != nil && m.rec.Active()
}

// items is the single source of truth for the menu: every property of every item, in display order. GetLayout, GetGroupProperties and GetProperty all read from here so the three views can never disagree about a label.
func (m *dbusMenu) items() []dbusMenuItemProps {
	pauseLabel := "Pause Tracking"
	if m.paused.Load() {
		pauseLabel = "Resume Tracking"
	}
	item := func(id int32, label string, enabled bool) dbusMenuItemProps {
		return dbusMenuItemProps{ID: id, Properties: map[string]dbus.Variant{
			"label":   dbus.MakeVariant(label),
			"enabled": dbus.MakeVariant(enabled),
			"visible": dbus.MakeVariant(true),
		}}
	}
	sep := func(id int32) dbusMenuItemProps {
		return dbusMenuItemProps{ID: id, Properties: map[string]dbus.Variant{
			"type":    dbus.MakeVariant("separator"),
			"enabled": dbus.MakeVariant(true),
			"visible": dbus.MakeVariant(true),
		}}
	}

	status := item(menuStatus, m.statusLabel(), false)
	status.Properties["icon-data"] = dbus.MakeVariant(m.statusIcon())

	return []dbusMenuItemProps{
		status,
		sep(menuSep1),
		item(menuPause, pauseLabel, true),
		item(menuMeeting, meetingLabel(m.recording()), true),
		sep(menuSep2),
		item(menuQuit, "Quit Ora", true),
	}
}

// refresh bumps the menu revision and tells the shell to re-read the layout, which is how a toggled label reaches the screen.
func (m *dbusMenu) refresh() {
	rev := m.menuRev.Add(1)
	if m.conn != nil {
		m.conn.Emit("/MenuBar", "com.canonical.dbusmenu.LayoutUpdated", rev, int32(0))
	}
}

func (m *dbusMenu) statusLabel() string {
	if m.paused.Load() {
		return "Tracking paused"
	}
	return "Ora is tracking"
}

// statusIcon returns the rendered status dot (PNG bytes) matching the current tracking state, for the menu item's icon-data property.
func (m *dbusMenu) statusIcon() []byte {
	if m.paused.Load() {
		return dotPaused
	}
	return dotActive
}

func (m *dbusMenu) GetLayout(parentId, recursionDepth int32, propertyNames []string) (uint32, dbusMenuLayout, *dbus.Error) {
	var children []dbus.Variant
	for _, it := range m.items() {
		children = append(children, dbus.MakeVariant(dbusMenuLayout{
			ID:         it.ID,
			Properties: it.Properties,
			Children:   []dbus.Variant{},
		}))
	}
	root := dbusMenuLayout{ID: 0, Properties: map[string]dbus.Variant{}, Children: children}
	return m.menuRev.Load(), root, nil
}

func (m *dbusMenu) Event(id int32, eventId string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if eventId != "clicked" {
		return nil
	}
	switch id {
	case menuPause:
		var endpoint string
		if m.paused.Load() {
			endpoint = "/resume"
			m.paused.Store(false)
		} else {
			endpoint = "/pause"
			m.paused.Store(true)
		}
		go authedDaemonGet("http://127.0.0.1:" + DaemonPort + endpoint)
		// signal from root so both the status label (id 1) and pause label (id 3) refresh
		m.refresh()
	case menuMeeting:
		m.toggleMeeting()
		m.refresh()
	case menuQuit:
		select {
		case m.quitCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// toggleMeeting starts or stops the meeting recording. Stopping hands off to background transcription and summarising, so neither branch blocks the D-Bus method call.
func (m *dbusMenu) toggleMeeting() {
	if m.rec == nil {
		slog.Warn("meeting recorder is not wired up, ignoring tray click")
		return
	}
	if m.rec.Active() {
		if _, err := m.rec.StopAndProcess(context.Background()); err != nil {
			slog.Error("failed to stop meeting recording", "error", err)
		}
		return
	}
	if err := m.rec.Start(); err != nil {
		slog.Error("failed to start meeting recording", "error", err)
	}
}

func (m *dbusMenu) AboutToShow(id int32) (bool, *dbus.Error) {
	return false, nil
}

func (m *dbusMenu) GetGroupProperties(ids []int32, propertyNames []string) ([]dbusMenuItemProps, *dbus.Error) {
	allItems := m.items()
	// filter to requested IDs if the caller specified any
	if len(ids) == 0 {
		return allItems, nil
	}
	want := make(map[int32]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	var out []dbusMenuItemProps
	for _, item := range allItems {
		if _, ok := want[item.ID]; ok {
			out = append(out, item)
		}
	}
	return out, nil
}

func (m *dbusMenu) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	for _, item := range m.items() {
		if item.ID != id {
			continue
		}
		if v, ok := item.Properties[name]; ok {
			return v, nil
		}
		break
	}
	return dbus.MakeVariant(""), nil
}

// dbusMenuItemProps is the (ia{sv}) tuple returned by GetGroupProperties.
type dbusMenuItemProps struct {
	ID         int32
	Properties map[string]dbus.Variant
}

// runDaemonSupervisor on Linux registers an SNI tray icon via D-Bus and provides menu items: Open Ora, Pause/Resume Tracking, and Quit Ora.
// If SNI registration fails it falls back to headless mode.
func runDaemonSupervisor(ctx context.Context, listener net.Listener) {
	stop, _, err := startDaemonServices(ctx, listener)
	if err != nil {
		slog.Error("failed to start daemon services", "error", err)
		listener.Close()
		return
	}

	quitCh := make(chan struct{}, 1)
	if trayErr := registerSNI(ctx, quitCh); trayErr != nil {
		slog.Warn("system tray unavailable, running headless", "error", trayErr)
		slog.Info("Daemon running in background (headless, no system tray).")
		select {
		case <-ctx.Done():
		case <-quitCh:
		}
		stop()
		return
	}

	slog.Info("Daemon running in background.")

	select {
	case <-ctx.Done():
		slog.Info("Context cancelled, shutting down systray")
	case <-quitCh:
		slog.Info("Quit requested via System Tray")
	}
	stop()
}

// registerSNI exports the StatusNotifierItem and dbusmenu objects and registers with the StatusNotifierWatcher.
// Returns an error if any step fails so the caller can fall back to headless operation.
func registerSNI(ctx context.Context, quitCh chan<- struct{}) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("connect session bus: %w", err)
	}

	pid := os.Getpid()
	serviceName := fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", pid)

	reply, err := conn.RequestName(serviceName, dbus.NameFlagDoNotQueue)
	if err != nil {
		conn.Close()
		return fmt.Errorf("request name %s: %w", serviceName, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		return fmt.Errorf("name %s already taken (reply=%d)", serviceName, reply)
	}

	// Export SNI methods at /StatusNotifierItem.
	sniPath := dbus.ObjectPath("/StatusNotifierItem")
	item := &sniItem{}
	if err := conn.Export(item, sniPath, "org.kde.StatusNotifierItem"); err != nil {
		conn.Close()
		return fmt.Errorf("export sni methods: %w", err)
	}

	// Export SNI properties via prop.Export.
	tooltip := sniToolTip{
		IconName:    "",
		IconPixmaps: []sniPixmap{},
		Title:       "Ora",
		Description: "Ora Context Runtime is active",
	}
	menuPath := dbus.ObjectPath("/MenuBar")

	iconName := ""
	pixmaps, err := trayIconPixmaps()
	if err != nil {
		slog.Warn("tray icon decode failed, using theme fallback", "error", err)
		iconName = "audio-input-microphone"
		pixmaps = []sniPixmap{}
	}

	propsSpec := prop.Map{
		"org.kde.StatusNotifierItem": {
			"Category":   {Value: "ApplicationStatus", Writable: false, Emit: prop.EmitConst},
			"Id":         {Value: "ora", Writable: false, Emit: prop.EmitConst},
			"Title":      {Value: "Ora", Writable: false, Emit: prop.EmitFalse},
			"Status":     {Value: "Active", Writable: false, Emit: prop.EmitFalse},
			"IconName":   {Value: iconName, Writable: false, Emit: prop.EmitFalse},
			"IconPixmap": {Value: pixmaps, Writable: false, Emit: prop.EmitFalse},
			"ToolTip":    {Value: tooltip, Writable: false, Emit: prop.EmitFalse},
			"ItemIsMenu": {Value: true, Writable: false, Emit: prop.EmitConst},
			"Menu":       {Value: menuPath, Writable: false, Emit: prop.EmitConst},
		},
	}
	if _, err := prop.Export(conn, sniPath, propsSpec); err != nil {
		conn.Close()
		return fmt.Errorf("export sni properties: %w", err)
	}

	// Export dbusmenu at /MenuBar.
	menu := &dbusMenu{quitCh: quitCh, conn: conn, rec: meetingRecorder}
	// The menu's own clicks redraw it themselves; this covers a recording started or stopped by anything else, which since the microphone watcher landed is how most of them begin.
	if meetingRecorder != nil {
		meetingRecorder.SetOnStateChange(menu.refresh)
	}
	if err := conn.Export(menu, menuPath, "com.canonical.dbusmenu"); err != nil {
		conn.Close()
		return fmt.Errorf("export dbusmenu: %w", err)
	}

	// Export dbusmenu properties.
	menuPropsSpec := prop.Map{
		"com.canonical.dbusmenu": {
			"Version": {Value: uint32(3), Writable: false, Emit: prop.EmitConst},
			"Status":  {Value: "normal", Writable: false, Emit: prop.EmitFalse},
		},
	}
	if _, err := prop.Export(conn, menuPath, menuPropsSpec); err != nil {
		conn.Close()
		return fmt.Errorf("export dbusmenu properties: %w", err)
	}

	// Register with the StatusNotifierWatcher. A missing watcher (GNOME's appindicator extension INACTIVE or not loaded yet) is deliberately not fatal: the NameOwnerChanged listener below registers the moment one appears, so the tray arrives late instead of never.
	if err := registerWithWatcher(conn); err != nil {
		slog.Warn("no StatusNotifierWatcher yet, tray will register when one appears", "error", err)
	}

	// Self-heal: the watcher (e.g. gnome-shell) drops all items when it restarts and re-announces itself with a new bus owner.
	// Watch for that and re-register so the tray icon survives a shell restart.
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, "org.kde.StatusNotifierWatcher"),
	); err != nil {
		slog.Warn("tray self-heal unavailable, watcher restarts won't re-register", "error", err)
	} else {
		sigCh := make(chan *dbus.Signal, 8)
		conn.Signal(sigCh)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case sig, ok := <-sigCh:
					if !ok {
						return
					}
					if shouldReregister(sig.Name, sig.Body) {
						if err := registerWithWatcher(conn); err != nil {
							slog.Warn("tray re-registration failed after watcher restart", "error", err)
						} else {
							slog.Info("tray re-registered after watcher restart")
						}
					}
				}
			}
		}()
	}

	// Close the connection when context is cancelled.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	return nil
}

// registerWithWatcher (re)registers this process's StatusNotifierItem with the org.kde.StatusNotifierWatcher under our unique bus name.
func registerWithWatcher(conn *dbus.Conn) error {
	watcher := conn.Object(
		"org.kde.StatusNotifierWatcher",
		"/StatusNotifierWatcher",
	)
	return watcher.Call(
		"org.kde.StatusNotifierWatcher.RegisterStatusNotifierItem",
		0,
		conn.Names()[0],
	).Err
}

// shouldReregister reports whether a D-Bus signal indicates the StatusNotifierWatcher has reappeared with a new owner (i.e. restarted), meaning our item was dropped and must be registered again.
// Must NOT fire when the watcher merely disappears (empty new owner).
func shouldReregister(signalName string, body []interface{}) bool {
	if signalName != "org.freedesktop.DBus.NameOwnerChanged" {
		return false
	}
	if len(body) != 3 {
		return false
	}
	name, _ := body[0].(string)
	newOwner, _ := body[2].(string)
	return name == "org.kde.StatusNotifierWatcher" && newOwner != ""
}
