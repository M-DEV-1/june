package recorder

import (
	"strings"
	"testing"
	"time"
)

// Across every pairing of quiet times on either side of the window, shouldWarn warns exactly when a stream has been quiet for the whole window or has dropped, and says which side it was.
func TestShouldWarn_WarnsWhenEitherStreamIsQuietForTheWindow(t *testing.T) {
	const window = time.Minute
	quiet := []time.Duration{0, window / 2, window - time.Nanosecond, window, 2 * window, time.Hour}
	for _, mic := range quiet {
		for _, call := range quiet {
			for _, dropped := range []bool{false, true} {
				warn, reason := shouldWarn(mic, call, dropped, window)
				micDead, callDead := mic >= window, call >= window
				if want := dropped || micDead || callDead; warn != want {
					t.Errorf("mic quiet %s, call quiet %s, dropped %v: warn = %v, want %v", mic, call, dropped, warn, want)
					continue
				}
				if !warn || dropped {
					continue
				}
				want := map[[2]bool]string{{true, true}: "Nothing from the microphone or the call", {true, false}: "Nothing from the microphone for", {false, true}: "Nothing from the call for"}[[2]bool{micDead, callDead}]
				if !strings.HasPrefix(reason, want) {
					t.Errorf("mic quiet %s, call quiet %s: the reason is %q, want it to start %q", mic, call, reason, want)
				}
			}
		}
	}
}
