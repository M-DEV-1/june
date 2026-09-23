//go:build linux

package input

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// A malformed response body is reported instead of panicking on a bad type assertion.
func TestParseResponseMalformed(t *testing.T) {
	if _, err := parseResponse([]interface{}{uint32(0)}); err == nil {
		t.Fatal("expected error for short body")
	}
}

// parseStreams pulls the PipeWire node id and, when present, the monitor's position and size out of every entry in the Start response's "streams" array of (u, a{sv}) structs, one per monitor the consent dialog granted. No "size" property (some compositors omit it) yields width and height 0, which callers treat as unknown bounds.
// Every case goes through a real encode and decode, because the types godbus produces are the whole point here: this test used to hand-build the Variant from a Go []interface{} literal, which is not what the decoder ever returns for a(ua{sv}) — it returns [][]interface{}. parseStreams asserted the literal's type, so it matched in the test and never once matched in production: every session ran with zero granted streams, clicks went out with node id 0, and the portal range-checked them against whichever monitor it picked. That is what "Invalid position", "the point is outside the monitor this pointer session reaches" and finally "coordinate (960, 557) is negative" all were.
func TestParseStreams(t *testing.T) {
	// position and size are (ii) structs on the wire, not arrays of int, so they are built as Go structs here too; an []int32 would encode as "ai" and decode to a shape pairOf never sees.
	type pair struct{ A, B int32 }
	type stream struct {
		Node  uint32
		Props map[string]dbus.Variant
	}
	cases := []struct {
		name string
		sent []stream
		want []streamInfo
	}{
		{"with size", []stream{{Node: 42, Props: map[string]dbus.Variant{
			"size": dbus.MakeVariant(pair{1920, 1080}),
		}}}, []streamInfo{{Node: 42, Rect: streamRect{W: 1920, H: 1080}}}},
		{"no size", []stream{{Node: 7, Props: map[string]dbus.Variant{}}}, []streamInfo{{Node: 7}}},
		{"empty", []stream{}, nil},
		{"with position", []stream{{Node: 42, Props: map[string]dbus.Variant{
			"position": dbus.MakeVariant(pair{1920, 0}),
			"size":     dbus.MakeVariant(pair{2560, 1440}),
		}}}, []streamInfo{{Node: 42, Rect: streamRect{X: 1920, W: 2560, H: 1440}}}},
		// Two monitors granted at once, as SelectSources' "multiple" option allows: both entries must survive parsing, not just the first.
		{"two monitors", []stream{
			{Node: 11, Props: map[string]dbus.Variant{
				"position": dbus.MakeVariant(pair{0, 0}),
				"size":     dbus.MakeVariant(pair{1920, 1080}),
			}},
			{Node: 12, Props: map[string]dbus.Variant{
				"position": dbus.MakeVariant(pair{1920, 0}),
				"size":     dbus.MakeVariant(pair{1920, 1080}),
			}},
		}, []streamInfo{
			{Node: 11, Rect: streamRect{W: 1920, H: 1080}},
			{Node: 12, Rect: streamRect{X: 1920, W: 1920, H: 1080}},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseStreams(overTheBus(t, c.sent))
			if len(got) != len(c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %+v, want %+v", got, c.want)
				}
			}
		})
	}
}

// overTheBus encodes one Response signal carrying the given streams and decodes it back, so what reaches parseStreams is the type godbus really produces rather than the one a literal happens to have. Input: the streams as the portal would send them. Output: the decoded "streams" Variant.
func overTheBus(t *testing.T, streams any) dbus.Variant {
	t.Helper()
	results := map[string]dbus.Variant{"streams": dbus.MakeVariant(streams)}
	msg := &dbus.Message{
		Type: dbus.TypeSignal,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldPath:      dbus.MakeVariant(dbus.ObjectPath("/org/freedesktop/portal/desktop/request/1")),
			dbus.FieldInterface: dbus.MakeVariant("org.freedesktop.portal.Request"),
			dbus.FieldMember:    dbus.MakeVariant("Response"),
			dbus.FieldSignature: dbus.MakeVariant(dbus.SignatureOf(uint32(0), results)),
		},
		Body: []interface{}{uint32(0), results},
	}
	var buf bytes.Buffer
	if err := msg.EncodeTo(&buf, binary.LittleEndian); err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := dbus.DecodeMessage(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	decoded, ok := back.Body[1].(map[string]dbus.Variant)
	if !ok {
		t.Fatalf("results decoded as %T, want map[string]dbus.Variant", back.Body[1])
	}
	return decoded["streams"]
}

// A grant that really carries no screen stream must fail at Open, not one click at a time. Without a stream there is no monitor to map a point onto, so every later click fails deep in the mapping with a message about the coordinate rather than about the missing grant.
func TestOpenSequenceRefusesAGrantWithNoStreams(t *testing.T) {
	p := &fakePortal{noStreams: true}
	_, _, _, err := openSequence(context.Background(), p, "")
	if err == nil {
		t.Fatal("openSequence accepted a grant with no screen stream")
	}
	if p.closedHandle != "session-1" {
		t.Errorf("closedHandle = %q, want the refused session closed", p.closedHandle)
	}
}

// A point on the second of two granted monitors maps into that stream's own space, not the first's, and carries that stream's node id so NotifyPointerMotionAbsolute is told the right one.
func TestToStreamPicksTheStreamThatCoversThePoint(t *testing.T) {
	streams := []streamInfo{
		{Node: 11, Rect: streamRect{X: 0, Y: 0, W: 1920, H: 1080}},
		{Node: 12, Rect: streamRect{X: 1920, Y: 0, W: 1920, H: 1080}},
	}
	x, y, node, err := toStream(2850, 423, streams)
	if err != nil {
		t.Fatalf("toStream: %v", err)
	}
	if node != 12 || x != 930 || y != 423 {
		t.Fatalf("toStream = (node %d, %v, %v), want (12, 930, 423)", node, x, y)
	}

	x, y, node, err = toStream(100, 100, streams)
	if err != nil || node != 11 || x != 100 || y != 100 {
		t.Fatalf("toStream on the first monitor = (node %d, %v, %v, %v), want (11, 100, 100, nil)", node, x, y, err)
	}
}

// A point in neither monitor's rectangle — off both edges, or in a gap between two monitors that do not touch — is refused with a CoordinateError naming how many monitors the session covers.
func TestToStreamRejectsAPointOnNeitherMonitor(t *testing.T) {
	streams := []streamInfo{
		{Node: 11, Rect: streamRect{X: 0, Y: 0, W: 1920, H: 1080}},
		{Node: 12, Rect: streamRect{X: 1920, Y: 0, W: 1920, H: 1080}},
	}
	_, _, _, err := toStream(5000, 100, streams)
	var coordErr *CoordinateError
	if !errors.As(err, &coordErr) {
		t.Fatalf("error should be a *CoordinateError, got %T (%v)", err, err)
	}
	if coordErr.Monitors != 2 {
		t.Fatalf("Monitors = %d, want 2", coordErr.Monitors)
	}
	if !strings.Contains(err.Error(), "2 monitor") {
		t.Fatalf("error %q should name the monitor count", err.Error())
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
			_, _, _, err := openSequence(context.Background(), c.p, "")
			if !errors.Is(err, errBoom) {
				t.Fatalf("err = %v, want errBoom", err)
			}
			if c.p.closedHandle != "session-1" {
				t.Fatalf("closedHandle = %q, want session-1", c.p.closedHandle)
			}
		})
	}
}

var errBoom = errors.New("boom")

// fakePortal is a portalOps double letting the openSequence tests force a failure at any step without a live D-Bus session.
type fakePortal struct {
	createSessionErr error
	selectDevicesErr error
	selectSourcesErr error
	startErr         error
	noStreams        bool // the portal answered Start with a grant carrying no screen stream
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

func (f *fakePortal) start(_ context.Context, _ string) ([]streamInfo, string, error) {
	if f.startErr != nil {
		return nil, "", f.startErr
	}
	if f.noStreams {
		return nil, "", nil
	}
	return []streamInfo{{Node: 42, Rect: streamRect{W: 1920, H: 1080}}}, "", nil
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

// A chord used to be one flat press-all-then-release-all list that stopped at the first error, so a press that failed halfway through skipped every release after it and left the modifiers held down from the compositor's point of view — the next key or click the model sent was then silently chorded with Ctrl+Shift. Every key that actually went down has to come back up, in reverse order, whether the rest of the chord made it or not.
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

// TypeText used to run its whole press/release sequence through a helper that stopped at the first error: if a character's own release call failed, that key stayed down from the compositor's point of view for whatever the model sent next (a click, a chord, more typed text), with no attempt to bring it back up. A failed release must be retried once before the error is returned, mirroring the recovery PressKey already does for a chord.
func TestTypeText_RetriesAReleaseThatFailsBeforeReturning(t *testing.T) {
	var events [][3]interface{}
	sent := 0
	s := &Session{handle: "/session/1", send: func(_ context.Context, method string, args ...interface{}) error {
		sent++
		events = append(events, [3]interface{}{method, args[2], args[3]})
		if sent == 2 {
			return errBoom
		}
		return nil
	}}

	err := s.TypeText("ab")
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	// Call 1: press 'a'. Call 2: release 'a', fails. Call 3 must be a retry of that same release, not a press of 'b'.
	if len(events) < 3 {
		t.Fatalf("recorded %d calls, want at least 3 (press, failed release, retried release)", len(events))
	}
	press, firstRelease, retry := events[0], events[1], events[2]
	if press[1].(int32) != 'a' || press[2].(uint32) != keyStatePressed {
		t.Fatalf("call 1 = %v, want a press of 'a'", press)
	}
	if firstRelease[1].(int32) != 'a' || firstRelease[2].(uint32) != keyStateReleased {
		t.Fatalf("call 2 = %v, want a release of 'a'", firstRelease)
	}
	if retry[1].(int32) != 'a' || retry[2].(uint32) != keyStateReleased {
		t.Fatalf("call 3 = %v, want a retried release of 'a', not moving on to 'b'", retry)
	}
}

// NotifyPointerMotionAbsolute takes coordinates in the granted stream's own space, while a click or a scroll arrives in whole-desktop coordinates read off a whole-desktop screenshot. On a monitor that does not start at the desktop origin the two differ by that monitor's position, and a click on the second screen used to be refused as outside the monitor or land on the first one.
func TestToStreamSubtractsTheMonitorOrigin(t *testing.T) {
	streams := []streamInfo{{Node: 1, Rect: streamRect{X: 1920, Y: 0, W: 2560, H: 1440}}}
	x, y, _, err := toStream(2020, 100, streams)
	if err != nil || x != 100 || y != 100 {
		t.Fatalf("toStream = (%v, %v, %v), want (100, 100, nil)", x, y, err)
	}
	// A point on the other monitor is off this stream entirely, and is refused rather than sent as a negative coordinate.
	if _, _, _, err := toStream(100, 100, streams); err == nil {
		t.Fatal("a point left of the granted monitor should be refused")
	}
	// Past the far edge of the granted monitor, measured from its own origin, is refused too.
	if _, _, _, err := toStream(4481, 100, streams); err == nil {
		t.Fatal("a point past the granted monitor's width should be refused")
	}
}

// A compositor that reports neither position nor size leaves the behaviour as it was: the point is passed through untouched and only a negative one is refused.
func TestToStreamWithoutStreamProperties(t *testing.T) {
	streams := []streamInfo{{Node: 1, Rect: streamRect{}}}
	x, y, _, err := toStream(50000, 50000, streams)
	if err != nil || x != 50000 || y != 50000 {
		t.Fatalf("toStream = (%v, %v, %v), want the point passed through", x, y, err)
	}
	if _, _, _, err := toStream(-1, 0, streams); err == nil {
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
	streams := []streamInfo{{Node: 1, Rect: rect}}
	x, y, _, err := toStream(1900, 1000, streams)
	if err != nil || x != 3800 || y != 2000 {
		t.Fatalf("toStream = (%v, %v, %v), want (3800, 2000, nil)", x, y, err)
	}
	// The far corner of the monitor is still the far corner of the stream, and a point beyond it is still refused.
	if _, _, _, err := toStream(1921, 0, streams); err == nil {
		t.Fatal("a point past the monitor's logical width should be refused")
	}

	// A second monitor at 1920,0 logical, 2560x1440 logical, streamed at 5120x2880.
	second := withScaleFrom(streamRect{X: 1920, Y: 0, W: 5120, H: 2880}, func(x, y int) (int, int, bool) { return 2560, 1440, true })
	x, y, _, err = toStream(2020, 100, []streamInfo{{Node: 2, Rect: second}})
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
		x, y, _, err := toStream(2020, 100, []streamInfo{{Node: 1, Rect: rect}})
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

// CreateSession is refused with "Missing token" unless the options name the session object with session_handle_token.
func TestCreateSessionOptionsNameTheSession(t *testing.T) {
	a, b := createSessionOptions(), createSessionOptions()
	ta, _ := a["session_handle_token"].Value().(string)
	tb, _ := b["session_handle_token"].Value().(string)
	if ta == "" || ta == tb {
		t.Fatalf("session_handle_token = %q then %q, want distinct non-empty tokens", ta, tb)
	}
}

// A click aimed off every monitor the session covers gets "Invalid position" back from mutter as the last-resort backstop (toStream should normally catch this first); the error must say so, or the model retries the same point (it tried x=3750 four times on 2026-09-08, back when a session held only one stream and toStream itself would have caught this case).
func TestClickAt_SaysWhenThePointIsOffTheSessionsMonitor(t *testing.T) {
	s := &Session{handle: "/session/1", streams: []streamInfo{{Node: 1, Rect: streamRect{}}}, send: func(_ context.Context, method string, _ ...interface{}) error {
		if method == "NotifyPointerMotionAbsolute" {
			return errors.New("Invalid position")
		}
		return nil
	}}
	err := s.ClickAt(3750, 54)
	if err == nil || !strings.Contains(err.Error(), "outside every monitor") {
		t.Errorf("err = %v, want it to name the monitors the session cannot reach", err)
	}
}

// The portal answers Start with no restore_token on a session it restored from the saved one; the saved token must survive that, or the consent dialog comes back on the open after next.
func TestSaveToken_BlankKeepsTheSavedToken(t *testing.T) {
	dir := t.TempDir()
	if err := saveToken(dir, "grant-1"); err != nil {
		t.Fatal(err)
	}
	if err := saveToken(dir, ""); err != nil {
		t.Fatal(err)
	}
	if got := loadToken(dir); got != "grant-1" {
		t.Errorf("token after a blank save = %q, want the saved one kept", got)
	}
}

// A session the portal opened without the keyboard or the pointer in it answers every event with "not allowed". The saved restore token is what brought that grant back, so it must go, or the next open restores the same empty grant instead of asking consent again (measured on this desk on 2026-09-08).
func TestNotAllowedForgetsTheSavedToken(t *testing.T) {
	dir := t.TempDir()
	if err := saveToken(dir, "tok"); err != nil {
		t.Fatal(err)
	}
	s := &Session{handle: "/session/1", dataDir: dir, streams: []streamInfo{{Node: 1, Rect: streamRect{}}}, send: func(context.Context, string, ...interface{}) error {
		return errors.New("Session is not allowed to call NotifyPointer methods")
	}}
	if err := s.ClickAt(10, 10); err == nil {
		t.Fatal("the press should fail")
	}
	if loadToken(dir) != "" {
		t.Fatal("the token that restores a device-less grant should be gone")
	}
}

// A context menu opens on the right pointer button and on nothing else, and the portal takes that button as an evdev code: BTN_RIGHT is 0x111, one above BTN_LEFT. A session that sends 0x110 for every click can never open a menu.
func TestRightClickAt_SendsTheRightButton(t *testing.T) {
	var buttons [][2]int32
	s := &Session{handle: "/session/1", streams: []streamInfo{{Node: 1, Rect: streamRect{W: 1920, H: 1080}}}, send: func(_ context.Context, method string, args ...interface{}) error {
		if method == "NotifyPointerButton" {
			buttons = append(buttons, [2]int32{args[2].(int32), int32(args[3].(uint32))})
		}
		return nil
	}}

	if err := s.RightClickAt(10, 20); err != nil {
		t.Fatalf("RightClickAt: %v", err)
	}
	want := [][2]int32{{0x111, 1}, {0x111, 0}}
	if len(buttons) != len(want) || !sameEvents(buttons, want) {
		t.Errorf("buttons = %v, want BTN_RIGHT pressed then released", buttons)
	}
}
