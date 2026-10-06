package tracker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Every call the capture path makes outside this process gets its own deadline, so one that never answers costs the tick loop that deadline and no more. Each seam below blocks forever and ignores the context it is handed — a gnome-shell that stopped replying to screenshots, an MPRIS player wedged on a property read, a model call that stalled — and the capture must still come back.
func TestTieredCapture_ReturnsWhenAnOutsideCallNeverAnswers(t *testing.T) {
	forever := make(chan struct{})
	defer close(forever)

	fakePNG := []byte("not really a png")

	cases := map[string]func(d *Daemon){
		"the text read never comes back": func(d *Daemon) {
			d.text = func() (string, error) { <-forever; return "", nil }
		},
		"the media player never answers the property read": func(d *Daemon) {
			d.media = func(context.Context) bool { <-forever; return false }
		},
		"the screenshot never comes back": func(d *Daemon) {
			d.screenshot = func(context.Context) ([]byte, error) { <-forever; return nil, nil }
		},
		"the vision model never replies": func(d *Daemon) {
			d.visionFn = func(context.Context, []byte) Sight { <-forever; return Sight{} }
		},
	}

	for name, wedge := range cases {
		t.Run(name, func(t *testing.T) {
			d := NewDaemon(nil, time.Second, time.Second, nil, nil)
			d.bounds = captureBounds{text: 20 * time.Millisecond, media: 20 * time.Millisecond, screenshot: 20 * time.Millisecond, vision: 20 * time.Millisecond}
			// Thin text so the capture escalates past accessibility and reaches the screenshot and the model.
			d.text = func() (string, error) { return "", nil }
			d.media = func(context.Context) bool { return false }
			d.screenshot = func(context.Context) ([]byte, error) { return fakePNG, nil }
			d.visionFn = func(context.Context, []byte) Sight { return Sight{UserActivity: "reading"} }
			wedge(d)

			var lastA11y, lastVision string
			lastVisionTime := time.Time{}

			done := make(chan time.Duration, 1)
			go func() {
				start := time.Now()
				d.tieredCapture(context.Background(), Activity{App: "Brave", Title: "a page"}, &lastA11y, &lastVision, &lastVisionTime)
				done <- time.Since(start)
			}()

			select {
			case elapsed := <-done:
				if elapsed > time.Second {
					t.Fatalf("the capture took %v against 20ms bounds", elapsed)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the capture never returned: the call that left the process had no deadline")
			}
		})
	}
}

// countingTracker records how many times the tick loop polled the active window, so a test can tell a loop that is still running from one that is stuck inside a capture.
type countingTracker struct {
	polls atomic.Int64
	act   Activity
}

func (c *countingTracker) GetActiveWindow() (*Activity, error) {
	c.polls.Add(1)
	a := c.act
	return &a, nil
}

// A capture that takes a long time must not cost a tick. The tick loop is what polls the active window, times dwell and drives recapture; running the capture on it means a slow screenshot or model call stops all three, and the ticker's one-slot buffer drops every tick missed in the meantime.
func TestStart_ASlowCaptureDoesNotStopTheTickLoop(t *testing.T) {
	eye := &countingTracker{act: Activity{App: "Code", Title: "daemon.go"}}
	events := make(chan Activity, 32)

	held := make(chan struct{})
	defer close(held)
	capturing := make(chan struct{}, 1)

	d := NewDaemon(eye, 5*time.Millisecond, 10*time.Millisecond, nil, events)
	d.SetCapturer(func() string {
		select {
		case capturing <- struct{}{}:
		default:
		}
		<-held
		return "some screen text"
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Start(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	select {
	case <-capturing:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop never reached a capture")
	}

	atStart := eye.polls.Load()
	time.Sleep(150 * time.Millisecond)
	after := eye.polls.Load()

	// 150ms of 5ms ticks is thirty polls; ten is a floor loose enough for a loaded machine and far above the zero a blocked loop manages.
	if after-atStart < 10 {
		t.Fatalf("the active window was polled %d times while a capture was in flight, so the capture is still on the tick goroutine", after-atStart)
	}
}

// A lock probe that never answers must not stop the tick loop. sessionLocked runs first thing on every tick, before the window read, so a gnome-shell wedged on the screensaver property held the loop for as long as it liked: no polling, no dwell emission, no recapture, and the ticker's one-slot buffer dropping every tick missed meanwhile.
func TestStart_AWedgedLockProbeDoesNotStopTheTickLoop(t *testing.T) {
	forever := make(chan struct{})

	restore := sessionLocked
	sessionLocked = func() bool { <-forever; return true }

	eye := &countingTracker{act: Activity{App: "Code", Title: "daemon.go"}}
	d := NewDaemon(eye, 5*time.Millisecond, 10*time.Millisecond, nil, make(chan Activity, 32))
	d.bounds.lock = 10 * time.Millisecond
	d.SetCapturer(func() string { return "" })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Start(ctx); close(done) }()

	time.Sleep(200 * time.Millisecond)
	polls := eye.polls.Load()

	cancel()
	close(forever)
	<-done
	sessionLocked = restore

	// 200ms of 5ms ticks against a 10ms probe budget is roughly thirteen polls; five is a floor loose enough for a loaded machine and far above the zero a wedged loop manages.
	if polls < 5 {
		t.Fatalf("the active window was polled %d times while the lock probe was wedged, want the tick loop still running", polls)
	}
}
