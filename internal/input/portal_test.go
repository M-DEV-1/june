//go:build linux

package input

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// The Request object path is predictable from the caller's own unique bus name and the handle_token, per the portal spec, so the Response signal can be subscribed to before the call is made.
func TestPredictRequestPath(t *testing.T) {
	got := predictRequestPath(":1.234", "tok1")
	want := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/1_234/tok1")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// SelectDevices asks for both keyboard and pointer, with persist_mode 2 (persist until revoked) so consent survives across runs.
func TestSelectDevicesOptionsNoToken(t *testing.T) {
	opts := selectDevicesOptions("")
	if got := opts["types"].Value().(uint32); got != deviceKeyboard|devicePointer {
		t.Fatalf("types = %v, want %v", got, deviceKeyboard|devicePointer)
	}
	if got := opts["persist_mode"].Value().(uint32); got != 2 {
		t.Fatalf("persist_mode = %v, want 2", got)
	}
	if _, ok := opts["restore_token"]; ok {
		t.Fatal("restore_token should be absent when none is stored")
	}
}

// A stored restore_token is forwarded so the compositor can skip the consent dialog.
func TestSelectDevicesOptionsWithToken(t *testing.T) {
	opts := selectDevicesOptions("saved-token")
	got, ok := opts["restore_token"]
	if !ok {
		t.Fatal("restore_token missing")
	}
	if got.Value().(string) != "saved-token" {
		t.Fatalf("restore_token = %v, want saved-token", got.Value())
	}
}

// SelectSources restricts to monitors and hides the cursor from the stream, since the stream is only ever used for its node id, not viewed.
func TestSelectSourcesOptions(t *testing.T) {
	opts := selectSourcesOptions()
	if got := opts["types"].Value().(uint32); got != 1 {
		t.Fatalf("types = %v, want 1 (monitor)", got)
	}
	if got := opts["cursor_mode"].Value().(uint32); got != 1 {
		t.Fatalf("cursor_mode = %v, want 1 (hidden)", got)
	}
}

// parseResponse reports success (code 0) with its results dict.
func TestParseResponseSuccess(t *testing.T) {
	results, err := parseResponse([]interface{}{
		uint32(0),
		map[string]dbus.Variant{"session_handle": dbus.MakeVariant("/session/1")},
	})
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if results["session_handle"].Value().(string) != "/session/1" {
		t.Fatalf("unexpected results %v", results)
	}
}

// A non-zero response code (user cancelled or declined) is reported as an error.
func TestParseResponseDeclined(t *testing.T) {
	_, err := parseResponse([]interface{}{uint32(1), map[string]dbus.Variant{}})
	if err == nil {
		t.Fatal("expected error for declined request")
	}
}

// A malformed response body is reported instead of panicking on a bad type assertion.
func TestParseResponseMalformed(t *testing.T) {
	if _, err := parseResponse([]interface{}{uint32(0)}); err == nil {
		t.Fatal("expected error for short body")
	}
}

// parseStream pulls the PipeWire node id and, when present, the monitor's (width, height) out of the Start response's "streams" array of (u, a{sv}) structs, which godbus decodes as []interface{} of []interface{} since the struct's Go shape isn't known statically. The "size" property is itself a (ii) struct, decoded the same way.
func TestParseStreamWithSize(t *testing.T) {
	streams := dbus.MakeVariant([]interface{}{
		[]interface{}{uint32(42), map[string]dbus.Variant{
			"size": dbus.MakeVariant([]interface{}{int32(1920), int32(1080)}),
		}},
	})
	node, rect := parseStream(streams)
	if node != 42 || rect.W != 1920 || rect.H != 1080 {
		t.Fatalf("got (%d, %+v), want node 42 sized 1920x1080", node, rect)
	}
}

// No "size" property (some compositors omit it) yields width and height 0, which callers treat as "unknown bounds".
func TestParseStreamNoSize(t *testing.T) {
	streams := dbus.MakeVariant([]interface{}{
		[]interface{}{uint32(7), map[string]dbus.Variant{}},
	})
	node, rect := parseStream(streams)
	if node != 7 || rect != (streamRect{}) {
		t.Fatalf("got (%d, %+v), want node 7 with no rectangle", node, rect)
	}
}

// An empty or malformed streams value yields all zeros rather than panicking; ClickAt still works for relative-only compositors, just without absolute positioning.
func TestParseStreamEmpty(t *testing.T) {
	node, rect := parseStream(dbus.MakeVariant([]interface{}{}))
	if node != 0 || rect != (streamRect{}) {
		t.Fatalf("got (%d, %+v), want zeros", node, rect)
	}
}

// With known monitor bounds from Open, an out-of-range coordinate is rejected with a typed error instead of being passed through to the portal.
func TestToStreamKnownBounds(t *testing.T) {
	rect := streamRect{W: 1920, H: 1080}
	if _, _, err := toStream(-1, 100, rect); err == nil {
		t.Fatal("expected error for negative x")
	}
	if _, _, err := toStream(100, 1081, rect); err == nil {
		t.Fatal("expected error for y past height")
	}
	if _, _, err := toStream(1920, 1080, rect); err != nil {
		t.Fatalf("boundary coordinate should be valid: %v", err)
	}
	var coordErr *CoordinateError
	if _, _, err := toStream(-1, 0, rect); !errors.As(err, &coordErr) {
		t.Fatalf("error should be a *CoordinateError, got %T", err)
	}
}

// Close clears the handle under the lock; a concurrent reader must never observe a torn or already-cleared value except as the documented "session closed" state. Run with -race: without the mutex this trips the race detector even though it never touches a live D-Bus connection.
func TestSessionHandleRaceWithClose(t *testing.T) {
	s := &Session{handle: "/session/1"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			s.currentHandle() //nolint:errcheck
		}
	}()
	s.closeLocked()
	<-done
}

// openSequence must close any session it opened before returning an error, at every failure point after createSession succeeds, so a failed Open never leaks a live portal session. The three portal calls that can fail after createSession — SelectDevices, SelectSources, and Start — must all trigger the same close.
func TestOpenSequenceClosesOnFailure(t *testing.T) {
	cases := []struct {
		name string
		p    *fakePortal
	}{
		{"SelectDevices fails", &fakePortal{selectDevicesErr: errBoom}},
		{"SelectSources fails", &fakePortal{selectSourcesErr: errBoom}},
		{"Start fails", &fakePortal{startErr: errBoom}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, _, err := openSequence(context.Background(), c.p, "")
			if !errors.Is(err, errBoom) {
				t.Fatalf("err = %v, want errBoom", err)
			}
			if c.p.closedHandle != "session-1" {
				t.Fatalf("closedHandle = %q, want session-1", c.p.closedHandle)
			}
		})
	}
}

// A createSession failure has no handle to leak, so closeSession must never be called.
func TestOpenSequenceCreateSessionFailureNoClose(t *testing.T) {
	p := &fakePortal{createSessionErr: errBoom}
	_, _, _, _, err := openSequence(context.Background(), p, "")
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	if p.closedHandle != "" {
		t.Fatalf("closedHandle = %q, want none", p.closedHandle)
	}
}

// The success path never calls closeSession.
func TestOpenSequenceSuccessNoClose(t *testing.T) {
	p := &fakePortal{}
	handle, stream, rect, _, err := openSequence(context.Background(), p, "")
	if err != nil {
		t.Fatalf("openSequence: %v", err)
	}
	if handle != "session-1" || stream != 42 || rect != (streamRect{W: 1920, H: 1080}) {
		t.Fatalf("got (%q, %d, %+v)", handle, stream, rect)
	}
	if p.closedHandle != "" {
		t.Fatalf("closedHandle = %q, want none", p.closedHandle)
	}
}

var errBoom = errors.New("boom")

// fakePortal is a portalOps double letting the openSequence tests force a failure at any step without a live D-Bus session.
type fakePortal struct {
	createSessionErr error
	selectDevicesErr error
	selectSourcesErr error
	startErr         error
	closedHandle     string
}

func (f *fakePortal) createSession(context.Context) (string, error) {
	if f.createSessionErr != nil {
		return "", f.createSessionErr
	}
	return "session-1", nil
}

func (f *fakePortal) selectDevices(_ context.Context, _ string, _ string) error {
	return f.selectDevicesErr
}

func (f *fakePortal) selectSources(_ context.Context, _ string) error {
	return f.selectSourcesErr
}

func (f *fakePortal) start(_ context.Context, _ string) (uint32, streamRect, string, error) {
	if f.startErr != nil {
		return 0, streamRect{}, "", f.startErr
	}
	return 42, streamRect{W: 1920, H: 1080}, "", nil
}

func (f *fakePortal) closeSession(handle string) error {
	f.closedHandle = handle
	return nil
}

// onceInput opens exactly one Session for the whole agent, and two asks can drive it at once. Each of PressKey, TypeText, ClickAt and ScrollAt is several separate D-Bus events paced keyDelay apart, so without a lock held for the whole call one ask's Ctrl press lands between another ask's key down and key up and the compositor chords it into that ask's keystrokes. Recorded here through a stub transport: every chord's four events must appear together, never one chord inside another.
func TestPressKeyChordsDoNotInterleave(t *testing.T) {
	var mu sync.Mutex
	var events [][2]int32
	s := &Session{handle: "/session/1", send: func(_ context.Context, _ string, args ...interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, [2]int32{args[2].(int32), int32(args[3].(uint32))})
		return nil
	}}

	const rounds = 5
	var wg sync.WaitGroup
	for _, name := range []string{"Ctrl+A", "Shift+B"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if err := s.PressKey(name); err != nil {
					t.Errorf("PressKey(%q): %v", name, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	ctrlA := [][2]int32{{29, 1}, {30, 1}, {30, 0}, {29, 0}}
	shiftB := [][2]int32{{42, 1}, {48, 1}, {48, 0}, {42, 0}}
	if len(events) != 2*rounds*4 {
		t.Fatalf("recorded %d events, want %d", len(events), 2*rounds*4)
	}
	for i := 0; i < len(events); i += 4 {
		block := events[i : i+4]
		if !sameEvents(block, ctrlA) && !sameEvents(block, shiftB) {
			t.Fatalf("events %d-%d are %v: one chord was sent inside another", i, i+3, block)
		}
	}
}

// sameEvents reports whether a run of recorded (keycode, state) events is exactly the wanted chord.
func sameEvents(got [][2]int32, want [][2]int32) bool {
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A chord used to be one flat press-all-then-release-all list handed to paceEvents, which stops at the first error, so a press that failed halfway through skipped every release after it and left the modifiers held down from the compositor's point of view — the next key or click the model sent was then silently chorded with Ctrl+Shift. Every key that actually went down has to come back up, in reverse order, whether the rest of the chord made it or not.
func TestPressKeyReleasesWhatItPressedWhenAPressFails(t *testing.T) {
	var events [][2]int32
	sent := 0
	s := &Session{handle: "/session/1", send: func(_ context.Context, _ string, args ...interface{}) error {
		sent++
		if sent == 3 {
			return errBoom
		}
		events = append(events, [2]int32{args[2].(int32), int32(args[3].(uint32))})
		return nil
	}}

	err := s.PressKey("Ctrl+Shift+L")
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	want := [][2]int32{{29, 1}, {42, 1}, {42, 0}, {29, 0}}
	if len(events) != len(want) || !sameEvents(events, want) {
		t.Fatalf("events = %v, want %v (Ctrl and Shift released in reverse order after L failed)", events, want)
	}
}

// The stream's "position" property says where the granted monitor's top-left corner sits on the desktop, which is what the pointer coordinates have to be measured from.
func TestParseStreamWithPosition(t *testing.T) {
	streams := dbus.MakeVariant([]interface{}{
		[]interface{}{uint32(42), map[string]dbus.Variant{
			"position": dbus.MakeVariant([]interface{}{int32(1920), int32(0)}),
			"size":     dbus.MakeVariant([]interface{}{int32(2560), int32(1440)}),
		}},
	})
	node, rect := parseStream(streams)
	if node != 42 || rect != (streamRect{X: 1920, Y: 0, W: 2560, H: 1440}) {
		t.Fatalf("got (%d, %+v), want node 42 at 1920,0 sized 2560x1440", node, rect)
	}
}

// NotifyPointerMotionAbsolute takes coordinates in the granted stream's own space, while a click or a scroll arrives in whole-desktop coordinates read off a whole-desktop screenshot. On a monitor that does not start at the desktop origin the two differ by that monitor's position, and a click on the second screen used to be refused as outside the monitor or land on the first one.
func TestToStreamSubtractsTheMonitorOrigin(t *testing.T) {
	rect := streamRect{X: 1920, Y: 0, W: 2560, H: 1440}
	x, y, err := toStream(2020, 100, rect)
	if err != nil || x != 100 || y != 100 {
		t.Fatalf("toStream = (%v, %v, %v), want (100, 100, nil)", x, y, err)
	}
	// A point on the other monitor is off this stream entirely, and is refused rather than sent as a negative coordinate.
	if _, _, err := toStream(100, 100, rect); err == nil {
		t.Fatal("a point left of the granted monitor should be refused")
	}
	// Past the far edge of the granted monitor, measured from its own origin, is refused too.
	if _, _, err := toStream(4481, 100, rect); err == nil {
		t.Fatal("a point past the granted monitor's width should be refused")
	}
}

// A compositor that reports neither position nor size leaves the behaviour as it was: the point is passed through untouched and only a negative one is refused.
func TestToStreamWithoutStreamProperties(t *testing.T) {
	x, y, err := toStream(50000, 50000, streamRect{})
	if err != nil || x != 50000 || y != 50000 {
		t.Fatalf("toStream = (%v, %v, %v), want the point passed through", x, y, err)
	}
	if _, _, err := toStream(-1, 0, streamRect{}); err == nil {
		t.Fatal("a negative coordinate should be refused")
	}
}

// Every RemoteDesktop event is a plain method call with no dialog behind it, and it is made with s.acting held. A portal that has stopped answering must cost one call its timeout rather than wedging every screen action in the daemon until a restart.
func TestCallTimesOutWhenThePortalDoesNotAnswer(t *testing.T) {
	old := portalCallTimeout
	portalCallTimeout = 50 * time.Millisecond
	defer func() { portalCallTimeout = old }()

	s := &Session{handle: "/session/1", send: func(ctx context.Context, _ string, _ ...interface{}) error {
		<-ctx.Done()
		return ctx.Err()
	}}

	done := make(chan error, 1)
	go func() { done <- s.PressKey("Enter") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("PressKey = nil, want the call's own timeout")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PressKey never returned; the call has no timeout")
	}
}

// The stream's position is in the compositor's logical layout space while its size is the stream's own video size, which on a scaled monitor is that monitor's device resolution. A logical x of 1900 on a 2x monitor is 3800 stream pixels, not 1900: sending it unscaled passed the bound check against a 3840-wide stream and landed the pointer at half the intended position.
func TestToStreamScalesToTheStreamsOwnPixels(t *testing.T) {
	// A 1920x1080 monitor at the desktop origin whose stream is 3840x2160: two stream pixels to one logical pixel.
	rect := withScaleFrom(streamRect{X: 0, Y: 0, W: 3840, H: 2160}, func(x, y int) (int, int, bool) { return 1920, 1080, true })
	if rect.Scale != 2 {
		t.Fatalf("scale = %v, want 2 from a 3840-wide stream on a 1920-wide monitor", rect.Scale)
	}
	x, y, err := toStream(1900, 1000, rect)
	if err != nil || x != 3800 || y != 2000 {
		t.Fatalf("toStream = (%v, %v, %v), want (3800, 2000, nil)", x, y, err)
	}
	// The far corner of the monitor is still the far corner of the stream, and a point beyond it is still refused.
	if _, _, err := toStream(1921, 0, rect); err == nil {
		t.Fatal("a point past the monitor's logical width should be refused")
	}

	// A second monitor at 1920,0 logical, 2560x1440 logical, streamed at 5120x2880.
	second := withScaleFrom(streamRect{X: 1920, Y: 0, W: 5120, H: 2880}, func(x, y int) (int, int, bool) { return 2560, 1440, true })
	x, y, err = toStream(2020, 100, second)
	if err != nil || x != 200 || y != 200 {
		t.Fatalf("toStream on the second monitor = (%v, %v, %v), want (200, 200, nil)", x, y, err)
	}
}

// An unscaled monitor, and a desktop whose monitor layout cannot be read at all, both map exactly as they did before the scale existed.
func TestToStreamAtScaleOneIsUnchanged(t *testing.T) {
	unscaled := withScaleFrom(streamRect{X: 1920, Y: 0, W: 2560, H: 1440}, func(x, y int) (int, int, bool) { return 2560, 1440, true })
	unknown := withScaleFrom(streamRect{X: 1920, Y: 0, W: 2560, H: 1440}, func(x, y int) (int, int, bool) { return 0, 0, false })
	if unscaled.Scale != 1 || unknown.Scale != 1 {
		t.Fatalf("scales = %v and %v, want 1 and 1", unscaled.Scale, unknown.Scale)
	}
	for _, rect := range []streamRect{unscaled, unknown, {X: 1920, Y: 0, W: 2560, H: 1440}} {
		x, y, err := toStream(2020, 100, rect)
		if err != nil || x != 100 || y != 100 {
			t.Errorf("toStream(%+v) = (%v, %v, %v), want (100, 100, nil)", rect, x, y, err)
		}
	}
}

// withScaleFrom is withStreamScale against a stubbed monitor layout, so the scale can be worked out in a test without a desktop.
func withScaleFrom(r streamRect, layout func(x, y int) (int, int, bool)) streamRect {
	old := monitorLayout
	monitorLayout = layout
	defer func() { monitorLayout = old }()
	return withStreamScale(r)
}
