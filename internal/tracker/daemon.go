package tracker

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"ora/internal/obs"

	"go.opentelemetry.io/otel/attribute"
)

type Daemon struct {
	eye       Tracker
	interval  time.Duration
	dwellTime time.Duration
	blocklist []string
	eventChan chan Activity
}

func NewDaemon(eye Tracker, interval time.Duration, dwellTime time.Duration, blocklist []string, eventChan chan Activity) *Daemon {
	if interval <= 0 {
		interval = 2 * time.Second // default polling
	}
	if dwellTime <= 0 {
		dwellTime = 3 * time.Second // default dwell time
	}

	return &Daemon{eye, interval, dwellTime, blocklist, eventChan}
}

func (d *Daemon) Start(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	var lastActivity *Activity
	var pendingActivity *Activity
	var pendingSince time.Time
	var emittedCurrent bool

	tracer := obs.GetTracer(ctx, "ora.tracker")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// this traces every single poll
			// not event drive, as i cant be bothered with windows api anymore
			// so i polled, since that is easier, and simpler plus very cheap on cpu
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

			// updates at every logged window change or 10 min heartbeat
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
				// window hasn't changed, check if it met dwell time
				if time.Since(pendingSince) >= d.dwellTime {
					span.SetAttributes(
						attribute.Bool("tracker.changed", false),
						attribute.String("tracker.state", "emitted"),
					)

					slog.Info("activity tracked", "app", pendingActivity.App, "title", pendingActivity.Title)

					select {
					case d.eventChan <- *pendingActivity: // successfully pushed
						emittedCurrent = true
					case <-ctx.Done():
						span.End()
						return // user exit + pipe full // safe exit
					}
				} else {
					span.SetAttributes(
						attribute.Bool("tracker.changed", false),
						attribute.String("tracker.state", "dwelling"),
					)
				}
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
