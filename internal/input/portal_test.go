//go:build linux

package input

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

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
		{"no size", []stream{{Node: 7, Props: map[string]dbus.Variant{}}}, []streamInfo{{Node: 7}}},
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

// openSequence must close any session it opened before returning an error, at every failure point after createSession succeeds, so a failed Open never leaks a live portal session. A grant that carries no screen stream fails at Open too, rather than one click at a time with a message about the coordinate instead of the missing grant.
func TestOpenSequenceClosesOnFailure(t *testing.T) {
	cases := []struct {
		name string
		p    *fakePortal
	}{
		{"SelectDevices fails", &fakePortal{selectDevicesErr: errBoom}},
		{"SelectSources fails", &fakePortal{selectSourcesErr: errBoom}},
		{"Start fails", &fakePortal{startErr: errBoom}},
		{"the grant has no screen stream", &fakePortal{noStreams: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, err := openSequence(context.Background(), c.p, "")
			if err == nil {
				t.Fatal("openSequence succeeded, want the failure reported")
			}
			if c.p.closedHandle != "session-1" {
				t.Fatalf("closedHandle = %q, want session-1", c.p.closedHandle)
			}
		})
	}
}

// toStream maps a whole-desktop point into the granted stream that covers it: the stream's own origin is subtracted, a scaled monitor's logical pixels become stream pixels, a point on no monitor is refused, and a compositor that reports neither position nor size gets the point passed through with only a negative one refused.
func TestToStream(t *testing.T) {
	two := []streamInfo{{Node: 11, Rect: streamRect{W: 1920, H: 1080}}, {Node: 12, Rect: streamRect{X: 1920, W: 1920, H: 1080}}}
	scaled := []streamInfo{{Node: 1, Rect: withScaleFrom(streamRect{W: 3840, H: 2160}, func(int, int) (int, int, bool) { return 1920, 1080, true })}}
	unreadLayout := []streamInfo{{Node: 1, Rect: withScaleFrom(streamRect{X: 1920, W: 2560, H: 1440}, func(int, int) (int, int, bool) { return 0, 0, false })}}
	bare := []streamInfo{{Node: 1}}
	for _, c := range []struct {
		name         string
		streams      []streamInfo
		x, y         float64
		node         uint32
		wantX, wantY float64
		wantErr      bool
	}{
		{"a point on the second monitor lands in that stream's own space", two, 2850, 423, 12, 930, 423, false},
		{"a point on neither monitor is refused", two, 5000, 100, 0, 0, 0, true},
		{"a 2x monitor maps logical pixels to stream pixels", scaled, 1900, 1000, 1, 3800, 2000, false},
		{"a layout that cannot be read leaves the scale at 1", unreadLayout, 2020, 100, 1, 100, 100, false},
		{"no position or size passes the point through", bare, 50000, 50000, 1, 50000, 50000, false},
		{"no position or size still refuses a negative point", bare, -1, 0, 0, 0, 0, true},
	} {
		x, y, node, err := toStream(c.x, c.y, c.streams)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.wantErr)
			continue
		}
		if !c.wantErr && (node != c.node || x != c.wantX || y != c.wantY) {
			t.Errorf("%s: toStream = (node %d, %v, %v), want (node %d, %v, %v)", c.name, node, x, y, c.node, c.wantX, c.wantY)
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

var errBoom = errors.New("boom")

// fakePortal is a portalOps double letting the openSequence tests force a failure at any step without a live D-Bus session.
type fakePortal struct {
	selectDevicesErr error
	selectSourcesErr error
	startErr         error
	noStreams        bool // the portal answered Start with a grant carrying no screen stream
	closedHandle     string
}

func (f *fakePortal) createSession(context.Context) (string, error) {
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
