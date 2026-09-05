//go:build linux

package input

import "time"

// keyDelay is the pause after every synthesized key event; the shell drops keys sent faster than a person could type. Matches internal/tracker/act_linux.go's keyDelay.
const keyDelay = 12 * time.Millisecond

// paceEvents runs each event in order, calling wait(delay) after every one that succeeds. Input: the events to run, the delay to wait after each, and the wait function (time.Sleep in production, a recording stub in tests). Output: the first error hit, which stops the sequence before any later event runs or any wait for the failed event.
func paceEvents(events []func() error, delay time.Duration, wait func(time.Duration)) error {
	for _, ev := range events {
		if err := ev(); err != nil {
			return err
		}
		wait(delay)
	}
	return nil
}
