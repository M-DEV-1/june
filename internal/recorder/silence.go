package recorder

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"
)

// defaultSilenceAfter is how long BOTH streams may stay quiet before the user is told. It was 45 seconds on the call side alone, which fired on 2026-09-02 in the middle of the user's own standup update (everyone else muted, so the call side was exact zeros) and again a minute after a call had ended. Sound on either side now counts, and the window is minutes rather than seconds.
const defaultSilenceAfter = 3 * time.Minute

// silenceFloor is the sample magnitude below which a stream counts as silent. A stream recorded from a sink nothing plays into is exact zeros; a real room floor with nobody speaking still sits above this.
const silenceFloor = 64

// silenceWatch passes samples through to the real writer while remembering when sound last came through. A meeting playing to a sink Ora is not recording writes an unbroken run of zeros, which is indistinguishable from a working recording until the transcript comes back empty.
type silenceWatch struct {
	w    io.Writer
	last atomic.Int64 // unix nanoseconds of the last sample above the noise floor
}

func newSilenceWatch(w io.Writer) *silenceWatch {
	s := &silenceWatch{w: w}
	s.last.Store(time.Now().UnixNano())
	return s
}

func (s *silenceWatch) Write(p []byte) (int, error) {
	if hasSound(p) {
		s.last.Store(time.Now().UnixNano())
	}
	return s.w.Write(p)
}

// quietFor returns how long it has been since sound last came through.
func (s *silenceWatch) quietFor() time.Duration {
	return time.Since(time.Unix(0, s.last.Load()))
}

// shouldWarn decides whether the recording has gone dead. micQuiet and callQuiet are how long each stream has been below the noise floor; dropped is whether the capture reports a stream that is no longer running; window is defaultSilenceAfter or the test override.
// Input: the two quiet durations, the dropped flag and the window. Output: true when the user should be told, and a one-line reason for the notification body.
func shouldWarn(micQuiet, callQuiet time.Duration, dropped bool, window time.Duration) (bool, string) {
	if dropped {
		return true, "An audio stream stopped."
	}
	if micQuiet < window || callQuiet < window {
		return false, ""
	}
	return true, fmt.Sprintf("Nothing from the microphone or the call for %s.", window.Round(time.Second))
}

// watchSilence warns once, at any point in the recording, when shouldWarn says the recording is dead: every stream quiet for the window, or a stream dropped. The mid-call case it exists for is the output device changing under the recording — earbuds connecting, or dying and the audio hopping back to the speakers — which is otherwise silently lost for the rest of the meeting.
// ponytail: the warning tells the user to fix it by hand. Re-running the active-sink detection and re-attaching the monitor stream to the new sink mid-recording would fix it without them, and is the named follow-up.
func (r *Recorder) watchSilence(mic, call *silenceWatch, window time.Duration, dropped func() bool, done <-chan struct{}) {
	tick := time.NewTicker(window / 3)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			warn, reason := shouldWarn(mic.quietFor(), call.quietFor(), dropped(), window)
			if !warn {
				continue
			}
			slog.Warn("the recording has gone dead", "reason", reason, "mic quiet for", mic.quietFor().Round(time.Second), "call quiet for", call.quietFor().Round(time.Second))
			r.notify("Meeting audio isn't reaching the recorder", reason+" Check the output device, then restart the recording.")
			return
		}
	}
}

// hasSound reports whether any little-endian 16-bit sample in p is louder than silenceFloor.
func hasSound(p []byte) bool {
	for i := 0; i+1 < len(p); i += 2 {
		v := int16(binary.LittleEndian.Uint16(p[i:]))
		if v > silenceFloor || v < -silenceFloor {
			return true
		}
	}
	return false
}
