package tracker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"ora/internal/obs"

	"go.opentelemetry.io/otel/attribute"
)

const recaptureInterval = 5 * time.Minute

// Tiered-capture tuning. AT-SPI/UIA text is tried first (free); vision (screenshot -> LLM) only fires when that text comes up nearly empty, and no more often than minVisionInterval.
const (
	thinTextThreshold = 200              // runes; below this, accessibility is treated as blind
	minVisionInterval = 90 * time.Second // floor between vision calls
)

// meetingCaptureInterval is how often a call in progress is read on its own, regardless of what has focus. A meeting window's participant list and presenter label change on the scale of a minute, and reading it costs one AT-SPI walk, so once a minute keeps the whole call in the timeline without crowding out the user's actual activity.
const meetingCaptureInterval = time.Minute

// Bounds for the three calls the capture path makes outside this process. Every one of them used to carry the daemon's own context, which has no deadline, so a peer that accepted the request and then stopped answering held the tick goroutine for as long as it liked: no window polling, no dwell emission, no recapture, and the ticker's one-slot buffer drops every tick missed meanwhile. They are three different numbers because they are three different calls — two messages to programs on this machine and one request over the internet — and one number would be either too short for the model or useless for the local ones.
const (
	// mediaProbeTimeout bounds the MPRIS read: ListNames on the session bus, then one PlaybackStatus property Get per media player registered on it. Measured on this desktop at 0.6-3.2 ms with a player running. Two seconds is not a performance budget, it is a floor under "this player is never answering" — godbus's own reply timeout is 25 seconds, ten times the tick interval, so without this a browser restarting mid-poll costs a dozen ticks.
	mediaProbeTimeout = 2 * time.Second
	// screenshotTimeout bounds the screen grab: a Screenshot call to gnome-shell, which writes the PNG to a temp file, plus reading that file back. Measured on this desktop at 510-527 ms for a 424 KB full-screen PNG. Five seconds is ten times that, which covers a compositor busy redrawing a second monitor, and stops short of the portal fallback's consent dialog: a dialog nobody is looking at should expire rather than hold the loop open.
	screenshotTimeout = 5 * time.Second
	// textReadTimeout bounds the accessibility read. On Linux that read states and enforces its own 2.5-second budget over the whole operation, dialling the bus included, and measured 7-13 ms on the focused window here; this sits half a second above that budget so it never pre-empts a read doing its job. It exists for the platforms whose reader has no budget of its own — the Windows path spawns a PowerShell process per read and waits on it with nothing bounding the wait.
	textReadTimeout = 3 * time.Second
	// lockProbeTimeout bounds the lock-screen probe, which runs first thing on every tick, ahead of the window read. The probe itself now gives its D-Bus call a second (see lock_linux.go), and this is the same second enforced from the tick loop, so a probe that wedges somewhere the call's own context does not reach still costs one tick rather than the whole loop.
	lockProbeTimeout = time.Second
	// visionTimeout bounds the vision model call: a few hundred kilobytes of PNG uploaded to the model and a structured reply generated from it, which is a network round trip and normally several seconds. It gets tens of seconds where the local calls get single digits because that is what the call costs when it is working, and it can afford them: minVisionInterval already limits it to one call per ninety seconds, so a slow one delays no other vision capture. Forty-five seconds covers a poor uplink and one internal retry and still returns before the next attempt is due.
	visionTimeout = 45 * time.Second
)

// captureBounds holds those deadlines, and the lock probe's, on the Daemon so a test can shrink them rather than wait out the real ones.
type captureBounds struct {
	text       time.Duration
	media      time.Duration
	screenshot time.Duration
	vision     time.Duration
	lock       time.Duration
}

type Daemon struct {
	eye       Tracker
	interval  time.Duration
	dwellTime time.Duration
	blocklist []string
	eventChan chan Activity
	capturer  func() string // injectable for tests; nil = real capture
	visionFn  func(ctx context.Context, png []byte) Sight
	paused    atomic.Bool

	// The three calls the capture path makes outside this process, held as fields so a test can stand in one that never answers and prove the caller still comes back. NewDaemon points them at the real ones.
	text       func() (string, error)
	media      func(ctx context.Context) bool
	screenshot func(ctx context.Context) ([]byte, error)
	bounds     captureBounds

	// meeting reads the window of a call in progress, wherever it is on the desktop, and meetingEvery is how often the watcher does it. Both are fields so a test can drive the watcher without an accessibility bus and without waiting a minute for a tick.
	meeting      func() (app, title, text string, ok bool)
	meetingEvery time.Duration

	// capturing is held for the whole of one capture, so only one runs at a time. The per-tier "last text" trackers are plain variables owned by the capture, and a second concurrent capture would race them as well as paying twice for the expensive tier.
	capturing atomic.Bool
	// lastCapture is when a capture last read the screen, as Unix nanoseconds, because the tick loop reads it while a capture goroutine writes it.
	lastCapture atomic.Int64
	// lastSeen is the window the tick loop most recently polled, including one it went on to skip. It is atomic because a capture goroutine reads it while the loop writes it, and it is what a finished capture checks the text it just read against.
	lastSeen atomic.Pointer[Activity]
}

// withBudget runs work and returns what it produced, or the zero value once ctx's deadline passes, whichever happens first. Input: a context carrying the deadline and a function doing the work. Output: the work's value, or the zero value when the deadline won.
// Work that overran keeps running on its own goroutine — nothing can stop a call that is not watching its context — but the caller gets its goroutine back, which is the thing the tick loop needs. Every use of this is either a call that does honour its context, in which case the goroutine ends by itself moments later, or a D-Bus dial, which ends when its socket does.
func withBudget[T any](ctx context.Context, work func() T) T {
	done := make(chan T, 1)
	go func() { done <- work() }()
	select {
	case v := <-done:
		return v
	case <-ctx.Done():
		var zero T
		return zero
	}
}

// shot is one screenshot attempt's bytes and error together, so the two-value call can pass through withBudget.
type shot struct {
	png []byte
	err error
}

// lastCaptureAt reports when the capture path last read the screen. Output: the zero time when no capture has run yet.
func (d *Daemon) lastCaptureAt() time.Time {
	n := d.lastCapture.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Pause suspends activity emission. The polling loop still runs so Resume takes effect promptly.
// sessionLocked reports whether the desktop session's lock screen is up; the platform file sets it (Linux: GNOME's screensaver over D-Bus). Nil means no way to know, which reads as unlocked.
var sessionLocked func() bool

// SessionLocked is the exported read of the lock probe for other packages (the overnight dreaming loop uses it as its idle signal). False when the platform gives no way to know.
func SessionLocked() bool { return sessionLocked != nil && sessionLocked() }

// inputIdle reports real time since the last keyboard/mouse input; the platform file sets it (Linux: GNOME Mutter's IdleMonitor over D-Bus). Nil means no way to know.
var inputIdle func() (time.Duration, error)

// errNoIdleProbe is returned when this platform has no input-idle probe wired.
var errNoIdleProbe = errors.New("tracker: no input-idle probe on this platform")

// InputIdle is the exported read of the input-idle probe for other packages (the overnight dreaming loop uses it to tell a screen-content change from the user actually touching the keyboard or mouse). An error means there is no way to know, and callers should fall back to their own heuristic.
func InputIdle() (time.Duration, error) {
	if inputIdle == nil {
		return 0, errNoIdleProbe
	}
	return inputIdle()
}

func (d *Daemon) Pause() { d.paused.Store(true) }

// Resume re-enables activity emission after a Pause.
func (d *Daemon) Resume() { d.paused.Store(false) }

// IsPaused reports whether tracking is currently paused.
func (d *Daemon) IsPaused() bool { return d.paused.Load() }

// SetCapturer replaces the screen capture function. Used in tests to avoid real capture.
func (d *Daemon) SetCapturer(fn func() string) {
	d.capturer = fn
}

// SetVisionFn injects the vision describer (image -> structured sight). When set, the tiered capturer falls back to a screenshot + this function whenever accessibility text is too thin to be useful. nil disables the vision tier (text-only).
func (d *Daemon) SetVisionFn(fn func(ctx context.Context, png []byte) Sight) {
	d.visionFn = fn
}

// MatchesBlocklist reports whether app should be blocked from tracking. Normalizes both sides (lowercase, trim, strip trailing ".exe") and matches by substring, so one blocklist entry works across platforms — Windows names carry ".exe", Linux names (AT-SPI/X11/Wayland) never do and often show up reverse-DNS or lowercase ("org.keepassxc.KeePassXC", "1password").
func MatchesBlocklist(app string, blocklist []string) bool {
	normalizedApp := normalizeAppIdentifier(app)
	if normalizedApp == "" {
		return false
	}
	for _, entry := range blocklist {
		normalizedEntry := normalizeAppIdentifier(entry)
		if normalizedEntry != "" && strings.Contains(normalizedApp, normalizedEntry) {
			return true
		}
	}
	return false
}

// normalizeAppIdentifier lowercases, trims whitespace, and strips a trailing ".exe" so Windows and Linux app-name forms can be compared uniformly.
func normalizeAppIdentifier(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".exe")
	return s
}

// skipReason names why the window just polled is not the user's activity, or "" when it is one to record. The three cases: a window nothing could identify, which carries zero information and would pollute every later summary with "Unknown | Unknown" lines; Ora's own window, because recording the hover files the assistant as an episode and pushes the window the user came from out of the live buffer, so /context answers with Ora itself; and an application on the blocklist.
// screenLocked reports whether the session's lock screen is up, giving the probe d.bounds.lock to answer. Input: none. Output: true only when the probe answered and said locked; a probe that has not answered in time reads as unlocked, because wrongly refusing to capture is the harmful direction to fail in.
func (d *Daemon) screenLocked() bool {
	if sessionLocked == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.bounds.lock)
	defer cancel()
	return withBudget(ctx, sessionLocked)
}

func (d *Daemon) skipReason(act Activity) string {
	switch {
	case act.App == "Unknown" && act.Title == "Unknown":
		return "unidentified"
	case IsOraWindow(act.App, act.Title):
		return "ora"
	case MatchesBlocklist(act.App, d.blocklist):
		return "blocked"
	}
	return ""
}

func NewDaemon(eye Tracker, interval time.Duration, dwellTime time.Duration, blocklist []string, eventChan chan Activity) *Daemon {
	SetBlocklist(blocklist)
	if interval <= 0 {
		interval = 2 * time.Second // default polling
	}
	if dwellTime <= 0 {
		dwellTime = 3 * time.Second // default dwell time
	}

	return &Daemon{
		eye: eye, interval: interval, dwellTime: dwellTime, blocklist: blocklist, eventChan: eventChan,
		text: extractText, media: mediaPlaying, screenshot: grabScreen,
		meeting: extractMeetingWindow, meetingEvery: meetingCaptureInterval,
		bounds: captureBounds{text: textReadTimeout, media: mediaProbeTimeout, screenshot: screenshotTimeout, vision: visionTimeout, lock: lockProbeTimeout},
	}
}

func (d *Daemon) Start(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	var lastActivity *Activity
	var pendingActivity *Activity
	var pendingSince time.Time
	var emittedCurrent bool

	// use injected capturer (tests) or default to tiered capture with diff tracking. The default path needs the current activity so vision can skip the bare desktop; the test capturer ignores it.
	// lastA11yText/lastVisionText are tracked separately (not one shared "last text") so a tier switch on an
	// unchanged screen doesn't compare one tier's text against the other's and falsely look like a change.
	// These are read and written only by the capture, which runs one at a time, so they need no lock of their own.
	var lastA11yText, lastVisionText string
	var lastVisionTime time.Time
	capture := func(act Activity) captureOut {
		if d.capturer != nil {
			return captureOut{text: d.capturer()}
		}
		return d.tieredCapture(ctx, act, &lastA11yText, &lastVisionText, &lastVisionTime)
	}

	tracer := obs.GetTracer(ctx, "ora.tracker")

	// The call is read on its own goroutine, not on this loop. One AT-SPI walk is allowed 2.5 seconds and this loop ticks every two, so doing it inline would stall window polling for longer than its own interval — and a big meeting window, the case this exists for, is exactly the slow walk.
	if d.capturer == nil {
		go d.watchMeetingWindow(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if d.paused.Load() {
				continue
			}

			_, span := tracer.Start(ctx, "Tracker.PollActiveWindow")

			// A locked screen is not the user's activity: capturing through the shield files the lock clock and "press a key to unlock" as episodes, which then surface in summaries as the day's doings. The pending activity is left alone for the same reason as a skipped window below — the user comes back to the window they locked the screen in.
			if d.screenLocked() {
				span.SetAttributes(attribute.Bool("tracker.locked", true))
				span.End()
				continue
			}

			activity, err := d.eye.GetActiveWindow()
			if err != nil {
				span.RecordError(err)
				slog.Error("tracker: failed to get active window", "error", err)
				span.End()
				continue
			}
			d.noteWindow(*activity)

			// A window that is not the user's activity is a transient skip, and the pending activity is deliberately left as it was. The user is glancing at Ora's hover or a password manager and comes straight back to the window they were in; the loop then sees that window as unchanged, and only a pending activity can be emitted, so clearing it here meant a window with a steady title was never recorded for as long as the user stayed in it.
			if reason := d.skipReason(*activity); reason != "" {
				span.SetAttributes(attribute.String("tracker.skipped", reason))
				span.End()
				continue
			}

			changed := lastActivity == nil || activity.App != lastActivity.App || activity.Title != lastActivity.Title

			if changed {
				lastActivity = activity
				pendingActivity = activity
				pendingSince = time.Now()
				emittedCurrent = false
				span.SetAttributes(
					attribute.Bool("tracker.changed", true),
					attribute.String("tracker.app", activity.App),
					attribute.String("tracker.state", "pending"),
				)
			} else if pendingActivity != nil && !emittedCurrent {
				if time.Since(pendingSince) >= d.dwellTime {
					span.SetAttributes(
						attribute.Bool("tracker.changed", false),
						attribute.String("tracker.state", "emitted"),
					)

					// Handed to a goroutine of its own, so a capture that takes seconds costs this loop nothing. emittedCurrent is set now rather than after the send, because the capture will do the send itself; a capture already in flight leaves it unset, so the next tick tries again.
					if d.captureAndEmit(ctx, *pendingActivity, capture, false) {
						emittedCurrent = true
					}
				} else {
					span.SetAttributes(
						attribute.Bool("tracker.changed", false),
						attribute.String("tracker.state", "dwelling"),
					)
				}
			} else if emittedCurrent && d.capturer == nil && time.Since(d.lastCaptureAt()) >= recaptureInterval {
				// periodic re-capture: same window, 5 min elapsed. Also off this goroutine, and dropped when the screen has not changed.
				d.captureAndEmit(ctx, *lastActivity, capture, true)
				span.SetAttributes(
					attribute.Bool("tracker.changed", false),
					attribute.String("tracker.state", "idle"),
				)
			} else {
				span.SetAttributes(
					attribute.Bool("tracker.changed", false),
					attribute.String("tracker.state", "idle"),
				)
			}

			span.End()
		}
	}
}

// captureAndEmit reads the screen for one activity and puts the result on the activity channel, on a goroutine of its own so the tick loop keeps polling while a slow screenshot or model call is in flight. Input: the context that ends at shutdown, the activity to capture for, the capture function, and whether a capture that found nothing new should be dropped rather than emitted. Output: false when a capture was already in flight and this one was skipped, true when one was started.
// Only one capture runs at a time, and the flag is held across the send as well as the capture, so the events two captures produce reach the channel in the order the loop asked for them.
func (d *Daemon) captureAndEmit(ctx context.Context, ev Activity, capture func(Activity) captureOut, dropIfUnchanged bool) bool {
	if !d.capturing.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer d.capturing.Store(false)
		out := capture(ev)
		// The capture reads whatever has focus while it runs, which is up to three seconds after the tick loop decided which window this episode is for and ran skipReason against it. So the window is read again here, and text that came out of a different application is dropped rather than filed under this one: dwell in Brave, Alt-Tab to KeePassXC, and the vault's contents were written as app=Brave with the blocklist never consulted.
		if d.movedOn(ev) {
			out = captureOut{}
		}
		if dropIfUnchanged && out.text == "" {
			return
		}
		applyCapture(&ev, out)
		slog.Debug("activity tracked", "app", ev.App)
		if dropIfUnchanged {
			// Periodic re-capture: nothing is waiting on this send, so a full channel (the consumer busy, or stalled) must drop the event rather than hold this goroutine — the next re-capture is five minutes away regardless, but a blocked send here would also wedge d.capturing and stop every dwell-triggered capture behind it.
			select {
			case d.eventChan <- ev:
			default:
				slog.Debug("dropped a periodic recapture, the activity channel was full")
			}
			return
		}
		select {
		case d.eventChan <- ev:
		case <-ctx.Done():
		}
	}()
	return true
}

// movedOn reports whether the user left the window a capture was started for while that capture was running. Input: the activity the capture was started for. Output: true only when the tick loop has since polled a different, named application; no poll yet, or one that could not name what it found, reports false, because "cannot tell" must not throw away a capture that is probably fine.
// It reads the tick loop's own latest poll rather than asking the window reader again: the loop polls every couple of seconds regardless, so its answer is at most one interval stale, and a second reader would put two goroutines on a Tracker that is only ever called from one.
// Only the application is compared. A title that changed under the same application is the same application's text, which is a page named a little wrongly rather than another program's contents filed under this one.
func (d *Daemon) movedOn(ev Activity) bool {
	now := d.lastSeen.Load()
	if now == nil || now.App == "" || now.App == "Unknown" {
		return false
	}
	return now.App != ev.App
}

// noteWindow records the window the tick loop just polled, whether or not it is one worth recording. Input: the polled activity. Output: none. A window that is about to be skipped is exactly the one a capture in flight needs to know about, so this runs before skipReason.
func (d *Daemon) noteWindow(a Activity) { d.lastSeen.Store(&a) }

// watchMeetingWindow reads the window of a call in progress once a minute, whether or not it has focus, and emits it as an activity of its own.
// The focused window is the wrong window during a meeting: on 2026-08-31 a thirty-nine minute standup produced twenty-seven episodes and not one was the call, because the user spent it in ClickUp and a terminal. Participant tiles and presenter labels are the only things on the machine that name who is talking, and none of them were ever captured.
func (d *Daemon) watchMeetingWindow(ctx context.Context) {
	tick := time.NewTicker(d.meetingEvery)
	defer tick.Stop()
	// lastText is the title and body together, so a call whose body is empty still deduplicates on its title changing — which is how a browser-hosted meeting reports who joined.
	var lastText string
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if d.paused.Load() || d.screenLocked() {
				continue
			}
			app, title, text, ok := d.meeting()
			// Logged either way, because a silent watcher makes "no call is open" and "this never ran" look identical — which is exactly what happened on 2026-09-01, when two recorded meetings produced no meeting-window episode and nothing on disk could say which of the two it was.
			if !ok {
				slog.Debug("no call window on screen", "checked", "meeting watcher")
				continue
			}
			slog.Debug("read the call's window", "app", app, "runes", len([]rune(text)))
			// The title is captured even when the body is empty, which is the normal case rather than an edge one: a Chromium window exposes no accessibility text unless the browser was launched with --force-renderer-accessibility, and a meeting in a browser tab is how most calls happen here. The title alone is what names the other person — a Teams tab reads "Chat | Priya Shah | Microsoft Teams" — so requiring body text threw away the only thing on the machine that answers "who was in the room".
			// The watcher emits onto the same channel the tick loop does, so it goes through the same two calls the tick loop makes before a window becomes an episode: Normalize, so a window with no application name is still filed under one rather than reaching the store with App and Title both empty, and skipReason, which is where the blocklist, Ora's own window and an unidentifiable window are refused.
			act := *Normalize(app, title)
			act.ScreenText = text
			if reason := d.skipReason(act); reason != "" {
				slog.Debug("skipped the call's window", "reason", reason)
				continue
			}
			key := act.Title + "\x00" + text
			if key == lastText {
				continue
			}
			// lastText is only advanced once the activity is actually on the channel. Recording it before the send would mean one full channel silently retires this meeting's window for good: the text does not change from minute to minute, so every later read would match what was never sent and be skipped.
			// Non-blocking on purpose: a full channel means the consumer is busy, and the next tick is a minute away. No ctx case here — a select with a default never blocks, so one would be unreachable; shutdown is the outer select's job.
			select {
			case d.eventChan <- act:
				lastText = key
			default:
				slog.Debug("dropped a meeting window capture, the activity channel was full")
			}
		}
	}
}

type captureOut struct {
	text  string
	sight Sight
	// frames holds one JPEG per monitor, the monitor the user is on first.
	frames [][]byte
}

func applyCapture(ev *Activity, out captureOut) {
	ev.ScreenText = out.text
	ev.UserActivity = out.sight.UserActivity
	ev.VisibleText = out.sight.VisibleText
	if len(out.frames) > 0 {
		ev.ImageJPEG = out.frames[0]
		ev.ExtraJPEG = out.frames[1:]
	}
}

// tieredCapture reads accessibility text first (free), and only escalates to vision (screenshot -> LLM) when that text is too thin to describe what's on screen.
// Returns empty text when content is unchanged since the last capture, so callers never re-emit the same screen. Vision is gated behind thinTextThreshold and minVisionInterval to keep cost down.
func (d *Daemon) tieredCapture(ctx context.Context, act Activity, lastA11yText, lastVisionText *string, lastVisionTime *time.Time) captureOut {
	// The accessibility read takes no context — the platform readers each open their own connection — so it is bounded from out here instead.
	textCtx, cancelText := context.WithTimeout(ctx, d.bounds.text)
	text := withBudget(textCtx, func() string {
		t, err := d.text()
		if err != nil {
			return ""
		}
		return t
	})
	cancelText()
	d.lastCapture.Store(time.Now().UnixNano())

	// vision only escalates for a real foreground app, never the bare desktop — or we'd snap and describe the wallpaper on a loop while the user is idle.
	visionEnabled := d.visionFn != nil && isVisionWorthy(act)

	mediaCtx, cancelMedia := context.WithTimeout(ctx, d.bounds.media)
	mediaActive := withBudget(mediaCtx, func() bool { return d.media(mediaCtx) })
	cancelMedia()

	if !shouldUseVision(len([]rune(text)), visionEnabled, mediaActive, time.Since(*lastVisionTime)) {
		return resolveCapture(lastA11yText, lastVisionText, text, false, "", Sight{}, nil)
	}

	shotCtx, cancelShot := context.WithTimeout(ctx, d.bounds.screenshot)
	grab := withBudget(shotCtx, func() shot {
		png, err := d.screenshot(shotCtx)
		return shot{png: png, err: err}
	})
	cancelShot()
	if grab.err != nil || len(grab.png) == 0 {
		return resolveCapture(lastA11yText, lastVisionText, text, false, "", Sight{}, nil)
	}
	png := grab.png
	*lastVisionTime = time.Now()

	visionCtx, cancelVision := context.WithTimeout(ctx, d.bounds.vision)
	sight := withBudget(visionCtx, func() Sight { return d.visionFn(visionCtx, png) })
	cancelVision()
	// Vision capture: drop a11y entirely. Browser/TUI chrome is why screenshots of Ora itself polluted memory.
	// Searchable text is only the model's structured description (or the window title if the model returned nothing).
	desc := sight.Text()
	if desc == "" {
		desc = strings.TrimSpace(act.Title)
	}
	return resolveCapture(lastA11yText, lastVisionText, text, true, desc, sight, screenFrames(png))
}

// resolveCapture decides what a capture emits and updates the right per-tier "last text" tracker.
// The accessibility text is diffed against lastA11yText and that tracker is updated every call, regardless of
// which tier ends up being used — accessibility text is read unconditionally by tieredCapture above, so this
// keeps lastA11yText current even on a call where vision fires instead.
// When useVision is set, the emitted text (and change decision) comes from diffing desc against
// lastVisionText instead — a separate tracker, so a vision capture's stored description never gets compared
// against the next accessibility-tier capture's raw a11y text of the same, unchanged screen (that mismatch
// used to look like a change and cause a re-emit, even with nothing on screen actually different).
func resolveCapture(lastA11yText, lastVisionText *string, a11yText string, useVision bool, desc string, sight Sight, frames [][]byte) captureOut {
	a11yChanged := diff(lastA11yText, a11yText)
	if !useVision {
		return captureOut{text: a11yChanged}
	}
	shown := diff(lastVisionText, desc)
	if shown == "" {
		return captureOut{}
	}
	return captureOut{text: shown, sight: sight, frames: frames}
}

// nonWindowApps are the desktop/compositor/shell identifiers that mean no real app is focused. Without this gate the vision tier would screenshot and describe the wallpaper on every idle tick.
var nonWindowApps = map[string]struct{}{
	"":                {},
	"unknown":         {},
	"gnome-shell":     {},
	"org.gnome.shell": {},
	"gjs":             {},
	"mutter":          {},
	"plasmashell":     {},
	"kwin":            {},
	"desktop":         {},
}

// isVisionWorthy reports whether the focused window is a real application worth describing with the vision tier (vs. the bare desktop / compositor shell).
func isVisionWorthy(act Activity) bool {
	app := strings.ToLower(strings.TrimSpace(act.App))
	_, isShell := nonWindowApps[app]
	return !isShell
}

// shouldUseVision decides whether to escalate to the (expensive) vision tier: needs vision enabled and the rate limit cleared.
// Text length normally gates it too (thin text = accessibility can't describe the screen), but mediaActive (an MPRIS player "Playing") bypasses that — a browser tab playing video returns plenty of chrome text while describing nothing about the video itself.
func shouldUseVision(textLen int, visionEnabled bool, mediaActive bool, sinceLastVision time.Duration) bool {
	if !visionEnabled {
		return false
	}
	if !mediaActive && textLen >= thinTextThreshold {
		return false
	}
	return sinceLastVision >= minVisionInterval
}

// diff returns text only when it differs from *last, updating *last in place.
func diff(last *string, text string) string {
	if text == "" || text == *last {
		return ""
	}
	*last = text
	return text
}
