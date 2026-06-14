package tracker

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"ora/internal/obs"

	"go.opentelemetry.io/otel/attribute"
)

const recaptureInterval = 5 * time.Minute

type Daemon struct {
	eye       Tracker
	interval  time.Duration
	dwellTime time.Duration
	blocklist []string
	eventChan chan Activity
	capturer  func() string // injectable for tests; nil = real OCR
}

// SetCapturer replaces the screen capture function. Used in tests to avoid real OCR.
func (d *Daemon) SetCapturer(fn func() string) {
	d.capturer = fn
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

	// use injected capturer or default to real OCR with diff tracking
	capturer := d.capturer
	if capturer == nil {
		var lastScreenText string
		capturer = func() string {
			return captureOCRDiff(&lastScreenText, &lastCaptureTime)
		}
	}

	tracer := obs.GetTracer(ctx, "ora.tracker")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, span := tracer.Start(ctx, "Tracker.PollActiveWindow")

			activity, err := d.eye.GetActiveWindow()
			if err != nil {
				span.RecordError(err)
				slog.Error("tracker: failed to get active window", "error", err)
				span.End()
				continue
			}

			blocked := false
			for _, blockedApp := range d.blocklist {
				if strings.EqualFold(activity.App, blockedApp) {
					blocked = true
					break
				}
			}

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
					ev.ScreenText = capturer()

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
				text := capturer()
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

// captureOCRDiff extracts screen text via UIA and returns it only if it changed since last capture.
// Updates lastText and lastCaptureTime in place.
func captureOCRDiff(lastText *string, lastCaptureTime *time.Time) string {
	text, err := extractText()
	if err != nil {
		return ""
	}

	*lastCaptureTime = time.Now()

	if text == *lastText {
		return "" // idle — same screen content
	}
	*lastText = text
	return text
}
