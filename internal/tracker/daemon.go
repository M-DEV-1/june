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
	return &Daemon{eye, interval, eventChan}
}

func (d *Daemon) Start(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	// short term mem
	var lastTitle string

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

			// starts with ""
			// updates at every logged window change
			if activity.Title != lastTitle {
				lastTitle = activity.Title
				d.eventChan <- *activity
			}
		}
	}
}
