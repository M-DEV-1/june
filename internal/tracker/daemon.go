package tracker

import (
	"context"
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

type Daemon struct {
	eye       Tracker
	interval  time.Duration
	dwellTime time.Duration
	blocklist []string
	eventChan chan Activity
	capturer  func() string // injectable for tests; nil = real capture
	visionFn  func(ctx context.Context, png []byte) Sight
	paused    atomic.Bool
}

// Pause suspends activity emission. The polling loop still runs so Resume takes effect promptly.
// sessionLocked reports whether the desktop session's lock screen is up; the platform file sets it (Linux: GNOME's screensaver over D-Bus). Nil means no way to know, which reads as unlocked.
var sessionLocked func() bool

// SessionLocked is the exported read of the lock probe for other packages (the overnight dreaming loop uses it as its idle signal). False when the platform gives no way to know.
func SessionLocked() bool { return sessionLocked != nil && sessionLocked() }

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

func NewDaemon(eye Tracker, interval time.Duration, dwellTime time.Duration, blocklist []string, eventChan chan Activity) *Daemon {
	if interval <= 0 {
		interval = 2 * time.Second // default polling
	}
	if dwellTime <= 0 {
		dwellTime = 3 * time.Second // default dwell time
	}

	return &Daemon{eye: eye, interval: interval, dwellTime: dwellTime, blocklist: blocklist, eventChan: eventChan}
}

func (d *Daemon) Start(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	var lastActivity *Activity
	var pendingActivity *Activity
	var pendingSince time.Time
	var emittedCurrent bool
	var lastCaptureTime time.Time

	// use injected capturer (tests) or default to tiered capture with diff tracking. The default path needs the current activity so vision can skip the bare desktop; the test capturer ignores it.
	// lastA11yText/lastVisionText are tracked separately (not one shared "last text") so a tier switch on an
	// unchanged screen doesn't compare one tier's text against the other's and falsely look like a change.
	var lastA11yText, lastVisionText string
	var lastVisionTime time.Time
	capture := func(act Activity) captureOut {
		if d.capturer != nil {
			return captureOut{text: d.capturer()}
		}
		return d.tieredCapture(ctx, act, &lastA11yText, &lastVisionText, &lastCaptureTime, &lastVisionTime)
	}

	tracer := obs.GetTracer(ctx, "ora.tracker")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if d.paused.Load() {
				continue
			}

			_, span := tracer.Start(ctx, "Tracker.PollActiveWindow")

			// A locked screen is not the user's activity: capturing through the shield files the lock clock and "press a key to unlock" as episodes, which then surface in summaries as the day's doings.
			if sessionLocked != nil && sessionLocked() {
				pendingActivity = nil
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

			// A window nothing could identify carries zero information; filing it pollutes every later summary with "Unknown | Unknown" lines.
			if activity.App == "Unknown" && activity.Title == "Unknown" {
				pendingActivity = nil
				span.End()
				continue
			}

			blocked := MatchesBlocklist(activity.App, d.blocklist)

			if blocked {
				span.SetAttributes(attribute.Bool("tracker.blocked", true))
				pendingActivity = nil
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

					ev := *pendingActivity
					applyCapture(&ev, capture(*pendingActivity))

					slog.Info("activity tracked", "app", ev.App, "title", ev.Title)

					select {
					case d.eventChan <- ev:
						emittedCurrent = true
					case <-ctx.Done():
						span.End()
						return
					}
				} else {
					span.SetAttributes(
						attribute.Bool("tracker.changed", false),
						attribute.String("tracker.state", "dwelling"),
					)
				}
			} else if emittedCurrent && d.capturer == nil && time.Since(lastCaptureTime) >= recaptureInterval {
				// periodic re-capture: same window, 5 min elapsed
				out := capture(*lastActivity)
				if out.text != "" {
					ev := *lastActivity
					applyCapture(&ev, out)
					select {
					case d.eventChan <- ev:
					case <-ctx.Done():
						span.End()
						return
					default:
						// drop if channel full — next tick will retry
					}
				}
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
func (d *Daemon) tieredCapture(ctx context.Context, act Activity, lastA11yText, lastVisionText *string, lastCaptureTime, lastVisionTime *time.Time) captureOut {
	text, err := extractText()
	if err != nil {
		text = ""
	}
	*lastCaptureTime = time.Now()

	// vision only escalates for a real foreground app, never the bare desktop — or we'd snap and describe the wallpaper on a loop while the user is idle.
	visionEnabled := d.visionFn != nil && isVisionWorthy(act)
	mediaActive := mediaPlaying(ctx)
	if !shouldUseVision(len([]rune(text)), visionEnabled, mediaActive, time.Since(*lastVisionTime)) {
		return resolveCapture(lastA11yText, lastVisionText, text, false, "", Sight{}, nil)
	}

	png, err := grabScreen(ctx)
	if err != nil || len(png) == 0 {
		return resolveCapture(lastA11yText, lastVisionText, text, false, "", Sight{}, nil)
	}
	*lastVisionTime = time.Now()

	sight := d.visionFn(ctx, png)
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
