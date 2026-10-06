//go:build linux

package cmd

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"net"
	"os"
	"sort"
	"sync/atomic"
	"time"

	"june/internal/recorder"
	"june/internal/tracker"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

//go:embed tray
var trayIcons embed.FS

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

// trayIconPixmaps decodes every embedded size of the June logo into one SNI icon pixmap each.
// SNI pixmaps are ARGB32 in network byte order: A,R,G,B per pixel.
// One entry per size because IconPixmap is an array and the host picks the one closest to its own panel height: handed a single large pixmap it scales that down itself, which is what left the face blurred. The files are drawn at these exact sizes rather than resampled from one master (see packaging/make-icons.py).
// Output: the pixmaps, smallest first, or an error if the embedded directory cannot be read or holds something that is not an image.
func trayIconPixmaps() ([]sniPixmap, error) {
	entries, err := trayIcons.ReadDir("tray")
	if err != nil {
		return nil, err
	}
	pixmaps := make([]sniPixmap, 0, len(entries))
	for _, entry := range entries {
		raw, err := trayIcons.ReadFile("tray/" + entry.Name())
		if err != nil {
			return nil, err
		}
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("tray icon %s: %w", entry.Name(), err)
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
		pixmaps = append(pixmaps, sniPixmap{Width: int32(w), Height: int32(h), Data: data})
	}
	if len(pixmaps) == 0 {
		return nil, fmt.Errorf("no tray icons are embedded")
	}
	sort.Slice(pixmaps, func(i, j int) bool { return pixmaps[i].Width < pixmaps[j].Width })
	return pixmaps, nil
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

// dbusMenu implements com.canonical.dbusmenu at /MenuBar. Its items are those of tray.go, in the same order and words as the Windows tray.
// Item IDs:
//
//	0 = root
//	1 = Status indicator  (disabled; label follows the tracker's own state)
//	2 = separator
//	7 = Open June  (brings the desktop window to the front)
//	3 = Pause watching  (submenu of 10, 11, 12: the window's three pauses; shown while June watches)
//	8 = Resume watching  (shown while paused)
//	9 = Finish setting up June…  (shown while observation waits for setup)
//	6 = Record a meeting / Stop recording  (label follows the recorder's own state)
//	4 = separator
//	5 = Quit June
type dbusMenu struct {
	// The daemon's own context, handed to a stop from the tray so the transcription it starts ends with the daemon instead of outliving it on the GPU.
	ctx    context.Context
	quitCh chan<- struct{}
	conn   *dbus.Conn // needed to emit LayoutUpdated when pause label changes
	rec    *recorder.Recorder
	// tracking is read for the pause labels on every redraw. The window pauses and resumes through the same pause control, so a flag only the tray's clicks flipped read "Pause" over a paused tracker and made the next click pause it again.
	tracking *tracker.Daemon
	// drawnStatus is the status line the last LayoutUpdated went out with, so watchPause can tell when the window, a pause running out or setup finishing has changed it since.
	drawnStatus atomic.Pointer[string]
	menuRev     atomic.Uint32
}

// menu item IDs, in the order they appear.
const (
	menuStatus  int32 = 1
	menuSep1    int32 = 2
	menuOpen    int32 = 7
	menuPause   int32 = 3
	menuResume  int32 = 8
	menuSetup   int32 = 9
	menuMeeting int32 = 6
	menuSep2    int32 = 4
	menuQuit    int32 = 5
)

// menuPauseChoice is the ID of the first of the pause submenu's items; pauseChoices[i] is menuPauseChoice+i.
const menuPauseChoice int32 = 10

// recording reports whether a meeting is being recorded. Nil-safe: the recorder is only wired in once the daemon's store exists, and the tray must still render before then.
func (m *dbusMenu) recording() bool {
	return m.rec != nil && m.rec.Active()
}

// menuNode is one item of the menu with the items of its submenu, if it has one.
type menuNode struct {
	props    dbusMenuItemProps
	children []menuNode
}

// tree is the single source of truth for the menu: every property of every item, in display order. GetLayout, GetGroupProperties and GetProperty all read from here so the three views can never disagree about a label. All three faces of the pause item are always there, the two not wanted now hidden, so a host that keeps items by ID never holds one that has vanished.
func (m *dbusMenu) tree() []menuNode {
	paused := m.tracking.IsPaused()
	mode := pauseMode(paused)
	item := func(id int32, label string, enabled, visible bool) dbusMenuItemProps {
		return dbusMenuItemProps{ID: id, Properties: map[string]dbus.Variant{
			"label":   dbus.MakeVariant(label),
			"enabled": dbus.MakeVariant(enabled),
			"visible": dbus.MakeVariant(visible),
		}}
	}
	sep := func(id int32) dbusMenuItemProps {
		return dbusMenuItemProps{ID: id, Properties: map[string]dbus.Variant{
			"type":    dbus.MakeVariant("separator"),
			"enabled": dbus.MakeVariant(true),
			"visible": dbus.MakeVariant(true),
		}}
	}

	status := item(menuStatus, trayStatus(paused), false, true)
	status.Properties["icon-data"] = dbus.MakeVariant(m.statusIcon())

	pause := item(menuPause, pauseMenuLabel, true, mode == modeWatching)
	pause.Properties["children-display"] = dbus.MakeVariant("submenu")
	choices := make([]menuNode, len(pauseChoices))
	for i, c := range pauseChoices {
		choices[i] = menuNode{props: item(menuPauseChoice+int32(i), c.label, true, true)}
	}

	return []menuNode{
		{props: status},
		{props: sep(menuSep1)},
		// The window has no tray icon of its own, so opening it lives here, on the one icon. The hover is not in the menu: it is what the keyboard shortcut is for, and a menu item for it would be a second name for the same thing.
		{props: item(menuOpen, "Open June", true, true)},
		{props: pause, children: choices},
		{props: item(menuResume, resumeLabel, true, mode == modePaused)},
		{props: item(menuSetup, finishSetupLabel, true, mode == modeSetup)},
		{props: item(menuMeeting, meetingLabel(m.recording()), true, true)},
		{props: sep(menuSep2)},
		{props: item(menuQuit, "Quit June", true, true)},
	}
}

// items is every item of the menu, submenus' included, flattened in display order.
func (m *dbusMenu) items() []dbusMenuItemProps {
	var out []dbusMenuItemProps
	var walk func([]menuNode)
	walk = func(nodes []menuNode) {
		for _, n := range nodes {
			out = append(out, n.props)
			walk(n.children)
		}
	}
	walk(m.tree())
	return out
}

// refresh bumps the menu revision and tells the shell to re-read the layout, which is how a toggled label reaches the screen.
func (m *dbusMenu) refresh() {
	status := trayStatus(m.tracking.IsPaused())
	m.drawnStatus.Store(&status)
	rev := m.menuRev.Add(1)
	if m.conn != nil {
		m.conn.Emit("/MenuBar", "com.canonical.dbusmenu.LayoutUpdated", rev, int32(0))
	}
}

// watchPause redraws the menu when the tracker has been paused or resumed by something other than the tray, a pause has run out or setup has finished, until ctx ends. Each of those changes the status line, which is what is compared.
// ponytail: polled, because the tracker tells no one when it is paused; a pause observer on tracker.Daemon would make this event-driven.
func (m *dbusMenu) watchPause(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			status := trayStatus(m.tracking.IsPaused())
			if drawn := m.drawnStatus.Load(); drawn == nil || *drawn != status {
				m.refresh()
			}
		}
	}
}

// statusIcon returns the rendered status dot (PNG bytes) matching the current tracking state, for the menu item's icon-data property. Waiting on setup is paused too.
func (m *dbusMenu) statusIcon() []byte {
	if m.tracking.IsPaused() {
		return dotPaused
	}
	return dotActive
}

// layout renders nodes as dbusmenu layouts, depth levels of submenu deep (-1 for all of them).
func layout(nodes []menuNode, depth int32) []dbus.Variant {
	children := []dbus.Variant{}
	if depth == 0 {
		return children
	}
	for _, n := range nodes {
		children = append(children, dbus.MakeVariant(dbusMenuLayout{
			ID:         n.props.ID,
			Properties: n.props.Properties,
			Children:   layout(n.children, depth-1),
		}))
	}
	return children
}

// findNode is the item with id among nodes and their submenus, or false when there is none.
func findNode(nodes []menuNode, id int32) (menuNode, bool) {
	for _, n := range nodes {
		if n.props.ID == id {
			return n, true
		}
		if found, ok := findNode(n.children, id); ok {
			return found, true
		}
	}
	return menuNode{}, false
}

// GetLayout answers the item parentId (0 for the whole menu) with its submenu, recursionDepth levels deep, -1 for all.
func (m *dbusMenu) GetLayout(parentId, recursionDepth int32, propertyNames []string) (uint32, dbusMenuLayout, *dbus.Error) {
	tree := m.tree()
	root := dbusMenuLayout{ID: 0, Properties: map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}, Children: layout(tree, recursionDepth)}
	if parentId != 0 {
		node, ok := findNode(tree, parentId)
		if !ok {
			return m.menuRev.Load(), root, dbus.MakeFailedError(fmt.Errorf("no menu item %d", parentId))
		}
		root = dbusMenuLayout{ID: node.props.ID, Properties: node.props.Properties, Children: layout(node.children, recursionDepth)}
	}
	return m.menuRev.Load(), root, nil
}

func (m *dbusMenu) Event(id int32, eventId string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if eventId != "clicked" {
		return nil
	}
	if i := int(id - menuPauseChoice); i >= 0 && i < len(pauseChoices) {
		pauseFromTray(pauseChoices[i].minutes)
		m.refresh()
		return nil
	}
	switch id {
	case menuOpen:
		go authedDaemonGet("http://127.0.0.1:" + DaemonPort + "/window?action=open")
	case menuResume, menuSetup:
		// The redraw waits for the change, since the labels are read off the tracker it changes.
		go func() {
			resumeFromTray()
			m.refresh()
		}()
	case menuMeeting:
		toggleMeeting(m.ctx, m.rec)
		m.refresh()
	case menuQuit:
		// Recorded as a quit the way POST /quit is, before the shutdown starts, so a restart asked for during it is refused rather than bringing June back after the user quit it.
		beginQuit()
		select {
		case m.quitCh <- struct{}{}:
		default:
		}
	}
	return nil
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

// runDaemonSupervisor on Linux registers an SNI tray icon via D-Bus with the same menu as Windows (see tray.go): a status line, Open June, the pause item, the meeting item, and Quit June.
// If SNI registration fails it falls back to headless mode.
func runDaemonSupervisor(ctx context.Context, listener net.Listener) {
	stop, tracking, err := startDaemonServices(ctx, listener)
	if err != nil {
		slog.Error("failed to start daemon services", "error", err)
		listener.Close()
		return
	}

	quitCh := make(chan struct{}, 1)
	trayErr := errors.New("JUNE_NO_TRAY is set")
	if trayWanted() {
		trayErr = registerSNI(ctx, quitCh, tracking)
	}
	if trayErr != nil {
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

// registerSNI exports the StatusNotifierItem and dbusmenu objects and registers with the StatusNotifierWatcher. Input: the daemon's context, the channel Quit June sends on, and the tracker whose pause state the menu shows.
// Returns an error if any step fails so the caller can fall back to headless operation.
func registerSNI(ctx context.Context, quitCh chan<- struct{}, tracking *tracker.Daemon) error {
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
		Title:       "June",
		Description: "June is running",
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
			"Id":         {Value: "june", Writable: false, Emit: prop.EmitConst},
			"Title":      {Value: "June", Writable: false, Emit: prop.EmitFalse},
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
	menu := &dbusMenu{ctx: ctx, quitCh: quitCh, conn: conn, rec: meetingRecorder, tracking: tracking}
	// The menu's own clicks redraw it themselves; this covers a recording started or stopped by anything else, which since the microphone watcher landed is how most of them begin.
	if meetingRecorder != nil {
		meetingRecorder.AddStateObserver(menu.refresh)
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

	go menu.watchPause(ctx)

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
