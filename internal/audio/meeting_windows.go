//go:build windows

package audio

import (
	"fmt"
	"io"
	"time"

	"github.com/moutend/go-wca/pkg/wca"
)

// MeetingCapture is a pair of running WASAPI recordings: the default communications microphone, and the loopback of the default playback device (everything the call plays, which is everyone else).
// MicStart and SystemStart are the wall-clock instants each stream began, so transcripts of the two files can be lined up on one clock.
type MeetingCapture struct {
	MicStart    time.Time
	SystemStart time.Time

	streams []*wasapiStream
}

// sampleRate16k is what whisper wants, and capturing at it avoids a resample step later.
const sampleRate16k = 16000

// StartMeetingCapture opens both streams and writes 16 kHz mono s16le samples into mic and system until Stop is called. The system side records the console default, the device the Linux side's default sink corresponds to: a browser call, and anything else that does not pick a device of its own, plays there, while the communications default is only where apps that ask for it play.
// Both are recordings, so they stay on the wall clock: the time a device was away, and the stretches the loopback delivers nothing because nothing plays, are written as silence, which keeps system.wav lined up with mic.wav and leaves it a file of silence rather than empty when the call side never made a sound.
// ponytail: a call routed to a device other than the default console one records silence on the system side; Linux follows the playing stream (meetingSink), Windows would need IAudioSessionManager2 on each render endpoint to find the one with a foreign active session.
// Input: two writers, one per stream. Output: a running capture, or an error if either stream could not be opened.
func StartMeetingCapture(mic, system io.Writer) (*MeetingCapture, error) {
	micStream, err := startWASAPI(wca.ECapture, wca.ECommunications, sampleRate16k, false, writeTo(mic))
	if err != nil {
		return nil, fmt.Errorf("open microphone stream: %w", err)
	}
	sysStream, err := startWASAPI(wca.ERender, wca.EConsole, sampleRate16k, false, writeTo(system))
	if err != nil {
		micStream.Close()
		return nil, fmt.Errorf("open loopback stream: %w", err)
	}
	return &MeetingCapture{
		MicStart:    micStream.started,
		SystemStart: sysStream.started,
		streams:     []*wasapiStream{micStream, sysStream},
	}, nil
}

// writeTo adapts a writer to a stream's emit callback.
func writeTo(w io.Writer) func([]byte) error {
	return func(pcm []byte) error {
		_, err := w.Write(pcm)
		return err
	}
}

// Dropped reports whether either stream has ended while the capture is still meant to be live: the device went away or a write failed. Input: none. Output: true when a stream is gone.
func (c *MeetingCapture) Dropped() bool {
	for _, s := range c.streams {
		if s.failed.Load() {
			return true
		}
	}
	return false
}

// Stop halts both streams and waits for their threads, so nothing writes after it returns and the caller can close the writers. The stream list is left in place because the silence watchdog may still call Dropped.
func (c *MeetingCapture) Stop() {
	for _, s := range c.streams {
		s.Close()
	}
}
