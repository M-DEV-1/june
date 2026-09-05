package tracker

import (
	"context"
	"testing"
	"time"
)

// The periodic re-capture path (dropIfUnchanged) must never block on a full event channel: the tick loop that started it is not waiting for a reply, and blocking here would stall every later capture behind this one goroutine. Only the dwell-emit path is allowed to wait for room (or for shutdown).
func TestCaptureAndEmit_PeriodicRecaptureDoesNotBlockOnAFullChannel(t *testing.T) {
	eventChan := make(chan Activity, 1)
	eventChan <- Activity{App: "already queued"}

	d := NewDaemon(nil, time.Second, time.Second, nil, eventChan)
	capture := func(Activity) captureOut { return captureOut{text: "new screen content"} }

	started := d.captureAndEmit(context.Background(), Activity{App: "Brave", Title: "a page"}, capture, true)
	if !started {
		t.Fatal("captureAndEmit reported a capture already in flight when none was")
	}

	deadline := time.After(time.Second)
	for d.capturing.Load() {
		select {
		case <-deadline:
			t.Fatal("the periodic recapture goroutine is still blocked on a full channel a second later")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if len(eventChan) != 1 {
		t.Fatalf("channel length = %d, want 1 (the dropped capture must not have been queued)", len(eventChan))
	}
}
