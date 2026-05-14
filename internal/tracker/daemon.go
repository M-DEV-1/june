package tracker

import (
	"context"
	"log/slog"
	"time"

	"ora/internal/obs"

	"go.opentelemetry.io/otel/attribute"
)

type Daemon struct {
	eye       Tracker
	interval  time.Duration
	eventChan chan Activity
}

func NewDaemon(eye Tracker, interval time.Duration, eventChan chan Activity) *Daemon {
	if interval <= 0 {
		interval = 2 * time.Second // default polling
	}

	return &Daemon{eye, interval, eventChan}
}

func (d *Daemon) Start(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	// short term mem (empty at first)
	var lastActivity *Activity
	var lastEmitTime time.Time
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

			// updates at every logged window change or 10 min heartbeat
			changed := lastActivity != nil && (activity.App != lastActivity.App || activity.Title != lastActivity.Title)
			if lastActivity == nil || changed || time.Since(lastEmitTime) > 10*time.Minute {
				span.SetAttributes(
					attribute.Bool("tracker.changed", changed),
					attribute.String("tracker.app", activity.App),
				)
				lastActivity = activity
				lastEmitTime = time.Now()
				select {
				case d.eventChan <- *activity: // successfully pushed
				case <-ctx.Done():
					span.End()
					return // user exit + pipe full // safe exit
				}
			} else {
				span.SetAttributes(attribute.Bool("tracker.changed", false))
			}
			span.End()
		}
	}
}
