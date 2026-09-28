//go:build linux

package tracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"
)

// The focus rule: an activate replaces whatever was remembered, a deactivate clears the memory only when it names the window currently remembered, and any signal at all marks the state as live so callers stop trusting the STATE_ACTIVE walk.
func TestFocusStateApply(t *testing.T) {
	chrome := aref{Name: ":1.3068", Path: "/org/a11y/atspi/accessible/1"}
	claude := aref{Name: ":1.7009", Path: "/org/a11y/atspi/accessible/1"}

	type signal struct {
		member string
		ref    aref
		app    string
		title  string
	}
	tests := []struct {
		name    string
		signals []signal
		wantRef aref
		wantApp string
		wantOK  bool
		wantAny bool
	}{
		{
			name:    "no signals means nothing is known and the walk is still allowed",
			wantOK:  false,
			wantAny: false,
		},
		{
			name:    "one activate is remembered",
			signals: []signal{{"Activate", chrome, "Google Chrome", ""}},
			wantRef: chrome, wantApp: "Google Chrome", wantOK: true, wantAny: true,
		},
		{
			name:    "a later activate replaces an earlier one",
			signals: []signal{{"Activate", chrome, "Google Chrome", ""}, {"activate", claude, "claude-desktop", ""}},
			wantRef: claude, wantApp: "claude-desktop", wantOK: true, wantAny: true,
		},
		{
			name:    "deactivating the remembered window leaves nothing focused",
			signals: []signal{{"Activate", chrome, "Google Chrome", ""}, {"Deactivate", chrome, "Google Chrome", ""}},
			wantOK:  false, wantAny: true,
		},
		{
			name:    "a deactivate for some other window does not clear the focused one",
			signals: []signal{{"Activate", claude, "claude-desktop", ""}, {"deactivate", chrome, "Google Chrome", ""}},
			wantRef: claude, wantApp: "claude-desktop", wantOK: true, wantAny: true,
		},
		{
			name:    "the switch away from Chrome is seen even when the new window sends no activate",
			signals: []signal{{"Activate", chrome, "Google Chrome", ""}, {"Deactivate", chrome, "Google Chrome", ""}},
			wantOK:  false, wantAny: true,
		},
		{
			name:    "an unrelated window signal is ignored but still counts as events flowing",
			signals: []signal{{"Restore", chrome, "Google Chrome", ""}},
			wantOK:  false, wantAny: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s focusState
			for _, sig := range tt.signals {
				s.apply(sig.member, sig.ref, sig.app, sig.title)
			}
			ref, app, ok := s.get()
			if ok != tt.wantOK {
				t.Fatalf("get() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && (ref != tt.wantRef || app != tt.wantApp) {
				t.Fatalf("get() = %v, %q, want %v, %q", ref, app, tt.wantRef, tt.wantApp)
			}
			if got := s.seenAny(); got != tt.wantAny {
				t.Fatalf("seenAny() = %v, want %v", got, tt.wantAny)
			}
		})
	}
}

// clear drops the remembered window when its application has left the bus, and does nothing when the window it names is not the one remembered.
func TestFocusStateClear(t *testing.T) {
	chrome := aref{Name: ":1.3068", Path: "/org/a11y/atspi/accessible/1"}
	claude := aref{Name: ":1.7009", Path: "/org/a11y/atspi/accessible/1"}

	var s focusState
	s.apply("Activate", claude, "claude-desktop", "Claude")
	s.clear(chrome)
	if _, _, ok := s.get(); !ok {
		t.Fatal("clear() of another window dropped the focused one")
	}
	s.clear(claude)
	if _, _, ok := s.get(); ok {
		t.Fatal("clear() of the focused window left it remembered")
	}
}

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

// previous answers what the user was last in even when nothing holds focus, which is the state whenever focus moves to an application that publishes no accessibility tree: that window is deactivated, no activate follows, and get() rightly reports no focus. The screen observer needs the window anyway — without it it scans the whole desktop and picks a background one.
func TestFocusStatePrevious(t *testing.T) {
	chrome := aref{Name: ":1.3068", Path: "/org/a11y/atspi/accessible/1"}
	june := aref{Name: ":1.9001", Path: "/org/a11y/atspi/accessible/1"}

	var s focusState
	if _, _, ok := s.previous(); ok {
		t.Fatal("previous() named a window before any signal arrived")
	}

	s.apply("Activate", chrome, "Google Chrome", "Teams")
	s.apply("Deactivate", chrome, "", "")
	if _, _, ok := s.get(); ok {
		t.Fatal("get() still reports a focused window after its deactivate")
	}
	if ref, app, ok := s.previous(); !ok || ref != chrome || app != "Google Chrome" {
		t.Fatalf("previous() = %v, %q, %v, want the Chrome window", ref, app, ok)
	}

	// June's own window never becomes the remembered one, or the hover would answer with itself.
	s.apply("Activate", june, "june", "June")
	if ref, _, _ := s.previous(); ref != chrome {
		t.Fatalf("previous() = %v after June took focus, want the Chrome window", ref)
	}

	// A window that has left the bus can no longer answer, so it is dropped from here too.
	s.clear(chrome)
	if ref, app, ok := s.previous(); ok {
		t.Fatalf("previous() = %v, %q, %v after that window left the bus, want nothing", ref, app, ok)
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

// The remembered window can go stale without a deactivate ever arriving — the window that took focus belonged to another session's systemd scope, or its activation came from mutter-x11-frames rather than the app, so no signal named it. STATE_ACTIVE on its own does not close that gap: probed on 2026-09-04, the Chrome window hosting the Teams PWA kept the bit set after losing focus, so the remembered window looked active while somebody else had the keyboard. STATE_FOCUSED is the second opinion, and activeOrFallback is that decision with the bus reads already resolved, so it can be tested without one.
func TestActiveOrFallback(t *testing.T) {
	files := aref{Name: ":1.4001", Path: "/org/a11y/atspi/accessible/1"}
	brave := aref{Name: ":1.4002", Path: "/org/a11y/atspi/accessible/1"}

	t.Run("a scan that finds only June's own window reports no focus", func(t *testing.T) {
		scan := func() (activeWindow, bool) {
			return activeWindow{ref: aref{Name: ":1.9", Path: "/org/a11y/atspi/accessible/1"}, app: "june", title: "June", focused: true}, true
		}
		if _, _, _, ok := activeOrFallback(files, "org.gnome.Nautilus", "Home", false, false, scan); ok {
			t.Fatal("activeOrFallback adopted June's own window as the focus")
		}
	})

	t.Run("remembered window still active and focused is reported unchanged", func(t *testing.T) {
		scanCalled := false
		scan := func() (activeWindow, bool) {
			scanCalled = true
			return activeWindow{}, false
		}
		ref, app, title, ok := activeOrFallback(files, "org.gnome.Nautilus", "Home", true, true, scan)
		if !ok || ref != files || app != "org.gnome.Nautilus" || title != "Home" {
			t.Fatalf("activeOrFallback = %v, %q, %q, %v, want the remembered window unchanged", ref, app, title, ok)
		}
		if scanCalled {
			t.Fatal("a window holding both the active and the focused bit still triggered a desktop scan")
		}
	})

	t.Run("remembered window inactive and another active reports the other", func(t *testing.T) {
		scan := func() (activeWindow, bool) {
			return activeWindow{ref: brave, app: "brave", title: "June - GitHub"}, true
		}
		ref, app, title, ok := activeOrFallback(files, "org.gnome.Nautilus", "Home", false, false, scan)
		if !ok || ref != brave || app != "brave" || title != "June - GitHub" {
			t.Fatalf("activeOrFallback = %v, %q, %q, %v, want the window the scan found active", ref, app, title, ok)
		}
	})

	// This is the failure the file documents and the code did not cover: Chrome hosting the Teams PWA keeps STATE_ACTIVE after losing focus, and the window that really took focus never announced itself, so the remembered window was reported for as long as the user worked elsewhere and the meeting's conversation was walked and stored the whole time.
	t.Run("remembered window active but not focused loses to the window that claims the keyboard", func(t *testing.T) {
		scan := func() (activeWindow, bool) {
			return activeWindow{ref: brave, app: "brave", title: "June - GitHub", focused: true}, true
		}
		ref, app, title, ok := activeOrFallback(files, "chrome", "Chat | Vexil | Microsoft Teams", true, false, scan)
		if !ok || ref != brave || app != "brave" || title != "June - GitHub" {
			t.Fatalf("activeOrFallback = %v, %q, %q, %v, want the window that claims the keyboard focus", ref, app, title, ok)
		}
	})

	// The other half of the same bit being untrustworthy: when nothing on the desktop claims the keyboard, a second window that merely kept STATE_ACTIVE is no better evidence than the remembered one, so the remembered window stands.
	t.Run("remembered window active but not focused keeps its place against another stale window", func(t *testing.T) {
		scan := func() (activeWindow, bool) {
			return activeWindow{ref: brave, app: "brave", title: "June - GitHub"}, true
		}
		ref, app, title, ok := activeOrFallback(files, "org.gnome.Nautilus", "Home", true, false, scan)
		if !ok || ref != files || app != "org.gnome.Nautilus" || title != "Home" {
			t.Fatalf("activeOrFallback = %v, %q, %q, %v, want the remembered window kept", ref, app, title, ok)
		}
	})

	t.Run("no active window anywhere reports nothing", func(t *testing.T) {
		scan := func() (activeWindow, bool) { return activeWindow{}, false }
		_, _, _, ok := activeOrFallback(files, "org.gnome.Nautilus", "Home", false, false, scan)
		if ok {
			t.Fatal("activeOrFallback reported a window when nothing on the desktop is active")
		}
	})
}

// Captured text is cut to a fixed length before it is stored, and cutting a UTF-8 string by bytes lands mid-rune on any screen that is not pure ASCII, so invalid UTF-8 reached episodes.screen_text and its FTS5 index. The cut is by runes.
func TestTrimText_CutsByRunesNotBytes(t *testing.T) {
	// Every rune here is three bytes, so a byte cut at maxTextLen falls inside one.
	long := strings.Repeat("\u3042", maxTextLen)
	got := trimText(long)
	if !utf8.ValidString(got) {
		t.Fatal("trimText produced invalid UTF-8: the cut landed inside a rune")
	}
	if n := utf8.RuneCountInString(got); n != maxTextLen {
		t.Fatalf("trimText kept %d runes, want %d", n, maxTextLen)
	}
}

// gnome-shell writes the screenshot file itself, under its own umask, so the path we hand it cannot be relied on to stay 0600. The directory around it is what keeps a picture of the whole desktop away from other accounts on the machine.
func TestShotTempPath_PutsTheFileInADirectoryOnlyWeCanRead(t *testing.T) {
	path, cleanup, err := shotTempPath()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("the screenshot directory is %o, want 700", perm)
	}
	if !info.IsDir() {
		t.Fatal("shotTempPath did not put the screenshot inside a directory of its own")
	}
}

// scanForActive is current()'s fallback and observeDesktop's stand-in for STATE_ACTIVE, so it must find a real window with the bit set wherever one is running.
func TestScanForActive_FindsARealWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := dialA11y(ctx)
	if err != nil {
		t.Skip("no accessibility bus:", err)
	}
	defer conn.Close()
	found, ok := scanForActive(ctx, conn)
	if !ok {
		t.Skip("nothing on this desktop reports STATE_ACTIVE")
	}
	if found.ref.Name == "" {
		t.Fatalf("scanForActive reported %q · %q as active with an empty ref", found.app, found.title)
	}
	t.Logf("active: %s · %s", found.app, found.title)
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
