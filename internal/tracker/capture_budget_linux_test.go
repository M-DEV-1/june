//go:build linux

package tracker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// The accessibility walk states a time budget, but the context carrying it only reaches the method calls: dialling the accessibility bus goes through dbus.Dial, Auth and Hello, none of which take a context, so a bus that accepts the connection and then stops answering held the reader for as long as it liked. These three readers all sit on a goroutine the daemon needs back — the tick loop's own window poll among them — so each must come back at its deadline whatever the bus is doing.
func TestATSPIReadersGiveUpAtTheirDeadline(t *testing.T) {
	blocked := make(chan struct{})
	hangs := busDialer(func(context.Context) (*dbus.Conn, error) {
		<-blocked
		return nil, errors.New("the bus never answered")
	})
	dialBus.Store(&hangs)
	t.Cleanup(func() {
		dialBus.Store(nil)
		close(blocked)
	})

	readers := map[string]func(ctx context.Context){
		"the focused window's text":  func(ctx context.Context) { atspiTextWithin(ctx) },
		"the window that has focus":  func(ctx context.Context) { atspiActiveWindow(ctx) },
		"the call window on screen":  func(ctx context.Context) { meetingWindowWithin(ctx) },
		"a named window's own title": func(ctx context.Context) { WindowTitleFor(ctx, "brave") },
	}

	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()

			done := make(chan time.Duration, 1)
			go func() {
				start := time.Now()
				read(ctx)
				done <- time.Since(start)
			}()

			select {
			case elapsed := <-done:
				if elapsed > time.Second {
					t.Fatalf("the read took %v against a 30ms budget", elapsed)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the read never returned: the stated budget does not cover dialling the bus")
			}
		})
	}
}
