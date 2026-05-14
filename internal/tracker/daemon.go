package tracker

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
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
	tracer := otel.Tracer("ora.tracker")

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
				fmt.Printf("Couldn't get last active window: %v", err)
				span.End()
				continue
			}

			// updates at every logged window change
			if lastActivity == nil || activity.App != lastActivity.App || activity.Title != lastActivity.Title {
				span.SetAttributes(
					attribute.Bool("tracker.changed", true),
					attribute.String("tracker.app", activity.App),
				)
				lastActivity = activity
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
