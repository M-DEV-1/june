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

// Tiered-capture tuning. AT-SPI/UIA text is free and tried first; the vision
// tier (screenshot -> LLM) only fires when accessibility comes up nearly empty
// (browsers, movies, games, canvas) and no more often than minVisionInterval to
// keep token/compute cost bounded.
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
	visionFn  func(ctx context.Context, png []byte) string
	paused    atomic.Bool
}

// Pause suspends activity emission. The polling loop still runs so Resume takes effect promptly.
func (d *Daemon) Pause() { d.paused.Store(true) }

// Resume re-enables activity emission after a Pause.
func (d *Daemon) Resume() { d.paused.Store(false) }

// IsPaused reports whether tracking is currently paused.
func (d *Daemon) IsPaused() bool { return d.paused.Load() }

// SetCapturer replaces the screen capture function. Used in tests to avoid real capture.
func (d *Daemon) SetCapturer(fn func() string) {
	d.capturer = fn
}

// SetVisionFn injects the vision describer (image -> text). When set, the tiered
// capturer falls back to a screenshot + this function whenever accessibility text
// is too thin to be useful. nil disables the vision tier (text-only).
func (d *Daemon) SetVisionFn(fn func(ctx context.Context, png []byte) string) {
	d.visionFn = fn
}

// MatchesBlocklist reports whether app should be blocked from tracking. It
// normalizes both the activity's app name and each blocklist entry (lowercase,
// trim whitespace, strip a trailing ".exe") and matches by substring, so a
// single blocklist entry works across platforms: Windows app names carry a
// ".exe" suffix ("1Password.exe"), while Linux app identifiers from AT-SPI/
// X11/Wayland never do and often take reverse-DNS or lowercase-binary forms
// ("1Password", "1password", "org.keepassxc.KeePassXC", "com.bitwarden.desktop").
// An exact-match-only comparison against a Windows-only default list would
// silently never block these on Linux.
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

// normalizeAppIdentifier lowercases, trims whitespace, and strips a trailing
// ".exe" so Windows and Linux app-name forms can be compared uniformly.
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

	// use injected capturer (tests) or default to tiered capture with diff tracking.
	// the default path needs the current activity so the vision tier can skip the
	// bare desktop; the injected test capturer ignores it.
	var lastScreenText string
	var lastVisionTime time.Time
	capture := func(act Activity) string {
		if d.capturer != nil {
			return d.capturer()
		}
		return d.tieredCapture(ctx, act, &lastScreenText, &lastCaptureTime, &lastVisionTime)
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

			activity, err := d.eye.GetActiveWindow()
			if err != nil {
				span.RecordError(err)
				slog.Error("tracker: failed to get active window", "error", err)
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
					ev.ScreenText = capture(*pendingActivity)

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
				text := capture(*lastActivity)
				if text != "" {
					ev := *lastActivity
					ev.ScreenText = text
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

// tieredCapture reads accessibility text first (free) and only escalates to the
// vision tier (screenshot -> LLM) when that text is too thin to describe what's
// on screen. Returns "" when content is unchanged since the last capture, so
// callers never re-emit or re-summarize the same screen.
//
// Cost guards: vision is gated behind thinTextThreshold AND minVisionInterval,
// and the diff check below means a static thin screen is summarized at most once.
func (d *Daemon) tieredCapture(ctx context.Context, act Activity, lastText *string, lastCaptureTime, lastVisionTime *time.Time) string {
	text, err := extractText()
	if err != nil {
		text = ""
	}
	*lastCaptureTime = time.Now()

	// vision only escalates for a real foreground app — never the bare desktop,
	// or we'd snap and describe the wallpaper on a loop while the user is idle.
	visionEnabled := d.visionFn != nil && isVisionWorthy(act)
	if !shouldUseVision(len([]rune(text)), visionEnabled, time.Since(*lastVisionTime)) {
		return diff(lastText, text)
	}

	png, err := grabScreen(ctx)
	if err != nil || len(png) == 0 {
		return diff(lastText, text)
	}
	*lastVisionTime = time.Now()

	vtext := d.visionFn(ctx, png)
	combined := strings.TrimSpace(strings.TrimSpace(text) + "\n" + strings.TrimSpace(vtext))
	return diff(lastText, combined)
}

// nonWindowApps are the desktop/compositor/shell identifiers that mean "no real
// application is focused" — the bare desktop. extractText is thin for these, so
// without this gate the vision tier would screenshot and describe the wallpaper
// on every idle tick. Normalize() maps an empty app to "Unknown".
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

// isVisionWorthy reports whether the focused window is a real application worth
// describing with the vision tier (vs. the bare desktop / compositor shell).
func isVisionWorthy(act Activity) bool {
	app := strings.ToLower(strings.TrimSpace(act.App))
	_, isShell := nonWindowApps[app]
	return !isShell
}

// shouldUseVision decides whether to escalate to the (expensive) vision tier:
// only when vision is enabled, accessibility text is thin, and enough time has
// passed since the last vision call. This is the cost guard.
func shouldUseVision(textLen int, visionEnabled bool, sinceLastVision time.Duration) bool {
	if !visionEnabled {
		return false
	}
	if textLen >= thinTextThreshold {
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
