//go:build linux

package tracker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// The signal loop fed the exact traffic seen on this desktop on 2026-09-04: Chrome hosting Teams activates, the user moves to the Claude window, then leaves it for a terminal that publishes no accessibility tree, which produces a deactivate with no matching activate. The answer at the end must be that nothing has focus, not that Teams still does.
func TestConsumeFollowsTheDesktop(t *testing.T) {
	chrome := aref{Name: ":1.3068", Path: "/org/a11y/atspi/accessible/1"}
	claude := aref{Name: ":1.7009", Path: "/org/a11y/atspi/accessible/1"}
	apps := map[aref]string{chrome: "Google Chrome", claude: "claude-desktop"}

	signal := func(member string, ref aref) *dbus.Signal {
		return &dbus.Signal{Name: "org.a11y.atspi.Event.Window." + member, Sender: ref.Name, Path: ref.Path}
	}

	var w focusWatcher
	sigs := make(chan *dbus.Signal, 8)
	for _, s := range []*dbus.Signal{
		signal("Activate", chrome),
		signal("Deactivate", chrome),
		signal("activate", claude),
		signal("Maximize", chrome),
		signal("deactivate", claude),
	} {
		sigs <- s
	}
	close(sigs)
	w.consume(sigs, func(ref aref) (string, string) { return apps[ref], "" })

	if _, _, ok := w.state.get(); ok {
		t.Fatal("a window still holds focus after the last one was deactivated")
	}
	if !w.state.seenAny() {
		t.Fatal("seenAny() is false after five signals, so the stale STATE_ACTIVE walk would run again")
	}
}

// Opening June's hover must not make June the answer: the state keeps the last window that was not June's own and reports that one while June holds focus, so /context still names where the user came from. When June is dismissed and the window under it activates again, that window is the answer directly.
func TestFocusStateReportsThePreviousWindowWhileJuneHasFocus(t *testing.T) {
	chrome := aref{Name: ":1.3068", Path: "/org/a11y/atspi/accessible/1"}
	june := aref{Name: ":1.9001", Path: "/org/a11y/atspi/accessible/1"}
	frame := aref{Name: ":1.9002", Path: "/org/a11y/atspi/accessible/1"}

	var s focusState
	s.apply("Activate", chrome, "Google Chrome", "Teams")
	s.apply("Deactivate", chrome, "", "")
	s.apply("Activate", june, "june", "June")

	ref, app, ok := s.get()
	if !ok || ref != chrome || app != "Google Chrome" {
		t.Fatalf("with June focused get() = %v, %q, %v, want the Chrome window", ref, app, ok)
	}

	// The XWayland frame process is June too, and must not displace the remembered window either.
	s.apply("Activate", frame, "mutter-x11-frames", "June")
	if ref, app, ok := s.get(); !ok || ref != chrome || app != "Google Chrome" {
		t.Fatalf("with the June frame focused get() = %v, %q, %v, want the Chrome window", ref, app, ok)
	}

	// A terminal that happens to be titled "June" is a real window and replaces it.
	term := aref{Name: ":1.9003", Path: "/org/a11y/atspi/accessible/1"}
	s.apply("Activate", term, "gnome-terminal", "June")
	if ref, _, _ := s.get(); ref != term {
		t.Fatalf("get() = %v, want the terminal: only the app name decides what is June", ref)
	}
}

// When June's hover closes, the window under it must be the answer again. The desktop does not say so on its own: that window sent its deactivate when June took focus and sends no fresh activate when June goes away, so blanking the state on June's own deactivate left nothing focused at all until the user next switched applications — no episodes recorded, and every screen tool answering that nothing has focus.
func TestFocusStateRestoresThePreviousWindowWhenJuneGoesAway(t *testing.T) {
	chrome := aref{Name: ":1.3068", Path: "/org/a11y/atspi/accessible/1"}
	june := aref{Name: ":1.9001", Path: "/org/a11y/atspi/accessible/1"}

	var s focusState
	s.apply("Activate", chrome, "Google Chrome", "Teams")
	s.apply("Deactivate", chrome, "", "")
	s.apply("Activate", june, "june", "June")
	s.apply("Deactivate", june, "", "")

	ref, app, ok := s.get()
	if !ok || ref != chrome || app != "Google Chrome" {
		t.Fatalf("after June's hover closed get() = %v, %q, %v, want the Chrome window back", ref, app, ok)
	}

	// Restoring is for June only: a real window's deactivate still means nothing holds focus, because the window that took over may simply publish no accessibility tree.
	s.apply("Deactivate", chrome, "", "")
	if ref, app, ok := s.get(); ok {
		t.Fatalf("after the restored window was deactivated get() = %v, %q, %v, want nothing focused", ref, app, ok)
	}
}

// The watcher must come back when its bus connection dies, which happens when the accessibility bus launcher restarts, when accessibility is toggled, and on resume from suspend: the D-Bus library closes every registered signal channel on a failed read, so the consume loop ends and its goroutine returns. Started once under a sync.Once there was no second start, and the dead watcher kept its seen flag set — the flag that turns the fallback tree walk off — so the daemon recorded nothing at all until someone restarted it.
func TestSuperviseReconnectsAfterTheBusDrops(t *testing.T) {
	chrome := aref{Name: ":1.3068", Path: "/org/a11y/atspi/accessible/1"}

	dialed := make(chan struct{})           // one send per dial attempt
	handout := make(chan chan *dbus.Signal) // the signal channel that attempt gets, or nil to make it fail
	slept := make(chan time.Duration)       // one send per backoff

	s := focusSupervisor{ready: make(chan struct{})}
	dial := func() (*focusWatcher, chan *dbus.Signal, error) {
		dialed <- struct{}{}
		sigs := <-handout
		if sigs == nil {
			return nil, nil, errors.New("no accessibility bus")
		}
		return &focusWatcher{}, sigs, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.supervise(ctx, dial, func(d time.Duration) { slept <- d })

	// The first caller waits for the first dial to finish instead of being told there is no accessibility bus while it is still in flight, which is all a short-lived process would ever see.
	firstRead := make(chan struct{})
	go func() { s.waitReady(); close(firstRead) }()
	<-dialed
	select {
	case <-firstRead:
		t.Fatal("a caller was let through while the first dial was still in flight")
	case <-time.After(20 * time.Millisecond):
	}

	// A dial that fails publishes no watcher and is retried after a wait.
	handout <- nil
	select {
	case <-firstRead:
	case <-time.After(2 * time.Second):
		t.Fatal("a caller was still waiting after the first dial had finished")
	}
	if d := <-slept; d <= 0 {
		t.Fatalf("backoff after a failed dial = %v, want a wait before the retry", d)
	}
	if s.current() != nil {
		t.Fatal("a failed dial published a watcher")
	}

	// The retry connects, and that watcher is the one callers read.
	<-dialed
	first := make(chan *dbus.Signal)
	handout <- first
	waitFor(t, "the connected watcher to be published", func() bool { return s.current() != nil })
	s.current().state.apply("Activate", chrome, "Google Chrome", "")

	// The bus goes away: the library closes the signal channel under the consume loop.
	close(first)

	// Nothing is published while there is no connection, so callers walk the tree instead of trusting a watcher that can no longer hear the desktop.
	if d := <-slept; d != focusRetryMin {
		t.Fatalf("backoff after a connection that worked = %v, want the shortest wait %v", d, focusRetryMin)
	}
	if w := s.current(); w != nil {
		t.Fatalf("the dead watcher is still published, seenAny() = %v", w.state.seenAny())
	}

	// And it reconnects with a state that has seen nothing: the seen flag must not outlive the connection it was learned on.
	<-dialed
	second := make(chan *dbus.Signal)
	handout <- second
	waitFor(t, "the watcher to reconnect", func() bool { return s.current() != nil })
	if s.current().state.seenAny() {
		t.Fatal("the reconnected watcher inherited the dead one's seen flag, so the fallback walk stays off")
	}

	cancel()
	close(second)
}

// waitFor polls a condition until it holds or two seconds pass. Input: the test, what is being waited for, and the condition. It exists because the supervisor publishes its watcher from its own goroutine, so the test has no send to synchronise on there.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
