package tracker

import (
	"context"
	"fmt"
	"time"
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

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			activity, err := d.eye.GetActiveWindow()
			if err != nil {
				fmt.Printf("Couldn't get last active window: %v", err)
				continue
			}

			// updates at every logged window change
			if lastActivity == nil || activity.App != lastActivity.App || activity.Title != lastActivity.Title {
				lastActivity = activity
				select {
				case d.eventChan <- *activity: // successfully pushed
				case <-ctx.Done():
					return // user exit + pipe full // safe exit
				}
			}
		}
	}
}
