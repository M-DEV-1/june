//go:build linux

// Package input drives GNOME's on-screen pointer and keyboard through
// xdg-desktop-portal's RemoteDesktop interface, so the agent can act on the
// screen the same way a person with a mouse and keyboard would. The user
// sees one consent dialog the first time; after that the granted permission
// is restored from a token saved to disk.
package input

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	portalDest         = "org.freedesktop.portal.Desktop"
	portalPath         = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	remoteDesktopIface = "org.freedesktop.portal.RemoteDesktop"
	screenCastIface    = "org.freedesktop.portal.ScreenCast"
	sessionIface       = "org.freedesktop.portal.Session"
	requestIface       = "org.freedesktop.portal.Request"
)

// Device type bitmask for SelectDevices, from the portal spec.
const (
	deviceKeyboard uint32 = 1
	devicePointer  uint32 = 2
)

const (
	keyStatePressed  uint32 = 1
	keyStateReleased uint32 = 0
)

// axisVertical is NotifyPointerAxisDiscrete's axis value for vertical scrolling.
const axisVertical uint32 = 0

// btnLeft and btnRight are BTN_LEFT and BTN_RIGHT from linux/input-event-codes.h, the evdev button codes NotifyPointerButton expects. The right button is what opens a context menu.
const (
	btnLeft  int32 = 0x110
	btnRight int32 = 0x111
)

var reqSeq atomic.Uint64

// errClosed is returned by any Session method called after Close.
var errClosed = errors.New("input: session is closed")

// CoordinateError reports a pointer coordinate rejected before it reached the portal.
type CoordinateError struct {
	X, Y          float64
	Width, Height int // the known bounds of the one monitor the session covers, or 0 if the stream never reported a size
	// Monitors is the number of monitors this session covers, set only when the point missed every one of them (the multi-monitor case). Zero means the single-stream, legacy shape below applies instead.
	Monitors int
	// TokenPath is where the saved restore token lives, so the message can tell the user how to re-grant. Filled in by the Session methods that know dataDir; blank when toStream is called directly.
	TokenPath string
}

func (e *CoordinateError) Error() string {
	if e.Monitors > 0 {
		msg := fmt.Sprintf("input: coordinate (%.0f, %.0f) is outside every monitor this pointer session reaches (granted %d monitor(s))", e.X, e.Y, e.Monitors)
		if e.TokenPath != "" {
			msg += fmt.Sprintf("; delete %s and let the next click ask again", e.TokenPath)
		}
		return msg
	}
	return fmt.Sprintf("input: coordinate (%.0f, %.0f) is outside the %dx%d monitor", e.X, e.Y, e.Width, e.Height)
}

// Session is an open RemoteDesktop portal session that can inject keyboard and pointer input into the GNOME Wayland desktop.
type Session struct {
	conn *dbus.Conn
	// streams is every ScreenCast stream the session was granted, one per monitor the user picked in the consent dialog (or restored via a saved token). Pointer coordinates arrive in whole-desktop space; toStream picks whichever stream's rectangle contains a given point and maps into that stream's own space before it is sent.
	streams []streamInfo
	// dataDir holds the saved restore token; blank in tests that never touch the disk.
	dataDir string

	// send routes one RemoteDesktop method call; nil means the live D-Bus connection. Tests set it to record the event stream without a bus. It takes the call's own context so the timeout call puts on every event is the one the test sees.
	send func(ctx context.Context, method string, args ...interface{}) error

	// acting is held for the whole of PressKey, TypeText, ClickAt and ScrollAt, each of which is several separate events paced keyDelay apart. One Session is shared by every ask, and without this one ask's modifier press lands between another's key down and key up.
	acting sync.Mutex

	mu     sync.Mutex
	handle string // session_handle; a plain string despite naming an object path (a documented portal spec quirk kept for backwards compatibility). Guarded by mu because Close clears it while another goroutine may be mid-call.
}

// portalCallTimeout bounds one RemoteDesktop event on the bus. Each one is a fire-and-forget Notify with no consent dialog behind it, so a reply that has not come in two seconds is a portal that has stopped answering, and waiting on it holds s.acting and wedges every screen action in the daemon behind it. A variable so a test can shorten it and cost no wall time.
var portalCallTimeout = 2 * time.Second

// call sends one RemoteDesktop method to the portal, bounded by portalCallTimeout. Input: the method name without its interface, and the arguments it takes. Output: the D-Bus error, the timeout when the portal did not answer, or whatever the test stub in send returns.
func (s *Session) call(method string, args ...interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), portalCallTimeout)
	defer cancel()
	var err error
	if s.send != nil {
		err = s.send(ctx, method, args...)
	} else {
		err = s.conn.Object(portalDest, portalPath).CallWithContext(ctx, remoteDesktopIface+"."+method, 0, args...).Err
	}
	// "not allowed to call Notify..." is a session granted without the keyboard or the pointer, and the saved token is what restores that empty grant, so it goes; the next open then shows the consent dialog again (2026-09-08).
	if err != nil && strings.Contains(err.Error(), "not allowed to call Notify") {
		forgetToken(s.dataDir)
	}
	return err
}

// currentHandle returns the live session handle, or errClosed once Close has run.
func (s *Session) currentHandle() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == "" {
		return "", errClosed
	}
	return s.handle, nil
}

// closeLocked clears the handle and returns whatever it was, atomically, so a concurrent currentHandle never observes a handle that's about to be invalidated without either fully seeing it or getting errClosed.
func (s *Session) closeLocked() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.handle
	s.handle = ""
	return h
}

// Open starts a RemoteDesktop session covering keyboard and pointer: it restores a saved restore_token from dataDir if one exists, otherwise the user sees the portal's one-time consent dialog. Also opens a ScreenCast stream per monitor the user granted, which NotifyPointerMotionAbsolute requires to map (x, y) to a screen. dataDir is created (mode 0700) if missing, since the restore_token must be written there.
func Open(ctx context.Context, dataDir string) (*Session, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}

	handle, streams, newToken, err := openSequence(ctx, &realPortal{conn: conn}, loadToken(dataDir))
	if err != nil {
		return nil, err
	}
	if err := saveToken(dataDir, newToken); err != nil {
		return nil, fmt.Errorf("save token: %w", err)
	}
	for i := range streams {
		streams[i].Rect = withStreamScale(streams[i].Rect)
	}
	// A saved token from an older, one-monitor grant keeps restoring that same one-monitor grant on every later Open; the portal never asks again on its own. Whether that is actually a problem depends on how many monitors the desk has, which isn't known here, so this is only a log line, not a hard failure: on a genuinely single-monitor desk it is expected and harmless.
	if len(streams) == 1 {
		fmt.Fprintf(os.Stderr, "input: pointer session covers 1 monitor; on a multi-monitor desk this usually means an old grant, delete %s and the next click will ask again\n", filepath.Join(dataDir, tokenFile))
	}
	return &Session{conn: conn, handle: handle, streams: streams, dataDir: dataDir}, nil
}

// portalOps is the set of fallible RemoteDesktop/ScreenCast steps openSequence drives. Splitting it out from Session lets the failure-cleanup logic in openSequence be tested against a fake, without a live D-Bus session.
type portalOps interface {
	createSession(ctx context.Context) (string, error)
	selectDevices(ctx context.Context, handle, restoreToken string) error
	selectSources(ctx context.Context, handle string) error
	start(ctx context.Context, handle string) (streams []streamInfo, restoreToken string, err error)
	closeSession(handle string) error
}

// openSequence runs the CreateSession -> SelectDevices -> SelectSources -> Start handshake. If any step after createSession fails, it closes the session it opened before returning the error, so a failed Open never leaks a live portal session (and the orphaned PipeWire stream that comes with it).
func openSequence(ctx context.Context, p portalOps, restoreToken string) (handle string, streams []streamInfo, newToken string, err error) {
	handle, err = p.createSession(ctx)
	if err != nil {
		return "", nil, "", err
	}

	if err = p.selectDevices(ctx, handle, restoreToken); err != nil {
		_ = p.closeSession(handle)
		return "", nil, "", err
	}
	if err = p.selectSources(ctx, handle); err != nil {
		_ = p.closeSession(handle)
		return "", nil, "", err
	}

	streams, newToken, err = p.start(ctx, handle)
	if err != nil {
		_ = p.closeSession(handle)
		return "", nil, "", err
	}
	// A grant with no screen stream has no monitor to map a point onto, so every click on it fails one at a time, deep in the mapping, with a message about the coordinate rather than about the missing grant. Refusing here says it once, in the one place that knows why.
	if len(streams) == 0 {
		_ = p.closeSession(handle)
		return "", nil, "", errors.New("the portal granted no screen stream, so the pointer has no monitor to aim at")
	}
	return handle, streams, newToken, nil
}

// Close ends the portal session so the compositor can release the associated PipeWire stream. Safe to call more than once.
func (s *Session) Close() error {
	h := s.closeLocked()
	if h == "" {
		return nil
	}
	return s.conn.Object(portalDest, dbus.ObjectPath(h)).Call(sessionIface+".Close", 0).Err
}

// streamRect is where the granted ScreenCast stream's monitor sits on the desktop and how big it is, read from the stream's "position" and "size" properties. All zero when the compositor reported neither, which is the case the mapping falls back to leaving a point alone.
// The two properties are not in the same units: position is in the compositor's logical layout space, while size is the stream's own video size, which on a scaled monitor is that monitor's device resolution. Scale is how many stream pixels one logical pixel is worth, worked out once when the session opens (see withStreamScale); it is 1 whenever the monitor's logical size is not known, which is what this assumed before it was worked out at all.
type streamRect struct {
	X, Y  int
	W, H  int
	Scale float64
}

// streamInfo pairs one granted ScreenCast stream's PipeWire node id with the rectangle it covers, so toStream can pick the right stream for a point and NotifyPointerMotionAbsolute can be told which node the coordinate belongs to.
type streamInfo struct {
	Node uint32
	Rect streamRect
}

// monitorLayout reports the logical size of the monitor holding a desktop point. It is nil until something wires a reader in (see UseMonitorLayout), and while it is nil every stream is taken to be one stream pixel to one logical pixel.
var monitorLayout func(x, y int) (w, h int, ok bool)

// UseMonitorLayout wires in the reader of the desktop's monitor layout, which is what lets a granted stream's scale be worked out from its size. Input: a function answering the logical width and height of the monitor holding a desktop point, and false when it cannot say (tracker.MonitorLogicalSize in the daemon, a stub in tests). Output: none. Call it before Open; a session already open keeps the scale it was opened with.
func UseMonitorLayout(f func(x, y int) (w, h int, ok bool)) { monitorLayout = f }

// withStreamScale fills in a granted stream's scale from its size against the logical size of the monitor it covers. Input: the rectangle as the portal reported it. Output: the same rectangle with Scale set — the stream's width divided by the monitor's logical width, or 1 when either is unknown.
func withStreamScale(r streamRect) streamRect {
	r.Scale = 1
	if monitorLayout == nil || r.W <= 0 {
		return r
	}
	logicalW, _, ok := monitorLayout(r.X, r.Y)
	if !ok || logicalW <= 0 {
		return r
	}
	r.Scale = float64(r.W) / float64(logicalW)
	return r
}

// toStream maps a point in whole-desktop logical coordinates into whichever granted stream's rectangle contains it, which is what NotifyPointerMotionAbsolute takes, and rejects a point that is on none of them. Input: the logical desktop point and every stream the session was granted. Output: the point measured from that monitor's own top-left corner and multiplied by its stream's scale, the node id of the stream it landed on, or a CoordinateError when the point is outside every stream whose size is known.
// The origin is taken off before the scale goes on, because a stream's position is in logical pixels and its size is in the stream's own. A stream whose scale could not be worked out is mapped at 1, which is right on an unscaled monitor and is the assumption that still stands everywhere else.
// A stream that reports no size at all (some compositors omit it) is treated as covering wherever a point lands, exactly as a single such stream did before multi-monitor support existed: only a negative coordinate is refused. That stream is tried last, after every sized stream has had a chance to claim the point.
func toStream(x, y float64, streams []streamInfo) (float64, float64, uint32, error) {
	var unsized *streamInfo
	for i := range streams {
		r := streams[i].Rect
		if r.W <= 0 && r.H <= 0 {
			if unsized == nil {
				unsized = &streams[i]
			}
			continue
		}
		scale := r.Scale
		if scale <= 0 {
			scale = 1
		}
		sx, sy := (x-float64(r.X))*scale, (y-float64(r.Y))*scale
		if sx >= 0 && sy >= 0 && sx <= float64(r.W) && sy <= float64(r.H) {
			return sx, sy, streams[i].Node, nil
		}
	}
	if unsized != nil {
		r := unsized.Rect
		scale := r.Scale
		if scale <= 0 {
			scale = 1
		}
		sx, sy := (x-float64(r.X))*scale, (y-float64(r.Y))*scale
		if sx < 0 || sy < 0 {
			return 0, 0, 0, &CoordinateError{X: x, Y: y, Monitors: len(streams)}
		}
		return sx, sy, unsized.Node, nil
	}
	if len(streams) == 1 {
		// Exactly one, sized stream: keep the old single-monitor message naming that monitor's bounds.
		return 0, 0, 0, &CoordinateError{X: x, Y: y, Width: streams[0].Rect.W, Height: streams[0].Rect.H}
	}
	return 0, 0, 0, &CoordinateError{X: x, Y: y, Monitors: len(streams)}
}

// PressKey sends a full press-then-release for the named key or chord (e.g. "Enter", "Ctrl+L"), paced keyDelay apart. Modifiers in a chord are pressed first and released last, in reverse order, so the compositor sees them held down for the whole chord.
// Every key that actually went down is released even when a later press fails, because a modifier left held is chorded by the compositor into whatever the model sends next. Input: the key or chord name. Output: the first error hit, pressing or releasing, or nil.
func (s *Session) PressKey(name string) error { return s.PressKeyContext(context.Background(), name) }

// PressKeyContext is PressKey that presses no further key of the chord once ctx has ended, which a stopped job does while it waits on the acting lock behind another call's typing; the keys already down are released as on any other failure. Input: the context and the key or chord name. Output: a *Stopped when ctx ended before the chord was all down, else what PressKey returns.
func (s *Session) PressKeyContext(ctx context.Context, name string) error {
	codes, err := chord(name)
	if err != nil {
		return err
	}
	s.acting.Lock()
	defer s.acting.Unlock()
	handle, err := s.currentHandle()
	if err != nil {
		return err
	}

	held := 0
	for _, c := range codes {
		if e := ctx.Err(); e != nil {
			err = &Stopped{Sent: held, Of: len(codes), Err: e}
			break
		}
		if err = s.notifyKeycode(handle, c, keyStatePressed); err != nil {
			break
		}
		held++
		time.Sleep(keyDelay)
	}
	for i := held - 1; i >= 0; i-- {
		if e := s.notifyKeycode(handle, codes[i], keyStateReleased); e != nil && err == nil {
			err = e
		}
		time.Sleep(keyDelay)
	}
	return err
}

// TypeText presses and releases each character of text in turn via its X11 keysym, paced keyDelay apart, so arbitrary Unicode text can be typed without needing a keycode mapping for every character. Characters with no sensible keysym (see runeKeysym) are silently skipped rather than sent as a bogus chord.
// Every key that goes down is retried on release before TypeText gives up, the same as PressKey does for a chord: a release call that fails would otherwise leave that key held from the compositor's point of view for whatever the model sends next.
func (s *Session) TypeText(text string) error { return s.TypeTextContext(context.Background(), text) }

// TypeTextContext is TypeText that stops between two characters once ctx has ended, so a job the user stopped stops typing within a character rather than after the whole text has gone out: at about two dozen milliseconds a character, a long dictation ran on for seconds after the stop. Input: the context and the text. Output: a *Stopped naming how many characters went out when ctx ended first, else what TypeText returns.
func (s *Session) TypeTextContext(ctx context.Context, text string) error {
	s.acting.Lock()
	defer s.acting.Unlock()
	handle, err := s.currentHandle()
	if err != nil {
		return err
	}

	chars := 0
	for _, r := range text {
		if _, ok := runeKeysym(r); ok {
			chars++
		}
	}
	typed := 0
	for _, r := range text {
		sym, ok := runeKeysym(r)
		if !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return &Stopped{Sent: typed, Of: chars, Err: err}
		}
		typed++
		if err := s.notifyKeysym(handle, sym, keyStatePressed); err != nil {
			return err
		}
		time.Sleep(keyDelay)
		if err := s.notifyKeysym(handle, sym, keyStateReleased); err != nil {
			releaseHeldKeysym(s, handle, sym)
			return err
		}
		time.Sleep(keyDelay)
	}
	return nil
}

// releaseHeldKeysym is the recovery TypeText runs when a release call fails: one more attempt to bring that key back up before the error is reported, so a single failed release does not leave a key chorded into whatever the model sends next. Its own error is not reported; the original release's error is what TypeText already has to give.
func releaseHeldKeysym(s *Session, handle string, sym int32) {
	s.notifyKeysym(handle, sym, keyStateReleased)
}

// coordinateForStream maps a whole-desktop point into whichever granted stream covers it, filling in the CoordinateError's TokenPath when the point is on none of them, so the message tells the user exactly what to delete to re-grant.
func (s *Session) coordinateForStream(x, y float64) (float64, float64, uint32, error) {
	sx, sy, node, err := toStream(x, y, s.streams)
	if err != nil {
		var ce *CoordinateError
		if errors.As(err, &ce) && ce.TokenPath == "" && s.dataDir != "" {
			ce.TokenPath = filepath.Join(s.dataDir, tokenFile)
		}
		return 0, 0, 0, err
	}
	return sx, sy, node, nil
}

// ClickAt moves the pointer to (x, y), in whole-desktop coordinates, and clicks the left button. The point is mapped into whichever granted stream covers it first, since that is what the portal's absolute motion takes.
func (s *Session) ClickAt(x, y float64) error { return s.clickButton(x, y, btnLeft) }

// RightClickAt moves the pointer to (x, y), in whole-desktop coordinates, and clicks the right button, which is what opens a context menu. Input: the point in whole-desktop logical pixels. Output: the portal's error, or a CoordinateError when the point is on none of the monitors this session was granted.
func (s *Session) RightClickAt(x, y float64) error { return s.clickButton(x, y, btnRight) }

// clickButton moves the pointer to a whole-desktop point and presses and releases one evdev button there. Input: the point in whole-desktop logical pixels and the button code (btnLeft or btnRight). Output: the first error from the mapping, the motion, or either half of the press.
func (s *Session) clickButton(x, y float64, button int32) error {
	x, y, node, err := s.coordinateForStream(x, y)
	if err != nil {
		return err
	}
	s.acting.Lock()
	defer s.acting.Unlock()
	handle, err := s.currentHandle()
	if err != nil {
		return err
	}
	if err := s.notifyMotion(handle, node, x, y); err != nil {
		return err
	}
	if err := s.notifyButton(handle, button, keyStatePressed); err != nil {
		return err
	}
	return s.notifyButton(handle, button, keyStateReleased)
}

// ScrollAt moves the pointer to (x, y), in whole-desktop coordinates, and scrolls dy discrete vertical steps (positive is down).
func (s *Session) ScrollAt(x, y float64, dy int32) error {
	x, y, node, err := s.coordinateForStream(x, y)
	if err != nil {
		return err
	}
	s.acting.Lock()
	defer s.acting.Unlock()
	handle, err := s.currentHandle()
	if err != nil {
		return err
	}
	if err := s.notifyMotion(handle, node, x, y); err != nil {
		return err
	}
	return s.call("NotifyPointerAxisDiscrete", dbus.ObjectPath(handle), map[string]dbus.Variant{}, axisVertical, dy)
}

// Monitors reports how many ScreenCast streams — one per monitor the consent dialog granted, or restored from a saved token — this session covers.
func (s *Session) Monitors() int {
	return len(s.streams)
}

// The Notify* methods below are fire-and-forget: they have no Request/Response handshake, just a plain method reply.

func (s *Session) notifyKeycode(handle string, code int32, state uint32) error {
	return s.call("NotifyKeyboardKeycode", dbus.ObjectPath(handle), map[string]dbus.Variant{}, code, state)
}

func (s *Session) notifyKeysym(handle string, sym int32, state uint32) error {
	return s.call("NotifyKeyboardKeysym", dbus.ObjectPath(handle), map[string]dbus.Variant{}, sym, state)
}

func (s *Session) notifyButton(handle string, button int32, state uint32) error {
	return s.call("NotifyPointerButton", dbus.ObjectPath(handle), map[string]dbus.Variant{}, button, state)
}

func (s *Session) notifyMotion(handle string, node uint32, x, y float64) error {
	err := s.call("NotifyPointerMotionAbsolute", dbus.ObjectPath(handle), map[string]dbus.Variant{}, node, x, y)
	// Mutter answers "Invalid position" for a point outside the stream it was sent against, which toStream should have already caught by picking the right stream for the point; this is the backstop for whatever toStream missed (a stream this session was never actually granted, or a gap between two monitors that neither stream's rectangle covers). Measured on this desk on 2026-09-08 with clicks at x=3750 on a two-monitor desk, back when a session held only one stream.
	if err != nil && strings.Contains(err.Error(), "Invalid position") {
		return fmt.Errorf("the point is outside every monitor this pointer session reaches (granted %d monitor(s)): %w", s.Monitors(), err)
	}
	return err
}

// realPortal implements portalOps against a live session bus connection.
type realPortal struct {
	conn *dbus.Conn
}

// createSessionOptions builds CreateSession's options. The portal refuses the call with "Missing token" unless session_handle_token names the session object it should create; handle_token for the request itself is added by portalRequest.
func createSessionOptions() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"session_handle_token": dbus.MakeVariant(fmt.Sprintf("june_session_%d", reqSeq.Add(1))),
	}
}

// createSession opens a new RemoteDesktop session and returns its session_handle.
func (p *realPortal) createSession(ctx context.Context) (string, error) {
	results, err := portalRequest(ctx, p.conn, remoteDesktopIface+".CreateSession", nil, createSessionOptions())
	if err != nil {
		return "", err
	}
	v, ok := results["session_handle"]
	if !ok {
		return "", fmt.Errorf("response missing session_handle")
	}
	handle, ok := v.Value().(string)
	if !ok || handle == "" {
		return "", fmt.Errorf("session_handle not a string")
	}
	return handle, nil
}

// selectDevicesOptions builds SelectDevices' options: both keyboard and pointer, persisted until explicitly revoked, restoring a prior grant when restoreToken is non-empty.
func selectDevicesOptions(restoreToken string) map[string]dbus.Variant {
	opts := map[string]dbus.Variant{
		"types":        dbus.MakeVariant(deviceKeyboard | devicePointer),
		"persist_mode": dbus.MakeVariant(uint32(2)),
	}
	if restoreToken != "" {
		opts["restore_token"] = dbus.MakeVariant(restoreToken)
	}
	return opts
}

func (p *realPortal) selectDevices(ctx context.Context, handle, restoreToken string) error {
	_, err := portalRequest(ctx, p.conn, remoteDesktopIface+".SelectDevices",
		[]interface{}{dbus.ObjectPath(handle)}, selectDevicesOptions(restoreToken))
	return err
}

// selectSourcesOptions restricts the ScreenCast sources to monitors with the cursor hidden from the video, since the stream here is only ever used for its PipeWire node id, never viewed. multiple lets the user pick every monitor in the consent dialog instead of just one, which is what lets the pointer reach every screen on a multi-monitor desk; with a saved restore_token the earlier choice (however many monitors that was) is what comes back, not this flag.
func selectSourcesOptions() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"types":       dbus.MakeVariant(uint32(1)), // monitor
		"cursor_mode": dbus.MakeVariant(uint32(1)), // hidden
		"multiple":    dbus.MakeVariant(true),
	}
}

func (p *realPortal) selectSources(ctx context.Context, handle string) error {
	_, err := portalRequest(ctx, p.conn, screenCastIface+".SelectSources",
		[]interface{}{dbus.ObjectPath(handle)}, selectSourcesOptions())
	return err
}

// start begins the session (the point at which the one-time consent dialog appears) and returns every granted ScreenCast stream's PipeWire node id and monitor size if reported, and any restore_token to save for next time.
func (p *realPortal) start(ctx context.Context, handle string) (streams []streamInfo, restoreToken string, err error) {
	results, err := portalRequest(ctx, p.conn, remoteDesktopIface+".Start",
		[]interface{}{dbus.ObjectPath(handle), ""}, map[string]dbus.Variant{})
	if err != nil {
		return nil, "", err
	}
	if v, ok := results["restore_token"]; ok {
		restoreToken, _ = v.Value().(string)
	}
	if v, ok := results["streams"]; ok {
		streams = parseStreams(v)
	}
	return streams, restoreToken, nil
}

func (p *realPortal) closeSession(handle string) error {
	return p.conn.Object(portalDest, dbus.ObjectPath(handle)).Call(sessionIface+".Close", 0).Err
}

// parseStreams pulls the PipeWire node id and, when present, the granted position and size of every stream out of a Start response's "streams" result: an array of (u node_id, a{sv} props) structs, one per monitor the consent dialog granted.
// The outer value is a{sv}'s "streams" entry with signature a(ua{sv}), and godbus decodes that whole array as [][]interface{} — one []interface{}{uint32, map[string]dbus.Variant} per stream. It is the array that is typed, not each element: asserting []interface{} on the outer value never matched, so this returned nil for every real reply and every session ran with no monitor to aim at. The inner "position" and "size" properties are (ii) structs on their own, each decoded as []interface{} of two int32, which is what pairOf reads.
// Each entry's rectangle is zero for whichever of position or size the compositor did not report. A malformed entry is skipped rather than aborting the whole list.
func parseStreams(v dbus.Variant) []streamInfo {
	raw, ok := v.Value().([][]interface{})
	if !ok || len(raw) == 0 {
		return nil
	}
	streams := make([]streamInfo, 0, len(raw))
	for _, entry := range raw {
		if len(entry) == 0 {
			continue
		}
		node, _ := entry[0].(uint32)
		var rect streamRect
		if len(entry) >= 2 {
			if props, ok := entry[1].(map[string]dbus.Variant); ok {
				rect.X, rect.Y = pairOf(props["position"])
				rect.W, rect.H = pairOf(props["size"])
			}
		}
		streams = append(streams, streamInfo{Node: node, Rect: rect})
	}
	return streams
}

// pairOf reads a portal (ii) struct property, which godbus hands over as []interface{} of two int32. Input: the property, which may be the zero Variant when the compositor did not report it. Output: the two numbers, or 0, 0 when it is missing or not that shape.
func pairOf(v dbus.Variant) (int, int) {
	pair, ok := v.Value().([]interface{})
	if !ok || len(pair) != 2 {
		return 0, 0
	}
	a, aok := pair[0].(int32)
	b, bok := pair[1].(int32)
	if !aok || !bok {
		return 0, 0
	}
	return int(a), int(b)
}

// predictRequestPath computes the Request object path the portal will use to reply to a call made with the given handle_token, per the spec: /org/freedesktop/portal/desktop/request/SENDER/TOKEN, where SENDER is the caller's own unique bus name with the leading ':' dropped and '.' replaced by '_'. Computing it up front lets the caller subscribe to the Response signal before making the call, so a fast reply is never missed.
func predictRequestPath(uniqueName, token string) dbus.ObjectPath {
	sender := strings.TrimPrefix(uniqueName, ":")
	sender = strings.ReplaceAll(sender, ".", "_")
	return dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + sender + "/" + token)
}

// parseResponse decodes a portal Request.Response signal body: (u response_code, a{sv} results). code 0 means success; any other code means the user cancelled or the backend declined.
func parseResponse(body []interface{}) (map[string]dbus.Variant, error) {
	if len(body) != 2 {
		return nil, fmt.Errorf("unexpected response body len %d", len(body))
	}
	code, _ := body[0].(uint32)
	results, _ := body[1].(map[string]dbus.Variant)
	if code != 0 {
		return results, fmt.Errorf("request declined or cancelled (code %d)", code)
	}
	return results, nil
}

// portalRequest calls a portal method that follows the Request pattern (the method returns a Request object path, and the actual result arrives asynchronously as that object's Response signal), and returns the response's results dict. It adds handle_token to options itself so the predicted subscription path always matches the one the portal replies to.
func portalRequest(ctx context.Context, conn *dbus.Conn, method string, args []interface{}, options map[string]dbus.Variant) (map[string]dbus.Variant, error) {
	token := fmt.Sprintf("june_input_%d", reqSeq.Add(1))
	options["handle_token"] = dbus.MakeVariant(token)
	handlePath := predictRequestPath(conn.Names()[0], token)

	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(handlePath),
		dbus.WithMatchInterface(requestIface),
		dbus.WithMatchMember("Response"),
	); err != nil {
		return nil, fmt.Errorf("subscribe response: %w", err)
	}
	defer conn.RemoveMatchSignal( //nolint:errcheck
		dbus.WithMatchObjectPath(handlePath),
		dbus.WithMatchInterface(requestIface),
		dbus.WithMatchMember("Response"),
	)

	sigCh := make(chan *dbus.Signal, 4)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

	callArgs := append(append([]interface{}{}, args...), options)
	var returned dbus.ObjectPath
	if err := conn.Object(portalDest, portalPath).CallWithContext(ctx, method, 0, callArgs...).Store(&returned); err != nil {
		return nil, fmt.Errorf("call %s: %w", method, err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case sig := <-sigCh:
			if sig.Path != handlePath && sig.Path != returned {
				continue
			}
			return parseResponse(sig.Body)
		}
	}
}
