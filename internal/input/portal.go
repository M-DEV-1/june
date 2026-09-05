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

// btnLeft is BTN_LEFT from linux/input-event-codes.h, the evdev button code NotifyPointerButton expects.
const btnLeft int32 = 0x110

var reqSeq atomic.Uint64

// errClosed is returned by any Session method called after Close.
var errClosed = errors.New("input: session is closed")

// CoordinateError reports a pointer coordinate rejected before it reached the portal.
type CoordinateError struct {
	X, Y          float64
	Width, Height int // the known monitor bounds, or 0 if the stream never reported a size
}

func (e *CoordinateError) Error() string {
	if e.Width == 0 && e.Height == 0 {
		return fmt.Sprintf("input: coordinate (%.0f, %.0f) is negative", e.X, e.Y)
	}
	return fmt.Sprintf("input: coordinate (%.0f, %.0f) is outside the %dx%d monitor", e.X, e.Y, e.Width, e.Height)
}

// Session is an open RemoteDesktop portal session that can inject keyboard and pointer input into the GNOME Wayland desktop.
type Session struct {
	conn   *dbus.Conn
	stream uint32 // ScreenCast PipeWire node id backing absolute pointer motion; 0 if none was granted
	width  int    // monitor size from the stream's "size" property; 0 if the compositor didn't report one
	height int

	// send routes one RemoteDesktop method call; nil means the live D-Bus connection. Tests set it to record the event stream without a bus.
	send func(method string, args ...interface{}) error

	// acting is held for the whole of PressKey, TypeText, ClickAt and ScrollAt, each of which is several separate events paced keyDelay apart. One Session is shared by every ask, and without this one ask's modifier press lands between another's key down and key up.
	acting sync.Mutex

	mu     sync.Mutex
	handle string // session_handle; a plain string despite naming an object path (a documented portal spec quirk kept for backwards compatibility). Guarded by mu because Close clears it while another goroutine may be mid-call.
}

// call sends one RemoteDesktop method to the portal. Input: the method name without its interface, and the arguments it takes. Output: the D-Bus error, or whatever the test stub in send returns.
func (s *Session) call(method string, args ...interface{}) error {
	if s.send != nil {
		return s.send(method, args...)
	}
	return s.conn.Object(portalDest, portalPath).Call(remoteDesktopIface+"."+method, 0, args...).Err
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

// Open starts a RemoteDesktop session covering keyboard and pointer: it restores a saved restore_token from dataDir if one exists, otherwise the user sees the portal's one-time consent dialog. Also opens a ScreenCast monitor stream, which NotifyPointerMotionAbsolute requires to map (x, y) to a screen. dataDir is created (mode 0700) if missing, since the restore_token must be written there.
func Open(ctx context.Context, dataDir string) (*Session, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}

	handle, stream, width, height, newToken, err := openSequence(ctx, &realPortal{conn: conn}, loadToken(dataDir))
	if err != nil {
		return nil, err
	}
	if err := saveToken(dataDir, newToken); err != nil {
		return nil, fmt.Errorf("save token: %w", err)
	}
	return &Session{conn: conn, handle: handle, stream: stream, width: width, height: height}, nil
}

// portalOps is the set of fallible RemoteDesktop/ScreenCast steps openSequence drives. Splitting it out from Session lets the failure-cleanup logic in openSequence be tested against a fake, without a live D-Bus session.
type portalOps interface {
	createSession(ctx context.Context) (string, error)
	selectDevices(ctx context.Context, handle, restoreToken string) error
	selectSources(ctx context.Context, handle string) error
	start(ctx context.Context, handle string) (stream uint32, width, height int, restoreToken string, err error)
	closeSession(handle string) error
}

// openSequence runs the CreateSession -> SelectDevices -> SelectSources -> Start handshake. If any step after createSession fails, it closes the session it opened before returning the error, so a failed Open never leaks a live portal session (and the orphaned PipeWire stream that comes with it).
func openSequence(ctx context.Context, p portalOps, restoreToken string) (handle string, stream uint32, width, height int, newToken string, err error) {
	handle, err = p.createSession(ctx)
	if err != nil {
		return "", 0, 0, 0, "", err
	}

	if err = p.selectDevices(ctx, handle, restoreToken); err != nil {
		_ = p.closeSession(handle)
		return "", 0, 0, 0, "", err
	}
	if err = p.selectSources(ctx, handle); err != nil {
		_ = p.closeSession(handle)
		return "", 0, 0, 0, "", err
	}

	stream, width, height, newToken, err = p.start(ctx, handle)
	if err != nil {
		_ = p.closeSession(handle)
		return "", 0, 0, 0, "", err
	}
	return handle, stream, width, height, newToken, nil
}

// Close ends the portal session so the compositor can release the associated PipeWire stream. Safe to call more than once.
func (s *Session) Close() error {
	h := s.closeLocked()
	if h == "" {
		return nil
	}
	return s.conn.Object(portalDest, dbus.ObjectPath(h)).Call(sessionIface+".Close", 0).Err
}

// validateCoords rejects a pointer coordinate before it reaches the portal. When the ScreenCast stream reported a monitor size in Open, both bounds are enforced; otherwise only negative coordinates (never valid on any monitor) are rejected.
func (s *Session) validateCoords(x, y float64) error {
	if s.width > 0 || s.height > 0 {
		if x < 0 || y < 0 || x > float64(s.width) || y > float64(s.height) {
			return &CoordinateError{X: x, Y: y, Width: s.width, Height: s.height}
		}
		return nil
	}
	if x < 0 || y < 0 {
		return &CoordinateError{X: x, Y: y}
	}
	return nil
}

// PressKey sends a full press-then-release for the named key or chord (e.g. "Enter", "Ctrl+L"), paced keyDelay apart. Modifiers in a chord are pressed first and released last, in reverse order, so the compositor sees them held down for the whole chord.
// Every key that actually went down is released even when a later press fails, because a modifier left held is chorded by the compositor into whatever the model sends next. Input: the key or chord name. Output: the first error hit, pressing or releasing, or nil.
func (s *Session) PressKey(name string) error {
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
func (s *Session) TypeText(text string) error {
	s.acting.Lock()
	defer s.acting.Unlock()
	handle, err := s.currentHandle()
	if err != nil {
		return err
	}

	var events []func() error
	for _, r := range text {
		sym, ok := runeKeysym(r)
		if !ok {
			continue
		}
		events = append(events,
			func() error { return s.notifyKeysym(handle, sym, keyStatePressed) },
			func() error { return s.notifyKeysym(handle, sym, keyStateReleased) },
		)
	}
	return paceEvents(events, keyDelay, time.Sleep)
}

// ClickAt moves the pointer to (x, y), in the ScreenCast stream's logical coordinate space, and clicks the left button.
func (s *Session) ClickAt(x, y float64) error {
	if err := s.validateCoords(x, y); err != nil {
		return err
	}
	s.acting.Lock()
	defer s.acting.Unlock()
	handle, err := s.currentHandle()
	if err != nil {
		return err
	}
	if err := s.notifyMotion(handle, x, y); err != nil {
		return err
	}
	if err := s.notifyButton(handle, btnLeft, keyStatePressed); err != nil {
		return err
	}
	return s.notifyButton(handle, btnLeft, keyStateReleased)
}

// ScrollAt moves the pointer to (x, y) and scrolls dy discrete vertical steps (positive is down).
func (s *Session) ScrollAt(x, y float64, dy int32) error {
	if err := s.validateCoords(x, y); err != nil {
		return err
	}
	s.acting.Lock()
	defer s.acting.Unlock()
	handle, err := s.currentHandle()
	if err != nil {
		return err
	}
	if err := s.notifyMotion(handle, x, y); err != nil {
		return err
	}
	return s.call("NotifyPointerAxisDiscrete", dbus.ObjectPath(handle), map[string]dbus.Variant{}, axisVertical, dy)
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

func (s *Session) notifyMotion(handle string, x, y float64) error {
	return s.call("NotifyPointerMotionAbsolute", dbus.ObjectPath(handle), map[string]dbus.Variant{}, s.stream, x, y)
}

// realPortal implements portalOps against a live session bus connection.
type realPortal struct {
	conn *dbus.Conn
}

// createSession opens a new RemoteDesktop session and returns its session_handle.
func (p *realPortal) createSession(ctx context.Context) (string, error) {
	results, err := portalRequest(ctx, p.conn, remoteDesktopIface+".CreateSession", nil, map[string]dbus.Variant{})
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

// selectSourcesOptions restricts the ScreenCast stream to monitors with the cursor hidden from the video, since the stream here is only ever used for its PipeWire node id, never viewed.
func selectSourcesOptions() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"types":       dbus.MakeVariant(uint32(1)), // monitor
		"cursor_mode": dbus.MakeVariant(uint32(1)), // hidden
	}
}

func (p *realPortal) selectSources(ctx context.Context, handle string) error {
	_, err := portalRequest(ctx, p.conn, screenCastIface+".SelectSources",
		[]interface{}{dbus.ObjectPath(handle)}, selectSourcesOptions())
	return err
}

// start begins the session (the point at which the one-time consent dialog appears) and returns the ScreenCast stream's PipeWire node id, its monitor size if reported, and any restore_token to save for next time.
func (p *realPortal) start(ctx context.Context, handle string) (stream uint32, width, height int, restoreToken string, err error) {
	results, err := portalRequest(ctx, p.conn, remoteDesktopIface+".Start",
		[]interface{}{dbus.ObjectPath(handle), ""}, map[string]dbus.Variant{})
	if err != nil {
		return 0, 0, 0, "", err
	}
	if v, ok := results["restore_token"]; ok {
		restoreToken, _ = v.Value().(string)
	}
	if v, ok := results["streams"]; ok {
		stream, width, height = parseStream(v)
	}
	return stream, width, height, restoreToken, nil
}

func (p *realPortal) closeSession(handle string) error {
	return p.conn.Object(portalDest, dbus.ObjectPath(handle)).Call(sessionIface+".Close", 0).Err
}

// parseStream pulls the PipeWire node id and, when present, the monitor's (width, height) out of a Start response's "streams" result: an array of (u node_id, a{sv} props) structs. godbus decodes an unknown-shape D-Bus struct as []interface{}, so each stream arrives as []interface{}{uint32, map[string]dbus.Variant}, and its "size" property (itself a (ii) struct) the same way. Returns all zeros if no stream, or no size, is present.
func parseStream(v dbus.Variant) (node uint32, width, height int) {
	streams, ok := v.Value().([]interface{})
	if !ok || len(streams) == 0 {
		return 0, 0, 0
	}
	entry, ok := streams[0].([]interface{})
	if !ok || len(entry) == 0 {
		return 0, 0, 0
	}
	node, _ = entry[0].(uint32)
	if len(entry) < 2 {
		return node, 0, 0
	}
	props, ok := entry[1].(map[string]dbus.Variant)
	if !ok {
		return node, 0, 0
	}
	size, ok := props["size"]
	if !ok {
		return node, 0, 0
	}
	dims, ok := size.Value().([]interface{})
	if !ok || len(dims) != 2 {
		return node, 0, 0
	}
	w, wok := dims[0].(int32)
	h, hok := dims[1].(int32)
	if !wok || !hok {
		return node, 0, 0
	}
	return node, int(w), int(h)
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
	token := fmt.Sprintf("ora_input_%d", reqSeq.Add(1))
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
