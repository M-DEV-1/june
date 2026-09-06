//go:build linux

package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
)

// extractText returns the text content of the focused window via AT-SPI over D-Bus. Returns ("", nil) on any failure (no bus, no focused window, etc) — callers never see a non-nil error here.
//
// Per-app requirements:
//   - GTK3: set toolkit-accessibility=true in ~/.config/gtk-3.0/settings.ini, or use
//     GNOME Settings > Accessibility > Enable, or pass per-app env AT_SPI_BUS_ADDRESS.
//   - GTK4: accessibility is on by default.
//   - Qt: set QT_ACCESSIBILITY=1 in the environment.
//   - Electron: launch with --force-renderer-accessibility.
func extractText() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	return atspiTextWithin(ctx), nil
}

// atspiTextWithin reads the focused window's accessibility text and gives up at ctx's deadline whatever the bus is doing. Input: a context carrying the budget. Output: the text, or "" when the budget ran out first.
// The budget has to be enforced from out here because the context does not reach all the way in: dialling the accessibility bus goes through dbus.Dial, Auth and Hello, none of which take a context, so a bus that accepted the connection and then stopped talking would otherwise hold the caller for good. The context is still passed down as well, so the method calls that do honour it stop on their own instead of running on unread.
func atspiTextWithin(ctx context.Context) string {
	return withBudget(ctx, func() string {
		text, _ := atspiExtract(ctx)
		return text
	})
}

// Walk bounds — deep enough to reach text in IDEs/terminals/rich native apps, but bounded so a pathological tree can't hang capture. captureTimeout is the hard ceiling regardless.
const (
	captureTimeout = 2500 * time.Millisecond
	// dbusProbeTimeout bounds the one-shot D-Bus probes that are not part of a capture: the lock check on every tick, the input-idle read the dreaming loop makes, and the accessibility switch daemon startup flips. godbus's plain Call has no reply timeout at all, so each of these could hold its caller for good on a peer that accepted the message and stopped answering.
	dbusProbeTimeout = time.Second
)

// namedWindow is an application name and a window title travelling together, so a two-value read can pass through withBudget.
type namedWindow struct {
	app   string
	title string
}

// meetingRead is one look for a call window: the application, the window title, the text read out of it, and whether a call window was found at all.
type meetingRead struct {
	app   string
	title string
	text  string
	ok    bool
}

// focusState remembers the window the desktop last said was activated, together with its application's name.
// It exists because STATE_ACTIVE is not trustworthy: probed on 2026-09-04, the Chrome window hosting the Teams PWA kept the bit set after losing focus while a second window also carried it, so a walk that returns the first ACTIVE window it meets reports Teams no matter where the user is working. The window:activate and window:deactivate signals are exact and arrive in milliseconds.
type focusState struct {
	mu     sync.Mutex
	ref    aref
	app    string
	ora    bool // whether the window in ref is Ora's own
	prev   aref // the last activated window that was not Ora's own
	prevAp string
	seen   bool
}

// apply records one AT-SPI window signal. Input: the signal's member name, the window accessible it names, and that window's application name and title (both read only for activations). An activate replaces whatever was remembered; a deactivate clears the memory only when it names the remembered window, so a late deactivate from a window already left cannot blank the current one. The member is compared case-insensitively because Chromium emits "Activate" and GTK4 emits "activate".
// Activating a window that is not Ora's own also records it as the previous window, which is what get hands back while Ora itself holds focus.
func (s *focusState) apply(member string, ref aref, app, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = true
	switch {
	case strings.EqualFold(member, "activate"):
		s.ref, s.app, s.ora = ref, app, IsOraWindow(app, title)
		if !s.ora {
			s.prev, s.prevAp = ref, app
		}
	case strings.EqualFold(member, "deactivate"):
		if s.ref != ref {
			return
		}
		// Ora's own window closing hands focus back to the window it was opened over, and that window sent its activate before Ora took focus, so it sends no other one. Restoring it here is the only way back: blanking the state left nothing focused until the user next switched applications, which meant no episodes and every screen tool answering that nothing is on screen.
		if s.ora && s.prev.Name != "" {
			s.ref, s.app, s.ora = s.prev, s.prevAp, false
			return
		}
		s.ref, s.app, s.ora = aref{}, "", false
	}
}

// get returns the window to report and its application name. While Ora's own window holds focus it returns the last window the user was in instead, because the hover is opened to ask about what is behind it. ok is false when nothing holds focus, either because no signal has arrived yet or because the last one was the deactivation of the window we were holding.
func (s *focusState) get() (aref, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ora && s.prev.Name != "" {
		return s.prev, s.prevAp, true
	}
	return s.ref, s.app, s.ref.Name != ""
}

// previous returns the last window that took focus and was not Ora's own, together with its application name. Unlike get it still answers after that window has been deactivated with nothing else taking focus, which is what happens whenever focus moves to an application that publishes no accessibility tree. ok is false when no such window has been seen, or when the one remembered has left the bus. Callers that must know what holds focus right now use get; this one answers "what was the user last in".
func (s *focusState) previous() (aref, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prev, s.prevAp, s.prev.Name != ""
}

// clear forgets a window that has left the bus and can no longer answer, whether it was the focused one, the previous one, or both.
func (s *focusState) clear(ref aref) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ref == ref {
		s.ref, s.app, s.ora = aref{}, "", false
	}
	if s.prev == ref {
		s.prev, s.prevAp = aref{}, ""
	}
}

// seenAny reports whether any window signal has arrived. Until one has, callers may still fall back to walking the tree for STATE_ACTIVE; after one has, the absence of a focused window is an answer in itself and the walk would only resurrect a stale bit.
func (s *focusState) seenAny() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

// signalMember returns the part of a D-Bus signal name after the last dot, e.g. "Activate" for "org.a11y.atspi.Event.Window.Activate".
func signalMember(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// focusWatcher holds one live subscription to AT-SPI window activations. Its connection carries the signals and also serves the title read, so reporting the focused window costs one round trip instead of a walk over every application on the bus. It lives exactly as long as that connection does; when the bus drops it, the supervisor builds a new one.
type focusWatcher struct {
	conn  *dbus.Conn
	state focusState
}

// How long the supervisor waits before dialling the accessibility bus again. The first retry is short because the usual cause is the bus launcher restarting and coming straight back; the ceiling stops a machine that has no accessibility bus at all from dialling in a tight loop for the life of the process.
const (
	focusRetryMin = time.Second
	focusRetryMax = time.Minute
)

// focusSupervisor keeps one connected focus watcher available to the process. It exists because the connection does not last: the accessibility bus launcher restarts, accessibility is toggled, the machine resumes from suspend, and on any of those the D-Bus library closes the signal channel, the consume loop ends and the watcher is deaf. Readers take the live watcher with current, which is nil while there is none, and fall back to walking the tree for STATE_ACTIVE.
type focusSupervisor struct {
	cur atomic.Pointer[focusWatcher]
	// ready is closed once the first dial has finished, win or lose, so the first caller waits for the answer instead of being told there is no bus while the dial is still in flight — which is all a short-lived process would ever see.
	ready   chan struct{}
	settled sync.Once
}

var (
	focusOnce sync.Once
	focusSup  focusSupervisor
)

// focus returns the process-wide window watcher, starting the supervisor that keeps one connected on first use. Output: nil while there is no connection to the accessibility bus, in which case callers fall back to walking the tree.
func focus() *focusWatcher {
	focusOnce.Do(func() {
		focusSup.ready = make(chan struct{})
		go focusSup.supervise(context.Background(), dialFocusWatcher, time.Sleep)
	})
	focusSup.waitReady()
	return focusSup.current()
}

// waitReady blocks until the first dial has finished, or captureTimeout passes, whichever comes first. Later calls return at once, including while a reconnection is in flight: the answer then is the honest "no watcher right now", and the caller walks the tree.
func (s *focusSupervisor) waitReady() {
	if s.ready == nil {
		return
	}
	t := time.NewTimer(captureTimeout)
	defer t.Stop()
	select {
	case <-s.ready:
	case <-t.C:
	}
}

// settle releases the callers waiting on the first dial. Called after every attempt and does its work only on the first.
func (s *focusSupervisor) settle() {
	s.settled.Do(func() {
		if s.ready != nil {
			close(s.ready)
		}
	})
}

// current returns the watcher callers should read, or nil while the supervisor has no connection to the accessibility bus.
func (s *focusSupervisor) current() *focusWatcher { return s.cur.Load() }

// supervise keeps a focus watcher connected until ctx ends. Input: a context, a dial function returning a watcher already subscribed to window signals together with the channel those signals arrive on, and a sleep used for the backoff — the last two are parameters so this loop can be tested without an accessibility bus. It publishes the watcher while its connection is up, consumes signals until that connection drops, withdraws the watcher, logs why it stopped, and dials again after a wait that grows to focusRetryMax while dialling keeps failing.
// The dead watcher is withdrawn rather than kept, and its replacement starts with an empty focus state, because both the seen flag and the window references belong to the connection they were learned on. A dead watcher left in place went on reporting that it had seen a focus signal, which is the flag that turns off the fallback tree walk, so its death took the healthy paths down with it.
func (s *focusSupervisor) supervise(ctx context.Context, dial func() (*focusWatcher, chan *dbus.Signal, error), sleep func(time.Duration)) {
	backoff := focusRetryMin
	for ctx.Err() == nil {
		w, sigs, err := dial()
		s.settle()
		if err != nil {
			slog.Warn("cannot watch window focus, the accessibility bus did not answer", "error", err, "retry_in", backoff)
		} else {
			backoff = focusRetryMin
			s.cur.Store(w)
			slog.Info("watching window focus over the accessibility bus")
			w.run(sigs) // returns when the bus closes the signal channel under it
			s.cur.Store(nil)
			w.close()
			slog.Warn("the window focus watcher lost its bus connection, reconnecting", "retry_in", backoff)
		}
		if ctx.Err() != nil {
			return
		}
		sleep(backoff)
		backoff = min(2*backoff, focusRetryMax)
	}
}

// dialFocusWatcher connects to the accessibility bus, asks the registry to emit window activations, and subscribes to them. Output: a watcher holding the connection and the channel its window signals will arrive on, or an error naming what failed — no bus, a registry that refused the registration, or a match rule the bus would not add.
func dialFocusWatcher() (*focusWatcher, chan *dbus.Signal, error) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	// The real dialler, not the dialBus seam: this runs on the supervisor's own goroutine, which nothing waits on, and a dial that never answers costs only a reconnection attempt.
	conn, err := dialA11y(ctx)
	if err != nil {
		return nil, nil, err
	}
	// The registry only routes an event class to listeners that have registered for it; without this the match below receives nothing.
	reg := conn.Object("org.a11y.atspi.Registry", dbus.ObjectPath("/org/a11y/atspi/registry"))
	for _, ev := range []string{"window:activate", "window:deactivate"} {
		if call := reg.CallWithContext(ctx, "org.a11y.atspi.Registry.RegisterEvent", 0, ev); call.Err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("the accessibility registry refused %s: %w", ev, call.Err)
		}
	}
	if err := conn.AddMatchSignal(dbus.WithMatchInterface("org.a11y.atspi.Event.Window")); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("the accessibility bus refused the window signal match: %w", err)
	}

	w := &focusWatcher{conn: conn}
	sigs := make(chan *dbus.Signal, 64)
	conn.Signal(sigs)
	return w, sigs, nil
}

// close drops the watcher's bus connection, which also closes its signal channel if the bus has not already done so. Safe on a watcher that never had a connection.
func (w *focusWatcher) close() {
	if w.conn != nil {
		w.conn.Close()
	}
}

// hearsTheDesktop reports whether this watcher is still connected and has had at least one window signal. Only then is "no window has focus" an answer rather than a gap: a watcher whose connection has died knows nothing, so callers walk the tree instead of trusting it while the supervisor is still on its way to withdrawing it.
func (w *focusWatcher) hearsTheDesktop() bool {
	return w.conn != nil && w.conn.Connected() && w.state.seenAny()
}

// frameAppLogged makes the mutter-x11-frames note below fire once per process rather than on every X11 activation, since every such window that takes focus goes through it.
var frameAppLogged sync.Once

// run applies every window signal to the focus state, naming each activated window by reading its own Name and its accessible parent's over the bus. It returns when the connection closes and the channel drains.
func (w *focusWatcher) run(sigs chan *dbus.Signal) {
	w.consume(sigs, func(ref aref) (string, string) {
		ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
		defer cancel()
		app := getName(ctx, w.conn, getParent(ctx, w.conn, ref))
		title := getName(ctx, w.conn, ref)
		if app == "mutter-x11-frames" {
			// An X11 window's activation is announced by the compositor's frame process, not by the app inside it: the frame's own Name is already the window's title, which title holds, but there is no cheaper way from AT-SPI alone to reach the real client's name. Logged once so it is visible without repeating for every such window, and at Debug because the title is often a mail subject or a document name — the same reason WriteEpisode keeps the title off its trace span.
			frameAppLogged.Do(func() {
				slog.Debug("an X11 window activated through its mutter frame; reporting the frame as the app name since the accessibility bus does not expose the real client here", "title", title)
			})
		}
		return app, title
	})
}

// consume is run's loop with the window lookup passed in, so the signal handling can be tested without a bus. Input: a channel of AT-SPI window signals and a function returning the application name and title of a window accessible. It records each activation and deactivation and ignores every other window signal.
func (w *focusWatcher) consume(sigs chan *dbus.Signal, describe func(aref) (app, title string)) {
	for s := range sigs {
		member := signalMember(s.Name)
		isActivate := strings.EqualFold(member, "activate")
		if !isActivate && !strings.EqualFold(member, "deactivate") {
			continue
		}
		ref := aref{Name: s.Sender, Path: s.Path}
		app, title := "", ""
		if isActivate {
			app, title = describe(ref)
		}
		w.state.apply(member, ref, app, title)
	}
}

// current returns the application name and title of the window that last took focus. The title is read live because a window keeps its identity while its title changes — a browser switching tabs sends no activation — and ok is false when no window holds focus or the remembered one has gone, which drops it from the state.
// The remembered window can also be alive but no longer the one in front: its successor's activation never arrived, which happens when the window that actually took focus belongs to another session's systemd scope, or was announced by its mutter-x11-frames client rather than by the app. So the remembered window's STATE_ACTIVE and STATE_FOCUSED bits are both read here, and the desktop is scanned for whichever window really holds the keyboard whenever it is not carrying both (see activeOrFallback and scanForActive); the state is updated to that window so the next call does not re-scan.
func (w *focusWatcher) current(ctx context.Context) (app, title string, ok bool) {
	ref, app, ok := w.state.get()
	if !ok {
		return "", "", false
	}
	title, err := readName(ctx, w.conn, ref)
	if err != nil {
		w.state.clear(ref)
		return "", "", false
	}
	active, focused := activeAndFocused(ctx, w.conn, ref)
	newRef, app, title, ok := activeOrFallback(ref, app, title, active, focused, func() (activeWindow, bool) {
		return scanForActive(ctx, w.conn)
	})
	if !ok {
		return "", "", false
	}
	if newRef != ref {
		w.state.apply("activate", newRef, app, title)
	}
	return app, title, true
}

// activeWindow is one window a desktop scan found: its accessible ref, its application's name, its own title, and whether it claims the keyboard focus as well as being active.
type activeWindow struct {
	ref     aref
	app     string
	title   string
	focused bool
}

// activeOrFallback decides what current() reports once it already knows the remembered window's live title. Input: the remembered window's ref, app and title, whether it still carries STATE_ACTIVE and STATE_FOCUSED, and a scan of the desktop for whichever window is active. Output: the window to report and true, or false when neither the remembered window nor any other is active. Kept apart from current() so the decision can be tested without a live accessibility bus.
// STATE_ACTIVE on its own is not proof of focus, which is the whole reason focusState exists: probed on 2026-09-04, the Chrome window hosting the Teams PWA kept the bit set after losing focus while a second window carried it too. Trusting it here reopened that hole from the other side — a window that took focus without announcing it (another session's systemd scope, or a client announced only through mutter-x11-frames) left Chrome reported as the focused window indefinitely, and the meeting's conversation was walked and stored as screen text for as long as the user worked elsewhere. So STATE_FOCUSED is read as a second opinion, and the desktop is scanned whenever it is missing.
func activeOrFallback(ref aref, app, title string, active, focused bool, scan func() (activeWindow, bool)) (aref, string, string, bool) {
	// The remembered window holding both bits is the ordinary case, and it costs no scan.
	if active && focused {
		return ref, app, title, true
	}
	found, ok := scan()
	switch {
	case !ok:
	// Ora's own window holding focus is the one case the remembered window exists to hide, so a scan that finds it changes nothing.
	case IsOraWindow(found.app, found.title):
	case found.ref == ref:
		return ref, app, title, true
	// A window that claims the keyboard beats a remembered window that has only kept STATE_ACTIVE. When nothing claims the keyboard, another merely-active window is no better evidence than the remembered one, so it only wins if the remembered window has lost its own active bit as well.
	case found.focused || !active:
		return found.ref, found.app, found.title, true
	}
	if active {
		return ref, app, title, true
	}
	return aref{}, "", "", false
}

// atspiExtract does the real work so we can return errors internally without leaking them to the caller.
func atspiExtract(ctx context.Context) (string, error) {
	conn, err := dialTheBus(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// Read the window the desktop last activated. Choosing by STATE_ACTIVE instead pulls in windows that have lost focus but kept the bit, which is how a Teams tab's text ended up filed against whatever the user was really doing.
	if w := focus(); w != nil {
		if ref, _, ok := w.state.get(); ok {
			return trimText(documentText(walkWindow(ctx, conn, ref))), nil
		}
		if w.hearsTheDesktop() {
			return "", nil
		}
	}

	// No signal has arrived yet: enumerate application-level accessibles from the registry root and take the windows that claim to be active.
	root := aref{Name: "org.a11y.atspi.Registry", Path: "/org/a11y/atspi/accessible/root"}
	apps, err := getChildren(ctx, conn, root)
	if err != nil {
		return "", err
	}

	var parts []string
	seen := make(map[string]struct{})

	for _, app := range apps {
		if ctx.Err() != nil {
			break
		}
		wins, err := getChildren(ctx, conn, app)
		if err != nil {
			continue
		}
		for _, win := range wins {
			if ctx.Err() != nil {
				break
			}
			if !hasState(ctx, conn, win, stateActive) {
				continue
			}
			// found the focused window — walk its subtree, then apply documentText's role-aware rule: browser chrome lives outside any DOCUMENT_WEB node, so keep only that when present; native apps have none and fall back to the full tree.
			tree := walkWindow(ctx, conn, win)
			if t := strings.TrimSpace(documentText(tree)); t != "" {
				if _, dup := seen[t]; !dup {
					seen[t] = struct{}{}
					parts = append(parts, t)
				}
			}
			break
		}
	}

	if len(parts) == 0 {
		return "", nil
	}

	return trimText(strings.Join(parts, "\n")), nil
}

// extractMeetingWindow finds a call in progress anywhere on the desktop and reads it, whether or not it has focus, returning its app, its window title and its text. ok is false when no meeting window is open.
// This exists because the focused window is the wrong window during a meeting. On 2026-08-31 a thirty-nine minute standup produced twenty-seven episodes and not one of them was the call: the user spent it in ClickUp and a terminal, so the participant tiles, the "X is presenting" label and the meeting chat — the only things on the machine that name who is speaking — were never captured at all.
// It walks the same registry tree as atspiExtract and differs in one line: the window is chosen by name rather than by being active.
func extractMeetingWindow() (app, title, text string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()

	r := meetingWindowWithin(ctx)
	return r.app, r.title, r.text, r.ok
}

// meetingWindowWithin is the look for a call window with its deadline made real: the whole read, dialling the bus included, is abandoned once ctx is done. Input: a context carrying the budget. Output: what was found, with ok false when there is no call window or the budget ran out first.
func meetingWindowWithin(ctx context.Context) meetingRead {
	return withBudget(ctx, func() meetingRead { return scanForMeetingWindow(ctx) })
}

// scanForMeetingWindow walks every application on the accessibility bus looking for a window whose name says a call is in progress, and reads it. Input: a context bounding the bus calls. Output: the call window's application, title and text, with ok false when no call window is open.
func scanForMeetingWindow(ctx context.Context) meetingRead {
	conn, err := dialTheBus(ctx)
	if err != nil {
		return meetingRead{}
	}
	defer conn.Close()

	root := aref{Name: "org.a11y.atspi.Registry", Path: "/org/a11y/atspi/accessible/root"}
	apps, err := getChildren(ctx, conn, root)
	if err != nil {
		return meetingRead{}
	}

	for _, a := range apps {
		if ctx.Err() != nil {
			break
		}
		wins, err := getChildren(ctx, conn, a)
		if err != nil {
			continue
		}
		appName := getName(ctx, conn, a)
		for _, win := range wins {
			if ctx.Err() != nil {
				break
			}
			winName := getName(ctx, conn, win)
			if !IsMeetingWindow(appName, winName) {
				continue
			}
			return meetingRead{app: appName, title: winName, text: trimText(documentText(walkWindow(ctx, conn, win))), ok: true}
		}
	}
	return meetingRead{}
}

// atspiActiveWindow returns (app, title) of the focused window via AT-SPI. It reads the window the desktop last activated, which the focus watcher keeps current from window:activate and window:deactivate signals, and only walks the registry tree for STATE_ACTIVE before the first such signal arrives.
// Returns ("", "") when nothing has focus or the bus is unreachable — the caller maps that to Unknown.
func atspiActiveWindow(ctx context.Context) (string, string) {
	w := withBudget(ctx, func() namedWindow {
		app, title := scanActiveWindow(ctx)
		return namedWindow{app: app, title: title}
	})
	return w.app, w.title
}

// scanActiveWindow is atspiActiveWindow's actual read, kept apart so the wrapper above can abandon it at the deadline. Input: a context bounding the bus calls. Output: the focused window's application name and title, both "" when nothing has focus or the bus is unreachable.
func scanActiveWindow(ctx context.Context) (string, string) {
	if w := focus(); w != nil {
		if app, title, ok := w.current(ctx); ok {
			return app, title
		}
		if w.hearsTheDesktop() {
			// The desktop is telling us which window has focus and right now that is none we can see, which is the honest answer: an application that publishes no accessibility tree, or the lock screen. Walking for STATE_ACTIVE here would hand back the last window that left its bit set.
			return "", ""
		}
	}

	conn, err := dialTheBus(ctx)
	if err != nil {
		return "", ""
	}
	defer conn.Close()

	found, ok := scanForActive(ctx, conn)
	if !ok {
		return "", ""
	}
	return found.app, found.title
}

// scanForActive walks every application's top-level windows on the accessibility bus — the same walk observeDesktop uses to list them — and returns the one that holds the focus. Input: a context bounding the bus calls and a live connection. Output: that window, with ok false when nothing on the bus is active right now.
// A window carrying STATE_FOCUSED as well as STATE_ACTIVE wins outright and ends the walk, because more than one window can be left carrying STATE_ACTIVE and only one holds the keyboard. When no window claims the keyboard, the first active one is returned, marked as not focused so the caller knows how weak the answer is.
func scanForActive(ctx context.Context, conn *dbus.Conn) (activeWindow, bool) {
	apps, err := getChildren(ctx, conn, registryRoot)
	if err != nil {
		return activeWindow{}, false
	}
	var firstActive activeWindow
	var foundAny bool
	for _, a := range apps {
		if ctx.Err() != nil {
			break
		}
		wins, err := getChildren(ctx, conn, a)
		if err != nil {
			continue
		}
		for _, win := range wins {
			if ctx.Err() != nil {
				break
			}
			active, focused := activeAndFocused(ctx, conn, win)
			if !active {
				continue
			}
			w := activeWindow{ref: win, app: getName(ctx, conn, a), title: getName(ctx, conn, win), focused: focused}
			if focused {
				return w, true
			}
			if !foundAny {
				firstActive, foundAny = w, true
			}
		}
	}
	return firstActive, foundAny
}

// WindowTitleFor returns the title of a window belonging to the named application, or empty when the desktop does not report one.
//
// Input: the application's process name, such as "brave" or "chrome". Output: the title of one of its windows, preferring the longest, or empty when nothing matches.
//
// This asks the desktop rather than Ora's own history, because history lags: a call is joined seconds before the tracker next records a window. It is best-effort and often returns nothing — an application publishes an accessibility tree only if it was built or launched to, and Brave installed as a snap frequently publishes none at all even when launched with --force-renderer-accessibility. The caller falls back to history, which is why this failing is not a failure.
// X11 would list every window regardless, but this desktop is Wayland and XWayland reports an empty _NET_CLIENT_LIST, so there is nothing to read there.
// The longest title is preferred because a browser's several windows include short utility ones and the call is the window that names itself fully. Nothing here knows which applications host meetings; it answers only "what is this program showing".
func WindowTitleFor(ctx context.Context, app string) string {
	if app == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()

	return withBudget(ctx, func() string { return scanWindowTitle(ctx, app) })
}

// scanWindowTitle is WindowTitleFor's actual read, kept apart so the wrapper above can abandon it at the deadline. Input: a context bounding the bus calls and the application's process name. Output: the longest window title that application publishes, or "" when it publishes none.
func scanWindowTitle(ctx context.Context, app string) string {
	conn, err := dialTheBus(ctx)
	if err != nil {
		return ""
	}
	defer conn.Close()

	root := aref{Name: "org.a11y.atspi.Registry", Path: "/org/a11y/atspi/accessible/root"}
	apps, err := getChildren(ctx, conn, root)
	if err != nil {
		return ""
	}
	want := strings.ToLower(app)
	best := ""
	for _, a := range apps {
		if ctx.Err() != nil {
			break
		}
		name := strings.ToLower(getName(ctx, conn, a))
		if name == "" || (!strings.Contains(name, want) && !strings.Contains(want, name)) {
			continue
		}
		wins, err := getChildren(ctx, conn, a)
		if err != nil {
			continue
		}
		for _, w := range wins {
			if t := strings.TrimSpace(getName(ctx, conn, w)); len(t) > len(best) {
				best = t
			}
		}
	}
	return best
}
